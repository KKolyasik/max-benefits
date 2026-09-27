// Package maxapi connects the bot to the MAX messenger Bot API through the
// official Go SDK: it receives updates by webhook or long polling, converts
// them to bot events and delivers bot messages while respecting the API rate
// limits.
package maxapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	maxbot "github.com/max-messenger/max-bot-api-client-go/v2"
	"github.com/max-messenger/max-bot-api-client-go/v2/model"

	"github.com/KKolyasik/max-benefits/internal/bot"
)

// MAX allows 30 requests per second per bot and two messages per second per
// dialog. Stay slightly below the global limit.
const (
	globalRPS   = 25
	dialogPause = 500 * time.Millisecond
	pollTimeout = 30 * time.Second
	maxBackoff  = 30 * time.Second
)

type Config struct {
	Token string
	// BaseURL overrides the API address, e.g. for tests. Empty means the
	// SDK default (https://platform-api2.max.ru).
	BaseURL string
	// CAFile is an extra root certificate to trust for the MAX API. The API
	// certificate is issued by the Russian Trusted Root CA, which is not in
	// the usual system bundles.
	CAFile string
}

// Client implements bot.Messenger on top of the MAX Bot API.
type Client struct {
	api    *maxbot.Api
	limits *limiter
	log    *slog.Logger
}

var _ bot.Messenger = (*Client)(nil)

func New(cfg Config, log *slog.Logger) (*Client, error) {
	if cfg.Token == "" {
		return nil, errors.New("empty bot token")
	}
	httpClient, err := newHTTPClient(cfg.CAFile, pollTimeout+15*time.Second, log)
	if err != nil {
		return nil, err
	}
	opts := []maxbot.Opt{
		maxbot.WithHTTPClient(httpClient),
		maxbot.WithPollingTimeout(pollTimeout),
	}
	if cfg.BaseURL != "" {
		opts = append(opts, maxbot.WithBaseURL(cfg.BaseURL))
	}
	api, err := maxbot.NewApi(cfg.Token, opts...)
	if err != nil {
		return nil, fmt.Errorf("init max api: %w", err)
	}
	return &Client{api: api, limits: newLimiter(globalRPS, dialogPause), log: log}, nil
}

// Me checks the token and returns the bot's name for logs.
func (c *Client) Me(ctx context.Context) (string, error) {
	info, err := c.api.Bots.GetMyInfo(ctx)
	if err != nil {
		return "", err
	}
	if info.Username != "" {
		return fmt.Sprintf("%s (@%s)", info.FirstName, info.Username), nil
	}
	return info.FirstName, nil
}

// Send implements bot.Messenger.
func (c *Client) Send(ctx context.Context, userID int64, msg bot.Message) error {
	if err := c.limits.wait(ctx, userID); err != nil {
		return err
	}
	m := maxbot.NewMessage().
		SetUser(userID).
		SetText(msg.Text).
		SetDisableLinkPreview(true)
	if msg.Markdown {
		m.SetFormat(model.FormatMarkdown)
	}
	if msg.Silent {
		m.WithoutNotify()
	}
	m.AddAttachments(attachments(msg.Keyboard))
	_, err := c.api.Messages.Send(ctx, m)
	return err
}

// AnswerCallback implements bot.Messenger.
func (c *Client) AnswerCallback(ctx context.Context, userID int64, callbackID string, answer bot.CallbackAnswer) error {
	if err := c.limits.wait(ctx, userID); err != nil {
		return err
	}
	var a model.CallbackAnswer
	if answer.Edit != nil {
		body := model.NewMessageBody{
			Text: answer.Edit.Text,
			// An empty array removes the keyboard; null would keep it.
			Attachments: attachments(answer.Edit.Keyboard),
		}
		if answer.Edit.Markdown {
			body.Format = model.FormatMarkdown
		}
		a.Message = &body
	}
	if answer.Notification != "" {
		a.Notification = &answer.Notification
	}
	res, err := c.api.Messages.AnswerOnCallback(ctx, callbackID, a)
	if err != nil {
		return err
	}
	if !res.Success {
		return fmt.Errorf("callback answer rejected: %s", res.Message)
	}
	return nil
}

// attachments converts a keyboard; it never returns nil.
func attachments(rows [][]bot.Button) []model.Attachment {
	if len(rows) == 0 {
		return []model.Attachment{}
	}
	kb := model.NewKeyboard()
	for _, row := range rows {
		r := kb.AddRow()
		for _, b := range row {
			if b.URL != "" {
				r.AddLink(b.Text, b.URL)
			} else {
				r.AddCallBack(b.Text, b.Payload)
			}
		}
	}
	return []model.Attachment{kb.Build()}
}

// Poll long-polls updates and passes bot events to handle until ctx is done.
// Network errors are retried with backoff, so a flaky connection never stops
// the bot.
func (c *Client) Poll(ctx context.Context, handle func(bot.Event)) {
	var marker int64
	backoff := time.Second
	for ctx.Err() == nil {
		updates, next, err := c.api.Subscriptions.GetUpdates(ctx, marker)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			// The long poll outlived the HTTP timeout: just poll again.
			if timeout := (*maxbot.TimeoutError)(nil); errors.As(err, &timeout) {
				continue
			}
			c.log.Warn("get updates failed", "err", err, "retry_in", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		backoff = time.Second
		if next != 0 {
			marker = next
		}
		for _, u := range updates {
			ev, ok := toEvent(u)
			if !ok {
				c.log.Debug("skip update", "type", u.UpdateType)
				continue
			}
			handle(ev)
		}
	}
}

// toEvent converts an update into a bot event. Only personal dialogs are
// supported: messages from group chats and channels are ignored.
func toEvent(u model.Update) (bot.Event, bool) {
	switch u.UpdateType {
	case model.UpdateBotStarted:
		user := u.GetUser()
		return bot.Event{Type: bot.EventStart, UserID: user.UserID}, user.UserID != 0

	case model.UpdateMessageCreated:
		m := u.GetMessage()
		if m.Recipient.ChatType != model.ChatTypeDialog || m.Sender.IsBot || m.Sender.UserID == 0 {
			return bot.Event{}, false
		}
		return bot.Event{Type: bot.EventText, UserID: m.Sender.UserID, Text: m.Body.Text}, true

	case model.UpdateMessageCallback:
		cb := u.GetCallback()
		m := u.GetMessage()
		// The message is missing if it was deleted before the press arrived.
		if m.Recipient.ChatType != "" && m.Recipient.ChatType != model.ChatTypeDialog {
			return bot.Event{}, false
		}
		if cb.User.UserID == 0 || cb.CallbackID == "" {
			return bot.Event{}, false
		}
		return bot.Event{
			Type:       bot.EventCallback,
			UserID:     cb.User.UserID,
			CallbackID: cb.CallbackID,
			Payload:    cb.Payload,
			SourceText: m.Body.Text,
		}, true
	}
	return bot.Event{}, false
}

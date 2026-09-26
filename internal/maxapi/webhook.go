package maxapi

import (
	"context"
	"fmt"
	"net/http"

	"github.com/max-messenger/max-bot-api-client-go/v2/model"

	"github.com/KKolyasik/max-benefits/internal/bot"
)

// A single update is a few kilobytes; anything bigger is not from MAX.
const maxWebhookBody = 1 << 20

// Webhook returns the HTTP handler for updates that MAX pushes to the bot.
// Requests without the secret are rejected. handle reports whether it took
// the event: if not, MAX gets 503 and delivers the update again later.
func (c *Client) Webhook(secret string, handle func(bot.Event) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBody)
		accepted := true
		// The SDK checks the secret and parses the update exactly as for
		// long polling; MAX gets 200 once the callback returns.
		c.api.GetHandler(func(_ context.Context, u model.Update) {
			ev, ok := toEvent(u)
			if !ok {
				c.log.Debug("skip update", "type", u.UpdateType)
				return
			}
			accepted = handle(ev)
		}, secret).ServeHTTP(w, r)
		if !accepted {
			http.Error(w, "shutting down", http.StatusServiceUnavailable)
		}
	})
}

// Subscribe points MAX at the webhook url and removes the bot's other
// webhooks, so every update goes to one place. It is safe on every start:
// MAX updates an existing subscription with the same url.
func (c *Client) Subscribe(ctx context.Context, url, secret string) error {
	hooks, err := c.Webhooks(ctx)
	if err != nil {
		return err
	}
	// Others are removed first: should MAX return our url in another
	// spelling, the subscription below brings it back.
	for _, h := range hooks {
		if h == url {
			continue
		}
		res, err := c.api.Subscriptions.Unsubscribe(ctx, h)
		if err != nil {
			return fmt.Errorf("remove webhook %s: %w", h, err)
		}
		if !res.Success {
			return fmt.Errorf("remove webhook %s: %s", h, res.Message)
		}
		c.log.Warn("removed another webhook of the bot", "url", h)
	}
	res, err := c.api.Subscriptions.Subscribe(ctx, url, secret, nil, "")
	if err != nil {
		return fmt.Errorf("subscribe to webhook %s: %w", url, err)
	}
	if !res.Success {
		return fmt.Errorf("subscribe to webhook %s: %s", url, res.Message)
	}
	return nil
}

// Webhooks returns the urls MAX sends the bot's updates to.
func (c *Client) Webhooks(ctx context.Context) ([]string, error) {
	res, err := c.api.Subscriptions.GetSubscriptions(ctx)
	if err != nil {
		return nil, fmt.Errorf("get webhooks: %w", err)
	}
	urls := make([]string, 0, len(res.Subscriptions))
	for _, s := range res.Subscriptions {
		urls = append(urls, s.URL)
	}
	return urls, nil
}

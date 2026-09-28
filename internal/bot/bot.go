// Package bot implements the dialogue as a finite state machine:
//
//	menu ──category──▶ survey ──last answer──▶ results ──▶ menu
//	  │                  ▲
//	  └──category (already filled)──▶ confirm ──"заполнить заново"──┘
//	                                     └──"показать подборку"──▶ results
//
// The state and answers live in session.Store and are saved after every
// event, before anything is sent, so progress is never lost.
//
// A pressed button changes its own message: the menu, the questions and the
// cards of the results are screens that replace one another in one message
// rather than a new message for every step. Only the admins' decisions on
// drafts stay in the chat as a trail.
package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/KKolyasik/max-benefits/internal/knowledge"
	"github.com/KKolyasik/max-benefits/internal/session"
	"github.com/KKolyasik/max-benefits/internal/survey"
)

type Bot struct {
	survey *survey.Survey
	kb     knowledge.Base
	store  session.Store
	out    Messenger
	log    *slog.Logger
	now    func() time.Time
	// mod and admins are set by WithModeration, adminForAll by
	// WithAdminForAll, agent by WithAgent.
	mod         Moderation
	admins      map[int64]bool
	adminForAll bool
	agent       Agent
}

func New(s *survey.Survey, kb knowledge.Base, store session.Store, out Messenger, log *slog.Logger) *Bot {
	return &Bot{survey: s, kb: kb, store: store, out: out, log: log, now: time.Now}
}

// reply is the outcome of a transition.
type reply struct {
	// screen takes the place of the message with the pressed button. A typed
	// command gets it as a new message.
	screen *Message
	// messages are sent as new ones, after the screen.
	messages []Message
	// notification is a toast for the one who pressed the button.
	notification string
	// forget deletes the session instead of saving it.
	forget bool
}

// show is a reply with the next screen.
func show(screen Message) reply {
	return reply{screen: &screen}
}

// Handle processes one user event. Events of the same user must be handled
// sequentially; the dispatcher guarantees that.
func (b *Bot) Handle(ctx context.Context, ev Event) error {
	b.log.Debug("event", "type", ev.Type, "user", ev.UserID, "payload", ev.Payload)

	sess, err := b.store.Load(ctx, ev.UserID)
	if err != nil {
		_ = b.deliver(ctx, ev, b.failure(ev))
		return fmt.Errorf("load session: %w", err)
	}

	r, transErr := b.transition(ctx, ev, sess)
	if transErr != nil {
		// Still save the session below: the answers given so far are
		// consistent and must not be lost because, say, the knowledge base
		// timed out.
		r = b.failure(ev)
	}

	if r.forget {
		err = b.store.Delete(ctx, ev.UserID)
	} else {
		sess.UpdatedAt = b.now()
		err = b.store.Save(ctx, sess)
	}
	if err != nil {
		_ = b.deliver(ctx, ev, b.failure(ev))
		return errors.Join(transErr, fmt.Errorf("persist session: %w", err))
	}

	return errors.Join(transErr, b.deliver(ctx, ev, r))
}

// failure tells the user something broke and offers the menu to go on.
func (b *Bot) failure(ev Event) reply {
	return show(b.menuMessage(ev.UserID, textFailed))
}

func (b *Bot) deliver(ctx context.Context, ev Event, r reply) error {
	var errs []error
	messages := r.messages
	switch {
	case ev.Type == EventCallback:
		answer := CallbackAnswer{Notification: r.notification}
		if ev.SourceText != "" {
			answer.Edit = r.screen
		} else if r.screen != nil {
			// The message was deleted before the press arrived: the screen
			// comes as a new one.
			messages = append([]Message{*r.screen}, messages...)
		}
		if answer.Edit == nil && answer.Notification == "" {
			// MAX needs either an edit or a notification to acknowledge a press.
			answer.Notification = "✓"
		}
		if err := b.out.AnswerCallback(ctx, ev.UserID, ev.CallbackID, answer); err != nil {
			errs = append(errs, fmt.Errorf("answer callback: %w", err))
		}
	case r.screen != nil:
		messages = append([]Message{*r.screen}, messages...)
	}
	for _, msg := range messages {
		if err := b.out.Send(ctx, ev.UserID, msg); err != nil {
			errs = append(errs, fmt.Errorf("send message: %w", err))
			break // keep the order: do not send the rest after a gap
		}
	}
	return errors.Join(errs...)
}

func (b *Bot) transition(ctx context.Context, ev Event, sess *session.Session) (reply, error) {
	switch ev.Type {
	case EventStart:
		b.toMenu(sess)
		return show(b.menuMessage(ev.UserID, textWelcome)), nil
	case EventText:
		return b.onText(ctx, ev, sess)
	case EventCallback:
		return b.onCallback(ctx, ev, sess)
	default:
		return reply{}, fmt.Errorf("unknown event type %d", ev.Type)
	}
}

func (b *Bot) onText(ctx context.Context, ev Event, sess *session.Session) (reply, error) {
	switch strings.ToLower(strings.TrimSpace(ev.Text)) {
	case "/start", "start", "начать", "старт", "привет":
		b.toMenu(sess)
		return show(b.menuMessage(ev.UserID, textWelcome)), nil
	case "/menu", "menu", "меню":
		b.toMenu(sess)
		return show(b.menuMessage(ev.UserID, textMenu)), nil
	case "/admin", "/drafts", "черновики":
		if b.isAdmin(ev.UserID) {
			b.toMenu(sess)
			return b.nextDraft(ctx, 0)
		}
	case "/agent", "агент":
		if b.isAdmin(ev.UserID) && b.agent != nil {
			b.toMenu(sess)
			return show(agentMessage()), nil
		}
	case "/profile", "анкета", "моя анкета":
		return show(b.profileMessage(sess.Answers)), nil
	case "/help", "/about", "помощь":
		return show(aboutMessage()), nil
	}

	// Free-form questions are out of scope for the MVP: gently steer the
	// user back to the buttons, repeating the current question if any.
	if c, q, pos := b.current(sess); q != nil {
		return show(b.questionMessage(c, q, pos, sess.Selected, textAnswerButton)), nil
	}
	b.toMenu(sess)
	return show(b.menuMessage(ev.UserID, textUseButtons+"\n\n"+textMenu)), nil
}

func (b *Bot) onCallback(ctx context.Context, ev Event, sess *session.Session) (reply, error) {
	action, args := parsePayload(ev.Payload)
	arg := func(i int) string {
		if i < len(args) {
			return args[i]
		}
		return ""
	}

	switch action {
	case actMenu:
		b.toMenu(sess)
		return show(b.menuMessage(ev.UserID, textMenu)), nil

	case actProfile:
		return show(b.profileMessage(sess.Answers)), nil

	case actAbout:
		return show(aboutMessage()), nil

	case actDelete:
		return show(deleteConfirmMessage()), nil

	case actDeleteConfirm:
		r := show(b.menuMessage(ev.UserID, textDeleted))
		r.forget = true
		return r, nil

	case actCategory:
		c, ok := b.survey.Category(arg(0))
		if !ok {
			return stale(), nil
		}
		return show(b.openCategory(sess, c)), nil

	case actUseSaved:
		c, ok := b.survey.Category(arg(0))
		if !ok {
			return stale(), nil
		}
		if !b.survey.Complete(c, sess.Answers) {
			// Questions were added to the category since the user filled it.
			return show(b.openCategory(sess, c)), nil
		}
		b.toMenu(sess)
		return b.results(ctx, c, sess.Answers, "", 0)

	case actRedo:
		c, ok := b.survey.Category(arg(0))
		if !ok {
			return stale(), nil
		}
		for _, qid := range c.Questions {
			delete(sess.Answers, qid)
		}
		return show(b.openCategory(sess, c)), nil

	case actResults, actCard:
		c, ok := b.survey.Category(arg(0))
		if !ok {
			return stale(), nil
		}
		if !b.survey.Complete(c, sess.Answers) {
			// The answers changed since the results were shown.
			return show(b.openCategory(sess, c)), nil
		}
		var id string
		part, _ := strconv.Atoi(arg(1))
		if len(args) > 2 {
			id = strings.Join(args[2:], ":")
		}
		return b.results(ctx, c, sess.Answers, id, part)

	case actAnswer:
		c, q, _ := b.current(sess)
		if q == nil || q.Multi || q.ID != arg(0) {
			return stale(), nil
		}
		o, ok := q.Option(arg(1))
		if !ok {
			return stale(), nil
		}
		sess.Answers[q.ID] = []string{o.ID}
		sess.Selected = nil
		return b.advance(ctx, c, sess)

	case actToggle:
		c, q, pos := b.current(sess)
		if q == nil || !q.Multi || q.ID != arg(0) {
			return stale(), nil
		}
		o, ok := q.Option(arg(1))
		if !ok {
			return stale(), nil
		}
		sess.Selected = toggle(q, sess.Selected, o)
		msg := b.questionMessage(c, q, pos, sess.Selected, "")
		if ev.SourceText != "" {
			// Only the ticks change. The text stays as it is, intro and all,
			// so the buttons do not jump under the finger.
			msg.Text = ev.SourceText
		}
		return show(msg), nil

	case actDone:
		c, q, _ := b.current(sess)
		if q == nil || !q.Multi || q.ID != arg(0) {
			return stale(), nil
		}
		if len(sess.Selected) == 0 {
			return reply{notification: textPickOne}, nil
		}
		sess.Answers[q.ID] = sess.Selected
		sess.Selected = nil
		return b.advance(ctx, c, sess)

	case actBack:
		c, q, pos := b.current(sess)
		if q == nil || q.ID != arg(0) || pos < 2 {
			return stale(), nil
		}
		// Forgetting the previous answer makes its question the current one
		// again; the old choice comes back ticked.
		prev := c.Questions[pos-2]
		sess.Selected = sess.Answers[prev]
		delete(sess.Answers, prev)
		q, pos = b.survey.Next(c, sess.Answers)
		return show(b.questionMessage(c, q, pos, sess.Selected, "")), nil

	case actDrafts, actApprove, actReject, actSkip, actAgent, actRun:
		return b.onAdmin(ctx, ev, action, arg(0))

	default:
		return stale(), nil
	}
}

func (b *Bot) toMenu(sess *session.Session) {
	sess.State = session.StateMenu
	sess.Category = ""
	sess.Selected = nil
}

// openCategory starts (or resumes) the category questionnaire, or offers to
// reuse the answers if it is already complete.
func (b *Bot) openCategory(sess *session.Session, c *survey.Category) Message {
	sess.Category = c.ID
	sess.Selected = nil
	if b.survey.Complete(c, sess.Answers) {
		sess.State = session.StateConfirm
		return b.confirmMessage(c, sess.Answers)
	}

	sess.State = session.StateSurvey
	q, pos := b.survey.Next(c, sess.Answers)
	note := strings.TrimSpace(c.Intro)
	if answered := b.survey.Answered(c, sess.Answers); answered > 0 {
		note = fmt.Sprintf("Часть ответов у меня уже есть, продолжаем с вопроса %d 👌", pos)
	}
	return b.questionMessage(c, q, pos, nil, note)
}

// advance asks the next question or, when the questionnaire is complete,
// shows the results.
func (b *Bot) advance(ctx context.Context, c *survey.Category, sess *session.Session) (reply, error) {
	if q, pos := b.survey.Next(c, sess.Answers); q != nil {
		return show(b.questionMessage(c, q, pos, nil, "")), nil
	}
	b.toMenu(sess)
	return b.results(ctx, c, sess.Answers, "", 0)
}

// results shows the cards found for the answers: the card with the given ID
// or, if there is no such card among them, the list of them all.
func (b *Bot) results(ctx context.Context, c *survey.Category, answers map[string][]string, id string, part int) (reply, error) {
	entries, err := b.kb.Find(ctx, knowledge.Request{Category: c.ID, Answers: answers})
	if err != nil {
		return reply{}, fmt.Errorf("knowledge base: %w", err)
	}
	if card, ok := cardMessage(c, entries, id, part); ok {
		return show(card), nil
	}
	return show(resultsMessage(c, entries)), nil
}

// current returns the question the user is expected to answer now.
func (b *Bot) current(sess *session.Session) (*survey.Category, *survey.Question, int) {
	if sess.State != session.StateSurvey {
		return nil, nil, 0
	}
	c, ok := b.survey.Category(sess.Category)
	if !ok {
		return nil, nil, 0
	}
	q, pos := b.survey.Next(c, sess.Answers)
	return c, q, pos
}

// toggle flips an option in a multi-choice selection. Selecting an exclusive
// option ("ничего из этого") clears the others and vice versa. The result
// keeps the question's option order.
func toggle(q *survey.Question, selected []string, o *survey.Option) []string {
	on := !slices.Contains(selected, o.ID)
	var out []string
	for _, opt := range q.Options {
		picked := slices.Contains(selected, opt.ID)
		switch {
		case opt.ID == o.ID:
			picked = on
		case on && (o.Exclusive || opt.Exclusive):
			picked = false
		}
		if picked {
			out = append(out, opt.ID)
		}
	}
	return out
}

// freeze keeps the pressed message in the chat with the choice made and
// without its buttons, and the rest of the reply comes in new messages. This
// way the admins' decisions leave a trail; other presses flip the screen.
func freeze(ev Event, choice string, r reply) reply {
	if ev.SourceText == "" {
		r.notification = choice
		return r
	}
	r.screen = &Message{Text: ev.SourceText + "\n\n👉 " + choice, Keyboard: [][]Button{}}
	return r
}

// stale answers a press on a button that no longer fits the dialog, e.g. a
// second tap before the screen changed or a button of an old message. The
// user is told, and the message is left as it is: it may be the current
// screen already.
func stale() reply {
	return reply{notification: textStale}
}

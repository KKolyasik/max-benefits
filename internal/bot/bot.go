// Package bot implements the dialogue as a finite state machine:
//
//	menu ──category──▶ survey ──last answer──▶ results ──▶ menu
//	  │                  ▲
//	  └──category (already filled)──▶ confirm ──"заполнить заново"──┘
//	                                     └──"показать подборку"──▶ results
//
// The state and answers live in session.Store and are saved after every
// event, before anything is sent, so progress is never lost.
package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
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
	// mod and admins are set by WithModeration.
	mod    Moderation
	admins map[int64]bool
}

func New(s *survey.Survey, kb knowledge.Base, store session.Store, out Messenger, log *slog.Logger) *Bot {
	return &Bot{survey: s, kb: kb, store: store, out: out, log: log, now: time.Now}
}

// reply is the outcome of a transition: what to answer to the pressed button
// and which messages to send afterwards.
type reply struct {
	answer   *CallbackAnswer
	messages []Message
	// forget deletes the session instead of saving it.
	forget bool
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
	r := reply{messages: []Message{b.menuMessage(ev.UserID, textFailed)}}
	if ev.Type == EventCallback {
		r.answer = &CallbackAnswer{Notification: textFailed}
	}
	return r
}

func (b *Bot) deliver(ctx context.Context, ev Event, r reply) error {
	var errs []error
	if ev.Type == EventCallback {
		answer := r.answer
		if answer == nil {
			answer = freeze(ev, "")
		}
		if answer.Edit == nil && answer.Notification == "" {
			// MAX needs either an edit or a notification to acknowledge a press.
			answer.Notification = "✓"
		}
		if err := b.out.AnswerCallback(ctx, ev.UserID, ev.CallbackID, *answer); err != nil {
			errs = append(errs, fmt.Errorf("answer callback: %w", err))
		}
	}
	for _, msg := range r.messages {
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
		return reply{messages: []Message{b.menuMessage(ev.UserID, textWelcome)}}, nil
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
		return reply{messages: []Message{b.menuMessage(ev.UserID, textWelcome)}}, nil
	case "/menu", "menu", "меню":
		b.toMenu(sess)
		return reply{messages: []Message{b.menuMessage(ev.UserID, textMenu)}}, nil
	case "/admin", "/drafts", "черновики":
		if b.isAdmin(ev.UserID) {
			b.toMenu(sess)
			return b.nextDraft(ctx, 0)
		}
	case "/profile", "анкета", "моя анкета":
		return reply{messages: []Message{b.profileMessage(sess.Answers)}}, nil
	case "/help", "/about", "помощь":
		return reply{messages: []Message{aboutMessage()}}, nil
	}

	// Free-form questions are out of scope for the MVP: gently steer the
	// user back to the buttons, repeating the current question if any.
	if c, q, pos := b.current(sess); q != nil {
		return reply{messages: []Message{b.questionMessage(c, q, pos, sess.Selected, textAnswerButton)}}, nil
	}
	b.toMenu(sess)
	return reply{messages: []Message{b.menuMessage(ev.UserID, textUseButtons+"\n\n"+textMenu)}}, nil
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
		return reply{answer: freeze(ev, labelMenu), messages: []Message{b.menuMessage(ev.UserID, textMenu)}}, nil

	case actProfile:
		return reply{answer: freeze(ev, labelProfile), messages: []Message{b.profileMessage(sess.Answers)}}, nil

	case actAbout:
		return reply{answer: freeze(ev, labelAbout), messages: []Message{aboutMessage()}}, nil

	case actDelete:
		return reply{answer: freeze(ev, labelDelete), messages: []Message{deleteConfirmMessage()}}, nil

	case actDeleteConfirm:
		return reply{
			answer:   freeze(ev, labelDeleteYes),
			messages: []Message{b.menuMessage(ev.UserID, textDeleted)},
			forget:   true,
		}, nil

	case actCategory:
		c, ok := b.survey.Category(arg(0))
		if !ok {
			return stale(ev), nil
		}
		r := b.openCategory(sess, c)
		r.answer = freeze(ev, c.Title)
		return r, nil

	case actUseSaved:
		c, ok := b.survey.Category(arg(0))
		if !ok {
			return stale(ev), nil
		}
		var r reply
		var err error
		if b.survey.Complete(c, sess.Answers) {
			b.toMenu(sess)
			r, err = b.results(ctx, c, sess)
		} else {
			// Questions were added to the category since the user filled it.
			r = b.openCategory(sess, c)
		}
		r.answer = freeze(ev, labelUseSaved)
		return r, err

	case actRedo:
		c, ok := b.survey.Category(arg(0))
		if !ok {
			return stale(ev), nil
		}
		for _, qid := range c.Questions {
			delete(sess.Answers, qid)
		}
		r := b.openCategory(sess, c)
		r.answer = freeze(ev, labelRedo)
		return r, nil

	case actAnswer:
		c, q, _ := b.current(sess)
		if q == nil || q.Multi || q.ID != arg(0) {
			return stale(ev), nil
		}
		o, ok := q.Option(arg(1))
		if !ok {
			return stale(ev), nil
		}
		sess.Answers[q.ID] = []string{o.ID}
		r, err := b.advance(ctx, c, sess)
		r.answer = freeze(ev, o.Title)
		return r, err

	case actToggle:
		c, q, pos := b.current(sess)
		if q == nil || !q.Multi || q.ID != arg(0) {
			return stale(ev), nil
		}
		o, ok := q.Option(arg(1))
		if !ok {
			return stale(ev), nil
		}
		sess.Selected = toggle(q, sess.Selected, o)
		edit := b.questionMessage(c, q, pos, sess.Selected, "")
		return reply{answer: &CallbackAnswer{Edit: &edit}}, nil

	case actDone:
		c, q, _ := b.current(sess)
		if q == nil || !q.Multi || q.ID != arg(0) {
			return stale(ev), nil
		}
		if len(sess.Selected) == 0 {
			return reply{answer: &CallbackAnswer{Notification: textPickOne}}, nil
		}
		sess.Answers[q.ID] = sess.Selected
		sess.Selected = nil
		r, err := b.advance(ctx, c, sess)
		r.answer = freeze(ev, strings.Join(q.Titles(sess.Answers[q.ID]), ", "))
		return r, err

	case actDrafts, actApprove, actReject, actSkip:
		return b.onAdmin(ctx, ev, action, arg(0))

	default:
		return stale(ev), nil
	}
}

func (b *Bot) toMenu(sess *session.Session) {
	sess.State = session.StateMenu
	sess.Category = ""
	sess.Selected = nil
}

// openCategory starts (or resumes) the category questionnaire, or offers to
// reuse the answers if it is already complete.
func (b *Bot) openCategory(sess *session.Session, c *survey.Category) reply {
	sess.Category = c.ID
	sess.Selected = nil
	if b.survey.Complete(c, sess.Answers) {
		sess.State = session.StateConfirm
		return reply{messages: []Message{b.confirmMessage(c, sess.Answers)}}
	}

	sess.State = session.StateSurvey
	q, pos := b.survey.Next(c, sess.Answers)
	preface := c.Title
	if c.Intro != "" {
		preface += "\n" + strings.TrimSpace(c.Intro)
	}
	if answered := b.survey.Answered(c, sess.Answers); answered > 0 {
		preface = fmt.Sprintf("%s\nЧасть ответов у меня уже есть, продолжаем с вопроса %d 👌", c.Title, pos)
	}
	return reply{messages: []Message{b.questionMessage(c, q, pos, nil, preface)}}
}

// advance asks the next question or, when the questionnaire is complete,
// shows the results.
func (b *Bot) advance(ctx context.Context, c *survey.Category, sess *session.Session) (reply, error) {
	if q, pos := b.survey.Next(c, sess.Answers); q != nil {
		return reply{messages: []Message{b.questionMessage(c, q, pos, sess.Selected, "")}}, nil
	}
	b.toMenu(sess)
	return b.results(ctx, c, sess)
}

func (b *Bot) results(ctx context.Context, c *survey.Category, sess *session.Session) (reply, error) {
	entries, err := b.kb.Find(ctx, knowledge.Request{Category: c.ID, Answers: sess.Answers})
	if err != nil {
		return reply{}, fmt.Errorf("knowledge base: %w", err)
	}
	return reply{messages: resultMessages(c, entries)}, nil
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

// freeze answers a button press by replacing the message's keyboard with the
// chosen option, so the chat reads like a conversation and old buttons
// cannot be pressed again.
func freeze(ev Event, choice string) *CallbackAnswer {
	if ev.SourceText == "" {
		return &CallbackAnswer{Notification: choice}
	}
	return &CallbackAnswer{Edit: frozen(ev.SourceText, choice)}
}

func frozen(text, choice string) *Message {
	if choice != "" {
		text += "\n\n👉 " + choice
	}
	return &Message{Text: text, Keyboard: [][]Button{}}
}

// stale handles a press on a button of an outdated message: the keyboard is
// removed and the user is told why nothing happened.
func stale(ev Event) reply {
	answer := &CallbackAnswer{Notification: textStale}
	if ev.SourceText != "" {
		answer.Edit = frozen(ev.SourceText, "")
	}
	return reply{answer: answer}
}

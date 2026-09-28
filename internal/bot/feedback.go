package bot

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/KKolyasik/max-benefits/internal/feedback"
	"github.com/KKolyasik/max-benefits/internal/session"
	"github.com/KKolyasik/max-benefits/internal/survey"
)

// maxFeedbackPerDay bounds what one user writes in a day: every feedback
// reaches the admins' phones.
const maxFeedbackPerDay = 5

// maxFeedbackRunes is how much of a feedback the admins read in the chat,
// with room for what it is about. The base keeps it whole.
const maxFeedbackRunes = 2500

// Feedback keeps what students write to the team.
type Feedback interface {
	AddFeedback(ctx context.Context, f feedback.Feedback) (int64, error)
	// UserFeedbackSince counts what the user has written since the time.
	UserFeedbackSince(ctx context.Context, userID int64, since time.Time) (int, error)
	// PendingFeedback counts the feedback no admin has resolved.
	PendingFeedback(ctx context.Context) (int, error)
	// NextFeedback returns the oldest unresolved feedback with an ID above
	// after.
	NextFeedback(ctx context.Context, after int64) (feedback.Feedback, bool, error)
	ResolveFeedback(ctx context.Context, id, admin int64) error
	// ForgetFeedbackAnswers deletes the answers kept with the user's
	// feedback; the texts stay.
	ForgetFeedbackAnswers(ctx context.Context, userID int64) error
}

// WithFeedback lets students write to the team: from the menu, about a
// question with no fitting option and about a category where nothing was
// found. The admins get every feedback at once and review them in the chat.
func (b *Bot) WithFeedback(f Feedback) *Bot {
	b.feedback = f
	return b
}

// askFeedback asks the user to write to the team; the next text is the
// feedback.
func (b *Bot) askFeedback(ctx context.Context, userID int64, sess *session.Session, category, question string) (reply, error) {
	if b.feedback == nil {
		return stale(), nil
	}
	c, q, ok := b.feedbackTopic(category, question)
	if !ok {
		return stale(), nil
	}
	n, err := b.feedback.UserFeedbackSince(ctx, userID, b.now().Add(-24*time.Hour))
	if err != nil {
		return reply{}, err
	}
	if n >= maxFeedbackPerDay {
		return reply{notification: textWroteEnough}, nil
	}
	// The ticks stay for the question the user comes back to.
	if cc, cq, _ := b.current(sess); cq == nil || cc != c || cq != q {
		sess.Selected = nil
	}
	sess.State = session.StateFeedback
	sess.Category = category
	sess.Question = question
	return show(feedbackPrompt(c, q, "")), nil
}

// feedbackTopic finds what a feedback is about: a question of a category, a
// category, or nothing. ok is false if the survey has no such category or
// question.
func (b *Bot) feedbackTopic(category, question string) (c *survey.Category, q *survey.Question, ok bool) {
	if category == "" {
		return nil, nil, question == ""
	}
	if c, ok = b.survey.Category(category); !ok {
		return nil, nil, false
	}
	if question == "" {
		return c, nil, true
	}
	if q, ok = b.survey.Question(question); !ok || !slices.Contains(c.Questions, question) {
		return nil, nil, false
	}
	return c, q, true
}

// feedbackPrompt asks to write the feedback; note goes on top.
func feedbackPrompt(c *survey.Category, q *survey.Question, note string) Message {
	text := textAskIdea
	switch {
	case q != nil:
		text = fmt.Sprintf(textAskOption, q.Label)
	case c != nil:
		text = fmt.Sprintf(textAskMissing, c.Title)
	}
	if note != "" {
		text = note + "\n\n" + text
	}
	return Message{Text: text, Keyboard: [][]Button{{{Text: labelCancel, Payload: actFeedbackCancel}}}}
}

// takeFeedback saves what the user wrote, passes it to the admins and takes
// the user back to where the feedback started.
func (b *Bot) takeFeedback(ctx context.Context, userID int64, sess *session.Session, text string) (reply, error) {
	c, q, _ := b.feedbackTopic(sess.Category, sess.Question)
	text = strings.TrimSpace(text)
	if text == "" {
		// A sticker or a photo without a caption.
		return show(feedbackPrompt(c, q, textAskText)), nil
	}
	f := feedback.Feedback{UserID: userID, Category: sess.Category, Question: sess.Question, Text: text, CreatedAt: b.now()}
	if c != nil && q == nil {
		f.Answers = map[string][]string{}
		for _, qid := range c.Questions {
			if a := sess.Answers[qid]; len(a) > 0 {
				f.Answers[qid] = a
			}
		}
	}
	id, err := b.feedback.AddFeedback(ctx, f)
	if err != nil {
		return reply{}, err
	}
	f.ID = id
	b.notifyFeedback(ctx, f)
	return b.closeFeedback(ctx, userID, sess, true)
}

// closeFeedback takes the user back to where the feedback started: the
// question, the empty results or the menu. sent tells whether the user wrote
// the feedback or cancelled it.
func (b *Bot) closeFeedback(ctx context.Context, userID int64, sess *session.Session, sent bool) (reply, error) {
	c, q, _ := b.feedbackTopic(sess.Category, sess.Question)
	b.leaveFeedback(sess)
	switch {
	case q != nil:
		if next, pos := b.survey.Next(c, sess.Answers); next != nil {
			var note string
			if sent {
				note = textThanksBack
			}
			return show(b.questionMessage(c, next, pos, sess.Selected, note)), nil
		}
	case c != nil && !sent && b.survey.Complete(c, sess.Answers):
		return b.results(ctx, c, sess.Answers, "", 0)
	}
	b.toMenu(sess)
	text := textMenu
	if sent {
		text = textThanks + "\n\n" + textMenu
	}
	return show(b.menuMessage(userID, text)), nil
}

// leaveFeedback stops waiting for the feedback: the user is back at the
// questionnaire the feedback is about, if any, or in the menu.
func (b *Bot) leaveFeedback(sess *session.Session) {
	if sess.State != session.StateFeedback {
		return
	}
	if sess.Question == "" {
		b.toMenu(sess)
		return
	}
	sess.State = session.StateSurvey
	sess.Question = ""
}

// notifyFeedback passes the feedback to the admins at once. It is saved
// already, so a failure only goes to the log: the admins find the feedback
// under «Отзывы».
func (b *Bot) notifyFeedback(ctx context.Context, f feedback.Feedback) {
	msg := Message{
		Text:     fmt.Sprintf("💬 Новый отзыв №%d\n%s", f.ID, b.feedbackText(f)),
		Keyboard: [][]Button{{{Text: labelResolve, Payload: payload(actResolveNotice, strconv.FormatInt(f.ID, 10))}}},
		Silent:   b.night(),
	}
	for id := range b.admins {
		if err := b.out.Send(ctx, id, msg); err != nil {
			b.log.Error("notify an admin of feedback", "admin", id, "feedback", f.ID, "err", err)
		}
	}
}

// feedbackText tells who wrote the feedback, from where and what. It is
// plain text: what a student wrote is shown as it is, and a resolved
// notification is frozen from the text MAX sends with the press, which has
// no markup.
func (b *Bot) feedbackText(f feedback.Feedback) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "От пользователя %d, %s\n📍 Откуда: ", f.UserID, f.CreatedAt.Format("02.01.2006"))
	c, known := b.survey.Category(f.Category)
	title := f.Category
	if known {
		title = c.Title
	}
	switch {
	case f.Category == "":
		sb.WriteString("главное меню")
	case f.Question != "":
		label := f.Question
		if q, ok := b.survey.Question(f.Question); ok {
			label = q.Label
		}
		fmt.Fprintf(&sb, "%s → вопрос «%s», нет подходящего варианта", title, label)
	case len(f.Answers) == 0:
		// Nothing is found only for a complete questionnaire: the answers
		// are gone with the data the student deleted.
		fmt.Fprintf(&sb, "%s, ничего не нашлось; ответы анкеты студент удалил", title)
	case known:
		fmt.Fprintf(&sb, "%s, ничего не нашлось по ответам:\n%s", title, b.summary(c.Questions, f.Answers))
	default:
		fmt.Fprintf(&sb, "%s, ничего не нашлось", title)
	}
	sb.WriteString("\n\n")
	sb.WriteString(cut(f.Text, maxFeedbackRunes))
	return sb.String()
}

// onInbox handles the admin buttons of the feedback. For anyone else they
// are stale.
func (b *Bot) onInbox(ctx context.Context, ev Event, action, arg string) (reply, error) {
	if !b.isAdmin(ev.UserID) || b.feedback == nil {
		return stale(), nil
	}
	if action == actInbox {
		return b.nextFeedback(ctx, 0)
	}
	id, err := strconv.ParseInt(arg, 10, 64)
	if err != nil {
		return stale(), nil
	}
	if action == actPass {
		return b.nextFeedback(ctx, id)
	}
	if err := b.feedback.ResolveFeedback(ctx, id, ev.UserID); err != nil {
		return reply{}, err
	}
	if action == actResolveNotice {
		return freeze(ev, textResolved, reply{}), nil
	}
	r, err := b.nextFeedback(ctx, id)
	r.notification = textResolved
	return r, err
}

// nextFeedback shows the oldest unresolved feedback after the given ID. The
// feedback is flipped through in one message, like the cards of the results.
func (b *Bot) nextFeedback(ctx context.Context, after int64) (reply, error) {
	f, ok, err := b.feedback.NextFeedback(ctx, after)
	if err != nil {
		return reply{}, err
	}
	pending, err := b.feedback.PendingFeedback(ctx)
	if err != nil {
		return reply{}, err
	}
	menu := []Button{{Text: labelMenu, Payload: actMenu}}
	switch {
	case ok:
		id := strconv.FormatInt(f.ID, 10)
		return show(Message{
			Text: fmt.Sprintf("📬 Отзыв №%d · неразобранных: %d\n%s", f.ID, pending, b.feedbackText(f)),
			Keyboard: [][]Button{
				{{Text: labelResolve, Payload: payload(actResolve, id)}, {Text: labelSkip, Payload: payload(actPass, id)}},
				menu,
			},
		}), nil
	case pending > 0:
		// The admin skipped the rest: offer to go round again.
		return show(Message{Text: fmt.Sprintf(textLastFeedback, pending), Keyboard: [][]Button{
			{{Text: labelFromStart, Payload: actInbox}},
			menu,
		}}), nil
	default:
		return show(Message{Text: textNoFeedback, Keyboard: [][]Button{menu}}), nil
	}
}

package bot

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/KKolyasik/max-benefits/internal/knowledge"
	"github.com/KKolyasik/max-benefits/internal/moderation"
)

// Moderation is what the admin screens need: the agent's drafts and the
// published cards. Approving a draft shows its card to students at once.
type Moderation interface {
	PendingDrafts(ctx context.Context) (int, error)
	// NextDraft returns the oldest pending draft with an ID above after.
	NextDraft(ctx context.Context, after int64) (moderation.Draft, bool, error)
	Card(ctx context.Context, id string) (knowledge.Card, bool, error)
	ApproveDraft(ctx context.Context, id, admin int64) (card knowledge.Card, replaced bool, err error)
	RejectDraft(ctx context.Context, id, admin int64) error
}

// WithModeration lets the admins review drafts in the chat. Admin rights are
// checked on every press, not only by hiding the buttons.
func (b *Bot) WithModeration(mod Moderation, admins []int64) *Bot {
	b.mod = mod
	b.admins = map[int64]bool{}
	for _, id := range admins {
		b.admins[id] = true
	}
	return b
}

func (b *Bot) isAdmin(userID int64) bool {
	return b.mod != nil && b.admins[userID]
}

// Agent is the agent that drafts the cards.
type Agent interface {
	// RunAgent asks the agent to run now; force makes it read the pages
	// that did not change too.
	RunAgent(ctx context.Context, force bool) error
}

// WithAgent lets the admins run the agent from the chat.
func (b *Bot) WithAgent(a Agent) *Bot {
	b.agent = a
	return b
}

// NotifyRun tells the admins how a run of the agent goes: that it started,
// if an admin started it, and how it ended. At night the news comes without
// a sound.
func (b *Bot) NotifyRun(ctx context.Context, r moderation.Run) error {
	if b.mod == nil {
		return nil
	}
	var msg Message
	switch r.Status {
	case moderation.RunStarted:
		if r.Trigger != moderation.ByCommand {
			return nil
		}
		msg.Text = textRunStarted
	case moderation.RunBusy:
		msg.Text = textRunBusy
	case moderation.RunDone, moderation.RunFailed:
		pending, err := b.mod.PendingDrafts(ctx)
		if err != nil {
			return err
		}
		msg.Text = runText(r, pending)
		if pending > 0 {
			msg.Keyboard = [][]Button{{{Text: labelReview, Payload: actDrafts}}}
		}
	default:
		return nil
	}
	if h := b.now().Hour(); h >= 22 || h < 8 {
		msg.Silent = true
	}
	var errs []error
	for id := range b.admins {
		if err := b.out.Send(ctx, id, msg); err != nil {
			errs = append(errs, fmt.Errorf("notify admin %d: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// runText tells how a run ended.
func runText(r moderation.Run, pending int) string {
	how := map[moderation.Trigger]string{
		moderation.BySchedule: " по расписанию",
		moderation.ByCommand:  " по кнопке",
		moderation.FromCLI:    " из консоли",
	}[r.Trigger]
	var sb strings.Builder
	if r.Status == moderation.RunFailed {
		fmt.Fprintf(&sb, "⚠️ Прогон агента%s сорвался.\nПричина: %s", how, cmp.Or(r.Error, "неизвестна"))
	} else {
		fmt.Fprintf(&sb, "🤖 Агент закончил прогон%s.", how)
	}
	if r.Queries > 0 {
		fmt.Fprintf(&sb, "\nЗапросов: %d, без изменений: %d", r.Queries, r.Unchanged)
		if r.Failed > 0 {
			fmt.Fprintf(&sb, ", с ошибкой: %d", r.Failed)
		}
		sb.WriteString(".")
	}
	if r.Deferred > 0 {
		fmt.Fprintf(&sb, "\nОтложено до следующего прогона, кончился лимит токенов: %d.", r.Deferred)
	}
	fmt.Fprintf(&sb, "\nЧерновиков прислал: %d, на проверке всего: %d.", r.Drafts, pending)
	if tokens := r.InputTokens + r.OutputTokens; tokens > 0 {
		fmt.Fprintf(&sb, "\nТокенов модели: %s.", groupDigits(tokens))
	}
	return sb.String()
}

// groupDigits writes a number with its thousands apart: 120 000.
func groupDigits(n int64) string {
	s := strconv.FormatInt(n, 10)
	var sb strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			sb.WriteByte(' ')
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

func agentMessage() Message {
	return Message{Text: textAgent, Keyboard: [][]Button{
		{{Text: labelRun, Payload: actRun}},
		{{Text: labelRunForce, Payload: payload(actRun, "force")}},
		{{Text: labelMenu, Payload: actMenu}},
	}}
}

// onAdmin handles the admin buttons. For anyone else they are stale.
func (b *Bot) onAdmin(ctx context.Context, ev Event, action, arg string) (reply, error) {
	if !b.isAdmin(ev.UserID) {
		return stale(ev), nil
	}
	switch action {
	case actDrafts:
		r, err := b.nextDraft(ctx, 0)
		r.answer = freeze(ev, labelDrafts)
		return r, err
	case actAgent, actRun:
		if b.agent == nil {
			return stale(ev), nil
		}
		if action == actAgent {
			return reply{answer: freeze(ev, labelAgent), messages: []Message{agentMessage()}}, nil
		}
		return b.runAgent(ctx, ev, arg == "force"), nil
	}

	id, err := strconv.ParseInt(arg, 10, 64)
	if err != nil {
		return stale(ev), nil
	}
	var choice string
	switch action {
	case actApprove:
		var replaced bool
		_, replaced, err = b.mod.ApproveDraft(ctx, id, ev.UserID)
		choice = textApproved
		if replaced {
			choice = textUpdated
		}
	case actReject:
		err = b.mod.RejectDraft(ctx, id, ev.UserID)
		choice = textRejected
	case actSkip:
		choice = textSkipped
	default:
		return stale(ev), nil
	}

	var invalid *moderation.InvalidError
	switch {
	case errors.As(err, &invalid):
		// The base or the survey changed since the draft was shown: show it
		// again with the reasons.
		r, err := b.nextDraft(ctx, id-1)
		r.answer = freeze(ev, textCantApprove)
		return r, err
	case errors.Is(err, moderation.ErrDecided), errors.Is(err, moderation.ErrNotFound):
		choice = textDecided
	case err != nil:
		return reply{}, err
	}
	r, err := b.nextDraft(ctx, id)
	r.answer = freeze(ev, choice)
	return r, err
}

// nextDraft shows the oldest pending draft after the given ID: the card the
// way students will see it, then what is known about it, with the buttons.
func (b *Bot) nextDraft(ctx context.Context, after int64) (reply, error) {
	d, ok, err := b.mod.NextDraft(ctx, after)
	if err != nil {
		return reply{}, err
	}
	pending, err := b.mod.PendingDrafts(ctx)
	if err != nil {
		return reply{}, err
	}
	if !ok {
		if pending > 0 {
			// The admin skipped the rest: offer to go round again.
			return reply{messages: []Message{{
				Text: fmt.Sprintf(textLastDraft, pending),
				Keyboard: [][]Button{
					{{Text: labelFromStart, Payload: actDrafts}},
					{{Text: labelMenu, Payload: actMenu}},
				},
			}}}, nil
		}
		return reply{messages: []Message{{Text: textNoDrafts, Keyboard: [][]Button{{{Text: labelMenu, Payload: actMenu}}}}}}, nil
	}

	var current *knowledge.Card
	if c, found, err := b.mod.Card(ctx, d.Card.ID); err != nil {
		return reply{}, err
	} else if found {
		current = &c
	}
	problems := moderation.Problems(d, b.survey, current)

	var messages []Message
	preview := []string{"👀 **Так карточку увидят студенты:**", renderEntry(d.Card.Preview())}
	// The sources go here, not next to the buttons: a pressed button edits
	// its message, and MAX edits a message only with a preview of its first
	// link.
	if len(d.Sources) > 0 {
		preview = append(preview, sourcesText(d.Sources))
	}
	for _, text := range pack(preview, "\n\n", maxMessageRunes) {
		messages = append(messages, Message{Text: text, Markdown: true})
	}
	return reply{messages: append(messages, b.draftControls(d, current, problems, pending))}, nil
}

// draftControls is plain text, like every message with buttons: it is frozen
// with the admin's decision when a button is pressed.
func (b *Bot) draftControls(d moderation.Draft, current *knowledge.Card, problems []string, pending int) Message {
	var sb strings.Builder
	if current != nil {
		fmt.Fprintf(&sb, "📝 Черновик №%d: обновление карточки «%s»", d.ID, current.Title)
	} else {
		fmt.Fprintf(&sb, "📝 Черновик №%d: новая карточка", d.ID)
	}
	fmt.Fprintf(&sb, "\nНа проверке всего: %d\n", pending)

	var cats []string
	for _, id := range d.Card.Categories {
		if c, ok := b.survey.Category(id); ok {
			cats = append(cats, c.Title)
		} else {
			cats = append(cats, id)
		}
	}
	fmt.Fprintf(&sb, "\nРазделы: %s", strings.Join(cats, ", "))
	if current != nil {
		changes := moderation.Changes(*current, d.Card)
		if len(changes) == 0 {
			changes = []string{"ничего"}
		}
		fmt.Fprintf(&sb, "\nЧто изменится: %s", strings.Join(changes, ", "))
	}
	if d.Query != "" {
		fmt.Fprintf(&sb, "\nЗапрос агента: «%s»", d.Query)
	}
	if !d.FoundAt.IsZero() {
		fmt.Fprintf(&sb, "\nНайдено: %s", d.FoundAt.Format("02.01.2006"))
	}
	sb.WriteString("\n\n" + audienceText(moderation.Audience(d.Card, b.survey)))
	if len(d.Notes) > 0 {
		sb.WriteString("\n\n⚠️ Замечания агента:")
		for _, n := range d.Notes {
			sb.WriteString("\n• " + n)
		}
	}
	if len(problems) > 0 {
		sb.WriteString("\n\n⛔ Одобрить нельзя:")
		for _, p := range problems {
			sb.WriteString("\n• " + p)
		}
	}

	id := strconv.FormatInt(d.ID, 10)
	decide := []Button{{Text: labelReject, Payload: payload(actReject, id)}}
	if len(problems) == 0 {
		decide = append([]Button{{Text: labelApprove, Payload: payload(actApprove, id)}}, decide...)
	}
	return Message{Text: sb.String(), Keyboard: [][]Button{
		decide,
		{{Text: labelSkip, Payload: payload(actSkip, id)}},
		{{Text: labelMenu, Payload: actMenu}},
	}}
}

// audienceText tells who will see the card: a condition a line, and the
// options of a condition a line each, as their titles may have commas and
// "или" of their own.
func audienceText(conds []moderation.Condition) string {
	if len(conds) == 0 {
		return "Кому покажется: всем в разделе"
	}
	var sb strings.Builder
	sb.WriteString("Кому покажется:")
	for _, c := range conds {
		switch len(c.Options) {
		case 0:
			fmt.Fprintf(&sb, "\n• %s: вариантов нет, никому", c.Question)
		case 1:
			fmt.Fprintf(&sb, "\n• %s: %s", c.Question, c.Options[0])
		default:
			fmt.Fprintf(&sb, "\n• %s, подходит любое из:", c.Question)
			for _, o := range c.Options {
				sb.WriteString("\n   – " + o)
			}
		}
	}
	return sb.String()
}

// sourcesText lists the pages the agent read, each a link named by its site.
func sourcesText(pages []string) string {
	var sb strings.Builder
	sb.WriteString("🔎 **Источники агента** (студенты их не видят):")
	for i, page := range pages {
		fmt.Fprintf(&sb, "\n%d. [%s](%s)", i+1, siteName(page), linkTarget(page))
	}
	return sb.String()
}

// siteName is the site of a page, without www.
func siteName(page string) string {
	u, err := url.Parse(page)
	if err != nil || u.Hostname() == "" {
		return "страница"
	}
	return strings.TrimPrefix(u.Hostname(), "www.")
}

// linkTarget keeps an address whole in a markdown link, where a parenthesis
// or a space would end it.
var linkTarget = strings.NewReplacer("(", "%28", ")", "%29", " ", "%20").Replace

// runAgent passes the command to the agent. The agent answers later, when
// it starts: NotifyRun.
func (b *Bot) runAgent(ctx context.Context, ev Event, force bool) reply {
	choice := labelRun
	if force {
		choice = labelRunForce
	}
	back := [][]Button{{{Text: labelMenu, Payload: actMenu}}}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := b.agent.RunAgent(ctx, force); err != nil {
		b.log.Error("run the agent", "admin", ev.UserID, "err", err)
		return reply{answer: freeze(ev, choice), messages: []Message{{Text: textAgentDown, Keyboard: back}}}
	}
	b.log.Info("the agent is asked to run", "admin", ev.UserID, "force", force)
	return reply{answer: freeze(ev, choice), messages: []Message{{Text: textAgentSent, Keyboard: back}}}
}

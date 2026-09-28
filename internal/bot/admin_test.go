package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KKolyasik/max-benefits/internal/knowledge"
	"github.com/KKolyasik/max-benefits/internal/moderation"
	"github.com/KKolyasik/max-benefits/internal/session"
	"github.com/KKolyasik/max-benefits/internal/survey"
)

const adminID = 7

// base is an in-memory knowledge base with drafts, standing in for Postgres.
// Students read the same cards the admins publish.
type base struct {
	mu     sync.Mutex
	survey *survey.Survey
	cards  []knowledge.Card
	drafts []moderation.Draft
}

func (b *base) Find(_ context.Context, req knowledge.Request) ([]knowledge.Entry, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return knowledge.Pick(b.cards, req), nil
}

func (b *base) PendingDrafts(context.Context) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, d := range b.drafts {
		if d.Status == moderation.Pending {
			n++
		}
	}
	return n, nil
}

func (b *base) NextDraft(_ context.Context, after int64) (moderation.Draft, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, d := range b.drafts {
		if d.ID > after && d.Status == moderation.Pending {
			return d, true, nil
		}
	}
	return moderation.Draft{}, false, nil
}

func (b *base) Card(_ context.Context, id string) (knowledge.Card, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	i := slices.IndexFunc(b.cards, func(c knowledge.Card) bool { return c.ID == id })
	if i < 0 {
		return knowledge.Card{}, false, nil
	}
	return b.cards[i], true, nil
}

func (b *base) ApproveDraft(_ context.Context, id, _ int64) (knowledge.Card, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	d, err := b.pending(id)
	if err != nil {
		return knowledge.Card{}, false, err
	}
	i := slices.IndexFunc(b.cards, func(c knowledge.Card) bool { return c.ID == d.Card.ID })
	var current *knowledge.Card
	if i >= 0 {
		current = &b.cards[i]
	}
	if problems := moderation.Problems(*d, b.survey, current); len(problems) > 0 {
		return knowledge.Card{}, false, &moderation.InvalidError{Problems: problems}
	}
	if i >= 0 {
		b.cards[i] = d.Card
	} else {
		b.cards = append(b.cards, d.Card)
	}
	d.Status = moderation.Approved
	return d.Card, i >= 0, nil
}

func (b *base) RejectDraft(_ context.Context, id, _ int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	d, err := b.pending(id)
	if err != nil {
		return err
	}
	d.Status = moderation.Rejected
	return nil
}

func (b *base) pending(id int64) (*moderation.Draft, error) {
	i := slices.IndexFunc(b.drafts, func(d moderation.Draft) bool { return d.ID == id })
	if i < 0 {
		return nil, moderation.ErrNotFound
	}
	if b.drafts[i].Status != moderation.Pending {
		return nil, moderation.ErrDecided
	}
	return &b.drafts[i], nil
}

func (b *base) addDraft(card knowledge.Card, updates string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.drafts = append(b.drafts, moderation.Draft{
		ID: int64(len(b.drafts) + 1), Card: card, Updates: updates, Query: "стипендия",
		Sources: []string{"https://a.example/page"}, FoundAt: time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC),
		Status: moderation.Pending,
	})
}

// chats routes messages to the chat of each user.
type chats map[int64]*chat

func (c chats) Send(ctx context.Context, userID int64, msg Message) error {
	return c[userID].Send(ctx, userID, msg)
}

func (c chats) AnswerCallback(ctx context.Context, userID int64, id string, a CallbackAnswer) error {
	return c[userID].AnswerCallback(ctx, userID, id, a)
}

// newAdminSetup returns an admin and a student of one bot over one base.
func newAdminSetup(t *testing.T) (admin, student *user, kb *base) {
	t.Helper()
	sv, err := survey.Parse([]byte(testSurvey))
	if err != nil {
		t.Fatal(err)
	}
	cards, err := knowledge.ParseCards([]byte(testKnowledge))
	if err != nil {
		t.Fatal(err)
	}
	kb = &base{survey: sv, cards: cards}
	out := chats{adminID: {}, 42: {}}
	store := session.NewMemory()
	b := New(sv, kb, store, out, slog.New(slog.DiscardHandler)).WithModeration(kb, []int64{adminID})
	admin = &user{t: t, id: adminID, bot: b, chat: out[adminID], store: store}
	student = &user{t: t, id: 42, bot: b, chat: out[42], store: store}
	return admin, student, kb
}

var grant = knowledge.Card{ID: "grant", Categories: []string{"money"}, Priority: 50, Title: "Грант", Summary: "Деньги всем.",
	Links: []knowledge.CardLink{{Title: "Сайт гранта", URL: "https://grant.example"}}}

func (u *user) hasButton(text string) bool {
	u.chat.mu.Lock()
	defer u.chat.mu.Unlock()
	for i := len(u.chat.messages) - 1; i >= 0; i-- {
		if kb := u.chat.messages[i].Keyboard; len(kb) > 0 {
			for _, row := range kb {
				for _, b := range row {
					if strings.Contains(b.Text, text) {
						return true
					}
				}
			}
			return false
		}
	}
	return false
}

func TestApprovedDraftIsShownToStudentsAtOnce(t *testing.T) {
	admin, student, kb := newAdminSetup(t)
	kb.addDraft(grant, "")

	student.start()
	student.press("Деньги")
	student.press("Вуз А")
	student.press("Ничего")
	student.press("Готово")
	if strings.Contains(student.allText(), "Грант") {
		t.Fatal("the draft must not be shown before approval")
	}

	admin.start()
	admin.press("Черновики")
	preview := admin.chat.messages[len(admin.chat.messages)-2]
	if !preview.Markdown || !strings.Contains(preview.Text, "**Грант**\nДеньги всем.") {
		t.Errorf("the card must be previewed as students see it:\n%s", preview.Text)
	}
	if !strings.Contains(preview.Text, "1. [a.example](https://a.example/page)") {
		t.Errorf("the sources go with the card:\n%s", preview.Text)
	}
	if kb := preview.Keyboard; len(kb) != 1 || kb[0][0].Text != "🔗 Сайт гранта" || kb[0][0].URL != "https://grant.example" {
		t.Errorf("the links of the card are buttons, as students get them: %+v", kb)
	}
	controls := admin.chat.last().Text
	for _, want := range []string{"Черновик №1: новая карточка", "На проверке всего: 1", "Разделы: Деньги",
		"Кому покажется: всем в разделе", "Запрос агента: «стипендия»"} {
		if !strings.Contains(controls, want) {
			t.Errorf("controls have no %q:\n%s", want, controls)
		}
	}
	// A pressed button edits the message, and MAX edits it with a preview of
	// the first link.
	if strings.Contains(controls, "https://") {
		t.Errorf("controls must have no links:\n%s", controls)
	}
	admin.press("Одобрить")
	if got := admin.chat.lastAnswer().Edit.Text; !strings.HasSuffix(got, "👉 "+textApproved) {
		t.Errorf("the decision must stay in the chat: %q", got)
	}
	if got := admin.chat.last().Text; got != textNoDrafts {
		t.Errorf("after the last draft: %q", got)
	}

	student.press("Другие разделы")
	student.press("Деньги")
	student.press("Показать подборку")
	student.press("Грант")
	if got := student.chat.last().Text; !strings.Contains(got, "**Грант**\nДеньги всем.") {
		t.Errorf("an approved card must be in the selection at once:\n%s", got)
	}
}

// Who will see the card is a list: an option may have commas and "или" of
// its own.
func TestDraftAudience(t *testing.T) {
	admin, _, kb := newAdminSetup(t)
	c := grant
	c.Match = knowledge.Condition{"status": {"orphan", "poor"}, "uni": {"a"}}
	kb.addDraft(c, "")
	admin.say("/admin")
	want := "Кому покажется:\n• Вуз: Вуз А\n• Статус, подходит любое из:\n   – Сирота\n   – Мало денег"
	if got := admin.chat.last().Text; !strings.Contains(got, want) {
		t.Errorf("controls:\n%s", got)
	}
}

// A source is named by its site, and its address stays whole in the link.
func TestSourcesText(t *testing.T) {
	got := sourcesText([]string{"https://www.hse.ru/scholarships/pgas", "https://ru.wikipedia.org/wiki/Стипендия_(значения)", "::"})
	for _, want := range []string{
		"1. [hse.ru](https://www.hse.ru/scholarships/pgas)",
		"2. [ru.wikipedia.org](https://ru.wikipedia.org/wiki/Стипендия_%28значения%29)",
		"3. [страница](::)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in\n%s", want, got)
		}
	}
}

func TestStudentsHaveNoAdmin(t *testing.T) {
	_, student, kb := newAdminSetup(t)
	kb.addDraft(grant, "")

	student.start()
	if student.hasButton("Черновики") {
		t.Error("only admins see the drafts button")
	}
	student.handle(Event{Type: EventCallback, CallbackID: "cb", Payload: payload(actApprove, "1"), SourceText: "подделка"})
	if got := student.chat.lastAnswer().Notification; got != textStale {
		t.Errorf("a forged admin press must be ignored, got %q", got)
	}
	student.say("/admin")
	if got := student.chat.last().Text; !strings.Contains(got, textUseButtons) {
		t.Errorf("/admin for a student: %q", got)
	}
	if n, _ := kb.PendingDrafts(context.Background()); n != 1 {
		t.Error("the draft must stay pending")
	}
}

// While reviewers try the bot, everyone is an admin: the screens open and the
// decisions work, but the reports of the agent still go only to the listed
// admins.
func TestAdminForAll(t *testing.T) {
	admin, student, kb := newAdminSetup(t)
	kb.addDraft(grant, "")
	admin.bot.WithAdminForAll()

	student.start()
	if !student.hasButton("Черновики") {
		t.Error("with the flag on, everyone sees the drafts button")
	}
	student.say("/admin")
	student.press("Одобрить")
	if n, _ := kb.PendingDrafts(context.Background()); n != 0 {
		t.Error("with the flag on, anyone can approve a draft")
	}

	seen := len(student.chat.messages)
	if err := admin.bot.NotifyRun(context.Background(), moderation.Run{Trigger: moderation.ByCommand, Status: moderation.RunBusy}); err != nil {
		t.Fatal(err)
	}
	if len(student.chat.messages) != seen {
		t.Error("only the listed admins hear about the agent")
	}
	if admin.chat.last().Text != textRunBusy {
		t.Errorf("the listed admin must hear about the agent, got %q", admin.chat.last().Text)
	}
}

func TestUpdateDraftShowsWhatChanges(t *testing.T) {
	admin, _, kb := newAdminSetup(t)
	current, _, _ := kb.Card(context.Background(), "for_all")
	updated := current
	updated.Title = "Для всех, 2027"
	kb.addDraft(updated, "for_all")

	admin.say("/admin")
	controls := admin.chat.last().Text
	if !strings.Contains(controls, "обновление карточки «Для всех»") || !strings.Contains(controls, "Что изменится: заголовок") {
		t.Errorf("controls:\n%s", controls)
	}
	admin.press("Одобрить")
	if got := admin.chat.lastAnswer().Edit.Text; !strings.HasSuffix(got, textUpdated) {
		t.Errorf("answer %q", got)
	}
	if c, _, _ := kb.Card(context.Background(), "for_all"); c.Title != "Для всех, 2027" {
		t.Errorf("card %+v", c)
	}
}

func TestInvalidDraftCannotBeApproved(t *testing.T) {
	admin, _, kb := newAdminSetup(t)
	bad := grant
	bad.Match = knowledge.Condition{"hobby": {"art"}}
	kb.addDraft(bad, "")

	admin.say("/admin")
	if !strings.Contains(admin.chat.last().Text, "⛔ Одобрить нельзя") || admin.hasButton("Одобрить") {
		t.Errorf("an invalid draft must have no approve button:\n%s", admin.chat.last().Text)
	}
	admin.press("Отклонить")
	if got := admin.chat.last().Text; got != textNoDrafts {
		t.Errorf("got %q", got)
	}
}

func TestBaseChangedBeforeApproval(t *testing.T) {
	admin, _, kb := newAdminSetup(t)
	kb.addDraft(grant, "")
	admin.say("/admin")

	// The same card was published meanwhile, e.g. from another draft.
	kb.mu.Lock()
	kb.cards = append(kb.cards, grant)
	kb.mu.Unlock()

	admin.press("Одобрить")
	if got := admin.chat.lastAnswer().Edit.Text; !strings.HasSuffix(got, textCantApprove) {
		t.Errorf("answer %q", got)
	}
	if !strings.Contains(admin.chat.last().Text, `card "grant" is already in the base`) {
		t.Errorf("the draft must be shown again with the reason:\n%s", admin.chat.last().Text)
	}
}

func TestSkipRejectAndGoRound(t *testing.T) {
	admin, _, kb := newAdminSetup(t)
	for i := range 2 {
		c := grant
		c.ID, c.Title = fmt.Sprintf("grant_%d", i), fmt.Sprintf("Грант %d", i)
		kb.addDraft(c, "")
	}

	admin.say("/admin")
	admin.press("Пропустить")
	if !strings.Contains(admin.chat.last().Text, "Черновик №2") {
		t.Fatalf("skip must show the next draft:\n%s", admin.chat.last().Text)
	}
	admin.press("Отклонить")
	if got := admin.chat.last().Text; got != fmt.Sprintf(textLastDraft, 1) {
		t.Fatalf("got %q", got)
	}
	admin.press("Сначала")
	if !strings.Contains(admin.chat.last().Text, "Черновик №1") {
		t.Errorf("the skipped draft must come back:\n%s", admin.chat.last().Text)
	}
}

func TestDraftDecidedByAnotherAdmin(t *testing.T) {
	admin, _, kb := newAdminSetup(t)
	kb.addDraft(grant, "")
	admin.say("/admin")
	if err := kb.RejectDraft(context.Background(), 1, 99); err != nil {
		t.Fatal(err)
	}

	admin.press("Одобрить")
	if got := admin.chat.lastAnswer().Edit.Text; !strings.HasSuffix(got, textDecided) {
		t.Errorf("answer %q", got)
	}
	if c, found, _ := kb.Card(context.Background(), "grant"); found {
		t.Errorf("a rejected draft must not be published: %+v", c)
	}
}

// agent records the commands to run.
type agent struct {
	mu     sync.Mutex
	forces []bool
	err    error
}

func (a *agent) RunAgent(_ context.Context, force bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.forces = append(a.forces, force)
	return a.err
}

func TestAdminRunsTheAgent(t *testing.T) {
	admin, student, _ := newAdminSetup(t)
	ag := &agent{}
	admin.bot.WithAgent(ag)

	student.start()
	if student.hasButton("Агент") {
		t.Error("only admins see the agent button")
	}
	student.handle(Event{Type: EventCallback, CallbackID: "cb", Payload: actRun, SourceText: "подделка"})
	if len(ag.forces) != 0 {
		t.Fatal("a forged press must not run the agent")
	}

	admin.start()
	admin.press("Агент")
	if !strings.Contains(admin.chat.last().Text, "По расписанию он запускается сам") {
		t.Errorf("the agent screen:\n%s", admin.chat.last().Text)
	}
	admin.press("Запустить")
	if got := admin.chat.last().Text; got != textAgentSent {
		t.Errorf("after the press: %q", got)
	}
	admin.say("/agent")
	admin.press("Перечитать всё")
	if !slices.Equal(ag.forces, []bool{false, true}) {
		t.Errorf("commands %v", ag.forces)
	}
}

// Without Kafka the command doesn't reach the agent, and the admin is told.
func TestAgentUnreachable(t *testing.T) {
	admin, _, _ := newAdminSetup(t)
	admin.bot.WithAgent(&agent{err: errors.New("kafka is down")})
	admin.say("/agent")
	admin.press("Запустить")
	if got := admin.chat.last().Text; got != textAgentDown {
		t.Errorf("got %q", got)
	}
}

func TestNotifyRun(t *testing.T) {
	admin, student, kb := newAdminSetup(t)
	kb.addDraft(grant, "")
	ctx := context.Background()
	notify := func(r moderation.Run) Message {
		t.Helper()
		n := len(admin.chat.messages)
		if err := admin.bot.NotifyRun(ctx, r); err != nil {
			t.Fatal(err)
		}
		if len(admin.chat.messages) == n {
			return Message{}
		}
		return admin.chat.last()
	}

	// An admin who pressed the button hears that the run started; the
	// schedule starts runs without a word.
	if got := notify(moderation.Run{Trigger: moderation.ByCommand, Status: moderation.RunStarted}); got.Text != textRunStarted {
		t.Errorf("started by the button: %q", got.Text)
	}
	if got := notify(moderation.Run{Trigger: moderation.BySchedule, Status: moderation.RunStarted}); got.Text != "" {
		t.Errorf("started by the schedule: %q", got.Text)
	}
	if got := notify(moderation.Run{Trigger: moderation.ByCommand, Status: moderation.RunBusy}); got.Text != textRunBusy {
		t.Errorf("busy: %q", got.Text)
	}

	done := notify(moderation.Run{Trigger: moderation.BySchedule, Status: moderation.RunDone, Queries: 13, Unchanged: 9,
		Failed: 1, Deferred: 2, Drafts: 1, InputTokens: 114_000, OutputTokens: 6_000})
	for _, want := range []string{"закончил прогон по расписанию", "Запросов: 13, без изменений: 9, с ошибкой: 1.",
		"кончился лимит токенов: 2", "Черновиков прислал: 1, на проверке всего: 1.", "Токенов модели: 120 000."} {
		if !strings.Contains(done.Text, want) {
			t.Errorf("the report has no %q:\n%s", want, done.Text)
		}
	}
	admin.press("Разобрать")
	if !strings.Contains(admin.chat.last().Text, "Черновик №1") {
		t.Errorf("the button must open the draft:\n%s", admin.chat.last().Text)
	}

	failed := notify(moderation.Run{Trigger: moderation.ByCommand, Status: moderation.RunFailed, Error: "search is down"})
	if !strings.Contains(failed.Text, "Прогон агента по кнопке сорвался.\nПричина: search is down") {
		t.Errorf("failed:\n%s", failed.Text)
	}
	if len(student.chat.messages) != 0 {
		t.Error("students must not hear about the agent")
	}
}

// At night the reports come without a sound.
func TestNightRunIsSilent(t *testing.T) {
	admin, _, _ := newAdminSetup(t)
	for hour, silent := range map[int]bool{3: true, 23: true, 8: false, 15: false} {
		admin.bot.now = func() time.Time { return time.Date(2026, 9, 28, hour, 0, 0, 0, time.Local) }
		if err := admin.bot.NotifyRun(context.Background(), moderation.Run{Trigger: moderation.BySchedule, Status: moderation.RunDone}); err != nil {
			t.Fatal(err)
		}
		if got := admin.chat.last().Silent; got != silent {
			t.Errorf("at %d:00 silent is %v", hour, got)
		}
	}
}

func TestGroupDigits(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 999: "999", 1000: "1 000", 120_000: "120 000", 1_234_567: "1 234 567"} {
		if got := groupDigits(n); got != want {
			t.Errorf("%d: %q", n, got)
		}
	}
}

package bot

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/KKolyasik/max-benefits/internal/knowledge"
	"github.com/KKolyasik/max-benefits/internal/session"
	"github.com/KKolyasik/max-benefits/internal/survey"
)

const testSurvey = `
categories:
  - id: money
    title: "Деньги"
    intro: "Пара вопросов."
    questions: [uni, status]
  - id: fun
    title: "Досуг"
    questions: [uni, hobby]
questions:
  - id: uni
    label: "Вуз"
    text: "Где учишься?"
    options:
      - {id: a, title: "Вуз А"}
      - {id: b, title: "Вуз Б"}
  - id: status
    label: "Статус"
    text: "Что про тебя?"
    multi: true
    options:
      - {id: orphan, title: "Сирота"}
      - {id: poor, title: "Мало денег"}
      - {id: none, title: "Ничего", exclusive: true}
  - id: hobby
    label: "Хобби"
    text: "Что любишь?"
    options:
      - {id: art, title: "Искусство"}
      - {id: sport, title: "Спорт"}
`

const testKnowledge = `
entries:
  - id: for_all
    categories: [money]
    title: "Для всех"
    summary: "Всем."
  - id: for_orphans
    categories: [money]
    priority: 10
    match: {status: [orphan]}
    title: "Для сирот"
    summary: "Сиротам."
    links:
      - {title: "Сайт А", url: "https://a.example", when: {uni: [a]}}
      - {title: "Сайт Б", url: "https://b.example", when: {uni: [b]}}
`

// chat records everything the bot sends, like a MAX dialog would show it.
type chat struct {
	mu       sync.Mutex
	messages []Message
	answers  []CallbackAnswer
	failSend bool
}

func (c *chat) Send(_ context.Context, _ int64, msg Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failSend {
		return errors.New("network down")
	}
	c.messages = append(c.messages, msg)
	return nil
}

func (c *chat) AnswerCallback(_ context.Context, _ int64, _ string, a CallbackAnswer) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.answers = append(c.answers, a)
	return nil
}

func (c *chat) last() Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.messages[len(c.messages)-1]
}

func (c *chat) lastAnswer() CallbackAnswer {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.answers[len(c.answers)-1]
}

// user drives the bot like a person tapping buttons.
type user struct {
	t     *testing.T
	id    int64
	bot   *Bot
	chat  *chat
	store session.Store
}

func newUser(t *testing.T) *user {
	t.Helper()
	sv, err := survey.Parse([]byte(testSurvey))
	if err != nil {
		t.Fatal(err)
	}
	kb, err := knowledge.ParseStatic([]byte(testKnowledge), sv)
	if err != nil {
		t.Fatal(err)
	}
	return newUserWith(t, sv, kb, session.NewMemory())
}

func newUserWith(t *testing.T, sv *survey.Survey, kb knowledge.Base, store session.Store) *user {
	c := &chat{}
	return &user{t: t, id: 42, bot: New(sv, kb, store, c, slog.New(slog.DiscardHandler)), chat: c, store: store}
}

func (u *user) handle(ev Event) {
	u.t.Helper()
	ev.UserID = u.id
	if err := u.bot.Handle(context.Background(), ev); err != nil {
		u.t.Fatalf("handle %+v: %v", ev, err)
	}
}

func (u *user) start()          { u.handle(Event{Type: EventStart}) }
func (u *user) say(text string) { u.handle(Event{Type: EventText, Text: text}) }

// press taps a button with the given text on the latest message that has a
// keyboard, the way a user would.
func (u *user) press(text string) {
	u.t.Helper()
	msg, btn := u.find(text)
	u.handle(Event{Type: EventCallback, CallbackID: "cb", Payload: btn.Payload, SourceText: msg.Text})
}

func (u *user) find(text string) (Message, Button) {
	u.t.Helper()
	u.chat.mu.Lock()
	defer u.chat.mu.Unlock()
	for i := len(u.chat.messages) - 1; i >= 0; i-- {
		msg := u.chat.messages[i]
		if len(msg.Keyboard) == 0 {
			continue
		}
		for _, row := range msg.Keyboard {
			for _, b := range row {
				if strings.Contains(b.Text, text) {
					return msg, b
				}
			}
		}
		u.t.Fatalf("no button %q in the latest keyboard:\n%s", text, msg.Text)
	}
	u.t.Fatalf("no keyboard sent yet")
	return Message{}, Button{}
}

func (u *user) session() *session.Session {
	s, err := u.store.Load(context.Background(), u.id)
	if err != nil {
		u.t.Fatal(err)
	}
	return s
}

func (u *user) allText() string {
	var sb strings.Builder
	for _, m := range u.chat.messages {
		sb.WriteString(m.Text + "\n")
	}
	return sb.String()
}

func TestSurveyFlowToResults(t *testing.T) {
	u := newUser(t)
	u.start()
	if !strings.Contains(u.chat.last().Text, "Навигатор студента") {
		t.Fatalf("no welcome: %q", u.chat.last().Text)
	}

	u.press("Деньги")
	if got := u.chat.lastAnswer().Edit.Text; !strings.HasSuffix(got, "👉 Деньги") {
		t.Errorf("menu message is not frozen with the choice: %q", got)
	}
	if q := u.chat.last().Text; !strings.Contains(q, "Пара вопросов.") || !strings.Contains(q, "Вопрос 1 из 2") {
		t.Errorf("first question should have the intro and position: %q", q)
	}

	u.press("Вуз А")
	if s := u.session(); !slices.Equal(s.Answers["uni"], []string{"a"}) || s.State != session.StateSurvey {
		t.Fatalf("answer not saved: %+v", s)
	}

	// Multi-choice: toggling edits the question in place.
	u.press("Сирота")
	edit := u.chat.lastAnswer().Edit
	if edit == nil || !strings.Contains(edit.Keyboard[0][0].Text, "✅") {
		t.Fatalf("toggle should re-render the question with a check mark: %+v", edit)
	}
	u.press("Мало денег")
	u.press("Готово")

	if s := u.session(); s.State != session.StateMenu || !slices.Equal(s.Answers["status"], []string{"orphan", "poor"}) {
		t.Fatalf("unexpected session after the last answer: %+v", s)
	}
	text := u.allText()
	// Higher priority first, university-specific link only for university A.
	if i, j := strings.Index(text, "Для сирот"), strings.Index(text, "Для всех"); i < 0 || j < 0 || i > j {
		t.Errorf("expected both entries, orphans first:\n%s", text)
	}
	if !strings.Contains(text, "[Сайт А](https://a.example)") || strings.Contains(text, "b.example") {
		t.Errorf("links are not filtered by university:\n%s", text)
	}
	if last := u.chat.last(); !strings.Contains(last.Text, "Что дальше?") {
		t.Errorf("results must end with the next-steps menu: %q", last.Text)
	}
}

func TestExclusiveOptionClearsOthers(t *testing.T) {
	u := newUser(t)
	u.start()
	u.press("Деньги")
	u.press("Вуз Б")
	u.press("Сирота")
	u.press("Ничего")
	if s := u.session(); !slices.Equal(s.Selected, []string{"none"}) {
		t.Fatalf("exclusive option must clear the others, got %v", s.Selected)
	}
	u.press("Сирота")
	if s := u.session(); !slices.Equal(s.Selected, []string{"orphan"}) {
		t.Fatalf("regular option must clear the exclusive one, got %v", s.Selected)
	}
}

func TestDoneRequiresSelection(t *testing.T) {
	u := newUser(t)
	u.start()
	u.press("Деньги")
	u.press("Вуз А")
	u.press("Готово")
	if a := u.chat.lastAnswer(); a.Notification != textPickOne || a.Edit != nil {
		t.Fatalf("expected a hint without editing, got %+v", a)
	}
	if s := u.session(); s.State != session.StateSurvey || len(s.Answers["status"]) != 0 {
		t.Fatalf("state must not change: %+v", s)
	}
}

func TestProgressIsResumedAndSharedBetweenCategories(t *testing.T) {
	u := newUser(t)
	u.start()
	u.press("Деньги")
	u.press("Вуз Б")
	u.say("/menu") // walk away in the middle of the questionnaire

	// Another category skips the already answered university question.
	u.press("Досуг")
	if q := u.chat.last().Text; !strings.Contains(q, "Вопрос 2 из 2") || !strings.Contains(q, "Что любишь?") {
		t.Fatalf("should continue from the unanswered question: %q", q)
	}
	u.press("Спорт")

	// Back to the first category: resumes from the status question.
	u.press("Другие разделы")
	u.press("Деньги")
	if q := u.chat.last().Text; !strings.Contains(q, "продолжаем с вопроса 2") || !strings.Contains(q, "Что про тебя?") {
		t.Fatalf("should resume the first category: %q", q)
	}
}

func TestFilledCategoryOffersReuseOrRedo(t *testing.T) {
	u := newUser(t)
	u.start()
	u.press("Деньги")
	u.press("Вуз А")
	u.press("Ничего")
	u.press("Готово")

	u.press("Другие разделы")
	u.press("Деньги")
	if s := u.session(); s.State != session.StateConfirm {
		t.Fatalf("expected confirm state, got %s", s.State)
	}
	if msg := u.chat.last().Text; !strings.Contains(msg, "• Вуз: Вуз А") || !strings.Contains(msg, "• Статус: Ничего") {
		t.Errorf("confirm should summarize the saved answers: %q", msg)
	}

	n := len(u.chat.messages)
	u.press("Показать подборку")
	if len(u.chat.messages) <= n || !strings.Contains(u.allText(), "Для всех") {
		t.Fatalf("reuse should show results")
	}

	u.press("Изменить анкету")
	s := u.session()
	if s.State != session.StateSurvey || len(s.Answers["uni"]) != 0 || len(s.Answers["status"]) != 0 {
		t.Fatalf("redo must clear the category answers: %+v", s)
	}
	if q := u.chat.last().Text; !strings.Contains(q, "Вопрос 1 из 2") {
		t.Errorf("redo should start from the first question: %q", q)
	}
}

func TestStaleButtonIsRejected(t *testing.T) {
	u := newUser(t)
	u.start()
	u.press("Деньги")
	oldQuestion, _ := u.find("Вуз А")
	u.press("Вуз А")

	// Tapping the already answered question again must not overwrite anything.
	u.handle(Event{Type: EventCallback, CallbackID: "cb", Payload: payload(actAnswer, "uni", "b"), SourceText: oldQuestion.Text})
	a := u.chat.lastAnswer()
	if a.Notification != textStale || a.Edit == nil || len(a.Edit.Keyboard) != 0 {
		t.Fatalf("stale press should remove the keyboard and notify: %+v", a)
	}
	if s := u.session(); !slices.Equal(s.Answers["uni"], []string{"a"}) {
		t.Fatalf("stale press changed the answer: %v", s.Answers["uni"])
	}

	u.handle(Event{Type: EventCallback, CallbackID: "cb", Payload: "garbage:::"})
	if a := u.chat.lastAnswer(); a.Notification != textStale {
		t.Fatalf("unknown payload should be stale: %+v", a)
	}
}

func TestTextDuringSurveyRepeatsQuestion(t *testing.T) {
	u := newUser(t)
	u.start()
	u.press("Деньги")
	u.say("а можно вопрос?")
	if q := u.chat.last().Text; !strings.Contains(q, textAnswerButton) || !strings.Contains(q, "Где учишься?") {
		t.Fatalf("should repeat the current question: %q", q)
	}
	if s := u.session(); s.State != session.StateSurvey {
		t.Fatalf("free text must not break the survey: %s", s.State)
	}

	u.say("/menu")
	u.say("привет, что ты умеешь?")
	if m := u.chat.last().Text; !strings.Contains(m, textUseButtons) {
		t.Fatalf("outside the survey free text should show the menu: %q", m)
	}
}

func TestProfileAndDelete(t *testing.T) {
	u := newUser(t)
	u.start()
	u.press("Моя анкета")
	if !strings.Contains(u.chat.last().Text, "пустая") {
		t.Fatalf("empty profile expected: %q", u.chat.last().Text)
	}
	u.press("В меню")
	u.press("Досуг")
	u.press("Вуз А")
	u.press("Искусство")
	u.press("Другие разделы")
	u.press("Моя анкета")
	if p := u.chat.last().Text; !strings.Contains(p, "• Вуз: Вуз А") || !strings.Contains(p, "• Хобби: Искусство") {
		t.Fatalf("profile should list answers: %q", p)
	}
	u.press("Удалить мои данные")
	u.press("Да, удалить")
	if s := u.session(); len(s.Answers) != 0 {
		t.Fatalf("answers must be deleted: %+v", s.Answers)
	}
}

type brokenKB struct{}

func (brokenKB) Find(context.Context, knowledge.Request) ([]knowledge.Entry, error) {
	return nil, errors.New("qdrant is down")
}

func TestAnswersSurviveKnowledgeBaseFailure(t *testing.T) {
	sv, err := survey.Parse([]byte(testSurvey))
	if err != nil {
		t.Fatal(err)
	}
	u := newUserWith(t, sv, brokenKB{}, session.NewMemory())
	u.start()
	u.press("Досуг")
	u.press("Вуз А")

	msg, btn := u.find("Спорт")
	err = u.bot.Handle(context.Background(), Event{Type: EventCallback, UserID: u.id, CallbackID: "cb", Payload: btn.Payload, SourceText: msg.Text})
	if err == nil {
		t.Fatal("expected the knowledge base error to be reported")
	}
	if s := u.session(); !slices.Equal(s.Answers["hobby"], []string{"sport"}) {
		t.Fatalf("the last answer must be saved even if results failed: %+v", s.Answers)
	}
	if m := u.chat.last().Text; !strings.Contains(m, textFailed) {
		t.Fatalf("user should be told about the failure: %q", m)
	}
}

func TestSendFailureIsReported(t *testing.T) {
	u := newUser(t)
	u.chat.failSend = true
	if err := u.bot.Handle(context.Background(), Event{Type: EventStart, UserID: u.id}); err == nil {
		t.Fatal("expected a send error")
	}
}

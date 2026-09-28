package bot

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
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

// chat records everything the bot sends, like a MAX dialog would show it: a
// pressed button's edit changes its message in place.
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

// AnswerCallback takes the callback ID for the index of the pressed message,
// the way user.press makes it.
func (c *chat) AnswerCallback(_ context.Context, _ int64, id string, a CallbackAnswer) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.answers = append(c.answers, a)
	if i, err := strconv.Atoi(id); err == nil && a.Edit != nil && i < len(c.messages) {
		c.messages[i] = *a.Edit
	}
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
	u.pressOn(u.find(text))
}

// pressOn taps a button on the i-th message of the chat.
func (u *user) pressOn(i int, btn Button) {
	u.t.Helper()
	u.chat.mu.Lock()
	source := u.chat.messages[i].Text
	u.chat.mu.Unlock()
	u.handle(Event{Type: EventCallback, CallbackID: strconv.Itoa(i), Payload: btn.Payload, SourceText: source})
}

// find returns the index of the latest message with a keyboard and its
// button with the given text.
func (u *user) find(text string) (int, Button) {
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
					return i, b
				}
			}
		}
		u.t.Fatalf("no button %q in the latest keyboard:\n%s", text, msg.Text)
	}
	u.t.Fatalf("no keyboard sent yet")
	return 0, Button{}
}

func (u *user) session() *session.Session {
	s, err := u.store.Load(context.Background(), u.id)
	if err != nil {
		u.t.Fatal(err)
	}
	return s
}

// allText is what the chat shows: the messages and their buttons.
func (u *user) allText() string {
	var sb strings.Builder
	for _, m := range u.chat.messages {
		sb.WriteString(m.Text + "\n")
		for _, row := range m.Keyboard {
			for _, b := range row {
				sb.WriteString(b.Text)
				sb.WriteString("\n")
			}
		}
	}
	return sb.String()
}

// readResults opens every card of the results on the last message, as a
// curious student would, and returns what they read: the list, the cards
// and the addresses of their links.
func (u *user) readResults() string {
	u.t.Helper()
	i := len(u.chat.messages) - 1
	list := u.chat.last()
	var sb strings.Builder
	sb.WriteString(list.Text)
	for _, row := range list.Keyboard {
		btn := row[0]
		if !strings.HasPrefix(btn.Payload, actCard+":") {
			continue
		}
		u.pressOn(i, btn)
		card := u.chat.last()
		sb.WriteString("\n")
		sb.WriteString(card.Text)
		for _, row := range card.Keyboard {
			for _, b := range row {
				if b.URL != "" {
					sb.WriteString("\n")
					sb.WriteString(b.URL)
				}
			}
		}
		u.press(labelList)
	}
	return sb.String()
}

// buttons lists the texts of the buttons of the last message, row by row.
func (u *user) buttons() [][]string {
	var rows [][]string
	for _, row := range u.chat.last().Keyboard {
		var texts []string
		for _, b := range row {
			texts = append(texts, b.Text)
		}
		rows = append(rows, texts)
	}
	return rows
}

func TestSurveyFlowToResults(t *testing.T) {
	u := newUser(t)
	u.start()
	if !strings.Contains(u.chat.last().Text, "Навигатор студента") {
		t.Fatalf("no welcome: %q", u.chat.last().Text)
	}

	u.press("Деньги")
	if q := u.chat.last().Text; !strings.Contains(q, "Деньги\nПара вопросов.") || !strings.Contains(q, "Вопрос 1 из 2") {
		t.Errorf("first question should have the title, the intro and the position: %q", q)
	}

	u.press("Вуз А")
	if s := u.session(); !slices.Equal(s.Answers["uni"], []string{"a"}) || s.State != session.StateSurvey {
		t.Fatalf("answer not saved: %+v", s)
	}
	if q := u.chat.last().Text; !strings.HasPrefix(q, "Деньги\n\nВопрос 2 из 2") {
		t.Errorf("the next question should go under the title, without the intro: %q", q)
	}

	// Multi-choice: toggling ticks the option.
	u.press("Сирота")
	if got := u.buttons()[0][0]; got != "✅ Сирота" {
		t.Fatalf("toggle should re-render the question with a check mark: %q", got)
	}
	u.press("Мало денег")
	u.press("Готово")

	if s := u.session(); s.State != session.StateMenu || !slices.Equal(s.Answers["status"], []string{"orphan", "poor"}) {
		t.Fatalf("unexpected session after the last answer: %+v", s)
	}
	list := u.chat.last()
	if !list.Markdown || !strings.Contains(list.Text, "**Деньги: подборка для тебя**\nНашёл 2 пункта") ||
		!strings.Contains(list.Text, textDisclaimer) {
		t.Errorf("results list:\n%s", list.Text)
	}
	// Higher priority first, then what to do next.
	want := [][]string{{"Для сирот"}, {"Для всех"}, {labelOtherTopics}, {labelEditAnswers}}
	if got := u.buttons(); !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("results buttons %q, want %q", got, want)
	}
}

// Every press changes the message with the button, so the whole dialog
// takes one message.
func TestButtonsChangeTheirMessage(t *testing.T) {
	u := newUser(t)
	u.start()
	for _, btn := range []string{"Деньги", "Вуз А", "Сирота", "Готово", "Для сирот", "Далее", "Назад", "Список",
		"Другие разделы", "Моя анкета", "Удалить мои данные", "Отмена", "В меню", "О боте", "В меню"} {
		u.press(btn)
	}
	if n := len(u.chat.messages); n != 1 {
		t.Fatalf("the buttons must change their message, got %d messages:\n%s", n, u.allText())
	}
	if got := u.chat.last().Text; got != textMenu {
		t.Errorf("the message ends as the menu: %q", got)
	}
}

// The results are a list of the cards, and every card is a screen of its own
// with its links as buttons and the arrows to the others.
func TestResultsAreFlippedThrough(t *testing.T) {
	u := newUser(t)
	u.start()
	u.press("Деньги")
	u.press("Вуз А")
	u.press("Сирота")
	u.press("Готово")

	u.press("Для сирот")
	card := u.chat.last()
	if !card.Markdown || !strings.HasPrefix(card.Text, "Деньги · 1 из 2\n\n**Для сирот**\nСиротам.") {
		t.Errorf("the first card:\n%s", card.Text)
	}
	// An edited message gets a preview of the first link in its text, so the
	// links are buttons, filtered by the university.
	if strings.Contains(card.Text, "https://") {
		t.Errorf("a card must have no links in its text:\n%s", card.Text)
	}
	link := card.Keyboard[0][0]
	if link.Text != "🔗 Сайт А" || link.URL != "https://a.example" || len(card.Keyboard) != 2 {
		t.Errorf("the links of the card: %+v", card.Keyboard)
	}
	if got := u.buttons()[1]; !slices.Equal(got, []string{labelList, labelNext}) {
		t.Errorf("the first card has no way back but to the list: %q", got)
	}

	u.press("Далее")
	if got := u.chat.last().Text; !strings.HasPrefix(got, "Деньги · 2 из 2\n\n**Для всех**") {
		t.Errorf("the second card:\n%s", got)
	}
	if got := u.buttons(); !slices.EqualFunc(got, [][]string{{labelBack, labelList}}, slices.Equal) {
		t.Errorf("the last card has no way further: %q", got)
	}
	u.press("Назад")
	if got := u.chat.last().Text; !strings.Contains(got, "**Для сирот**") {
		t.Errorf("back to the first card:\n%s", got)
	}
	u.press("Список")
	if got := u.chat.last().Text; !strings.Contains(got, "подборка для тебя") {
		t.Errorf("back to the list:\n%s", got)
	}
	if s := u.session(); s.State != session.StateMenu {
		t.Errorf("reading the cards changes nothing: %+v", s)
	}
}

// A card that left the results since they were shown opens the list, and
// results of a category with answers missing open its questions.
func TestOutdatedResults(t *testing.T) {
	u := newUser(t)
	u.start()
	u.press("Деньги")
	u.press("Вуз А")
	u.press("Ничего")
	u.press("Готово")

	u.handle(Event{Type: EventCallback, CallbackID: "0", Payload: payload(actCard, "money", "0", "for_orphans"), SourceText: "список"})
	if got := u.chat.last().Text; !strings.Contains(got, "Нашёл 1 пункт.") {
		t.Errorf("a card that is not for the user shows the list:\n%s", got)
	}

	u.press("Изменить анкету")
	u.handle(Event{Type: EventCallback, CallbackID: "0", Payload: payload(actResults, "money"), SourceText: "список"})
	if got := u.chat.last().Text; !strings.Contains(got, "Вопрос 1 из 2") {
		t.Errorf("results without the answers ask the questions:\n%s", got)
	}
}

// Going back forgets the previous answer and asks the question again, with
// the old answer ticked.
func TestBackToPreviousQuestion(t *testing.T) {
	u := newUser(t)
	u.start()
	u.press("Деньги")
	if slices.ContainsFunc(u.buttons(), func(row []string) bool { return slices.Contains(row, labelBack) }) {
		t.Error("the first question has nothing to go back to")
	}
	u.press("Вуз А")
	u.press("Сирота")
	u.press("Назад")

	if got := u.chat.last().Text; !strings.Contains(got, "Вопрос 1 из 2\nГде учишься?") {
		t.Fatalf("back to the first question:\n%s", got)
	}
	if got := u.buttons()[0]; !slices.Equal(got, []string{"✅ Вуз А", "Вуз Б"}) {
		t.Errorf("the old answer must be ticked: %q", got)
	}
	if s := u.session(); len(s.Answers["uni"]) != 0 || s.State != session.StateSurvey {
		t.Fatalf("the answer must be open again: %+v", s)
	}

	u.press("Вуз Б")
	if s := u.session(); !slices.Equal(s.Answers["uni"], []string{"b"}) || len(s.Selected) != 0 {
		t.Fatalf("the new answer must be saved: %+v", s)
	}
	if got := u.chat.last().Text; !strings.Contains(got, "Вопрос 2 из 2") {
		t.Errorf("then the next question again:\n%s", got)
	}

	// A stale back from the first question changes nothing.
	u.handle(Event{Type: EventCallback, CallbackID: "cb", Payload: payload(actBack, "uni"), SourceText: "вопрос"})
	if s := u.session(); !slices.Equal(s.Answers["uni"], []string{"b"}) || u.chat.lastAnswer().Notification != textStale {
		t.Errorf("a stale back: %+v", s)
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
	resumed := u.chat.last().Text
	if !strings.Contains(resumed, "продолжаем с вопроса 2") || !strings.Contains(resumed, "Что про тебя?") {
		t.Fatalf("should resume the first category: %q", resumed)
	}
	// A tick changes the buttons only: the text keeps the note, and the
	// buttons do not jump under the finger.
	u.press("Сирота")
	if got := u.chat.last().Text; got != resumed {
		t.Errorf("a tick changed the text to\n%s", got)
	}
}

// A press on a message deleted meanwhile gets the next screen as a new
// message.
func TestPressOnDeletedMessage(t *testing.T) {
	u := newUser(t)
	u.start()
	u.handle(Event{Type: EventCallback, CallbackID: "cb", Payload: payload(actCategory, "money")})
	if n := len(u.chat.messages); n != 2 || !strings.Contains(u.chat.last().Text, "Вопрос 1 из 2") {
		t.Fatalf("the question must come as a new message:\n%s", u.allText())
	}
	if a := u.chat.lastAnswer(); a.Edit != nil || a.Notification == "" {
		t.Errorf("the press must still be acknowledged: %+v", a)
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

	u.press("Показать подборку")
	if !u.hasButton("Для всех") {
		t.Fatalf("reuse should show results:\n%s", u.chat.last().Text)
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
	i, _ := u.find("Вуз А")
	oldQuestion := u.chat.messages[i].Text
	u.press("Вуз А")

	// A second tap on the answered question, e.g. before the screen changed,
	// must not overwrite anything, nor take away the buttons of the message:
	// it shows the next question already.
	u.handle(Event{Type: EventCallback, CallbackID: "0", Payload: payload(actAnswer, "uni", "b"), SourceText: oldQuestion})
	if a := u.chat.lastAnswer(); a.Notification != textStale || a.Edit != nil {
		t.Fatalf("stale press should only notify: %+v", a)
	}
	if s := u.session(); !slices.Equal(s.Answers["uni"], []string{"a"}) {
		t.Fatalf("stale press changed the answer: %v", s.Answers["uni"])
	}
	if !u.hasButton("Готово") {
		t.Fatalf("the next question must keep its buttons:\n%s", u.chat.last().Text)
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

	i, btn := u.find("Спорт")
	ev := Event{Type: EventCallback, UserID: u.id, CallbackID: strconv.Itoa(i), Payload: btn.Payload, SourceText: u.chat.messages[i].Text}
	if err := u.bot.Handle(context.Background(), ev); err == nil {
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

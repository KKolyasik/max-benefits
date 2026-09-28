package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/KKolyasik/max-benefits/internal/feedback"
	"github.com/KKolyasik/max-benefits/internal/session"
)

// inbox is an in-memory store of the feedback, standing in for Postgres.
type inbox struct {
	mu    sync.Mutex
	items []feedback.Feedback
	// resolved maps a feedback ID to the admin who resolved it.
	resolved map[int64]int64
	// down fails deleting the answers.
	down bool
}

func (in *inbox) AddFeedback(_ context.Context, f feedback.Feedback) (int64, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	f.ID = int64(len(in.items) + 1)
	in.items = append(in.items, f)
	return f.ID, nil
}

func (in *inbox) UserFeedbackSince(_ context.Context, userID int64, since time.Time) (int, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	n := 0
	for _, f := range in.items {
		if f.UserID == userID && f.CreatedAt.After(since) {
			n++
		}
	}
	return n, nil
}

func (in *inbox) PendingFeedback(context.Context) (int, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	return len(in.items) - len(in.resolved), nil
}

func (in *inbox) NextFeedback(_ context.Context, after int64) (feedback.Feedback, bool, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	for _, f := range in.items {
		if _, done := in.resolved[f.ID]; f.ID > after && !done {
			return f, true, nil
		}
	}
	return feedback.Feedback{}, false, nil
}

func (in *inbox) ResolveFeedback(_ context.Context, id, admin int64) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	if _, done := in.resolved[id]; !done {
		in.resolved[id] = admin
	}
	return nil
}

func (in *inbox) ForgetFeedbackAnswers(_ context.Context, userID int64) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.down {
		return errors.New("postgres is down")
	}
	for i := range in.items {
		if in.items[i].UserID == userID {
			in.items[i].Answers = nil
		}
	}
	return nil
}

func (in *inbox) all() []feedback.Feedback {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.items
}

// newFeedbackSetup returns an admin and a student of a bot that takes
// feedback.
func newFeedbackSetup(t *testing.T) (admin, student *user, in *inbox) {
	t.Helper()
	admin, student, _ = newAdminSetup(t)
	in = &inbox{resolved: map[int64]int64{}}
	admin.bot.WithFeedback(in)
	return admin, student, in
}

// An idea from the menu goes to the admins, and the student is back in the
// menu.
func TestIdeaFromMenu(t *testing.T) {
	admin, student, in := newFeedbackSetup(t)
	student.start()
	student.press(labelFeedback)
	if got := student.chat.last().Text; got != textAskIdea {
		t.Errorf("the prompt: %q", got)
	}
	student.say("Добавьте раздел про общежития")
	if got := student.chat.last().Text; got != textThanks+"\n\n"+textMenu {
		t.Errorf("after the feedback: %q", got)
	}
	if s := student.session(); s.State != session.StateMenu {
		t.Errorf("the student must be back in the menu: %+v", s)
	}
	if got := in.all(); len(got) != 1 || got[0].UserID != 42 || got[0].Category != "" ||
		got[0].Text != "Добавьте раздел про общежития" {
		t.Fatalf("saved: %+v", got)
	}

	notice := admin.chat.last()
	assertHas(t, notice.Text, "💬 Новый отзыв №1\nОт пользователя 42", "📍 Откуда: главное меню",
		"\n\nДобавьте раздел про общежития")
	if notice.Markdown {
		t.Error("what a student wrote must not be markdown")
	}
	admin.press(labelResolve)
	if got := admin.chat.lastAnswer().Edit.Text; !strings.HasSuffix(got, "👉 "+textResolved) || !strings.Contains(got, "общежития") {
		t.Errorf("the resolved notification must stay in the chat: %q", got)
	}
	if in.resolved[1] != adminID {
		t.Errorf("resolved: %v", in.resolved)
	}
}

// No fitting option: the student writes which one, and the questionnaire
// goes on from the same question, ticks and all.
func TestNoFittingOption(t *testing.T) {
	admin, student, in := newFeedbackSetup(t)
	student.start()
	student.press("Деньги")
	student.press("Вуз А")
	student.press("Сирота")
	student.press(labelNoOption)
	if got := student.chat.last().Text; got != fmt.Sprintf(textAskOption, "Статус") {
		t.Errorf("the prompt: %q", got)
	}
	student.say("Я иностранный студент")

	if got := student.chat.last().Text; !strings.HasPrefix(got, "Деньги\n"+textThanksBack+"\n\nВопрос 2 из 2") {
		t.Errorf("back to the question:\n%s", got)
	}
	if got := student.buttons()[0][0]; got != "✅ Сирота" {
		t.Errorf("the ticks must stay: %q", got)
	}
	if s := student.session(); s.State != session.StateSurvey || s.Question != "" {
		t.Errorf("the questionnaire goes on: %+v", s)
	}
	if got := in.all(); len(got) != 1 || got[0].Category != "money" || got[0].Question != "status" || got[0].Answers != nil {
		t.Fatalf("saved: %+v", got)
	}
	assertHas(t, admin.chat.last().Text, "📍 Откуда: Деньги → вопрос «Статус», нет подходящего варианта")

	student.press("Готово")
	if s := student.session(); len(s.Answers["status"]) != 1 {
		t.Errorf("the ticks must be the answer: %+v", s)
	}
}

// Nothing found: the admins get the answers the base has nothing for.
func TestNothingFoundFeedback(t *testing.T) {
	admin, student, in := newFeedbackSetup(t)
	student.start()
	student.press("Досуг")
	student.press("Вуз А")
	student.press("Искусство")
	if !strings.Contains(student.chat.last().Text, textNothingFound) || student.buttons()[0][0] != labelMissing {
		t.Fatalf("empty results must ask what is missing:\n%s", student.allText())
	}
	student.press(labelMissing)
	student.say("Скидки в театры")

	if got := student.chat.last().Text; got != textThanks+"\n\n"+textMenu {
		t.Errorf("after the feedback: %q", got)
	}
	got := in.all()
	if len(got) != 1 || got[0].Category != "fun" || got[0].Question != "" ||
		fmt.Sprint(got[0].Answers) != "map[hobby:[art] uni:[a]]" {
		t.Fatalf("saved: %+v", got)
	}
	assertHas(t, admin.chat.last().Text, "📍 Откуда: Досуг, ничего не нашлось по ответам:\n• Вуз: Вуз А\n• Хобби: Искусство\n\nСкидки в театры")
}

// Cancel goes back to where the feedback started, in the same message.
func TestFeedbackCancel(t *testing.T) {
	_, student, in := newFeedbackSetup(t)
	student.start()
	student.press(labelFeedback)
	student.press(labelCancel)
	if got := student.chat.last().Text; got != textMenu {
		t.Errorf("back to the menu: %q", got)
	}

	student.press("Деньги")
	student.press("Вуз А")
	student.press("Сирота")
	student.press(labelNoOption)
	student.press(labelCancel)
	if got := student.chat.last().Text; !strings.HasPrefix(got, "Деньги\n\nВопрос 2 из 2") {
		t.Errorf("back to the question:\n%s", got)
	}
	if got := student.buttons()[0][0]; got != "✅ Сирота" {
		t.Errorf("the ticks must stay: %q", got)
	}
	if s := student.session(); s.State != session.StateSurvey {
		t.Errorf("the questionnaire goes on: %+v", s)
	}

	student.press("Готово")
	student.press(labelOtherTopics)
	student.press("Досуг")
	student.press("Искусство")
	student.press(labelMissing)
	student.press(labelCancel)
	if got := student.chat.last().Text; !strings.Contains(got, textNothingFound) {
		t.Errorf("back to the results:\n%s", got)
	}

	if n := len(student.chat.messages); n != 1 {
		t.Errorf("cancel must change the message, got %d messages:\n%s", n, student.allText())
	}
	if len(in.all()) > 0 {
		t.Error("nothing must be saved")
	}
	student.handle(Event{Type: EventCallback, CallbackID: "cb", Payload: actFeedbackCancel, SourceText: "отзыв"})
	if got := student.chat.lastAnswer().Notification; got != textStale {
		t.Errorf("a second cancel: %q", got)
	}
}

// Commands and buttons of other messages leave the feedback unwritten; an
// empty message is asked again as text.
func TestLeavingFeedback(t *testing.T) {
	_, student, in := newFeedbackSetup(t)
	student.start()
	student.press(labelFeedback)
	student.say("меню")
	if s := student.session(); s.State != session.StateMenu || student.chat.last().Text != textMenu {
		t.Errorf("a command leaves the feedback: %+v", s)
	}

	student.press(labelFeedback)
	student.say(" ")
	if got := student.chat.last().Text; got != textAskText+"\n\n"+textAskIdea {
		t.Errorf("an empty message: %q", got)
	}
	student.say("/profile")
	student.say("просто текст")
	if got := student.chat.last().Text; !strings.Contains(got, textUseButtons) {
		t.Errorf("free text after leaving the feedback: %q", got)
	}

	student.press(labelFeedback)
	// "В меню" of the profile, sent before.
	student.pressOn(len(student.chat.messages)-2, Button{Payload: actMenu})
	student.say("ещё текст")
	if got := student.chat.last().Text; !strings.Contains(got, textUseButtons) {
		t.Errorf("free text after a button of another message: %q", got)
	}

	student.press("Деньги")
	student.press(labelNoOption)
	student.say("/help")
	if s := student.session(); s.State != session.StateSurvey || s.Category != "money" {
		t.Errorf("a command leaves the feedback for the questionnaire: %+v", s)
	}
	student.say("/unknown")
	if s := student.session(); s.State != session.StateSurvey {
		t.Errorf("an unknown command is not the feedback: %+v", s)
	}
	if len(in.all()) > 0 {
		t.Errorf("nothing must be saved: %+v", in.all())
	}
}

func TestFeedbackLimit(t *testing.T) {
	_, student, in := newFeedbackSetup(t)
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	student.bot.now = func() time.Time { return now }
	student.start()
	for i := range maxFeedbackPerDay {
		student.press(labelFeedback)
		student.say(fmt.Sprintf("идея %d", i+1))
	}
	student.press(labelFeedback)
	if got := student.chat.lastAnswer().Notification; got != textWroteEnough {
		t.Errorf("over the limit: %q", got)
	}
	if s := student.session(); s.State != session.StateMenu {
		t.Errorf("over the limit the bot waits for nothing: %+v", s)
	}

	now = now.Add(25 * time.Hour)
	student.press(labelFeedback)
	if s := student.session(); s.State != session.StateFeedback {
		t.Errorf("a day later the student may write again: %+v", s)
	}
	if n := len(in.all()); n != maxFeedbackPerDay {
		t.Errorf("saved %d", n)
	}
}

// The admin flips through the feedback in one message: resolves it or skips
// it and comes back to the skipped ones.
func TestInbox(t *testing.T) {
	admin, student, in := newFeedbackSetup(t)
	student.start()
	for _, text := range []string{"первый", "второй", "третий"} {
		student.press(labelFeedback)
		student.say(text)
	}
	admin.start()
	sent := len(admin.chat.messages)

	admin.press(labelInbox)
	screen := func(want string) {
		t.Helper()
		if got := admin.chat.last().Text; !strings.HasPrefix(got, want) {
			t.Errorf("want %q, got:\n%s", want, got)
		}
	}
	screen("📬 Отзыв №1 · неразобранных: 3\n")
	admin.press(labelSkip)
	screen("📬 Отзыв №2 · неразобранных: 3\n")
	admin.press(labelResolve)
	if got := admin.chat.lastAnswer().Notification; got != textResolved {
		t.Errorf("toast: %q", got)
	}
	screen("📬 Отзыв №3 · неразобранных: 2\n")
	admin.press(labelResolve)
	screen(fmt.Sprintf(textLastFeedback, 1))
	admin.press(labelFromStart)
	screen("📬 Отзыв №1 · неразобранных: 1\n")
	admin.press(labelResolve)
	screen(textNoFeedback)

	if n := len(admin.chat.messages); n != sent {
		t.Errorf("the feedback must be flipped through in one message, got %d new", n-sent)
	}
	if len(in.resolved) != 3 || in.resolved[2] != adminID {
		t.Errorf("resolved: %v", in.resolved)
	}

	admin.say("/feedback")
	screen(textNoFeedback)
}

func TestStudentsHaveNoInbox(t *testing.T) {
	_, student, in := newFeedbackSetup(t)
	student.start()
	student.press(labelFeedback)
	student.say("идея")
	if student.hasButton(labelInbox) {
		t.Error("only admins see the feedback")
	}
	for _, p := range []string{actInbox, payload(actResolve, "1"), payload(actResolveNotice, "1")} {
		student.handle(Event{Type: EventCallback, CallbackID: "cb", Payload: p, SourceText: "подделка"})
		if got := student.chat.lastAnswer().Notification; got != textStale {
			t.Errorf("a forged %s press must be ignored, got %q", p, got)
		}
	}
	student.say("/feedback")
	if got := student.chat.last().Text; !strings.Contains(got, textUseButtons) {
		t.Errorf("/feedback for a student: %q", got)
	}
	if len(in.resolved) > 0 {
		t.Error("the feedback must stay unresolved")
	}
}

// Without a store for the feedback there are no buttons for it.
func TestNoFeedbackWithoutStore(t *testing.T) {
	u := newUser(t)
	u.start()
	u.press("Досуг")
	u.press("Вуз А")
	u.press("Искусство")
	for _, label := range []string{labelFeedback, labelNoOption, labelMissing} {
		if strings.Contains(u.allText(), label) {
			t.Errorf("no %q without a store", label)
		}
	}
	u.handle(Event{Type: EventCallback, CallbackID: "0", Payload: actFeedback, SourceText: "меню"})
	if got := u.chat.lastAnswer().Notification; got != textStale {
		t.Errorf("a press on the feedback without a store: %q", got)
	}
}

// The longest feedback about the longest category fits a message of MAX,
// and the base keeps it whole.
func TestLongFeedbackFits(t *testing.T) {
	sv, kb := loadData(t)
	b := New(sv, kb, session.NewMemory(), &chat{}, slog.New(slog.DiscardHandler))
	for _, c := range sv.Categories {
		answers := map[string][]string{}
		for _, qid := range c.Questions {
			q, _ := sv.Question(qid)
			for _, o := range q.Options {
				answers[qid] = append(answers[qid], o.ID)
			}
		}
		f := feedback.Feedback{ID: 1_000_000, UserID: 1 << 62, Category: c.ID, Answers: answers,
			Text: strings.Repeat("я", 4000), CreatedAt: time.Now()}
		for _, text := range []string{
			fmt.Sprintf("💬 Новый отзыв №%d\n%s", f.ID, b.feedbackText(f)),
			fmt.Sprintf("📬 Отзыв №%d · неразобранных: %d\n%s", f.ID, 1_000_000, b.feedbackText(f)),
		} {
			if n := utf8.RuneCountInString(text); n > maxMessageRunes {
				t.Errorf("%s: %d runes", c.ID, n)
			}
		}
	}
}

// Deleting the data deletes the answers sent with the feedback too; the
// text stays for the admins.
func TestDeleteForgetsFeedbackAnswers(t *testing.T) {
	admin, student, in := newFeedbackSetup(t)
	student.start()
	student.press("Досуг")
	student.press("Вуз А")
	student.press("Искусство")
	student.press(labelMissing)
	student.say("Скидки в театры")

	student.press(labelProfile)
	student.press(labelDelete)
	student.press(labelDeleteYes)
	if got := student.chat.last().Text; got != textDeleted {
		t.Errorf("after deleting: %q", got)
	}
	if got := in.all(); len(got) != 1 || got[0].Answers != nil || got[0].Text != "Скидки в театры" || got[0].UserID != 42 {
		t.Fatalf("the answers must be gone, the text must stay: %+v", got)
	}
	admin.start()
	admin.press(labelInbox)
	got := admin.chat.last().Text
	assertHas(t, got, "📍 Откуда: Досуг, ничего не нашлось; ответы анкеты студент удалил", "Скидки в театры")
	assertHasNot(t, got, "Вуз А")
}

// If the answers of the feedback can't be deleted, nothing is.
func TestDeleteFailsWhole(t *testing.T) {
	_, student, in := newFeedbackSetup(t)
	student.start()
	student.press("Деньги")
	student.press("Вуз А")
	student.press(labelMenu)
	student.press(labelProfile)
	student.press(labelDelete)

	in.down = true
	i, btn := student.find(labelDeleteYes)
	ev := Event{Type: EventCallback, UserID: student.id, CallbackID: strconv.Itoa(i), Payload: btn.Payload, SourceText: student.chat.messages[i].Text}
	if err := student.bot.Handle(context.Background(), ev); err == nil {
		t.Fatal("expected the error to be reported")
	}
	if got := student.chat.last().Text; !strings.Contains(got, textFailed) {
		t.Errorf("the student must be told: %q", got)
	}
	if s := student.session(); len(s.Answers["uni"]) == 0 {
		t.Errorf("the answers must stay until they can be deleted everywhere: %+v", s)
	}
}

// While reviewers try the bot, everyone reads the feedback, but only the
// listed admins are told about a new one.
func TestFeedbackWithAdminForAll(t *testing.T) {
	admin, student, _ := newFeedbackSetup(t)
	admin.bot.WithAdminForAll()
	student.start()
	student.press(labelFeedback)
	student.say("идея")
	if got := admin.chat.last().Text; !strings.HasPrefix(got, "💬 Новый отзыв №1") {
		t.Errorf("the listed admin must be told: %q", got)
	}
	if strings.Contains(student.allText(), "Новый отзыв") {
		t.Error("only the listed admins are told about the feedback")
	}
	student.press(labelInbox)
	if got := student.chat.last().Text; !strings.HasPrefix(got, "📬 Отзыв №1") {
		t.Errorf("with the flag on, everyone reads the feedback: %q", got)
	}
}

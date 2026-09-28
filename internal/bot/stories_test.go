package bot

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/KKolyasik/max-benefits/internal/knowledge"
	"github.com/KKolyasik/max-benefits/internal/session"
	"github.com/KKolyasik/max-benefits/internal/survey"
)

// These tests run the real questionnaire and knowledge base from data/.

func loadData(t testing.TB) (*survey.Survey, *knowledge.Static) {
	t.Helper()
	sv, err := survey.Load("../../data/survey.yaml")
	if err != nil {
		t.Fatal(err)
	}
	kb, err := knowledge.LoadStatic("../../data/knowledge.yaml", sv)
	if err != nil {
		t.Fatal(err)
	}
	return sv, kb
}

func dataUser(t *testing.T) *user {
	sv, kb := loadData(t)
	return newUserWith(t, sv, kb, session.NewMemory())
}

func assertHas(t *testing.T, text string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(text, w) {
			t.Errorf("expected %q in the answer", w)
		}
	}
}

func assertHasNot(t *testing.T, text string, unwanted ...string) {
	t.Helper()
	for _, w := range unwanted {
		if strings.Contains(text, w) {
			t.Errorf("did not expect %q in the answer", w)
		}
	}
}

// User story 1 from the README: Даша поступила в Горный, денег нет,
// родители пенсионеры, у неё инвалидность. Она хочет узнать свои льготы.
func TestStoryDashaBenefits(t *testing.T) {
	u := dataUser(t)
	u.start()
	u.press("Льготы")
	u.press("Горный")
	u.press("Очная")
	u.press("Бюджет")
	u.press("Из другого региона")
	u.press("Снимаю жильё")
	u.press("Есть инвалидность")
	u.press("Семья с низким доходом")
	u.press("Родители-пенсионеры")
	u.press("Готово")

	text := u.readResults()
	assertHas(t, text,
		"Государственная социальная стипендия",
		"Справка о государственной социальной помощи",
		"Материальная помощь от вуза",
		"Поддержка студентов с инвалидностью",
		"Студенческий проездной",
		"Место в общежитии",
		"Поликлиника в Петербурге",
		"Временная регистрация",
		"https://spmi.ru/materialnaya-i-socialnaya-podderzhka-obuchayuschimsya",
	)
	assertHasNot(t, text, "Как перейти с платного на бюджет", "spb.hse.ru")
}

// User story 2 from the README: Петя поступил в ВШЭ и хочет узнать, как
// поднять стипендию. Сессии ещё не было, достижений пока нет.
func TestStoryPetyaScholarships(t *testing.T) {
	u := dataUser(t)
	u.start()
	u.press("Стипендии")
	u.press("ВШЭ")
	u.press("Очная")
	u.press("Бюджет")
	u.press("1 курс")
	u.press("Сессии ещё не было")
	u.press("Пока ничего")
	u.press("Готово")
	u.press("Ничего из этого")
	u.press("Готово")

	text := u.readResults()
	assertHas(t, text,
		"Государственная академическая стипендия",
		"С чего начать, чтобы получать повышенную стипендию",
		"Олимпиада «Я — профессионал»",
		"Стипендии вуза и компаний-партнёров",
		"https://spb.hse.ru/news/845530619.html",
	)
	assertHasNot(t, text, "Государственная социальная стипендия", "Повышенная академическая стипендия (ПГАС)")

	// Later Петя opens "Льготы": the shared questions are not asked again.
	u.press("Другие разделы")
	u.press("Льготы")
	if q := u.chat.last().Text; !strings.Contains(q, "Откуда ты приехал") {
		t.Fatalf("expected to continue from the first new question, got %q", q)
	}
}

// Back to a multi-choice question brings its ticks back.
func TestBackToMultiChoice(t *testing.T) {
	u := dataUser(t)
	u.start()
	u.press("Стипендии")
	u.press("ИТМО")
	u.press("Очная")
	u.press("Бюджет")
	u.press("2 курс")
	u.press("На «хорошо» и «отлично»")
	u.press("Спорт")
	u.press("Творчество")
	u.press("Готово")

	u.press("Назад")
	if got := u.allText(); !strings.Contains(got, "Вопрос 6 из 7") || !strings.Contains(got, "✅ Спорт") ||
		!strings.Contains(got, "✅ Творчество") || !strings.Contains(got, "▫️ Олимпиады") {
		t.Fatalf("the old choice must be ticked:\n%s", got)
	}
	u.press("Готово")
	if s := u.session(); !slices.Equal(s.Answers["achievements"], []string{"sport", "culture"}) {
		t.Errorf("the choice must be saved again: %v", s.Answers["achievements"])
	}
	if got := u.chat.last().Text; !strings.Contains(got, "Вопрос 7 из 7") {
		t.Errorf("then the last question:\n%s", got)
	}
}

// TestEveryProfile walks through the answer combinations of every category
// and checks that the content is sane: every profile gets something, every
// screen fits into MAX limits, and every entry is reachable.
func TestEveryProfile(t *testing.T) {
	sv, kb := loadData(t)
	reached := map[string]bool{}
	for _, c := range sv.Categories {
		n := 0
		for answers := range profiles(sv, c.Questions) {
			n++
			entries, err := kb.Find(context.Background(), knowledge.Request{Category: c.ID, Answers: answers})
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) == 0 {
				t.Fatalf("%s: nothing found for %v", c.ID, answers)
			}
			for _, e := range entries {
				reached[e.ID] = true
			}
			for _, msg := range screens(c, entries) {
				if err := fits(msg); err != nil {
					t.Fatalf("%s: %v for %v:\n%s", c.ID, err, answers, msg.Text)
				}
			}
		}
		t.Logf("%s: %d profiles checked", c.ID, n)
	}

	for _, id := range kb.IDs() {
		if !reached[id] {
			t.Errorf("entry %q is never shown to anyone", id)
		}
	}
}

// profiles yields answer combinations: every option of single-choice
// questions and, for multi-choice ones, each option alone plus all regular
// options together (the longest result).
func profiles(sv *survey.Survey, qids []string) func(func(map[string][]string) bool) {
	return func(yield func(map[string][]string) bool) {
		var walk func(i int, acc map[string][]string) bool
		walk = func(i int, acc map[string][]string) bool {
			if i == len(qids) {
				return yield(maps.Clone(acc))
			}
			q, _ := sv.Question(qids[i])
			for _, variant := range variants(q) {
				acc[q.ID] = variant
				if !walk(i+1, acc) {
					return false
				}
			}
			return true
		}
		walk(0, map[string][]string{})
	}
}

func variants(q *survey.Question) [][]string {
	var out [][]string
	var all []string
	for _, o := range q.Options {
		out = append(out, []string{o.ID})
		if !o.Exclusive {
			all = append(all, o.ID)
		}
	}
	if q.Multi && len(all) > 1 {
		out = append(out, all)
	}
	return out
}

func TestPackSplitsLongText(t *testing.T) {
	blocks := []string{strings.Repeat("а", 30), strings.Repeat("б", 30), strings.Repeat("в", 30)}
	got := pack(blocks, "\n\n", 64)
	if len(got) != 2 || got[0] != blocks[0]+"\n\n"+blocks[1] || got[1] != blocks[2] {
		t.Fatalf("unexpected packing: %q", got)
	}

	long := strings.Repeat("строка\n", 50) + strings.Repeat("я", 100)
	for _, part := range pack([]string{"заголовок", long}, "\n\n", 40) {
		if l := utf8.RuneCountInString(part); l > 40 {
			t.Fatalf("part of %d runes exceeds the limit: %q", l, part)
		}
	}
}

func TestLayout(t *testing.T) {
	short := layout([]Button{{Text: "СПбГУ"}, {Text: "ВШЭ"}, {Text: "ИТМО"}})
	if len(short) != 2 || len(short[0]) != 2 {
		t.Fatalf("short buttons should go two per row: %v", short)
	}
	long := layout([]Button{{Text: "Сирота или без попечения родителей"}, {Text: "Нет"}})
	if len(long) != 2 || len(long[0]) != 1 {
		t.Fatalf("long buttons should go one per row: %v", long)
	}
}

// screens are all the screens of the results: the list and every page of
// every card.
func screens(c *survey.Category, entries []knowledge.Entry) []Message {
	out := []Message{resultsMessage(c, entries)}
	for _, p := range cardPages(entries) {
		msg, _ := cardMessage(c, entries, entries[p.entry].ID, p.part)
		out = append(out, msg)
	}
	return out
}

// fits checks a message against the limits of MAX: 4000 characters, 30
// rows of buttons, 7 buttons a row or 3 links, 128 characters a button.
func fits(msg Message) error {
	if l := utf8.RuneCountInString(msg.Text); l > 4000 {
		return fmt.Errorf("a message of %d runes", l)
	}
	if len(msg.Keyboard) > 30 {
		return fmt.Errorf("%d rows of buttons", len(msg.Keyboard))
	}
	for _, row := range msg.Keyboard {
		links := 0
		for _, b := range row {
			if b.URL != "" {
				links++
			}
			if l := utf8.RuneCountInString(b.Text); l == 0 || l > 128 {
				return fmt.Errorf("a button of %d runes", l)
			}
		}
		if len(row) > 7 || links > 3 {
			return fmt.Errorf("a row of %d buttons, %d of them links", len(row), links)
		}
	}
	return nil
}

// A card too long for a message takes a few screens, flipped through like
// the cards.
func TestLongCardTakesPages(t *testing.T) {
	c := &survey.Category{ID: "money", Title: "Деньги"}
	long := knowledge.Entry{ID: "long", Title: "Длинная", Summary: "Много шагов.",
		Links: []knowledge.Link{{Title: "Сайт", URL: "https://long.example"}}}
	for range 200 {
		long.Steps = append(long.Steps, strings.Repeat("шаг ", 10))
	}
	entries := []knowledge.Entry{long, {ID: "short", Title: "Короткая", Summary: "Коротко."}}

	first, ok := cardMessage(c, entries, "long", 0)
	if !ok || !strings.HasPrefix(first.Text, "Деньги · 1 из 2, часть 1 из 3\n\n**Длинная**") {
		t.Fatalf("the first page:\n%.200s", first.Text)
	}
	next := first.Keyboard[len(first.Keyboard)-1]
	if next[len(next)-1].Payload != payload(actCard, "money", "1", "long") {
		t.Errorf("the arrow leads to the next part: %+v", next)
	}
	if first.Keyboard[0][0].URL != "https://long.example" {
		t.Errorf("every part has the links of the card: %+v", first.Keyboard)
	}
	short, _ := cardMessage(c, entries, "short", 0)
	if back := short.Keyboard[0][0]; back.Text != labelBack || back.Payload != payload(actCard, "money", "2", "long") {
		t.Errorf("back from the next card leads to the last part: %+v", back)
	}
	for _, msg := range screens(c, entries) {
		if err := fits(msg); err != nil {
			t.Errorf("%v:\n%.200s", err, msg.Text)
		}
	}
}

// The list has room for the next steps under the cards however many there
// are: the ones that do not fit are a flip away.
func TestManyResults(t *testing.T) {
	c := &survey.Category{ID: "money", Title: "Деньги"}
	var entries []knowledge.Entry
	for i := range maxListed + 4 {
		entries = append(entries, knowledge.Entry{ID: fmt.Sprint(i), Title: strings.Repeat("Очень длинный заголовок ", 10), Summary: "Да."})
	}
	list := resultsMessage(c, entries)
	if err := fits(list); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(list.Text, "Нашёл 29 пунктов") {
		t.Errorf("the list:\n%s", list.Text)
	}
	more := list.Keyboard[maxListed][0]
	if more.Text != "Ещё 4 пункта ▶️" || more.Payload != payload(actCard, "money", "0", fmt.Sprint(maxListed)) {
		t.Errorf("the button to the rest: %+v", more)
	}
}

func TestPlural(t *testing.T) {
	for n, want := range map[int]string{1: "пункт", 2: "пункта", 4: "пункта", 5: "пунктов", 11: "пунктов",
		12: "пунктов", 14: "пунктов", 21: "пункт", 22: "пункта", 25: "пунктов", 111: "пунктов"} {
		if got := plural(n, "пункт", "пункта", "пунктов"); got != want {
			t.Errorf("%d %s, want %s", n, got, want)
		}
	}
}

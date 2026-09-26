package bot

import (
	"context"
	"maps"
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

	text := u.allText()
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

	text := u.allText()
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

// TestEveryProfile walks through the answer combinations of every category
// and checks that the content is sane: every profile gets something, every
// message fits into MAX limits, and every entry is reachable.
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
			for _, msg := range resultMessages(c, entries) {
				if l := utf8.RuneCountInString(msg.Text); l > 4000 {
					t.Fatalf("%s: message of %d runes for %v", c.ID, l, answers)
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

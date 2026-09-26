package knowledge

import (
	"context"
	"strings"
	"testing"

	"github.com/KKolyasik/max-benefits/internal/survey"
)

const testSurvey = `
categories:
  - {id: money, title: "Деньги", questions: [uni, status]}
  - {id: fun, title: "Досуг", questions: [hobby]}
questions:
  - id: uni
    label: "Вуз"
    text: "Где учишься?"
    options: [{id: a, title: "А"}, {id: b, title: "Б"}]
  - id: status
    label: "Статус"
    text: "Что про тебя?"
    multi: true
    options: [{id: orphan, title: "Сирота"}, {id: poor, title: "Бедный"}, {id: none, title: "Ничего", exclusive: true}]
  - id: hobby
    label: "Хобби"
    text: "Что любишь?"
    options: [{id: art, title: "Искусство"}, {id: sport, title: "Спорт"}]
`

func parseSurvey(t *testing.T) *survey.Survey {
	t.Helper()
	s, err := survey.Parse([]byte(testSurvey))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestFindMatchesAllConditions(t *testing.T) {
	kb, err := ParseStatic([]byte(`
entries:
  - {id: low, categories: [money], priority: 1, title: "Низкий", summary: "s"}
  - id: both
    categories: [money]
    priority: 5
    match: {uni: [a], status: [orphan, poor]}
    title: "Оба условия"
    summary: "s"
    links:
      - {title: "Для А", url: "https://a.example", when: {uni: [a]}}
      - {title: "Всем", url: "https://all.example"}
  - {id: fun, categories: [fun], title: "Другой раздел", summary: "s"}
`), parseSurvey(t))
	if err != nil {
		t.Fatal(err)
	}

	ids := func(answers map[string][]string) string {
		entries, err := kb.Find(context.Background(), Request{Category: "money", Answers: answers})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range entries {
			out = append(out, e.ID)
		}
		return strings.Join(out, ",")
	}

	if got := ids(map[string][]string{"uni": {"a"}, "status": {"none", "poor"}}); got != "both,low" {
		t.Errorf("any of the options should satisfy a condition, priority first: got %q", got)
	}
	if got := ids(map[string][]string{"uni": {"b"}, "status": {"orphan"}}); got != "low" {
		t.Errorf("every condition must hold: got %q", got)
	}
	if got := ids(nil); got != "low" {
		t.Errorf("unanswered questions never match: got %q", got)
	}

	entries, _ := kb.Find(context.Background(), Request{Category: "money", Answers: map[string][]string{"uni": {"a"}, "status": {"orphan"}}})
	if len(entries[0].Links) != 2 {
		t.Errorf("expected both links for university A: %+v", entries[0].Links)
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]string{
		"unknown category": `
entries:
  - {id: x, categories: [nope], title: "t", summary: "s"}`,
		"unknown question": `
entries:
  - {id: x, categories: [money], title: "t", summary: "s", match: {age: [young]}}`,
		"question from another category": `
entries:
  - {id: x, categories: [money], title: "t", summary: "s", match: {hobby: [art]}}`,
		"unknown option": `
entries:
  - {id: x, categories: [money], title: "t", summary: "s", match: {uni: [c]}}`,
		"bad link condition": `
entries:
  - id: x
    categories: [money]
    title: "t"
    summary: "s"
    links: [{title: "l", url: "https://x.example", when: {uni: [zzz]}}]`,
		"insecure link": `
entries:
  - {id: x, categories: [money], title: "t", summary: "s", links: [{title: "l", url: "http://x.example"}]}`,
		"duplicate id": `
entries:
  - {id: x, categories: [money], title: "t", summary: "s"}
  - {id: x, categories: [money], title: "t", summary: "s"}`,
		"missing summary": `
entries:
  - {id: x, categories: [money], title: "t"}`,
	}
	s := parseSurvey(t)
	for name, yml := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseStatic([]byte(yml), s); err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
}

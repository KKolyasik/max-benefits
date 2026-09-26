package moderation

import (
	"strings"
	"testing"

	"github.com/KKolyasik/max-benefits/internal/knowledge"
	"github.com/KKolyasik/max-benefits/internal/survey"
)

const testSurvey = `
categories:
  - {id: money, title: "Деньги", questions: [form, basis]}
questions:
  - id: form
    label: "Форма"
    text: "Как учишься?"
    options: [{id: full_time, title: "Очно"}, {id: part_time, title: "Заочно"}]
  - id: basis
    label: "Основа"
    text: "Бюджет?"
    options: [{id: budget, title: "Бюджет"}, {id: contract, title: "Платно"}]
`

var card = knowledge.Card{ID: "pass", Categories: []string{"money"}, Priority: 10, Title: "Проездной", Summary: "s",
	Match: knowledge.Condition{"form": {"full_time"}, "basis": {"budget", "contract"}}}

func parse(t *testing.T) *survey.Survey {
	t.Helper()
	s, err := survey.Parse([]byte(testSurvey))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestProblems(t *testing.T) {
	s := parse(t)
	existing := card
	bad := card
	bad.Match = knowledge.Condition{"form": {"nope"}}
	renamed := card
	renamed.ID = "other"

	cases := map[string]struct {
		d       Draft
		current *knowledge.Card
		want    string
	}{
		"new card":                 {Draft{Card: card}, nil, ""},
		"update":                   {Draft{Card: card, Updates: "pass"}, &existing, ""},
		"new card with a taken id": {Draft{Card: card}, &existing, `card "pass" is already in the base`},
		"updated card is gone":     {Draft{Card: card, Updates: "pass"}, nil, "is no longer in the base"},
		"update with another id":   {Draft{Card: renamed, Updates: "pass"}, nil, `the draft updates "pass" but its id is "other"`},
		"unknown option":           {Draft{Card: bad}, nil, `has no option "nope"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := strings.Join(Problems(tc.d, s, tc.current), "; ")
			if tc.want == "" && got != "" || !strings.Contains(got, tc.want) {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestChanges(t *testing.T) {
	updated := card
	updated.Title = "Новый проездной"
	updated.Summary = "  s  " // the same text, wrapped differently
	updated.Links = []knowledge.CardLink{{Title: "Сайт", URL: "https://a.example"}}
	if got := strings.Join(Changes(card, updated), ", "); got != "заголовок, ссылки" {
		t.Errorf("got %q", got)
	}
	same := card
	same.Steps = []string{}
	if got := Changes(card, same); len(got) != 0 {
		t.Errorf("nil and empty lists are the same: %v", got)
	}
}

func TestAudience(t *testing.T) {
	s := parse(t)
	if got := Audience(card, s); got != "Основа: Бюджет или Платно; Форма: Очно" {
		t.Errorf("got %q", got)
	}
	if got := Audience(knowledge.Card{}, s); got != "всем в разделе" {
		t.Errorf("got %q", got)
	}
}

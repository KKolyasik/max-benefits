package knowledge

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/KKolyasik/max-benefits/internal/survey"
)

func TestMarshalCardFormat(t *testing.T) {
	got := MarshalCard(Card{
		ID:         "pgas",
		Categories: []string{"scholarships"},
		Priority:   80,
		Match:      Condition{"study_basis": {"budget"}, "course": {"2", "3plus"}},
		Title:      `🏆 Повышенная стипендия "ПГАС"`,
		Summary: "Платят за успехи в учёбе, науке, общественной работе, спорте или творчестве. " +
			"Размер решает вуз.",
		Steps: []string{"Собери подтверждения."},
		Links: []CardLink{{Title: "Положение", URL: "https://spbu.ru/pgas", When: Condition{"university": {"spbu"}}}},
	})
	want := `id: pgas
categories: [scholarships]
priority: 80
match:
  course: ["2", "3plus"]
  study_basis: [budget]
title: "🏆 Повышенная стипендия \"ПГАС\""
summary: >-
  Платят за успехи в учёбе, науке, общественной работе, спорте или
  творчестве. Размер решает вуз.
steps:
  - "Собери подтверждения."
links:
  - title: "Положение"
    url: "https://spbu.ru/pgas"
    when: {university: [spbu]}
`
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// Every card of the real base survives marshaling unchanged, so rewriting a
// card never alters what the bot shows.
func TestMarshalCardRoundTrip(t *testing.T) {
	cards, s := realBase(t)
	var b strings.Builder
	b.WriteString("entries:\n")
	for _, c := range cards {
		// As a sequence item: "  - id: x" and the rest indented under it.
		b.WriteString("  - " + strings.ReplaceAll(strings.TrimSuffix(MarshalCard(c), "\n"), "\n", "\n    ") + "\n")
	}
	again, err := ParseCards([]byte(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(again, s); err != nil {
		t.Fatal(err)
	}
	for i := range cards {
		if want, got := normalize(cards[i]), normalize(again[i]); !reflect.DeepEqual(want, got) {
			t.Errorf("card %s changed:\n got %+v\nwant %+v", cards[i].ID, got, want)
		}
	}
}

func realBase(t *testing.T) ([]Card, *survey.Survey) {
	t.Helper()
	s, err := survey.Load(filepath.Join("..", "..", "data", "survey.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cards, err := ParseCards([]byte(readFile(t, filepath.Join("..", "..", "data", "knowledge.yaml"))))
	if err != nil {
		t.Fatal(err)
	}
	return cards, s
}

func normalize(c Card) Card {
	c.Summary = strings.Join(strings.Fields(c.Summary), " ")
	return c
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

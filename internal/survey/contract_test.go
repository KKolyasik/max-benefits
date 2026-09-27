package survey

import (
	"reflect"
	"strings"
	"testing"
)

// The message of the shipped survey has every section and every question
// with its options.
func TestSurveyContract(t *testing.T) {
	s, err := Load("../../data/survey.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m := s.Contract()
	if len(m.Categories) != len(s.Categories) || len(m.Questions) != len(s.AllQuestions()) {
		t.Fatalf("%d categories, %d questions", len(m.Categories), len(m.Questions))
	}
	for i, q := range s.AllQuestions() {
		got := m.Questions[i]
		if got.ID != q.ID || got.Label != q.Label || got.Multi != q.Multi || len(got.Options) != len(q.Options) {
			t.Errorf("question %s: %+v", q.ID, got)
		}
		for j, o := range q.Options {
			if got.Options[j].ID != o.ID || got.Options[j].Exclusive != o.Exclusive {
				t.Errorf("question %s, option %s: %+v", q.ID, o.ID, got.Options[j])
			}
		}
	}
	if c := m.Categories[0]; c.ID != s.Categories[0].ID || len(c.Questions) != len(s.Categories[0].Questions) {
		t.Errorf("category %+v", c)
	}
}

// The survey comes back from its message as it was, and a broken message
// is refused like a broken file.
func TestSurveyFromContract(t *testing.T) {
	s, err := Load("../../data/survey.yaml")
	if err != nil {
		t.Fatal(err)
	}
	got, err := FromContract(s.Contract())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, s) {
		t.Errorf("the survey changed on the way:\n got %+v\nwant %+v", got, s)
	}

	m := s.Contract()
	m.Categories[0].Questions = append(m.Categories[0].Questions, "nope")
	if _, err := FromContract(m); err == nil || !strings.Contains(err.Error(), `unknown question "nope"`) {
		t.Errorf("a section with an unknown question: %v", err)
	}
}

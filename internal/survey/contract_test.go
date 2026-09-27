package survey

import "testing"

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

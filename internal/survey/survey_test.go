package survey

import (
	"testing"
)

func TestShippedSurveyIsValid(t *testing.T) {
	s, err := Load("../../data/survey.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Categories) == 0 || len(s.AllQuestions()) == 0 {
		t.Fatal("empty survey")
	}
}

func TestNext(t *testing.T) {
	s, err := Parse([]byte(`
categories:
  - {id: c, title: "C", questions: [q1, q2]}
questions:
  - {id: q1, label: "Q1", text: "?", options: [{id: a, title: "A"}, {id: b, title: "B"}]}
  - {id: q2, label: "Q2", text: "?", options: [{id: a, title: "A"}, {id: b, title: "B"}]}
`))
	if err != nil {
		t.Fatal(err)
	}
	c, _ := s.Category("c")

	answers := map[string][]string{"q2": {"a"}}
	if q, pos := s.Next(c, answers); q.ID != "q1" || pos != 1 {
		t.Fatalf("got %s at %d", q.ID, pos)
	}
	answers["q1"] = []string{"b"}
	if q, _ := s.Next(c, answers); q != nil || !s.Complete(c, answers) {
		t.Fatal("expected the category to be complete")
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]string{
		"no categories": `questions: []`,
		"unknown question": `
categories: [{id: c, title: "C", questions: [q]}]`,
		"colon in id": `
categories: [{id: "c:1", title: "C", questions: [q]}]
questions: [{id: q, label: "Q", text: "?", options: [{id: a, title: "A"}, {id: b, title: "B"}]}]`,
		"single option": `
categories: [{id: c, title: "C", questions: [q]}]
questions: [{id: q, label: "Q", text: "?", options: [{id: a, title: "A"}]}]`,
		"exclusive in single choice": `
categories: [{id: c, title: "C", questions: [q]}]
questions: [{id: q, label: "Q", text: "?", options: [{id: a, title: "A", exclusive: true}, {id: b, title: "B"}]}]`,
		"duplicate option": `
categories: [{id: c, title: "C", questions: [q]}]
questions: [{id: q, label: "Q", text: "?", options: [{id: a, title: "A"}, {id: a, title: "B"}]}]`,
	}
	for name, yml := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(yml)); err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
}

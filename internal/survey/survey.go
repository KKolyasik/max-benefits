// Package survey describes the rubricator (categories) and the questionnaire
// the bot walks the user through before picking knowledge base entries.
//
// Everything is data-driven: categories, questions and answer options are
// loaded from a YAML file, so content people can change the flow without
// touching the code.
package survey

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Survey is a validated, read-only set of categories and questions.
type Survey struct {
	Categories []*Category
	questions  map[string]*Question
	categories map[string]*Category
}

// Category is a rubricator section, e.g. "benefits" or "scholarships".
type Category struct {
	ID    string `yaml:"id"`
	Title string `yaml:"title"`
	// Intro is shown before the first question of the category.
	Intro string `yaml:"intro"`
	// Questions are question IDs asked for this category, in order.
	Questions []string `yaml:"questions"`
}

// Question is a single questionnaire step answered with buttons.
type Question struct {
	ID string `yaml:"id"`
	// Label is a short name used in the profile summary, e.g. "Вуз".
	Label string `yaml:"label"`
	Text  string `yaml:"text"`
	// Multi allows picking several options before pressing "Готово".
	Multi   bool      `yaml:"multi"`
	Options []*Option `yaml:"options"`
}

// Option is an answer button.
type Option struct {
	ID    string `yaml:"id"`
	Title string `yaml:"title"`
	// Exclusive options (like "ничего из этого") deselect every other option
	// in a multi-choice question.
	Exclusive bool `yaml:"exclusive"`
}

type file struct {
	Categories []*Category `yaml:"categories"`
	Questions  []*Question `yaml:"questions"`
}

// Load reads and validates a survey definition from a YAML file.
func Load(path string) (*Survey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read survey: %w", err)
	}
	s, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("survey %s: %w", path, err)
	}
	return s, nil
}

// Parse builds a survey from YAML and validates it.
func Parse(data []byte) (*Survey, error) {
	var f file
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}

	s := &Survey{
		Categories: f.Categories,
		questions:  make(map[string]*Question, len(f.Questions)),
		categories: make(map[string]*Category, len(f.Categories)),
	}
	var errs []error
	for _, q := range f.Questions {
		errs = append(errs, validateQuestion(q)...)
		if _, dup := s.questions[q.ID]; dup {
			errs = append(errs, fmt.Errorf("question %q is defined twice", q.ID))
		}
		s.questions[q.ID] = q
	}
	if len(f.Categories) == 0 {
		errs = append(errs, errors.New("no categories defined"))
	}
	for _, c := range f.Categories {
		if !validID(c.ID) {
			errs = append(errs, fmt.Errorf("category id %q must be non-empty and must not contain ':'", c.ID))
		}
		if _, dup := s.categories[c.ID]; dup {
			errs = append(errs, fmt.Errorf("category %q is defined twice", c.ID))
		}
		s.categories[c.ID] = c
		if strings.TrimSpace(c.Title) == "" {
			errs = append(errs, fmt.Errorf("category %q: empty title", c.ID))
		}
		if len(c.Questions) == 0 {
			errs = append(errs, fmt.Errorf("category %q: no questions", c.ID))
		}
		seen := map[string]bool{}
		for _, qid := range c.Questions {
			if _, ok := s.questions[qid]; !ok {
				errs = append(errs, fmt.Errorf("category %q: unknown question %q", c.ID, qid))
			}
			if seen[qid] {
				errs = append(errs, fmt.Errorf("category %q: question %q listed twice", c.ID, qid))
			}
			seen[qid] = true
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return s, nil
}

func validateQuestion(q *Question) []error {
	var errs []error
	if !validID(q.ID) {
		errs = append(errs, fmt.Errorf("question id %q must be non-empty and must not contain ':'", q.ID))
	}
	if strings.TrimSpace(q.Text) == "" {
		errs = append(errs, fmt.Errorf("question %q: empty text", q.ID))
	}
	if strings.TrimSpace(q.Label) == "" {
		errs = append(errs, fmt.Errorf("question %q: empty label", q.ID))
	}
	if len(q.Options) < 2 {
		errs = append(errs, fmt.Errorf("question %q: need at least two options", q.ID))
	}
	seen := map[string]bool{}
	for _, o := range q.Options {
		if !validID(o.ID) {
			errs = append(errs, fmt.Errorf("question %q: option id %q must be non-empty and must not contain ':'", q.ID, o.ID))
		}
		if seen[o.ID] {
			errs = append(errs, fmt.Errorf("question %q: option %q listed twice", q.ID, o.ID))
		}
		seen[o.ID] = true
		if strings.TrimSpace(o.Title) == "" {
			errs = append(errs, fmt.Errorf("question %q: option %q has empty title", q.ID, o.ID))
		}
		if o.Exclusive && !q.Multi {
			errs = append(errs, fmt.Errorf("question %q: option %q is exclusive, but the question is single-choice", q.ID, o.ID))
		}
	}
	return errs
}

// IDs end up in callback payloads that use ':' as a separator.
func validID(id string) bool {
	return id != "" && !strings.Contains(id, ":")
}

// Category returns a category by ID.
func (s *Survey) Category(id string) (*Category, bool) {
	c, ok := s.categories[id]
	return c, ok
}

// Question returns a question by ID.
func (s *Survey) Question(id string) (*Question, bool) {
	q, ok := s.questions[id]
	return q, ok
}

// Option returns an answer option of the question by ID.
func (q *Question) Option(id string) (*Option, bool) {
	for _, o := range q.Options {
		if o.ID == id {
			return o, true
		}
	}
	return nil, false
}

// Titles converts option IDs to their titles, skipping unknown ones.
func (q *Question) Titles(ids []string) []string {
	titles := make([]string, 0, len(ids))
	for _, id := range ids {
		if o, ok := q.Option(id); ok {
			titles = append(titles, o.Title)
		}
	}
	return titles
}

// Next returns the first unanswered question of the category together with
// its 1-based position. It returns nil once every question is answered.
func (s *Survey) Next(c *Category, answers map[string][]string) (q *Question, pos int) {
	for i, qid := range c.Questions {
		if len(answers[qid]) == 0 {
			return s.questions[qid], i + 1
		}
	}
	return nil, 0
}

// Complete reports whether every question of the category is answered.
func (s *Survey) Complete(c *Category, answers map[string][]string) bool {
	q, _ := s.Next(c, answers)
	return q == nil
}

// Answered counts answered questions of the category.
func (s *Survey) Answered(c *Category, answers map[string][]string) int {
	n := 0
	for _, qid := range c.Questions {
		if len(answers[qid]) > 0 {
			n++
		}
	}
	return n
}

// AllQuestions returns every question used by any category, in the order
// they first appear. Used to render the profile.
func (s *Survey) AllQuestions() []*Question {
	var out []*Question
	seen := map[string]bool{}
	for _, c := range s.Categories {
		for _, qid := range c.Questions {
			if !seen[qid] {
				seen[qid] = true
				out = append(out, s.questions[qid])
			}
		}
	}
	return out
}

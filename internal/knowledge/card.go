package knowledge

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/KKolyasik/max-benefits/internal/survey"
)

// Card is an entry as stored in the knowledge base file: the guide plus the
// conditions for showing it. The agent drafts cards in the same format, so a
// drafted card loads exactly like a hand-written one.
type Card struct {
	ID         string     `yaml:"id" json:"id"`
	Categories []string   `yaml:"categories" json:"categories"`
	Priority   int        `yaml:"priority" json:"priority"`
	Match      Condition  `yaml:"match" json:"match,omitempty"`
	Title      string     `yaml:"title" json:"title"`
	Summary    string     `yaml:"summary" json:"summary"`
	Steps      []string   `yaml:"steps" json:"steps,omitempty"`
	Documents  []string   `yaml:"documents" json:"documents,omitempty"`
	Where      string     `yaml:"where" json:"where,omitempty"`
	Links      []CardLink `yaml:"links" json:"links,omitempty"`
}

// CardLink is a link to an official source.
type CardLink struct {
	Title string `yaml:"title" json:"title"`
	URL   string `yaml:"url" json:"url"`
	// When limits the link to some users, e.g. a university-specific page.
	When Condition `yaml:"when" json:"when,omitempty"`
}

// Condition maps a question ID to the options that satisfy it. A condition
// holds when the user picked at least one of the listed options for every
// listed question. An empty condition always holds.
type Condition map[string][]string

type file struct {
	Entries []Card `yaml:"entries"`
}

// ParseCards reads the cards of a knowledge base file in file order, without
// validating them.
func ParseCards(data []byte) ([]Card, error) {
	var f file
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	return f.Entries, nil
}

// Validate checks every card against the survey and that IDs are unique, so
// a typo in a question or option ID fails loudly instead of silently hiding
// a card.
func Validate(cards []Card, s *survey.Survey) error {
	var errs []error
	seen := map[string]bool{}
	for _, c := range cards {
		name := cmp.Or(c.ID, c.Title)
		for _, err := range ValidateCard(c, s) {
			errs = append(errs, fmt.Errorf("entry %q: %w", name, err))
		}
		if c.ID != "" && seen[c.ID] {
			errs = append(errs, fmt.Errorf("entry %q: defined twice", name))
		}
		seen[c.ID] = true
	}
	return errors.Join(errs...)
}

// ValidateCard returns the problems of a single card; none means the bot
// will accept it.
func ValidateCard(c Card, s *survey.Survey) []error {
	var errs []error
	if c.ID == "" {
		errs = append(errs, errors.New("empty id"))
	}
	if strings.TrimSpace(c.Title) == "" || strings.TrimSpace(c.Summary) == "" {
		errs = append(errs, errors.New("title and summary are required"))
	}
	if len(c.Categories) == 0 {
		errs = append(errs, errors.New("no categories"))
	}
	for _, cid := range c.Categories {
		cat, ok := s.Category(cid)
		if !ok {
			errs = append(errs, fmt.Errorf("unknown category %q", cid))
			continue
		}
		errs = append(errs, validateCondition("match", c.Match, cat, s)...)
		for _, l := range c.Links {
			errs = append(errs, validateCondition(fmt.Sprintf("link %q", l.Title), l.When, cat, s)...)
		}
	}
	for _, l := range c.Links {
		if strings.TrimSpace(l.Title) == "" {
			errs = append(errs, fmt.Errorf("link %q has no title", l.URL))
		}
		if u, err := url.Parse(l.URL); err != nil || u.Scheme != "https" || u.Host == "" {
			errs = append(errs, fmt.Errorf("link %q must be an absolute https URL", l.URL))
		}
	}
	return errs
}

// A condition may only use questions that the category actually asks,
// otherwise it could never hold.
func validateCondition(where string, cond Condition, c *survey.Category, s *survey.Survey) []error {
	var errs []error
	for _, qid := range slices.Sorted(maps.Keys(cond)) {
		opts := cond[qid]
		if !slices.Contains(c.Questions, qid) {
			errs = append(errs, fmt.Errorf("%s: question %q is not asked in category %q", where, qid, c.ID))
			continue
		}
		q, _ := s.Question(qid)
		if len(opts) == 0 {
			errs = append(errs, fmt.Errorf("%s: question %q has no options listed", where, qid))
		}
		for _, oid := range opts {
			if _, ok := q.Option(oid); !ok {
				errs = append(errs, fmt.Errorf("%s: question %q has no option %q", where, qid, oid))
			}
		}
	}
	return errs
}

func (c Condition) holds(answers map[string][]string) bool {
	for qid, want := range c {
		if !slices.ContainsFunc(answers[qid], func(got string) bool { return slices.Contains(want, got) }) {
			return false
		}
	}
	return true
}

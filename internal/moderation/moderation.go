// Package moderation describes drafts of knowledge base cards and the rules
// for approving them. The agent proposes drafts, admins review them in the
// bot, and an approved card is shown to students at once.
package moderation

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/KKolyasik/max-benefits/internal/knowledge"
	"github.com/KKolyasik/max-benefits/internal/survey"
)

// Status of a draft.
type Status string

const (
	Pending  Status = "pending"
	Approved Status = "approved"
	Rejected Status = "rejected"
)

var (
	// ErrNotFound means there is no such draft.
	ErrNotFound = errors.New("draft not found")
	// ErrDecided means another admin has already approved or rejected the
	// draft.
	ErrDecided = errors.New("draft is already decided")
)

// Draft is a card proposed by the agent.
type Draft struct {
	ID   int64
	Card knowledge.Card
	// Updates is the ID of the existing card this draft rewrites, or "" for a
	// new card.
	Updates string
	// Query is the search query the agent found the card by.
	Query   string
	Sources []string
	// Notes are problems the agent could not fix.
	Notes   []string
	FoundAt time.Time
	Status  Status
}

// InvalidError lists why a draft can't be approved.
type InvalidError struct {
	Problems []string
}

func (e *InvalidError) Error() string {
	return "draft can't be approved: " + strings.Join(e.Problems, "; ")
}

// Problems returns why the draft can't be approved as it is; none means it
// can. current is the card with the same ID in the base, if there is one.
// They are checked against the base and the survey of the moment, since both
// may have changed after the agent wrote the draft.
func Problems(d Draft, s *survey.Survey, current *knowledge.Card) []string {
	var out []string
	for _, err := range knowledge.ValidateCard(d.Card, s) {
		out = append(out, err.Error())
	}
	switch {
	case d.Updates != "" && d.Card.ID != d.Updates:
		out = append(out, fmt.Sprintf("the draft updates %q but its id is %q", d.Updates, d.Card.ID))
	case d.Updates != "" && current == nil:
		out = append(out, fmt.Sprintf("card %q, which the draft updates, is no longer in the base", d.Updates))
	case d.Updates == "" && current != nil:
		out = append(out, fmt.Sprintf("card %q is already in the base", d.Card.ID))
	}
	return out
}

// Changes names the parts of the card a draft would change, in the order
// they are shown to students.
func Changes(old, new knowledge.Card) []string {
	fields := []struct {
		name     string
		old, new any
	}{
		{"разделы", old.Categories, new.Categories},
		{"кому показывать", old.Match, new.Match},
		{"приоритет", old.Priority, new.Priority},
		{"заголовок", old.Title, new.Title},
		{"описание", strings.Join(strings.Fields(old.Summary), " "), strings.Join(strings.Fields(new.Summary), " ")},
		{"шаги", old.Steps, new.Steps},
		{"документы", old.Documents, new.Documents},
		{"куда идти", old.Where, new.Where},
		{"ссылки", old.Links, new.Links},
	}
	var out []string
	for _, f := range fields {
		if !same(f.old, f.new) {
			out = append(out, f.name)
		}
	}
	return out
}

// same compares field values, treating nil and empty slices or maps alike.
func same(a, b any) bool {
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	if (va.Kind() == reflect.Slice || va.Kind() == reflect.Map) && va.Len() == 0 && vb.Len() == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

// Audience describes a card's conditions in words: who will see it.
func Audience(c knowledge.Card, s *survey.Survey) string {
	var parts []string
	for _, qid := range slices.Sorted(maps.Keys(c.Match)) {
		opts := c.Match[qid]
		q, ok := s.Question(qid)
		if !ok {
			parts = append(parts, qid+": "+strings.Join(opts, ", "))
			continue
		}
		parts = append(parts, q.Label+": "+strings.Join(q.Titles(opts), " или "))
	}
	if len(parts) == 0 {
		return "всем в разделе"
	}
	return strings.Join(parts, "; ")
}

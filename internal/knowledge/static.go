package knowledge

import (
	"context"
	"fmt"
	"os"
	"slices"

	"github.com/KKolyasik/max-benefits/internal/survey"
)

// Static is a Base backed by a human-readable YAML file. Entries are picked
// by hard tags: every condition in `match` must hold for the user's answers.
type Static struct {
	cards []Card
}

var _ Base = (*Static)(nil)

// LoadStatic reads entries from a YAML file and validates them against the
// survey, so a typo in a question or option ID fails at startup instead of
// silently hiding an entry.
func LoadStatic(path string, s *survey.Survey) (*Static, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read knowledge base: %w", err)
	}
	kb, err := ParseStatic(data, s)
	if err != nil {
		return nil, fmt.Errorf("knowledge base %s: %w", path, err)
	}
	return kb, nil
}

// ParseStatic builds a Static base from YAML and validates it.
func ParseStatic(data []byte, s *survey.Survey) (*Static, error) {
	cards, err := ParseCards(data)
	if err != nil {
		return nil, err
	}
	if err := Validate(cards, s); err != nil {
		return nil, err
	}
	return &Static{cards: cards}, nil
}

// Find implements Base.
func (k *Static) Find(_ context.Context, req Request) ([]Entry, error) {
	return Pick(k.cards, req), nil
}

// Cards returns the cards in file order.
func (k *Static) Cards() []Card {
	return slices.Clone(k.cards)
}

// IDs lists all entry IDs.
func (k *Static) IDs() []string {
	ids := make([]string, len(k.cards))
	for i, e := range k.cards {
		ids[i] = e.ID
	}
	return ids
}

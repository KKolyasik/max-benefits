// Package agent collects fresh information for the knowledge base: it
// searches the web by the queries from a config, reads the found pages and
// asks a language model to draft cards from them. Drafts wait for a human:
// only an approved card is written to the knowledge base.
package agent

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/KKolyasik/max-benefits/internal/survey"
)

// Query is one search from the queries file.
type Query struct {
	Text string `yaml:"text"`
	// Category is the bot category the found cards are meant for.
	Category string `yaml:"category"`
	// Results is how many pages (one per site) to read.
	Results int `yaml:"results"`
}

type queriesFile struct {
	Defaults struct {
		Results int `yaml:"results"`
	} `yaml:"defaults"`
	Queries []Query `yaml:"queries"`
}

// LoadQueries reads the queries file. It is read on every run, so queries
// can change without restarting anything.
func LoadQueries(path string, s *survey.Survey) ([]Query, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read queries: %w", err)
	}
	var f queriesFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("queries %s: %w", path, err)
	}
	var errs []error
	for i := range f.Queries {
		q := &f.Queries[i]
		q.Text = strings.TrimSpace(q.Text)
		if q.Text == "" {
			errs = append(errs, fmt.Errorf("query %d: empty text", i+1))
		}
		if _, ok := s.Category(q.Category); !ok {
			errs = append(errs, fmt.Errorf("query %q: unknown category %q", q.Text, q.Category))
		}
		if q.Results == 0 {
			q.Results = cmp.Or(f.Defaults.Results, 5)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("queries %s: %w", path, err)
	}
	return f.Queries, nil
}

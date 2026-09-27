package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/KKolyasik/max-benefits/internal/knowledge"
)

const draftHeader = `# Черновик карточки от агента, записанный без Kafka: бот его не увидит.
# Чтобы черновики доходили до админов, задайте KAFKA_BROKERS и
# SCHEMA_REGISTRY_URL.
#
# draft — откуда взялась карточка, в базу не попадает. card — сама карточка
# в формате data/knowledge.yaml.

`

// Sink takes the drafts of a run.
type Sink interface {
	// Write keeps a draft and says where it went; false means a draft of
	// the same card already waits for review, and this one is dropped.
	Write(ctx context.Context, d Draft) (where string, written bool, err error)
}

// Draft is a card waiting for review, with where it came from.
type Draft struct {
	// Path is the draft file; set when the draft is read.
	Path    string    `yaml:"-"`
	Query   string    `yaml:"query"`
	FoundAt time.Time `yaml:"found_at"`
	// Updates is the ID of the existing card this draft rewrites.
	Updates string   `yaml:"updates,omitempty"`
	Sources []string `yaml:"sources"`
	// Notes are problems the agent could not fix; the reviewer has to.
	Notes []string       `yaml:"notes,omitempty"`
	Card  knowledge.Card `yaml:"-"`
}

type draftFile struct {
	Draft Draft          `yaml:"draft"`
	Card  knowledge.Card `yaml:"card"`
}

// Drafts is a directory with one YAML file per draft.
type Drafts struct {
	Dir string
}

// List returns the drafts sorted by file name.
func (d Drafts) List() ([]Draft, error) {
	paths, err := filepath.Glob(filepath.Join(d.Dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	slices.Sort(paths)
	drafts := make([]Draft, 0, len(paths))
	for _, p := range paths {
		dr, err := d.Load(p)
		if err != nil {
			return nil, err
		}
		drafts = append(drafts, dr)
	}
	return drafts, nil
}

// Load reads a draft file.
func (d Drafts) Load(path string) (Draft, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Draft{}, fmt.Errorf("read draft: %w", err)
	}
	var f draftFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return Draft{}, fmt.Errorf("draft %s: %w", path, err)
	}
	f.Draft.Path, f.Draft.Card = path, f.Card
	return f.Draft, nil
}

// Write saves a new draft named after its card. It doesn't overwrite a
// draft that already waits for review, as it may have been edited, and
// reports whether it wrote the file.
func (d Drafts) Write(_ context.Context, dr Draft) (path string, written bool, err error) {
	path = filepath.Join(d.Dir, fileName(dr.Card)+".yaml")
	if _, err := os.Stat(path); err == nil {
		return path, false, nil
	}
	var meta bytes.Buffer
	enc := yaml.NewEncoder(&meta)
	enc.SetIndent(2)
	if err := enc.Encode(struct {
		Draft Draft `yaml:"draft"`
	}{dr}); err != nil {
		return "", false, err
	}
	var b strings.Builder
	b.WriteString(draftHeader)
	b.Write(meta.Bytes())
	b.WriteString("card:\n")
	for _, line := range strings.SplitAfter(knowledge.MarshalCard(dr.Card), "\n") {
		if line != "" {
			b.WriteString("  ")
			b.WriteString(line)
		}
	}
	if err := os.MkdirAll(d.Dir, 0o755); err != nil {
		return "", false, fmt.Errorf("write draft: %w", err)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", false, fmt.Errorf("write draft: %w", err)
	}
	return path, true, nil
}

// Delete removes the draft file.
func (d Drafts) Delete(dr Draft) error {
	if err := os.Remove(dr.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete draft: %w", err)
	}
	return nil
}

func fileName(c knowledge.Card) string {
	if validID.MatchString(c.ID) {
		return c.ID
	}
	sum := sha256.Sum256([]byte(c.Title))
	return "draft_" + hex.EncodeToString(sum[:4])
}

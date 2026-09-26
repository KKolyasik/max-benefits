package inbox

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/KKolyasik/max-benefits/internal/agent"
	"github.com/KKolyasik/max-benefits/internal/knowledge"
	"github.com/KKolyasik/max-benefits/internal/moderation"
)

type sink map[string]moderation.Draft

func (s sink) AddDraft(_ context.Context, id string, d moderation.Draft) (bool, error) {
	if _, ok := s[id]; ok {
		return false, nil
	}
	s[id] = d
	return true, nil
}

func TestImport(t *testing.T) {
	dir := t.TempDir()
	files := agent.Drafts{Dir: dir}
	draft := agent.Draft{
		Query: "стипендия", FoundAt: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC), Updates: "grant",
		Sources: []string{"https://a.example"}, Notes: []string{"проверь сумму"},
		Card: knowledge.Card{ID: "grant", Categories: []string{"money"}, Title: "Грант", Summary: "s"},
	}
	path, _, err := files.Write(draft)
	if err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(path)
	// A file the agent is still writing.
	partial := filepath.Join(dir, "partial.yaml")
	if err := os.WriteFile(partial, []byte("draft: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := sink{}
	log := slog.New(slog.DiscardHandler)

	added, err := Import(context.Background(), dir, s, log)
	if err != nil || added != 1 {
		t.Fatalf("added %d, %v", added, err)
	}
	for _, d := range s {
		if d.Card.ID != "grant" || d.Updates != "grant" || d.Query != "стипендия" || d.Notes[0] != "проверь сумму" {
			t.Errorf("draft %+v", d)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("an imported file must be removed")
	}
	if _, err := os.Stat(partial); err != nil {
		t.Error("an unreadable file must wait for the next time")
	}

	// The same draft delivered again is not new.
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if added, err := Import(context.Background(), dir, s, log); err != nil || added != 0 {
		t.Errorf("added %d, %v", added, err)
	}
}

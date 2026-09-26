package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadQueries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.yaml")
	write := func(s string) {
		if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("defaults: {results: 3}\nqueries:\n  - {text: \" проездной \", category: money}\n  - {text: музеи, category: fun, results: 7}\n")
	qs, err := LoadQueries(path, testSurveyParsed(t))
	if err != nil {
		t.Fatal(err)
	}
	if qs[0].Text != "проездной" || qs[0].Results != 3 || qs[1].Results != 7 {
		t.Errorf("queries %+v", qs)
	}

	write("queries:\n  - {text: \"\", category: nope}\n")
	if _, err := LoadQueries(path, testSurveyParsed(t)); err == nil || !strings.Contains(err.Error(), `unknown category "nope"`) {
		t.Errorf("got %v", err)
	}
}

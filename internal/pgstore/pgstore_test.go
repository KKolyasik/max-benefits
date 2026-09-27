package pgstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/KKolyasik/max-benefits/internal/knowledge"
	"github.com/KKolyasik/max-benefits/internal/moderation"
	"github.com/KKolyasik/max-benefits/internal/survey"
)

// The tests need a PostgreSQL server; each creates its own database there:
//
//	docker run -d --rm -e POSTGRES_PASSWORD=test -p 127.0.0.1:55432:5432 postgres:17-alpine
//	TEST_DATABASE_URL=postgres://postgres:test@localhost:55432/postgres go test ./internal/pgstore/

const testSurvey = `
categories:
  - {id: money, title: "Деньги", questions: [form, uni]}
  - {id: fun, title: "Досуг", questions: [age]}
questions:
  - id: form
    label: "Форма"
    text: "Как учишься?"
    options: [{id: full_time, title: "Очно"}, {id: part_time, title: "Заочно"}]
  - id: uni
    label: "Вуз"
    text: "Где?"
    options: [{id: a, title: "А"}, {id: b, title: "Б"}]
  - id: age
    label: "Возраст"
    text: "Сколько лет?"
    options: [{id: young, title: "До 23"}, {id: old, title: "23+"}]
`

var seedCards = []knowledge.Card{
	{ID: "low", Categories: []string{"money"}, Priority: 1, Title: "Низкий", Summary: "s"},
	{ID: "pass", Categories: []string{"money"}, Priority: 100, Match: knowledge.Condition{"form": {"full_time"}},
		Title: "Проездной", Summary: "s", Links: []knowledge.CardLink{
			{Title: "Для А", URL: "https://a.example", When: knowledge.Condition{"uni": {"a"}}},
			{Title: "Всем", URL: "https://all.example"},
		}},
	{ID: "also_low", Categories: []string{"money"}, Priority: 1, Title: "Тоже низкий", Summary: "s"},
	{ID: "museum", Categories: []string{"fun"}, Priority: 50, Title: "Музей", Summary: "s"},
}

func testStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run the database tests")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("pgstore_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = name
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		_, _ = admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)")
		_ = admin.Close(ctx)
	})
	// Twice: an already migrated database is left as it is.
	for range 2 {
		if err := Migrate(ctx, db); err != nil {
			t.Fatal(err)
		}
	}
	sv, err := survey.Parse([]byte(testSurvey))
	if err != nil {
		t.Fatal(err)
	}
	return New(db, sv)
}

func ids(entries []knowledge.Entry) string {
	var out []string
	for _, e := range entries {
		out = append(out, e.ID)
	}
	return strings.Join(out, ",")
}

func find(t *testing.T, s *Store, category string, answers map[string][]string) []knowledge.Entry {
	t.Helper()
	entries, err := s.Find(context.Background(), knowledge.Request{Category: category, Answers: answers})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestSeedAndFind(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	n, err := s.Seed(ctx, seedCards, "seed")
	if err != nil || n != len(seedCards) {
		t.Fatalf("seed: %d, %v", n, err)
	}
	if n, err := s.Seed(ctx, seedCards[:1], "again"); err != nil || n != 0 {
		t.Fatalf("a filled base must not be seeded again: %d, %v", n, err)
	}

	got := find(t, s, "money", map[string][]string{"form": {"full_time"}, "uni": {"b"}})
	// Priority first, then the order the cards were added in, like the YAML base.
	if ids(got) != "pass,low,also_low" {
		t.Errorf("got %s", ids(got))
	}
	if len(got[0].Links) != 1 || got[0].Links[0].Title != "Всем" {
		t.Errorf("links for university B: %+v", got[0].Links)
	}
	if got := find(t, s, "money", nil); ids(got) != "low,also_low" {
		t.Errorf("conditions must hold: %s", ids(got))
	}
	if got := find(t, s, "fun", nil); ids(got) != "museum" {
		t.Errorf("fun: %s", ids(got))
	}

	var history int
	if err := s.db.QueryRow(ctx, "SELECT count(*) FROM card_history WHERE note = 'seed'").Scan(&history); err != nil || history != 4 {
		t.Errorf("history rows %d, %v", history, err)
	}
}

func TestCardsBrokenBySurveyAreHidden(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	broken := knowledge.Card{ID: "broken", Categories: []string{"money"}, Match: knowledge.Condition{"form": {"removed"}},
		Title: "Сломанная", Summary: "s"}
	if _, err := s.Seed(ctx, []knowledge.Card{seedCards[0], broken}, "seed"); err != nil {
		t.Fatal(err)
	}
	if got := find(t, s, "money", map[string][]string{"form": {"removed"}}); ids(got) != "low" {
		t.Errorf("got %s", ids(got))
	}
	b, err := s.Broken(ctx)
	if err != nil || len(b) != 1 || len(b["broken"]) == 0 {
		t.Errorf("broken %v, %v", b, err)
	}
}

func draft(card knowledge.Card, updates string) moderation.Draft {
	return moderation.Draft{Card: card, Updates: updates, Query: "стипендия", Sources: []string{"https://a.example"},
		FoundAt: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)}
}

func TestDraftApprovedIsShownAtOnce(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Seed(ctx, seedCards, "seed"); err != nil {
		t.Fatal(err)
	}
	card := knowledge.Card{ID: "grant", Categories: []string{"money"}, Priority: 60, Title: "Грант", Summary: "Деньги."}

	if added, err := s.AddDraft(ctx, "file-1", draft(card, "")); err != nil || !added {
		t.Fatalf("added %v, %v", added, err)
	}
	if added, _ := s.AddDraft(ctx, "file-1", draft(card, "")); added {
		t.Error("the same draft delivered twice must be stored once")
	}
	if n, _ := s.PendingDrafts(ctx); n != 1 {
		t.Errorf("pending %d", n)
	}
	d, ok, err := s.NextDraft(ctx, 0)
	if err != nil || !ok || d.Card.Title != "Грант" || d.Query != "стипендия" || d.Sources[0] != "https://a.example" {
		t.Fatalf("draft %+v %v %v", d, ok, err)
	}

	got, replaced, err := s.ApproveDraft(ctx, d.ID, 486)
	if err != nil || replaced || got.ID != "grant" {
		t.Fatalf("approve: %+v %v %v", got, replaced, err)
	}
	if got := find(t, s, "money", nil); ids(got) != "grant,low,also_low" {
		t.Errorf("an approved card must be shown at once: %s", ids(got))
	}
	if n, _ := s.PendingDrafts(ctx); n != 0 {
		t.Errorf("pending %d", n)
	}
	var by int64
	if err := s.db.QueryRow(ctx, "SELECT changed_by FROM card_history WHERE draft_id = $1", d.ID).Scan(&by); err != nil || by != 486 {
		t.Errorf("history: %d, %v", by, err)
	}
	if _, _, err := s.ApproveDraft(ctx, d.ID, 486); !errors.Is(err, moderation.ErrDecided) {
		t.Errorf("second approval: %v", err)
	}
	if err := s.RejectDraft(ctx, d.ID, 486); !errors.Is(err, moderation.ErrDecided) {
		t.Errorf("reject after approval: %v", err)
	}
	if _, _, err := s.ApproveDraft(ctx, 999, 486); !errors.Is(err, moderation.ErrNotFound) {
		t.Errorf("missing draft: %v", err)
	}
}

func TestApprovedUpdateReplacesInPlace(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Seed(ctx, seedCards, "seed"); err != nil {
		t.Fatal(err)
	}
	updated := seedCards[0]
	updated.Title = "Низкий, обновлённый"
	if _, err := s.AddDraft(ctx, "u", draft(updated, "low")); err != nil {
		t.Fatal(err)
	}
	d, _, _ := s.NextDraft(ctx, 0)
	if _, replaced, err := s.ApproveDraft(ctx, d.ID, 1); err != nil || !replaced {
		t.Fatalf("replaced %v, %v", replaced, err)
	}
	got := find(t, s, "money", nil)
	// The card keeps its place among the cards of the same priority.
	if ids(got) != "low,also_low" || got[0].Title != "Низкий, обновлённый" {
		t.Errorf("got %s %q", ids(got), got[0].Title)
	}
}

func TestInvalidDraftStaysPending(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Seed(ctx, seedCards, "seed"); err != nil {
		t.Fatal(err)
	}
	taken := knowledge.Card{ID: "low", Categories: []string{"money"}, Title: "Т", Summary: "s"}
	badOption := knowledge.Card{ID: "bad", Categories: []string{"money"}, Title: "Т", Summary: "s",
		Match: knowledge.Condition{"form": {"nope"}}}
	for i, c := range []knowledge.Card{taken, badOption} {
		if _, err := s.AddDraft(ctx, fmt.Sprint(i), draft(c, "")); err != nil {
			t.Fatal(err)
		}
	}
	for after := int64(0); ; {
		d, ok, err := s.NextDraft(ctx, after)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		var invalid *moderation.InvalidError
		if _, _, err := s.ApproveDraft(ctx, d.ID, 1); !errors.As(err, &invalid) {
			t.Errorf("draft %s: %v", d.Card.ID, err)
		}
		after = d.ID
	}
	if n, _ := s.PendingDrafts(ctx); n != 2 {
		t.Errorf("invalid drafts must stay pending: %d", n)
	}
}

func TestSkipAndReject(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for i := range 3 {
		c := knowledge.Card{ID: fmt.Sprintf("c%d", i), Categories: []string{"money"}, Title: "Т", Summary: "s"}
		if _, err := s.AddDraft(ctx, c.ID, draft(c, "")); err != nil {
			t.Fatal(err)
		}
	}
	first, _, _ := s.NextDraft(ctx, 0)
	second, _, _ := s.NextDraft(ctx, first.ID)
	if second.Card.ID != "c1" {
		t.Fatalf("next after the first: %s", second.Card.ID)
	}
	if err := s.RejectDraft(ctx, first.ID, 1); err != nil {
		t.Fatal(err)
	}
	if again, _, _ := s.NextDraft(ctx, 0); again.Card.ID != "c1" {
		t.Errorf("a rejected draft must not come back: %s", again.Card.ID)
	}
	if n, _ := s.PendingDrafts(ctx); n != 2 {
		t.Errorf("pending %d", n)
	}
}

// Two admins press "approve" on the same draft at once: exactly one wins.
func TestConcurrentApproval(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	c := knowledge.Card{ID: "once", Categories: []string{"money"}, Title: "Т", Summary: "s"}
	if _, err := s.AddDraft(ctx, "once", draft(c, "")); err != nil {
		t.Fatal(err)
	}
	d, _, _ := s.NextDraft(ctx, 0)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Go(func() { _, _, errs[i] = s.ApproveDraft(ctx, d.ID, int64(i)) })
	}
	wg.Wait()
	ok, decided := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, moderation.ErrDecided):
			decided++
		default:
			t.Errorf("unexpected: %v", err)
		}
	}
	if ok != 1 || decided != 1 {
		t.Errorf("ok %d, decided %d", ok, decided)
	}
}

// A newer draft of a card supersedes the one still waiting for review; the
// same draft delivered again changes nothing.
func TestNewerDraftSupersedesPending(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Seed(ctx, seedCards, "seed"); err != nil {
		t.Fatal(err)
	}
	older, newer := seedCards[1], seedCards[1]
	older.Summary, newer.Summary = "старая", "новая"
	gym := knowledge.Card{ID: "gym", Categories: []string{"fun"}, Priority: 5, Title: "Зал", Summary: "зал"}
	for _, add := range []struct {
		id string
		d  moderation.Draft
	}{{"old", draft(older, "pass")}, {"gym", draft(gym, "")}, {"new", draft(newer, "pass")}} {
		if _, err := s.AddDraft(ctx, add.id, add.d); err != nil {
			t.Fatal(err)
		}
	}
	if added, err := s.AddDraft(ctx, "new", draft(newer, "pass")); err != nil || added {
		t.Fatalf("delivered again: added %v, %v", added, err)
	}

	var pending []string
	for d, ok, err := s.NextDraft(ctx, 0); ok || err != nil; d, ok, err = s.NextDraft(ctx, d.ID) {
		if err != nil {
			t.Fatal(err)
		}
		pending = append(pending, d.Card.Summary)
	}
	if strings.Join(pending, ",") != "зал,новая" {
		t.Errorf("pending %v", pending)
	}
}

// Decisions leave events for the agent in the outbox, in the same
// transaction, and the relay can read what was decided.
func TestDecisionsGoToTheOutbox(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Seed(ctx, seedCards, "seed"); err != nil {
		t.Fatal(err)
	}
	var drafts []int64
	for _, c := range []knowledge.Card{
		{ID: "gym", Categories: []string{"fun"}, Priority: 5, Title: "Зал", Summary: "s"},
		{ID: "cinema", Categories: []string{"fun"}, Priority: 5, Title: "Кино", Summary: "s"},
		{ID: "zoo", Categories: []string{"fun"}, Priority: 5, Title: "Зоопарк", Summary: "s"},
	} {
		if _, err := s.AddDraft(ctx, "agent-"+c.ID, draft(c, "")); err != nil {
			t.Fatal(err)
		}
		d, _, _ := s.NextDraft(ctx, 0)
		for ok := true; ok && d.Card.ID != c.ID; {
			d, ok, _ = s.NextDraft(ctx, d.ID)
		}
		drafts = append(drafts, d.ID)
	}
	if _, _, err := s.ApproveDraft(ctx, drafts[0], 7); err != nil {
		t.Fatal(err)
	}
	if err := s.RejectDraft(ctx, drafts[1], 7); err != nil {
		t.Fatal(err)
	}

	events, err := s.Events(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	gym, cinema := strconv.FormatInt(drafts[0], 10), strconv.FormatInt(drafts[1], 10)
	want := []Event{{Kind: EventCard, Ref: "gym"}, {Kind: EventDecision, Ref: gym}, {Kind: EventDecision, Ref: cinema}}
	if len(events) != len(want) {
		t.Fatalf("events %+v", events)
	}
	for i := range want {
		if events[i].Kind != want[i].Kind || events[i].Ref != want[i].Ref {
			t.Errorf("event %d: %+v, want %+v", i, events[i], want[i])
		}
	}

	d, ok, err := s.DraftDecision(ctx, gym)
	if err != nil || !ok || d.ExternalID != "agent-gym" || d.CardID != "gym" || d.Status != moderation.Approved || d.DecidedAt.IsZero() {
		t.Errorf("decision on gym: %+v %v %v", d, ok, err)
	}
	if d, ok, _ := s.DraftDecision(ctx, cinema); !ok || d.Status != moderation.Rejected {
		t.Errorf("decision on cinema: %+v %v", d, ok)
	}
	if _, ok, err := s.DraftDecision(ctx, strconv.FormatInt(drafts[2], 10)); ok || err != nil {
		t.Errorf("a pending draft has no decision: %v %v", ok, err)
	}

	if err := s.DeleteEvents(ctx, []int64{events[0].ID, events[1].ID}); err != nil {
		t.Fatal(err)
	}
	if left, _ := s.Events(ctx, 10); len(left) != 1 || left[0].Ref != cinema {
		t.Errorf("left %+v", left)
	}
}

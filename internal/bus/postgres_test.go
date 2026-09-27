package bus

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/KKolyasik/max-benefits/contract"
	"github.com/KKolyasik/max-benefits/internal/knowledge"
	"github.com/KKolyasik/max-benefits/internal/pgstore"
	"github.com/KKolyasik/max-benefits/internal/survey"
)

// TestWithPostgres runs the bus over the real database: a draft from Kafka
// waits for review, and the approval reaches Kafka as the new card and the
// decision. Needs TEST_DATABASE_URL, see the pgstore tests.
func TestWithPostgres(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run the database tests")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("bus_test_%d", time.Now().UnixNano())
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
	if err := pgstore.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	sv, err := survey.Load("../../data/survey.yaml")
	if err != nil {
		t.Fatal(err)
	}
	store := pgstore.New(db, sv)
	base, err := knowledge.LoadStatic("../../data/knowledge.yaml", sv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Seed(ctx, base.Cards(), "seed"); err != nil {
		t.Fatal(err)
	}

	b := start(t, store)
	cards := base.Cards()
	newer := cards[0]
	newer.Summary = "Проездной подорожал."
	b.produce(t, contract.TopicDrafts, "d1",
		&contract.Draft{ID: "d1", Card: newer.Contract(), Updates: newer.ID, Query: "проездной", FoundAt: at})

	var draftID int64
	waitFor(t, "the draft waiting for review", func() bool {
		d, ok, err := store.NextDraft(ctx, 0)
		draftID = d.ID
		return ok && err == nil
	})
	if _, _, err := store.ApproveDraft(ctx, draftID, 7); err != nil {
		t.Fatal(err)
	}

	published := b.read(t, contract.TopicCards, len(cards)+1)
	var c contract.Card
	if err := b.codec.Decode(ctx, published[len(cards)].Value, &c); err != nil || c.Summary != newer.Summary {
		t.Errorf("the approved card %+v, %v", c, err)
	}
	var d contract.Decision
	if err := b.codec.Decode(ctx, b.read(t, contract.TopicDecisions, 1)[0].Value, &d); err != nil ||
		d.DraftID != "d1" || d.CardID != newer.ID || d.Verdict != contract.VerdictApproved {
		t.Errorf("the decision %+v, %v", d, err)
	}
	waitFor(t, "an empty outbox", func() bool {
		events, err := store.Events(ctx, 10)
		return err == nil && len(events) == 0
	})
}

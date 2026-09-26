package session

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func testStore(t *testing.T, store Store) {
	t.Helper()
	ctx := context.Background()

	s, err := store.Load(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if s.UserID != 7 || s.State != StateMenu || s.Answers == nil {
		t.Fatalf("unexpected fresh session: %+v", s)
	}

	s.State = StateSurvey
	s.Category = "benefits"
	s.Answers["university"] = []string{"spbu"}
	s.Selected = []string{"orphan"}
	if err := store.Save(ctx, s); err != nil {
		t.Fatal(err)
	}

	// Mutating the loaded copy must not affect the stored one.
	s.Answers["university"] = []string{"hse"}

	got, err := store.Load(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateSurvey || got.Category != "benefits" ||
		!slices.Equal(got.Answers["university"], []string{"spbu"}) || !slices.Equal(got.Selected, []string{"orphan"}) {
		t.Fatalf("session not restored: %+v", got)
	}

	if err := store.Delete(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.Load(ctx, 7); len(got.Answers) != 0 {
		t.Fatalf("session not deleted: %+v", got)
	}
}

func TestMemory(t *testing.T) {
	testStore(t, NewMemory())
}

func TestRedis(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	store := NewRedis(client, time.Hour)
	testStore(t, store)

	if err := store.Save(context.Background(), New(8)); err != nil {
		t.Fatal(err)
	}
	if ttl := mr.TTL("session:8"); ttl != time.Hour {
		t.Fatalf("expected the TTL to be set, got %v", ttl)
	}
}

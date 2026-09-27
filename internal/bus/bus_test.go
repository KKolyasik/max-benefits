package bus

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/KKolyasik/max-benefits/contract"
	"github.com/KKolyasik/max-benefits/contract/contracttest"
	"github.com/KKolyasik/max-benefits/internal/knowledge"
	"github.com/KKolyasik/max-benefits/internal/moderation"
	"github.com/KKolyasik/max-benefits/internal/pgstore"
	"github.com/KKolyasik/max-benefits/internal/survey"
)

func TestMain(m *testing.M) {
	relayEvery = 20 * time.Millisecond
	firstPause = 10 * time.Millisecond
	os.Exit(m.Run())
}

var at = time.Date(2026, 9, 27, 3, 0, 0, 0, time.UTC)

var (
	pass   = knowledge.Card{ID: "pass", Categories: []string{"benefits"}, Priority: 100, Title: "🚇 Проездной", Summary: "s"}
	museum = knowledge.Card{ID: "museums_free", Categories: []string{"leisure"}, Priority: 50, Title: "🖼 Музеи", Summary: "s"}
)

// fakeStore keeps the bot's database in memory.
type fakeStore struct {
	mu        sync.Mutex
	cards     []knowledge.Card
	events    []pgstore.Event
	lastEvent int64
	decisions map[string]pgstore.Decision
	drafts    map[string]moderation.Draft
	// failAdds is how many times AddDraft fails before it works.
	failAdds int
}

func newStore(cards ...knowledge.Card) *fakeStore {
	return &fakeStore{cards: cards, decisions: map[string]pgstore.Decision{}, drafts: map[string]moderation.Draft{}}
}

func (s *fakeStore) Cards(context.Context) ([]knowledge.Card, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.cards), nil
}

func (s *fakeStore) Card(_ context.Context, id string) (knowledge.Card, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.cards, func(c knowledge.Card) bool { return c.ID == id })
	if i < 0 {
		return knowledge.Card{}, false, nil
	}
	return s.cards[i], true, nil
}

func (s *fakeStore) Events(_ context.Context, limit int) ([]pgstore.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.events[:min(limit, len(s.events))]), nil
}

func (s *fakeStore) DeleteEvents(_ context.Context, ids []int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = slices.DeleteFunc(s.events, func(ev pgstore.Event) bool { return slices.Contains(ids, ev.ID) })
	return nil
}

func (s *fakeStore) DraftDecision(_ context.Context, ref string) (pgstore.Decision, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.decisions[ref]
	return d, ok, nil
}

func (s *fakeStore) AddDraft(_ context.Context, id string, d moderation.Draft) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failAdds > 0 {
		s.failAdds--
		return false, errors.New("the database is down")
	}
	if _, ok := s.drafts[id]; ok {
		return false, nil
	}
	s.drafts[id] = d
	return true, nil
}

// event records an event, as a change in the database does.
func (s *fakeStore) event(kind, ref string) {
	s.lastEvent++
	s.events = append(s.events, pgstore.Event{ID: s.lastEvent, Kind: kind, Ref: ref})
}

func (s *fakeStore) setCard(c knowledge.Card) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := slices.IndexFunc(s.cards, func(old knowledge.Card) bool { return old.ID == c.ID }); i >= 0 {
		s.cards[i] = c
	} else {
		s.cards = append(s.cards, c)
	}
	s.event(pgstore.EventCard, c.ID)
}

func (s *fakeStore) removeCard(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cards = slices.DeleteFunc(s.cards, func(c knowledge.Card) bool { return c.ID == id })
	s.event(pgstore.EventCard, id)
}

func (s *fakeStore) decide(ref string, d *pgstore.Decision) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d != nil {
		s.decisions[ref] = *d
	}
	s.event(pgstore.EventDecision, ref)
}

func (s *fakeStore) outboxEmpty() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events) == 0
}

func (s *fakeStore) draft(id string) (moderation.Draft, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.drafts[id]
	return d, ok
}

type bench struct {
	brokers  []string
	codec    *contract.Codec
	notified chan int
	survey   *survey.Survey
}

// start runs the bus over a Kafka in memory and a fake registry until the
// test ends.
func start(t *testing.T, store Store) *bench {
	t.Helper()
	cluster, err := kfake.NewCluster(kfake.NumBrokers(1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cluster.Close)
	sv, err := survey.Load("../../data/survey.yaml")
	if err != nil {
		t.Fatal(err)
	}
	reg := contracttest.NewRegistry(t)
	b := &bench{brokers: cluster.ListenAddrs(), notified: make(chan int, 100), survey: sv, codec: contract.NewCodec(reg.Client())}
	ctx, cancel := context.WithCancel(context.Background())
	notify := func(_ context.Context, n int) error { b.notified <- n; return nil }
	wait, err := Start(ctx, Config{Brokers: b.brokers, RegistryURL: reg.URL(), Group: "bot"}, store, sv, notify,
		slog.New(slog.DiscardHandler))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		wait()
	})
	if err := b.codec.Register(ctx); err != nil {
		t.Fatal(err)
	}
	return b
}

// read waits until the topic has n records and returns them.
func (b *bench) read(t *testing.T, topic string, n int) []*kgo.Record {
	t.Helper()
	cl, err := kgo.NewClient(kgo.SeedBrokers(b.brokers...), kgo.ConsumeTopics(topic), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out []*kgo.Record
	for len(out) < n {
		fetches := cl.PollFetches(ctx)
		if ctx.Err() != nil {
			t.Fatalf("%s has %d records, want %d", topic, len(out), n)
		}
		out = append(out, fetches.Records()...)
	}
	return out
}

func (b *bench) produce(t *testing.T, key string, value []byte) {
	t.Helper()
	cl, err := kgo.NewClient(kgo.SeedBrokers(b.brokers...))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	if err := cl.ProduceSync(context.Background(), &kgo.Record{Topic: contract.TopicDrafts, Key: []byte(key), Value: value}).FirstErr(); err != nil {
		t.Fatal(err)
	}
}

func (b *bench) draft(t *testing.T, d contract.Draft) []byte {
	t.Helper()
	data, err := b.codec.Encode(&d)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// waitFor waits until cond holds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// On start the agent gets the whole base: the survey and every card.
func TestStartPublishesTheBase(t *testing.T) {
	b := start(t, newStore(pass, museum))

	rec := b.read(t, contract.TopicSurvey, 1)[0]
	var s contract.Survey
	if err := b.codec.Decode(context.Background(), rec.Value, &s); err != nil {
		t.Fatal(err)
	}
	if string(rec.Key) != contract.SurveyKey || len(s.Categories) != len(b.survey.Categories) {
		t.Errorf("survey %q with %d categories", rec.Key, len(s.Categories))
	}

	for i, rec := range b.read(t, contract.TopicCards, 2) {
		var c contract.Card
		if err := b.codec.Decode(context.Background(), rec.Value, &c); err != nil {
			t.Fatal(err)
		}
		want := []knowledge.Card{pass, museum}[i]
		if string(rec.Key) != want.ID || knowledge.CardFromContract(c).Title != want.Title {
			t.Errorf("card %d: key %q, %+v", i, rec.Key, c)
		}
	}
}

// Changes in the outbox reach Kafka as the current state, and the outbox is
// cleaned once Kafka has them.
func TestOutboxGoesToKafka(t *testing.T) {
	store := newStore(pass, museum)
	b := start(t, store)
	newer := pass
	newer.Summary = "новая"
	store.setCard(newer)
	store.removeCard(museum.ID)
	store.decide("7", &pgstore.Decision{ExternalID: "agent-7", CardID: "pass", Status: moderation.Approved, DecidedAt: at})
	store.decide("8", &pgstore.Decision{ExternalID: "agent-8", CardID: "gym", Status: moderation.Rejected, DecidedAt: at})
	store.decide("9", nil) // superseded meanwhile: nothing to tell

	cards := b.read(t, contract.TopicCards, 4) // two at start, the change and the removal
	var c contract.Card
	if err := b.codec.Decode(context.Background(), cards[2].Value, &c); err != nil || c.Summary != "новая" {
		t.Errorf("changed card %+v, %v", c, err)
	}
	if string(cards[3].Key) != museum.ID || cards[3].Value != nil {
		t.Errorf("removal must be a tombstone: %q %v", cards[3].Key, cards[3].Value)
	}

	decisions := b.read(t, contract.TopicDecisions, 2)
	want := []contract.Decision{
		{DraftID: "agent-7", CardID: "pass", Verdict: contract.VerdictApproved, DecidedAt: at},
		{DraftID: "agent-8", CardID: "gym", Verdict: contract.VerdictRejected, DecidedAt: at},
	}
	for i, rec := range decisions {
		var d contract.Decision
		if err := b.codec.Decode(context.Background(), rec.Value, &d); err != nil {
			t.Fatal(err)
		}
		if d != want[i] || string(rec.Key) != want[i].DraftID {
			t.Errorf("decision %d: %q %+v", i, rec.Key, d)
		}
	}
	waitFor(t, "an empty outbox", store.outboxEmpty)
}

// Drafts go into the database; one delivered twice counts once, and a
// record that is not a draft doesn't stop the rest.
func TestDraftsGoToTheStore(t *testing.T) {
	store := newStore(pass)
	b := start(t, store)
	d1 := contract.Draft{ID: "d1", Card: pass.Contract(), Updates: "pass", Query: "проездной",
		Sources: []string{"https://metro.spb.ru"}, FoundAt: at}
	d2 := contract.Draft{ID: "d2", Card: museum.Contract(), Query: "музеи", FoundAt: at}
	b.produce(t, "d1", b.draft(t, d1))
	b.produce(t, "junk", []byte("not a draft"))
	b.produce(t, "d1", b.draft(t, d1))
	b.produce(t, "d2", b.draft(t, d2))

	waitFor(t, "both drafts", func() bool {
		_, ok := store.draft("d2")
		return ok
	})
	got, _ := store.draft("d1")
	if got.Card.Title != pass.Title || got.Updates != "pass" || got.Sources[0] != "https://metro.spb.ru" || !got.FoundAt.Equal(at) {
		t.Errorf("draft %+v", got)
	}
	total := 0
	waitFor(t, "the admins told about 2 drafts", func() bool {
		select {
		case n := <-b.notified:
			total += n
		default:
		}
		return total == 2
	})
}

// A database that is down for a while doesn't lose a draft.
func TestStoreFailureIsRetried(t *testing.T) {
	store := newStore(pass)
	store.failAdds = 2
	b := start(t, store)
	b.produce(t, "d1", b.draft(t, contract.Draft{ID: "d1", Card: pass.Contract(), Query: "проездной", FoundAt: at}))
	waitFor(t, "the draft", func() bool {
		_, ok := store.draft("d1")
		return ok
	})
	if n := <-b.notified; n != 1 {
		t.Errorf("notified about %d drafts", n)
	}
}

// Without a registry the bot can't talk to the agent, and says so at start.
func TestStartFailsWithoutRegistry(t *testing.T) {
	cluster, err := kfake.NewCluster(kfake.NumBrokers(1))
	if err != nil {
		t.Fatal(err)
	}
	defer cluster.Close()
	sv, _ := survey.Load("../../data/survey.yaml")
	_, err = Start(context.Background(), Config{Brokers: cluster.ListenAddrs(), RegistryURL: "http://127.0.0.1:1", Group: "bot"},
		newStore(), sv, func(context.Context, int) error { return nil }, slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "schema registry") {
		t.Errorf("start without a registry: %v", err)
	}
}

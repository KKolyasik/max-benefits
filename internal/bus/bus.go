// Package bus is the bot's end of the contract with the agent (see package
// contract). It takes the agent's drafts from Kafka into the database, tells
// the admins how the agent's runs go and passes their commands to run it. It
// tells the agent the survey, the cards and the admins' decisions: changes
// go through the outbox table, which is written in the same transaction as
// the change, so none is lost while Kafka is down.
package bus

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sr"

	"github.com/KKolyasik/max-benefits/contract"
	"github.com/KKolyasik/max-benefits/internal/knowledge"
	"github.com/KKolyasik/max-benefits/internal/moderation"
	"github.com/KKolyasik/max-benefits/internal/pgstore"
	"github.com/KKolyasik/max-benefits/internal/survey"
)

// Store is what the bus needs from the bot's database.
type Store interface {
	Cards(ctx context.Context) ([]knowledge.Card, error)
	Card(ctx context.Context, id string) (knowledge.Card, bool, error)
	Events(ctx context.Context, limit int) ([]pgstore.Event, error)
	DeleteEvents(ctx context.Context, ids []int64) error
	DraftDecision(ctx context.Context, ref string) (pgstore.Decision, bool, error)
	AddDraft(ctx context.Context, externalID string, d moderation.Draft) (bool, error)
}

// Admins are told how the agent's runs go.
type Admins interface {
	NotifyRun(ctx context.Context, r moderation.Run) error
}

// Config is how to reach Kafka and the Schema Registry.
type Config struct {
	Brokers     []string
	RegistryURL string
	// Group is the consumer group that takes the drafts and the reports.
	// Every group gets every draft, so the console simulator uses a group of
	// its own.
	Group string
}

// Bus is the bot's connection to the agent.
type Bus struct {
	kafka *kgo.Client
	codec *contract.Codec
	wg    sync.WaitGroup
}

// Timings, shortened in tests.
var (
	// relayEvery is how often the outbox is checked.
	relayEvery = 2 * time.Second
	// firstPause is the first wait before retrying; each next one is twice
	// as long, up to half a minute.
	firstPause = time.Second
)

// Start creates the topics, registers the schemas and publishes the survey
// and every card. Then until ctx is done it publishes the outbox, takes the
// drafts into the store and tells the admins about the runs of the agent.
// An error means the bot can't talk to the agent: Kafka or the registry is
// down, or a schema is incompatible.
func Start(ctx context.Context, cfg Config, store Store, sv *survey.Survey, admins Admins, log *slog.Logger) (_ *Bus, err error) {
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(cfg.Brokers...),
		kgo.ConsumerGroup(cfg.Group),
		kgo.ConsumeTopics(contract.TopicDrafts, contract.TopicRuns),
		// A new group takes the drafts sent before the bot first started.
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		// Offsets are committed once the drafts are in the database.
		kgo.DisableAutoCommit(),
		// A dead Kafka fails a publish instead of hanging it; the outbox
		// keeps the change for the next try.
		kgo.RecordDeliveryTimeout(30*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("kafka: %w", err)
	}
	defer func() {
		if err != nil {
			cl.Close()
		}
	}()
	reg, err := sr.NewClient(sr.URLs(cfg.RegistryURL))
	if err != nil {
		return nil, fmt.Errorf("schema registry: %w", err)
	}
	codec := contract.NewCodec(reg)

	setup, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := contract.EnsureTopics(setup, kadm.NewClient(cl), contract.Topics...); err != nil {
		return nil, fmt.Errorf("kafka: %w", err)
	}
	if err := codec.Register(setup); err != nil {
		return nil, fmt.Errorf("schema registry: %w", err)
	}
	r := &relay{store: store, kafka: cl, codec: codec, log: log}
	if err := r.syncAll(setup, sv); err != nil {
		return nil, err
	}

	in := &inbox{store: store, kafka: cl, codec: codec, admins: admins, log: log}
	b := &Bus{kafka: cl, codec: codec}
	b.wg.Go(func() { r.run(ctx) })
	b.wg.Go(func() { in.run(ctx) })
	return b, nil
}

// RunAgent asks the agent to run now; force makes it read the pages that
// did not change too.
func (b *Bus) RunAgent(ctx context.Context, force bool) error {
	id := make([]byte, 8)
	_, _ = rand.Read(id)
	cmd := contract.RunCommand{ID: hex.EncodeToString(id), RequestedAt: time.Now(), Force: force}
	value, err := b.codec.Encode(&cmd)
	if err != nil {
		return err
	}
	if err := b.kafka.ProduceSync(ctx, &kgo.Record{Topic: contract.TopicCommands, Key: []byte(cmd.ID), Value: value}).FirstErr(); err != nil {
		return fmt.Errorf("send the command to the agent: %w", err)
	}
	return nil
}

// Wait blocks until the loops stop, then closes the connection.
func (b *Bus) Wait() {
	b.wg.Wait()
	b.kafka.Close()
}

// retry waits before the next attempt of what failed with err; false means
// ctx is done.
func retry(ctx context.Context, log *slog.Logger, pause *time.Duration, what string, err error) bool {
	log.Error(what+", will retry", "in", *pause, "err", err)
	select {
	case <-ctx.Done():
		return false
	case <-time.After(*pause):
	}
	*pause = min(*pause*2, 30*time.Second)
	return true
}

package bus

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/KKolyasik/max-benefits/contract"
	"github.com/KKolyasik/max-benefits/contract/contracttest"
	"github.com/KKolyasik/max-benefits/internal/agent"
	"github.com/KKolyasik/max-benefits/internal/knowledge"
	"github.com/KKolyasik/max-benefits/internal/survey"
)

func TestMain(m *testing.M) {
	firstPause = 10 * time.Millisecond
	os.Exit(m.Run())
}

var at = time.Date(2026, 9, 27, 3, 0, 0, 0, time.UTC)

var (
	transport = knowledge.Card{ID: "transport", Categories: []string{"benefits"}, Priority: 100,
		Title: "🚇 Проездной", Summary: "Льготный проездной.",
		Links: []knowledge.CardLink{{Title: "Метро", URL: "https://metro.spb.ru/ticket.html"}}}
	museum = knowledge.Card{ID: "museums_free", Categories: []string{"leisure"}, Priority: 50, Title: "🖼 Музеи", Summary: "s"}
)

// bench is a Kafka in memory with the topics of the contract and a fake
// registry with its schemas.
type bench struct {
	cluster *kfake.Cluster
	brokers []string
	// kafka sends with a short delivery timeout, as the agent's client.
	kafka  *kgo.Client
	codec  *contract.Codec
	survey *survey.Survey
}

func newBench(t *testing.T) *bench {
	t.Helper()
	cluster, err := kfake.NewCluster(kfake.NumBrokers(1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cluster.Close)
	cl, err := kgo.NewClient(kgo.SeedBrokers(cluster.ListenAddrs()...), kgo.RecordDeliveryTimeout(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cl.Close)
	codec := contract.NewCodec(contracttest.NewRegistry(t).Client())
	ctx := context.Background()
	if err := contract.EnsureTopics(ctx, kadm.NewClient(cl), contract.Topics...); err != nil {
		t.Fatal(err)
	}
	if err := codec.Register(ctx); err != nil {
		t.Fatal(err)
	}
	sv, err := survey.Load("../../../data/survey.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return &bench{cluster: cluster, brokers: cluster.ListenAddrs(), kafka: cl, codec: codec, survey: sv}
}

// produce sends v under the key: a message of the contract, raw bytes, or a
// tombstone for nil.
func (b *bench) produce(t *testing.T, topic, key string, v any) {
	t.Helper()
	var value []byte
	switch v := v.(type) {
	case nil:
	case []byte:
		value = v
	default:
		var err error
		if value, err = b.codec.Encode(v); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.kafka.ProduceSync(context.Background(), &kgo.Record{Topic: topic, Key: []byte(key), Value: value}).FirstErr(); err != nil {
		t.Fatal(err)
	}
}

// command sends a command as the bot does when an admin presses the button.
func (b *bench) command(t *testing.T, id string, force bool) {
	t.Helper()
	b.produce(t, contract.TopicCommands, id, &contract.RunCommand{ID: id, RequestedAt: time.Now(), Force: force})
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

// reports waits for n run reports and returns them in order.
func (b *bench) reports(t *testing.T, n int) []contract.RunReport {
	t.Helper()
	var out []contract.RunReport
	for _, rec := range b.read(t, contract.TopicRuns, n) {
		var r contract.RunReport
		if err := b.codec.Decode(context.Background(), rec.Value, &r); err != nil {
			t.Fatal(err)
		}
		if string(rec.Key) != r.RunID {
			t.Errorf("report of %s under the key %q", r.RunID, rec.Key)
		}
		out = append(out, r)
	}
	return out
}

func (b *bench) runner(run RunFunc) *Runner {
	return &Runner{Kafka: b.kafka, Codec: b.codec, Run: run, Log: slog.New(slog.DiscardHandler)}
}

// serve runs the agent until the returned stop or the end of the test.
func (b *bench) serve(t *testing.T, cfg Config, run RunFunc) (stop func()) {
	t.Helper()
	cfg.Brokers, cfg.Group = b.brokers, "agent"
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, cfg, b.runner(run), slog.New(slog.DiscardHandler)) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("serve: %v", err)
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

// quick is a run that finds nothing new.
func quick(context.Context, string, bool) (agent.Report, error) {
	return agent.Report{Queries: 1, Unchanged: 1}, nil
}

// receive waits for a value from ch.
func receive[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

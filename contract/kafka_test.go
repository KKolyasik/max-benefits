package contract

import (
	"context"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sr"
)

// TestThroughKafka checks the contract against a real Kafka and Schema
// Registry, e.g. from docker compose:
//
//	docker compose up -d --wait kafka schema-registry
//	KAFKA_BROKERS=localhost:9092 SCHEMA_REGISTRY_URL=http://localhost:8085 go test ./contract/
//
// It uses topics and a subject of its own and removes them afterwards, so
// the services' ones stay as they are.
func TestThroughKafka(t *testing.T) {
	brokers, registryURL := os.Getenv("KAFKA_BROKERS"), os.Getenv("SCHEMA_REGISTRY_URL")
	if brokers == "" || registryURL == "" {
		t.Skip("set KAFKA_BROKERS and SCHEMA_REGISTRY_URL to run against Kafka")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	seeds := kgo.SeedBrokers(strings.Split(brokers, ",")...)
	cl, err := kgo.NewClient(seeds)
	if err != nil {
		t.Fatal(err)
	}
	// Cleanups run in reverse order: the client closes after the topics go.
	t.Cleanup(cl.Close)
	adm := kadm.NewClient(cl)

	prefix := "test." + strconv.FormatInt(time.Now().UnixNano(), 36) + "."
	drafts := Topic{Name: prefix + TopicDrafts, Configs: retention(day)}
	cards := Topic{Name: prefix + TopicCards, Configs: compacted}
	t.Cleanup(func() { _, _ = adm.DeleteTopics(context.Background(), drafts.Name, cards.Name) })
	for range 2 { // creating existing topics is harmless
		if err := EnsureTopics(ctx, adm, drafts, cards); err != nil {
			t.Fatal(err)
		}
	}
	configs, err := adm.DescribeTopicConfigs(ctx, cards.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := configs.On(cards.Name, func(rc *kadm.ResourceConfig) error {
		for _, c := range rc.Configs {
			if c.Key == "cleanup.policy" && c.MaybeValue() != "compact" {
				t.Errorf("cleanup.policy of %s is %q", cards.Name, c.MaybeValue())
			}
		}
		return rc.Err
	}); err != nil {
		t.Fatal(err)
	}

	reg, err := sr.NewClient(sr.URLs(registryURL))
	if err != nil {
		t.Fatal(err)
	}
	codec := NewCodec(reg)
	for range 2 { // registering the same schemas is harmless
		if err := codec.Register(ctx); err != nil {
			t.Fatal(err)
		}
	}

	draft := Draft{ID: prefix + "d1", Card: card, Updates: "pgas", Query: "ПГАС", Sources: []string{"https://spbu.ru"},
		Notes: []string{"проверь сумму"}, FoundAt: at, RunID: prefix + "r1"}
	value, err := codec.Encode(&draft)
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.ProduceSync(ctx, &kgo.Record{Topic: drafts.Name, Key: []byte(draft.ID), Value: value}).FirstErr(); err != nil {
		t.Fatal(err)
	}
	consumer, err := kgo.NewClient(seeds, kgo.ConsumeTopics(drafts.Name), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		t.Fatal(err)
	}
	defer consumer.Close()
	fetches := consumer.PollRecords(ctx, 1)
	if err := fetches.Err(); err != nil {
		t.Fatal(err)
	}
	records := fetches.Records()
	if len(records) != 1 || string(records[0].Key) != draft.ID {
		t.Fatalf("records %v", records)
	}
	var got Draft
	if err := codec.Decode(ctx, records[0].Value, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, draft) {
		t.Errorf("draft changed:\n got %+v\nwant %+v", got, draft)
	}

	// The registry refuses a version that old readers could not read: here
	// the query field, which has no default, is gone.
	subject := prefix + "maxbenefits.Draft"
	t.Cleanup(func() {
		_, _ = reg.DeleteSubject(context.Background(), subject, sr.SoftDelete)
		_, _ = reg.DeleteSubject(context.Background(), subject, sr.HardDelete)
	})
	for _, res := range reg.SetCompatibility(ctx, sr.SetCompatibility{Level: sr.CompatFullTransitive}, subject) {
		if res.Err != nil {
			t.Fatal(res.Err)
		}
	}
	current := schemas[reflect.TypeFor[Draft]()].json
	if _, err := reg.RegisterSchema(ctx, subject, sr.Schema{Schema: current}, -1, -1); err != nil {
		t.Fatal(err)
	}
	broken := strings.Replace(current, `{"doc":"The search query that found it.","name":"query","type":"string"},`, "", 1)
	if broken == current {
		t.Fatal("the query field is not where the test expects it")
	}
	if _, err := reg.RegisterSchema(ctx, subject, sr.Schema{Schema: broken}, -1, -1); err == nil {
		t.Error("the registry took a schema without a required field")
	}
}

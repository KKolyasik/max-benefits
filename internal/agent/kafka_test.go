package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/KKolyasik/max-benefits/contract"
	"github.com/KKolyasik/max-benefits/contract/contracttest"
	"github.com/KKolyasik/max-benefits/internal/knowledge"
)

func kafkaSink(t *testing.T) (*KafkaSink, *kfake.Cluster) {
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
	return &KafkaSink{Client: cl, Codec: codec, RunID: "run-1"}, cluster
}

func TestKafkaSink(t *testing.T) {
	sink, cluster := kafkaSink(t)
	d := Draft{Query: "проездной", FoundAt: time.Date(2026, 9, 27, 3, 0, 0, 0, time.UTC), Updates: "transport",
		Sources: []string{pageURL}, Notes: []string{"проверь цену"}, Card: transport}
	where, written, err := sink.Write(context.Background(), d)
	if err != nil || !written || where == "" {
		t.Fatalf("where %q, written %v, err %v", where, written, err)
	}

	cl, err := kgo.NewClient(kgo.SeedBrokers(cluster.ListenAddrs()...), kgo.ConsumeTopics(contract.TopicDrafts),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	records := cl.PollRecords(ctx, 1).Records()
	if len(records) != 1 {
		t.Fatalf("records %v", records)
	}
	var m contract.Draft
	if err := sink.Codec.Decode(ctx, records[0].Value, &m); err != nil {
		t.Fatal(err)
	}
	if m.ID != DraftID(d) || string(records[0].Key) != m.ID || m.RunID != "run-1" || m.Updates != "transport" ||
		m.Notes[0] != "проверь цену" || knowledge.CardFromContract(m.Card).Title != transport.Title {
		t.Errorf("draft %+v", m)
	}
}

// The ID stands for the proposal: when and by which query it was found
// doesn't matter, what it proposes does.
func TestDraftID(t *testing.T) {
	d := Draft{Query: "проездной", Updates: "transport", Card: transport}
	again := d
	again.Query, again.FoundAt, again.Sources = "другой запрос", time.Now(), []string{"https://other.example"}
	if DraftID(d) != DraftID(again) {
		t.Error("the same proposal must keep its ID")
	}
	changed := d
	changed.Card.Summary = "другой текст"
	asNew := d
	asNew.Updates = ""
	if DraftID(changed) == DraftID(d) || DraftID(asNew) == DraftID(d) {
		t.Error("another proposal must get another ID")
	}
}

// When Kafka is gone the draft can't be delivered, and the run stops.
func TestKafkaSinkDown(t *testing.T) {
	sink, cluster := kafkaSink(t)
	cluster.Close()
	_, _, err := sink.Write(context.Background(), Draft{Card: transport, Query: "проездной"})
	if !errors.Is(err, ErrSink) || !fatal(err) {
		t.Errorf("got %v", err)
	}
}

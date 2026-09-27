package bus

import (
	"context"
	"errors"
	"testing"

	"github.com/KKolyasik/max-benefits/contract"
	"github.com/KKolyasik/max-benefits/internal/agent"
	"github.com/KKolyasik/max-benefits/internal/knowledge"
)

func TestKafkaSink(t *testing.T) {
	b := newBench(t)
	sink := &KafkaSink{Client: b.kafka, Codec: b.codec, RunID: "run-1"}
	d := agent.Draft{Query: "проездной", FoundAt: at, Updates: "transport",
		Sources: []string{transport.Links[0].URL}, Notes: []string{"проверь цену"}, Card: transport}
	where, written, err := sink.Write(context.Background(), d)
	if err != nil || !written || where == "" {
		t.Fatalf("where %q, written %v, err %v", where, written, err)
	}

	rec := b.read(t, contract.TopicDrafts, 1)[0]
	var m contract.Draft
	if err := b.codec.Decode(context.Background(), rec.Value, &m); err != nil {
		t.Fatal(err)
	}
	if m.ID != agent.DraftID(d) || string(rec.Key) != m.ID || m.RunID != "run-1" || m.Updates != "transport" ||
		m.Notes[0] != "проверь цену" || knowledge.CardFromContract(m.Card).Title != transport.Title {
		t.Errorf("draft %+v", m)
	}
}

// When Kafka is gone the draft can't be delivered, and the run stops.
func TestKafkaSinkDown(t *testing.T) {
	b := newBench(t)
	sink := &KafkaSink{Client: b.kafka, Codec: b.codec}
	b.cluster.Close()
	_, _, err := sink.Write(context.Background(), agent.Draft{Card: transport, Query: "проездной"})
	if !errors.Is(err, agent.ErrSink) {
		t.Errorf("got %v", err)
	}
}

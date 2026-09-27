// Package bus is the agent's end of the contract with the bot (see package
// contract). It reads the base the bot publishes, sends the drafts and the
// reports of the runs, and serves the agent: runs it on the admins'
// commands and on a schedule, one run at a time.
package bus

import (
	"context"
	"fmt"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/KKolyasik/max-benefits/contract"
	"github.com/KKolyasik/max-benefits/internal/agent"
)

// KafkaSink sends drafts to the bot through Kafka.
type KafkaSink struct {
	// Client should have a delivery timeout (kgo.RecordDeliveryTimeout):
	// without one it waits for Kafka forever.
	Client *kgo.Client
	Codec  *contract.Codec
	// RunID marks the drafts of one run.
	RunID string
}

// Write sends the draft. Every draft is sent: the bot keeps only the newest
// one of a card for review.
func (k *KafkaSink) Write(ctx context.Context, d agent.Draft) (string, bool, error) {
	m := contract.Draft{
		ID: agent.DraftID(d), Card: d.Card.Contract(), Updates: d.Updates, Query: d.Query,
		Sources: d.Sources, Notes: d.Notes, FoundAt: d.FoundAt, RunID: k.RunID,
	}
	value, err := k.Codec.Encode(&m)
	if err != nil {
		return "", false, err
	}
	if err := k.Client.ProduceSync(ctx, &kgo.Record{Topic: contract.TopicDrafts, Key: []byte(m.ID), Value: value}).FirstErr(); err != nil {
		return "", false, fmt.Errorf("%w: send the draft of %s to kafka: %w", agent.ErrSink, d.Card.ID, err)
	}
	return contract.TopicDrafts + "/" + m.ID[:12], true, nil
}

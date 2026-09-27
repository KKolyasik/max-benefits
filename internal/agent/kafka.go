package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/KKolyasik/max-benefits/contract"
	"github.com/KKolyasik/max-benefits/internal/knowledge"
)

// ErrSink means drafts can't be delivered: the run stops rather than spend
// tokens on drafts nobody gets.
var ErrSink = errors.New("drafts can't be delivered")

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
func (k *KafkaSink) Write(ctx context.Context, d Draft) (string, bool, error) {
	m := contract.Draft{
		ID: DraftID(d), Card: d.Card.Contract(), Updates: d.Updates, Query: d.Query,
		Sources: d.Sources, Notes: d.Notes, FoundAt: d.FoundAt, RunID: k.RunID,
	}
	value, err := k.Codec.Encode(&m)
	if err != nil {
		return "", false, err
	}
	if err := k.Client.ProduceSync(ctx, &kgo.Record{Topic: contract.TopicDrafts, Key: []byte(m.ID), Value: value}).FirstErr(); err != nil {
		return "", false, fmt.Errorf("%w: send the draft of %s to kafka: %w", ErrSink, d.Card.ID, err)
	}
	return contract.TopicDrafts + "/" + m.ID[:12], true, nil
}

// DraftID derives the ID of a draft from what it proposes: the card and the
// card it rewrites. So the same proposal sent again is one draft for the
// bot, and a rejected one stays rejected.
func DraftID(d Draft) string {
	sum := sha256.Sum256([]byte(d.Updates + "\n" + knowledge.MarshalCard(d.Card)))
	return hex.EncodeToString(sum[:])
}

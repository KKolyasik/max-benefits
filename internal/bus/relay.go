package bus

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/KKolyasik/max-benefits/contract"
	"github.com/KKolyasik/max-benefits/internal/knowledge"
	"github.com/KKolyasik/max-benefits/internal/moderation"
	"github.com/KKolyasik/max-benefits/internal/pgstore"
	"github.com/KKolyasik/max-benefits/internal/survey"
)

// relayBatch is how many outbox events go to Kafka at once.
const relayBatch = 100

// relay publishes the outbox to Kafka.
type relay struct {
	store Store
	kafka *kgo.Client
	codec *contract.Codec
	log   *slog.Logger
}

// syncAll publishes the survey and every card, so the compacted topics hold
// the whole base even if they were created anew or a change went astray.
func (r *relay) syncAll(ctx context.Context, sv *survey.Survey) error {
	s := sv.Contract()
	value, err := r.codec.Encode(&s)
	if err != nil {
		return err
	}
	records := []*kgo.Record{{Topic: contract.TopicSurvey, Key: []byte(contract.SurveyKey), Value: value}}
	cards, err := r.store.Cards(ctx)
	if err != nil {
		return err
	}
	for _, c := range cards {
		rec, err := r.cardRecord(c)
		if err != nil {
			return err
		}
		records = append(records, rec)
	}
	if err := r.kafka.ProduceSync(ctx, records...).FirstErr(); err != nil {
		return fmt.Errorf("publish the survey and the cards: %w", err)
	}
	r.log.Info("published the survey and the cards to kafka", "cards", len(cards))
	return nil
}

// run publishes the outbox every relayEvery until ctx is done.
func (r *relay) run(ctx context.Context) {
	t := time.NewTicker(relayEvery)
	defer t.Stop()
	for {
		if err := r.drain(ctx); err != nil && ctx.Err() == nil {
			r.log.Warn("publish the outbox to kafka, will retry", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// drain publishes the outbox until it is empty. An event leaves the outbox
// only after Kafka has its record, so a failure means a later retry, and
// the agent may see a record twice but never miss one.
func (r *relay) drain(ctx context.Context) error {
	for {
		events, err := r.store.Events(ctx, relayBatch)
		if err != nil || len(events) == 0 {
			return err
		}
		var records []*kgo.Record
		ids := make([]int64, len(events))
		for i, ev := range events {
			rec, err := r.record(ctx, ev)
			if err != nil {
				return err
			}
			if rec != nil {
				records = append(records, rec)
			}
			ids[i] = ev.ID
		}
		if err := r.kafka.ProduceSync(ctx, records...).FirstErr(); err != nil {
			return err
		}
		if err := r.store.DeleteEvents(ctx, ids); err != nil {
			return err
		}
	}
}

// record builds the Kafka record of an event from the current state: the
// card as it is now, a tombstone if it is gone, the decision on a draft.
// Nil means there is nothing to publish.
func (r *relay) record(ctx context.Context, ev pgstore.Event) (*kgo.Record, error) {
	switch ev.Kind {
	case pgstore.EventCard:
		c, ok, err := r.store.Card(ctx, ev.Ref)
		if err != nil {
			return nil, err
		}
		if !ok {
			return &kgo.Record{Topic: contract.TopicCards, Key: []byte(ev.Ref)}, nil
		}
		return r.cardRecord(c)
	case pgstore.EventDecision:
		d, ok, err := r.store.DraftDecision(ctx, ev.Ref)
		if err != nil || !ok {
			return nil, err
		}
		verdict := contract.VerdictRejected
		if d.Status == moderation.Approved {
			verdict = contract.VerdictApproved
		}
		value, err := r.codec.Encode(&contract.Decision{DraftID: d.ExternalID, CardID: d.CardID, Verdict: verdict, DecidedAt: d.DecidedAt})
		if err != nil {
			return nil, err
		}
		return &kgo.Record{Topic: contract.TopicDecisions, Key: []byte(d.ExternalID), Value: value}, nil
	}
	r.log.Warn("skip an outbox event of an unknown kind", "kind", ev.Kind, "ref", ev.Ref)
	return nil, nil
}

func (r *relay) cardRecord(c knowledge.Card) (*kgo.Record, error) {
	m := c.Contract()
	value, err := r.codec.Encode(&m)
	if err != nil {
		return nil, err
	}
	return &kgo.Record{Topic: contract.TopicCards, Key: []byte(c.ID), Value: value}, nil
}

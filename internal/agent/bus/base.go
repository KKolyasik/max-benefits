package bus

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/KKolyasik/max-benefits/contract"
	"github.com/KKolyasik/max-benefits/internal/knowledge"
	"github.com/KKolyasik/max-benefits/internal/survey"
)

// ErrNoBase means the bot has not published the survey yet.
var ErrNoBase = errors.New("the bot has not published the survey to kafka yet: start the bot with KAFKA_BROKERS first")

// Base is what a run learns from the bot.
type Base struct {
	Survey *survey.Survey
	// Cards are in the order the bot published them.
	Cards []knowledge.Card
	// Rejected are the IDs of the drafts admins rejected.
	Rejected map[string]bool
}

// ReadBase reads the survey, the cards and the admins' decisions from their
// compacted topics, as they are now.
func ReadBase(ctx context.Context, brokers []string, codec *contract.Codec) (Base, error) {
	topics, err := readLatest(ctx, brokers, contract.TopicSurvey, contract.TopicCards, contract.TopicDecisions)
	if err != nil {
		return Base{}, err
	}
	var b Base
	for _, rec := range topics[contract.TopicSurvey] {
		if string(rec.Key) != contract.SurveyKey {
			continue
		}
		var m contract.Survey
		if err := codec.Decode(ctx, rec.Value, &m); err != nil {
			return Base{}, err
		}
		if b.Survey, err = survey.FromContract(m); err != nil {
			return Base{}, fmt.Errorf("the survey from the bot: %w", err)
		}
	}
	if b.Survey == nil {
		return Base{}, ErrNoBase
	}
	for _, rec := range topics[contract.TopicCards] {
		var m contract.Card
		if err := codec.Decode(ctx, rec.Value, &m); err != nil {
			return Base{}, fmt.Errorf("card %s: %w", rec.Key, err)
		}
		b.Cards = append(b.Cards, knowledge.CardFromContract(m))
	}
	b.Rejected = map[string]bool{}
	for _, rec := range topics[contract.TopicDecisions] {
		var m contract.Decision
		if err := codec.Decode(ctx, rec.Value, &m); err != nil {
			return Base{}, fmt.Errorf("decision on draft %s: %w", rec.Key, err)
		}
		if m.Verdict == contract.VerdictRejected {
			b.Rejected[m.DraftID] = true
		}
	}
	return b, nil
}

// readLatest reads the topics from the start to where they end now. It
// returns the latest record of every key of each topic, in the order of
// their offsets; a key whose latest record is a tombstone is gone.
func readLatest(ctx context.Context, brokers []string, topics ...string) (map[string][]*kgo.Record, error) {
	cl, err := kgo.NewClient(kgo.SeedBrokers(brokers...), kgo.ConsumeTopics(topics...),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		return nil, fmt.Errorf("kafka: %w", err)
	}
	defer cl.Close()
	what := strings.Join(topics, ", ")
	adm := kadm.NewClient(cl)
	starts, err := adm.ListStartOffsets(ctx, topics...)
	if err == nil {
		err = starts.Error()
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", what, err)
	}
	ends, err := adm.ListEndOffsets(ctx, topics...)
	if err == nil {
		err = ends.Error()
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", what, err)
	}
	// ahead is where each partition with records in it ends.
	type partition struct {
		topic string
		p     int32
	}
	ahead := map[partition]int64{}
	ends.Each(func(end kadm.ListedOffset) {
		if start, ok := starts.Lookup(end.Topic, end.Partition); ok && end.Offset > start.Offset {
			ahead[partition{end.Topic, end.Partition}] = end.Offset
		}
	})

	latest := map[string]map[string]*kgo.Record{}
	for len(ahead) > 0 {
		fetches := cl.PollFetches(ctx)
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("read %s: %w", what, err)
		}
		if errs := fetches.Errors(); len(errs) > 0 {
			return nil, fmt.Errorf("read %s: %w", errs[0].Topic, errs[0].Err)
		}
		fetches.EachRecord(func(rec *kgo.Record) {
			if latest[rec.Topic] == nil {
				latest[rec.Topic] = map[string]*kgo.Record{}
			}
			latest[rec.Topic][string(rec.Key)] = rec
			at := partition{rec.Topic, rec.Partition}
			if end, ok := ahead[at]; ok && rec.Offset+1 >= end {
				delete(ahead, at)
			}
		})
	}

	out := map[string][]*kgo.Record{}
	for topic, byKey := range latest {
		records := slices.DeleteFunc(slices.Collect(maps.Values(byKey)), func(rec *kgo.Record) bool { return rec.Value == nil })
		slices.SortFunc(records, func(a, b *kgo.Record) int {
			return cmp.Or(cmp.Compare(a.Partition, b.Partition), cmp.Compare(a.Offset, b.Offset))
		})
		out[topic] = records
	}
	return out, nil
}

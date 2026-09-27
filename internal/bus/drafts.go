package bus

import (
	"context"
	"errors"
	"log/slog"
	"net/url"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sr"

	"github.com/KKolyasik/max-benefits/contract"
	"github.com/KKolyasik/max-benefits/internal/knowledge"
	"github.com/KKolyasik/max-benefits/internal/moderation"
)

// drafts takes the agent's drafts from Kafka into the database.
type drafts struct {
	store  Store
	kafka  *kgo.Client
	codec  *contract.Codec
	notify func(context.Context, int) error
	log    *slog.Logger
}

// run takes drafts until ctx is done. The offsets are committed after a
// batch is in the database, so a crash means reading it again, and the
// database keeps a draft once however many times it comes.
func (d *drafts) run(ctx context.Context) {
	for {
		fetches := d.kafka.PollFetches(ctx)
		if ctx.Err() != nil || fetches.IsClientClosed() {
			return
		}
		fetches.EachError(func(topic string, _ int32, err error) {
			d.log.Warn("read drafts from kafka", "topic", topic, "err", err)
		})
		added := 0
		fetches.EachRecord(func(rec *kgo.Record) {
			if d.take(ctx, rec) {
				added++
			}
		})
		if ctx.Err() != nil {
			return // the batch comes again after a restart
		}
		if err := d.kafka.CommitUncommittedOffsets(ctx); err != nil {
			d.log.Warn("commit the offsets of drafts", "err", err)
		}
		if added > 0 {
			d.log.Info("new drafts", "count", added)
			if err := d.notify(ctx, added); err != nil {
				d.log.Error("notify admins", "err", err)
			}
		}
	}
}

// take stores a draft and reports whether it is new. A record that can never
// be read is logged and skipped; the registry and the database are retried
// until they work, as a draft must not be lost.
func (d *drafts) take(ctx context.Context, rec *kgo.Record) bool {
	var m contract.Draft
	for pause := firstPause; ; {
		err := d.codec.Decode(ctx, rec.Value, &m)
		if err == nil {
			break
		}
		if !transient(err) {
			d.log.Error("skip a record that is not a draft", "offset", rec.Offset, "err", err)
			return false
		}
		if !retry(ctx, d.log, &pause, "read a draft", err) {
			return false
		}
	}
	draft := moderation.Draft{
		Card: knowledge.CardFromContract(m.Card), Updates: m.Updates, Query: m.Query,
		Sources: m.Sources, Notes: m.Notes, FoundAt: m.FoundAt,
	}
	for pause := firstPause; ; {
		added, err := d.store.AddDraft(ctx, m.ID, draft)
		if err == nil {
			return added
		}
		if !retry(ctx, d.log, &pause, "store a draft", err) {
			return false
		}
	}
}

// transient tells a failure of the registry, which passes, from a record
// that can never be read.
func transient(err error) bool {
	var resp *sr.ResponseError
	if errors.As(err, &resp) {
		return resp.StatusCode >= 500
	}
	var netErr *url.Error
	return errors.As(err, &netErr)
}

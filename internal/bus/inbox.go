package bus

import (
	"context"
	"log/slog"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/KKolyasik/max-benefits/contract"
	"github.com/KKolyasik/max-benefits/internal/knowledge"
	"github.com/KKolyasik/max-benefits/internal/moderation"
)

// staleRun is the age after which a report of a run no longer goes to the
// admins: a new consumer group reads the reports of the last month.
const staleRun = 24 * time.Hour

// inbox takes what the agent sends: the drafts go into the database, and
// the admins hear how the runs go.
type inbox struct {
	store  Store
	kafka  *kgo.Client
	codec  *contract.Codec
	admins Admins
	log    *slog.Logger
}

// run takes the records until ctx is done. The offsets are committed after
// a batch is handled, so a crash means reading it again, and the database
// keeps a draft once however many times it comes.
func (in *inbox) run(ctx context.Context) {
	for {
		fetches := in.kafka.PollFetches(ctx)
		if ctx.Err() != nil || fetches.IsClientClosed() {
			return
		}
		fetches.EachError(func(topic string, _ int32, err error) {
			in.log.Warn("read from kafka", "topic", topic, "err", err)
		})
		// The drafts go first: a report tells how many wait for review.
		var drafts, runs []*kgo.Record
		fetches.EachRecord(func(rec *kgo.Record) {
			if rec.Topic == contract.TopicRuns {
				runs = append(runs, rec)
			} else {
				drafts = append(drafts, rec)
			}
		})
		added := 0
		for _, rec := range drafts {
			if in.takeDraft(ctx, rec) {
				added++
			}
		}
		if added > 0 {
			in.log.Info("new drafts", "count", added)
		}
		for _, rec := range runs {
			in.takeRun(ctx, rec)
		}
		if ctx.Err() != nil {
			return // the batch comes again after a restart
		}
		if err := in.kafka.CommitUncommittedOffsets(ctx); err != nil {
			in.log.Warn("commit the offsets", "err", err)
		}
	}
}

// takeDraft stores a draft and reports whether it is new. A record that can
// never be read is logged and skipped; the registry and the database are
// retried until they work, as a draft must not be lost.
func (in *inbox) takeDraft(ctx context.Context, rec *kgo.Record) bool {
	var m contract.Draft
	if !in.decode(ctx, rec, &m) {
		return false
	}
	draft := moderation.Draft{
		Card: knowledge.CardFromContract(m.Card), Updates: m.Updates, Query: m.Query,
		Sources: m.Sources, Notes: m.Notes, FoundAt: m.FoundAt,
	}
	for pause := firstPause; ; {
		added, err := in.store.AddDraft(ctx, m.ID, draft)
		if err == nil {
			return added
		}
		if !retry(ctx, in.log, &pause, "store a draft", err) {
			return false
		}
	}
}

// takeRun tells the admins about a run. A report that failed to reach them
// is not sent again: the drafts wait in the database all the same.
func (in *inbox) takeRun(ctx context.Context, rec *kgo.Record) {
	var m contract.RunReport
	if !in.decode(ctx, rec, &m) {
		return
	}
	run := moderation.Run{
		ID: m.RunID, Trigger: triggers[m.Trigger], Status: statuses[m.Status], StartedAt: m.StartedAt, FinishedAt: m.FinishedAt,
		Queries: m.Queries, Unchanged: m.Unchanged, Failed: m.Failed, Deferred: m.Deferred, Drafts: m.Drafts,
		InputTokens: m.InputTokens, OutputTokens: m.OutputTokens, Error: m.Error,
	}
	at := run.FinishedAt
	if at.IsZero() {
		at = run.StartedAt
	}
	switch {
	case run.Status == "":
		in.log.Warn("skip a report of a run in an unknown status", "run", run.ID, "status", m.Status)
	case time.Since(at) > staleRun:
		in.log.Info("skip an old report of a run", "run", run.ID, "status", m.Status, "at", at)
	default:
		in.log.Info("agent run", "run", run.ID, "trigger", m.Trigger, "status", m.Status, "drafts", m.Drafts, "err", m.Error)
		if err := in.admins.NotifyRun(ctx, run); err != nil {
			in.log.Error("tell the admins about the run", "run", run.ID, "err", err)
		}
	}
}

var (
	triggers = map[contract.RunTrigger]moderation.Trigger{
		contract.RunTriggerSchedule: moderation.BySchedule,
		contract.RunTriggerCommand:  moderation.ByCommand,
		contract.RunTriggerCLI:      moderation.FromCLI,
	}
	statuses = map[contract.RunStatus]moderation.RunStatus{
		contract.RunStatusStarted: moderation.RunStarted,
		contract.RunStatusDone:    moderation.RunDone,
		contract.RunStatusFailed:  moderation.RunFailed,
		contract.RunStatusBusy:    moderation.RunBusy,
	}
)

// decode reads a message, retrying while the registry fails. A record that
// can never be read is logged and skipped: false.
func (in *inbox) decode(ctx context.Context, rec *kgo.Record, v any) bool {
	for pause := firstPause; ; {
		err := in.codec.Decode(ctx, rec.Value, v)
		if err == nil {
			return true
		}
		if !contract.Temporary(err) {
			in.log.Error("skip a record that can't be read", "topic", rec.Topic, "offset", rec.Offset, "err", err)
			return false
		}
		if !retry(ctx, in.log, &pause, "read "+rec.Topic, err) {
			return false
		}
	}
}

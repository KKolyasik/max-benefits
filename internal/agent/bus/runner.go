package bus

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/KKolyasik/max-benefits/contract"
	"github.com/KKolyasik/max-benefits/internal/agent"
)

// RunFunc does one run of the agent: it collects drafts and sends them
// marked with the run ID.
type RunFunc func(ctx context.Context, runID string, force bool) (agent.Report, error)

// Runner runs the agent and tells the bot how the run goes: STARTED when it
// starts, DONE or FAILED when it ends.
type Runner struct {
	// Kafka should have a delivery timeout (kgo.RecordDeliveryTimeout).
	Kafka *kgo.Client
	Codec *contract.Codec
	Run   RunFunc
	Log   *slog.Logger
	// Now is overridable in tests.
	Now func() time.Time
}

// Do runs the agent once. A run the bot can't be told about doesn't start:
// its drafts would not reach the bot either.
func (r *Runner) Do(ctx context.Context, trigger contract.RunTrigger, commandID string, force bool) (agent.Report, error) {
	started := r.now()
	report := contract.RunReport{RunID: newRunID(started), Trigger: trigger, CommandID: commandID,
		Status: contract.RunStatusStarted, StartedAt: started}
	log := r.Log.With("run", report.RunID)
	if err := r.send(ctx, report); err != nil {
		return agent.Report{}, fmt.Errorf("%w: %w", agent.ErrSink, err)
	}
	log.Info("run started", "trigger", trigger, "command", commandID, "force", force)

	rep, err := r.Run(ctx, report.RunID, force)
	report.Status, report.FinishedAt = contract.RunStatusDone, r.now()
	report.Queries, report.Unchanged, report.Failed, report.Deferred = rep.Queries, rep.Unchanged, rep.Failed, rep.Deferred
	report.Drafts, report.InputTokens, report.OutputTokens = len(rep.Drafts), int64(rep.InputTokens), int64(rep.OutputTokens)
	if err != nil {
		report.Status = contract.RunStatusFailed
		report.Error = err.Error()
		if ctx.Err() != nil {
			// Admins read it: a stop, e.g. for a deploy, is no failure of
			// the agent.
			report.Error = "агент остановили посреди прогона, например для деплоя"
		}
	}
	log.Info("run finished", "status", report.Status, "queries", report.Queries, "unchanged", report.Unchanged,
		"failed", report.Failed, "deferred", report.Deferred, "drafts", report.Drafts,
		"input_tokens", report.InputTokens, "output_tokens", report.OutputTokens, "err", report.Error)
	// The run may end because the agent stops; the report goes all the same.
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if serr := r.send(sendCtx, report); serr != nil {
		err = errors.Join(err, serr)
	}
	return rep, err
}

// Busy tells the bot that the command came during another run and is
// skipped.
func (r *Runner) Busy(ctx context.Context, commandID string) error {
	now := r.now()
	return r.send(ctx, contract.RunReport{RunID: newRunID(now), Trigger: contract.RunTriggerCommand, CommandID: commandID,
		Status: contract.RunStatusBusy, StartedAt: now, FinishedAt: now})
}

func (r *Runner) send(ctx context.Context, report contract.RunReport) error {
	value, err := r.Codec.Encode(&report)
	if err != nil {
		return err
	}
	rec := &kgo.Record{Topic: contract.TopicRuns, Key: []byte(report.RunID), Value: value}
	if err := r.Kafka.ProduceSync(ctx, rec).FirstErr(); err != nil {
		r.Log.Error("send the run report to kafka", "run", report.RunID, "status", report.Status, "err", err)
		return fmt.Errorf("send the report of run %s: %w", report.RunID, err)
	}
	return nil
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// newRunID names a run by its start and a few random bytes.
func newRunID(started time.Time) string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return started.UTC().Format("20060102T150405") + "-" + hex.EncodeToString(b)
}

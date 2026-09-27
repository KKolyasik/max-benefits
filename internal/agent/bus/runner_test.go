package bus

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/KKolyasik/max-benefits/contract"
	"github.com/KKolyasik/max-benefits/internal/agent"
)

// The bot learns that a run started before it goes on, and how it ended.
func TestRunnerReports(t *testing.T) {
	b := newBench(t)
	var runID string
	var force bool
	r := b.runner(func(_ context.Context, id string, f bool) (agent.Report, error) {
		runID, force = id, f
		if got := b.reports(t, 1); got[0].Status != contract.RunStatusStarted {
			t.Errorf("before the run the bot must know it started: %+v", got[0])
		}
		return agent.Report{Queries: 13, Unchanged: 9, Failed: 1, Deferred: 1, Drafts: []string{"a", "b"},
			InputTokens: 120_000, OutputTokens: 6_000}, nil
	})
	if _, err := r.Do(context.Background(), contract.RunTriggerCommand, "c1", true); err != nil {
		t.Fatal(err)
	}
	if !force {
		t.Error("force must reach the run")
	}
	reports := b.reports(t, 2)
	started, done := reports[0], reports[1]
	if started.RunID != runID || started.Trigger != contract.RunTriggerCommand || started.CommandID != "c1" ||
		!started.FinishedAt.IsZero() {
		t.Errorf("started %+v", started)
	}
	if done.RunID != runID || done.Status != contract.RunStatusDone || done.CommandID != "c1" ||
		done.Queries != 13 || done.Unchanged != 9 || done.Failed != 1 || done.Deferred != 1 || done.Drafts != 2 ||
		done.InputTokens != 120_000 || done.OutputTokens != 6_000 || done.Error != "" ||
		!done.StartedAt.Equal(started.StartedAt) || done.FinishedAt.Before(done.StartedAt) {
		t.Errorf("done %+v", done)
	}
}

func TestRunnerFailure(t *testing.T) {
	b := newBench(t)
	r := b.runner(func(context.Context, string, bool) (agent.Report, error) {
		return agent.Report{Queries: 2, Drafts: []string{"a"}}, errors.New("yandex search: 403 Forbidden")
	})
	if _, err := r.Do(context.Background(), contract.RunTriggerSchedule, "", false); err == nil {
		t.Error("the error must reach the caller")
	}
	failed := b.reports(t, 2)[1]
	if failed.Status != contract.RunStatusFailed || failed.Error != "yandex search: 403 Forbidden" ||
		failed.Drafts != 1 || failed.Trigger != contract.RunTriggerSchedule {
		t.Errorf("failed %+v", failed)
	}
}

// An agent stopped in the middle of a run still reports it.
func TestRunnerStopped(t *testing.T) {
	b := newBench(t)
	ctx, cancel := context.WithCancel(context.Background())
	r := b.runner(func(ctx context.Context, _ string, _ bool) (agent.Report, error) {
		cancel()
		return agent.Report{Queries: 3}, ctx.Err()
	})
	_, _ = r.Do(ctx, contract.RunTriggerCLI, "", false)
	stopped := b.reports(t, 2)[1]
	if stopped.Status != contract.RunStatusFailed || !strings.Contains(stopped.Error, "остановили") || stopped.Queries != 3 {
		t.Errorf("stopped %+v", stopped)
	}
}

// Without Kafka the drafts would be lost, so the run doesn't start.
func TestRunnerWithoutKafka(t *testing.T) {
	b := newBench(t)
	r := b.runner(func(context.Context, string, bool) (agent.Report, error) {
		t.Error("the run must not start")
		return agent.Report{}, nil
	})
	b.cluster.Close()
	if _, err := r.Do(context.Background(), contract.RunTriggerCommand, "c1", false); !errors.Is(err, agent.ErrSink) {
		t.Errorf("got %v", err)
	}
}

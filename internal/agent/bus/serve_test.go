package bus

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/KKolyasik/max-benefits/contract"
	"github.com/KKolyasik/max-benefits/internal/agent"
)

// every is a schedule with a moment every so often.
type every time.Duration

func (e every) Next(t time.Time) time.Time {
	return t.Truncate(time.Duration(e)).Add(time.Duration(e))
}

// A command runs the agent; a command that comes during the run is
// answered with BUSY.
func TestCommandsRunTheAgent(t *testing.T) {
	b := newBench(t)
	forces := make(chan bool, 10)
	release := make(chan struct{})
	b.serve(t, Config{}, func(ctx context.Context, _ string, force bool) (agent.Report, error) {
		forces <- force
		select {
		case <-release:
			return agent.Report{Queries: 13, Drafts: []string{"a"}}, nil
		case <-ctx.Done():
			return agent.Report{}, ctx.Err()
		}
	})

	b.command(t, "c1", true)
	if !receive(t, forces, "the run") {
		t.Error("force must reach the run")
	}
	b.command(t, "c2", false)
	busy := b.reports(t, 2)[1]
	if busy.Status != contract.RunStatusBusy || busy.CommandID != "c2" || busy.Trigger != contract.RunTriggerCommand {
		t.Errorf("busy %+v", busy)
	}
	close(release)
	done := b.reports(t, 3)[2]
	if done.Status != contract.RunStatusDone || done.CommandID != "c1" || done.Drafts != 1 {
		t.Errorf("done %+v", done)
	}

	// Once the run is over, the next command runs again.
	b.command(t, "c3", false)
	receive(t, forces, "the next run")
	if last := b.reports(t, 5)[4]; last.Status != contract.RunStatusDone || last.CommandID != "c3" {
		t.Errorf("last %+v", last)
	}
}

// A record that is not a command doesn't stop the ones after it.
func TestJunkCommandIsSkipped(t *testing.T) {
	b := newBench(t)
	b.produce(t, contract.TopicCommands, "junk", []byte("not a command"))
	b.command(t, "c1", false)
	b.serve(t, Config{}, quick)
	if done := b.reports(t, 2)[1]; done.CommandID != "c1" || done.Status != contract.RunStatusDone {
		t.Errorf("done %+v", done)
	}
}

func TestScheduleRunsTheAgent(t *testing.T) {
	b := newBench(t)
	b.serve(t, Config{Schedule: every(100 * time.Millisecond)}, quick)
	for _, r := range b.reports(t, 2) {
		if r.Trigger != contract.RunTriggerSchedule || r.CommandID != "" {
			t.Errorf("report %+v", r)
		}
	}
}

// A run missed while the agent was down starts once it is up.
func TestMissedRunStartsAtOnce(t *testing.T) {
	b := newBench(t)
	long := time.Now().Add(-2 * time.Hour)
	b.produce(t, contract.TopicRuns, "old", &contract.RunReport{RunID: "old", Trigger: contract.RunTriggerSchedule,
		Status: contract.RunStatusDone, StartedAt: long, FinishedAt: long})
	b.serve(t, Config{Schedule: every(time.Hour)}, quick)
	if started := b.reports(t, 2)[1]; started.Trigger != contract.RunTriggerSchedule || started.Status != contract.RunStatusStarted {
		t.Errorf("started %+v", started)
	}
}

// The next run is due at the first moment of the schedule after the latest
// run: a missed one comes at once, and only one. With no run known the
// agent waits for the schedule.
func TestDue(t *testing.T) {
	up := time.Date(2026, 9, 27, 12, 30, 0, 0, time.UTC)
	s := &server{cfg: Config{Schedule: every(time.Hour)}, up: up}
	if got := s.due(time.Time{}); !got.Equal(up.Add(30 * time.Minute)) {
		t.Errorf("with no run known: %v", got)
	}
	s.last = up.Add(-5 * time.Hour)
	if got := s.due(time.Time{}); !got.Equal(up.Add(-4*time.Hour - 30*time.Minute)) {
		t.Errorf("after missed runs: %v", got)
	}
	// That moment was served, e.g. skipped for a command's run.
	if got := s.due(up.Add(-4*time.Hour - 30*time.Minute)); !got.Equal(up.Add(-3*time.Hour - 30*time.Minute)) {
		t.Errorf("after the moment served: %v", got)
	}
	s.last = up.Add(10 * time.Minute)
	if got := s.due(up.Add(-4 * time.Hour)); !got.Equal(up.Add(30 * time.Minute)) {
		t.Errorf("after a run: %v", got)
	}
}

// A stopped agent still reports the run it had to leave.
func TestStopDuringARun(t *testing.T) {
	b := newBench(t)
	started := make(chan struct{})
	stop := b.serve(t, Config{}, func(ctx context.Context, _ string, _ bool) (agent.Report, error) {
		close(started)
		<-ctx.Done()
		return agent.Report{Queries: 3}, ctx.Err()
	})
	b.command(t, "c1", false)
	receive(t, started, "the run")
	stop()
	if failed := b.reports(t, 2)[1]; failed.Status != contract.RunStatusFailed || !strings.Contains(failed.Error, "остановили") {
		t.Errorf("failed %+v", failed)
	}
}

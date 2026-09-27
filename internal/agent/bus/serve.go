package bus

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/KKolyasik/max-benefits/contract"
)

// Schedule tells when the agent runs by itself; a cron.Schedule is one.
type Schedule interface {
	// Next returns the first moment of the schedule after t.
	Next(t time.Time) time.Time
}

// Config is how the agent serves.
type Config struct {
	Brokers []string
	// Group is the consumer group that takes the commands.
	Group string
	// Schedule is when the agent runs by itself; nil means only on commands.
	Schedule Schedule
}

// firstPause is the first wait before retrying; each next one is twice as
// long, up to half a minute. Shortened in tests.
var firstPause = time.Second

// Serve runs the agent on the admins' commands and on the schedule until ctx
// is done, one run at a time: a command that comes during a run is answered
// with BUSY. A scheduled run missed while the agent was down starts at once.
// Serve returns once the run in progress has stopped and reported.
func Serve(ctx context.Context, cfg Config, runner *Runner, log *slog.Logger) error {
	readCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	last, err := lastRun(readCtx, cfg.Brokers, runner.Codec)
	cancel()
	if err != nil {
		return err
	}
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(cfg.Brokers...),
		kgo.ConsumerGroup(cfg.Group),
		kgo.ConsumeTopics(contract.TopicCommands),
		// Commands sent while the agent was down count too: the topic
		// keeps them for a day.
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.DisableAutoCommit(),
	)
	if err != nil {
		return fmt.Errorf("kafka: %w", err)
	}
	defer cl.Close()

	s := &server{cfg: cfg, runner: runner, log: log, last: last, up: runner.now()}
	var attrs []any
	if !last.IsZero() {
		attrs = append(attrs, "last_run", last)
	}
	if cfg.Schedule != nil {
		attrs = append(attrs, "next_run", s.due(time.Time{}))
	}
	log.Info("agent is running", attrs...)
	var wg sync.WaitGroup
	wg.Go(func() { s.schedule(ctx) })
	s.commands(ctx, cl)
	wg.Wait()
	s.runs.Wait()
	return nil
}

type server struct {
	cfg    Config
	runner *Runner
	log    *slog.Logger
	// up is when the agent started.
	up time.Time

	mu      sync.Mutex
	running bool
	// last is when the latest run started; zero if none is known.
	last time.Time
	runs sync.WaitGroup
}

// start starts a run unless another one goes on.
func (s *server) start(ctx context.Context, trigger contract.RunTrigger, commandID string, force bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running || ctx.Err() != nil {
		return false
	}
	s.running, s.last = true, s.runner.now()
	s.runs.Go(func() {
		defer func() {
			s.mu.Lock()
			s.running = false
			s.mu.Unlock()
		}()
		// The runner logs and reports how the run went.
		_, _ = s.runner.Do(ctx, trigger, commandID, force)
	})
	return true
}

// schedule starts the scheduled runs until ctx is done.
func (s *server) schedule(ctx context.Context) {
	if s.cfg.Schedule == nil {
		return
	}
	// served is the latest moment of the schedule handled: its run started
	// or was skipped for another one.
	var served time.Time
	for {
		due := s.due(served)
		timer := time.NewTimer(due.Sub(s.runner.now()))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		served = due
		if !s.start(ctx, contract.RunTriggerSchedule, "", false) && ctx.Err() == nil {
			s.log.Info("the scheduled run is skipped: another run goes on")
		}
	}
}

// due returns when the next scheduled run is due: at the first moment of
// the schedule after the latest run started or the latest moment served. So
// runs missed while the agent was down make one run at once. When no run is
// known, e.g. the first time the agent starts, it waits for the schedule.
func (s *server) due(served time.Time) time.Time {
	s.mu.Lock()
	after := s.last
	s.mu.Unlock()
	if after.IsZero() {
		after = s.up
	}
	if served.After(after) {
		after = served
	}
	return s.cfg.Schedule.Next(after)
}

// commands takes the admins' commands until ctx is done. A command counts
// as taken once its run started or BUSY went back.
func (s *server) commands(ctx context.Context, cl *kgo.Client) {
	for {
		fetches := cl.PollFetches(ctx)
		if ctx.Err() != nil || fetches.IsClientClosed() {
			return
		}
		fetches.EachError(func(topic string, _ int32, err error) {
			s.log.Warn("read commands from kafka", "topic", topic, "err", err)
		})
		fetches.EachRecord(func(rec *kgo.Record) { s.command(ctx, rec) })
		if ctx.Err() != nil {
			return // the commands come again after a restart
		}
		if err := cl.CommitUncommittedOffsets(ctx); err != nil {
			s.log.Warn("commit the offsets of commands", "err", err)
		}
	}
}

func (s *server) command(ctx context.Context, rec *kgo.Record) {
	var cmd contract.RunCommand
	for pause := firstPause; ; {
		err := s.runner.Codec.Decode(ctx, rec.Value, &cmd)
		if err == nil {
			break
		}
		if !contract.Temporary(err) {
			s.log.Error("skip a record that is not a command", "offset", rec.Offset, "err", err)
			return
		}
		if !retry(ctx, s.log, &pause, "read a command", err) {
			return
		}
	}
	if s.start(ctx, contract.RunTriggerCommand, cmd.ID, cmd.Force) || ctx.Err() != nil {
		return
	}
	s.log.Info("the command is skipped: another run goes on", "command", cmd.ID)
	_ = s.runner.Busy(ctx, cmd.ID) // the runner logs a failure
}

// lastRun returns when the latest run started; zero if none is known.
func lastRun(ctx context.Context, brokers []string, codec *contract.Codec) (time.Time, error) {
	topics, err := readLatest(ctx, brokers, contract.TopicRuns)
	if err != nil {
		return time.Time{}, err
	}
	var last time.Time
	for _, rec := range topics[contract.TopicRuns] {
		var r contract.RunReport
		if err := codec.Decode(ctx, rec.Value, &r); err != nil {
			return time.Time{}, fmt.Errorf("report of run %s: %w", rec.Key, err)
		}
		if r.Status != contract.RunStatusBusy && r.StartedAt.After(last) {
			last = r.StartedAt
		}
	}
	return last, nil
}

// retry waits before the next attempt of what failed with err; false means
// ctx is done.
func retry(ctx context.Context, log *slog.Logger, pause *time.Duration, what string, err error) bool {
	log.Error(what+", will retry", "in", *pause, "err", err)
	select {
	case <-ctx.Done():
		return false
	case <-time.After(*pause):
	}
	*pause = min(*pause*2, 30*time.Second)
	return true
}

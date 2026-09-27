package moderation

import "time"

// Run is how a run of the agent went, as admins are told.
type Run struct {
	ID      string
	Trigger Trigger
	Status  RunStatus
	// StartedAt and FinishedAt; FinishedAt is zero while the run goes on.
	StartedAt  time.Time
	FinishedAt time.Time
	Queries    int
	// Unchanged counts queries whose pages were the same as last time.
	Unchanged int
	Failed    int
	// Deferred counts queries left for the next run for lack of tokens.
	Deferred     int
	Drafts       int
	InputTokens  int64
	OutputTokens int64
	// Error is why the run failed.
	Error string
}

// Trigger is what started a run; empty if the bot doesn't know it.
type Trigger string

const (
	BySchedule Trigger = "schedule"
	// ByCommand is a run an admin started with the button.
	ByCommand Trigger = "command"
	// FromCLI is a run started from the command line.
	FromCLI Trigger = "cli"
)

// RunStatus is where a run is; empty if the bot doesn't know it.
type RunStatus string

const (
	RunStarted RunStatus = "started"
	RunDone    RunStatus = "done"
	RunFailed  RunStatus = "failed"
	// RunBusy means the command came during another run and was skipped.
	RunBusy RunStatus = "busy"
)

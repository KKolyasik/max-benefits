package contract

import "time"

// Card is a knowledge base card. It is the value of knowledge.cards, keyed
// by the ID; a deleted card leaves a tombstone, a record without a value.
type Card struct {
	ID         string   `avro:"id"`
	Categories []string `avro:"categories"`
	Priority   int      `avro:"priority"`
	// Match maps a question ID to the options a student must have picked
	// to see the card. Empty means everyone in the card's sections.
	Match     map[string][]string `avro:"match"`
	Title     string              `avro:"title"`
	Summary   string              `avro:"summary"`
	Steps     []string            `avro:"steps"`
	Documents []string            `avro:"documents"`
	Where     string              `avro:"where"`
	Links     []CardLink          `avro:"links"`
}

// CardLink is a link of a card.
type CardLink struct {
	Title string `avro:"title"`
	URL   string `avro:"url"`
	// When is who sees the link, like Match of the card.
	When map[string][]string `avro:"when"`
}

// SurveyKey is the key of the only record in knowledge.survey that counts:
// the latest one.
const SurveyKey = "survey"

// Survey is the questionnaire: the bot's sections and the questions asked
// in them.
type Survey struct {
	Categories []Category `avro:"categories"`
	Questions  []Question `avro:"questions"`
}

// Category is a section of the bot.
type Category struct {
	ID    string `avro:"id"`
	Title string `avro:"title"`
	Intro string `avro:"intro"`
	// Questions are the IDs of the questions asked in the section, in order.
	Questions []string `avro:"questions"`
}

// Question is a question of the survey.
type Question struct {
	ID string `avro:"id"`
	// Label is a short name for the profile summary.
	Label   string   `avro:"label"`
	Text    string   `avro:"text"`
	Multi   bool     `avro:"multi"`
	Options []Option `avro:"options"`
}

// Option is an answer to a question.
type Option struct {
	ID        string `avro:"id"`
	Title     string `avro:"title"`
	Exclusive bool   `avro:"exclusive"`
}

// Draft is a card the agent drafted for admins to review. It is the value
// of knowledge.drafts, keyed by the ID.
type Draft struct {
	// ID is derived from the content: a draft delivered twice is one draft.
	ID   string `avro:"id"`
	Card Card   `avro:"card"`
	// Updates is the ID of the card the draft rewrites, empty for a new card.
	Updates string   `avro:"updates"`
	Query   string   `avro:"query"`
	Sources []string `avro:"sources"`
	// Notes are problems the agent could not fix.
	Notes   []string  `avro:"notes"`
	FoundAt time.Time `avro:"found_at"`
	RunID   string    `avro:"run_id"`
}

// Verdict is what admins decided about a draft.
type Verdict string

// Verdicts. A reader of an older version of the schema sees an unknown
// verdict as VerdictUnknown.
const (
	VerdictUnknown  Verdict = "UNKNOWN"
	VerdictApproved Verdict = "APPROVED"
	VerdictRejected Verdict = "REJECTED"
)

// Decision is what admins decided about a draft. It is the value of
// knowledge.decisions, keyed by the draft ID; the topic is compacted, so the
// agent remembers the decisions for good.
type Decision struct {
	DraftID   string    `avro:"draft_id"`
	CardID    string    `avro:"card_id"`
	Verdict   Verdict   `avro:"verdict"`
	DecidedAt time.Time `avro:"decided_at"`
}

// RunCommand asks the agent to run now. It is the value of agent.commands,
// keyed by the ID.
type RunCommand struct {
	ID          string    `avro:"id"`
	RequestedAt time.Time `avro:"requested_at"`
	// Force sends pages to the model even if they did not change.
	Force bool `avro:"force"`
}

// RunTrigger is what started a run.
type RunTrigger string

// Run triggers.
const (
	RunTriggerUnknown  RunTrigger = "UNKNOWN"
	RunTriggerSchedule RunTrigger = "SCHEDULE"
	RunTriggerCommand  RunTrigger = "COMMAND"
)

// RunStatus is how a run ended.
type RunStatus string

// Run statuses. RunStatusBusy means a command came during another run and
// was skipped.
const (
	RunStatusUnknown RunStatus = "UNKNOWN"
	RunStatusDone    RunStatus = "DONE"
	RunStatusFailed  RunStatus = "FAILED"
	RunStatusBusy    RunStatus = "BUSY"
)

// RunReport is how an agent run went. It is the value of agent.runs, keyed
// by the run ID.
type RunReport struct {
	RunID   string     `avro:"run_id"`
	Trigger RunTrigger `avro:"trigger"`
	// CommandID is the command that started the run, if any.
	CommandID    string    `avro:"command_id"`
	Status       RunStatus `avro:"status"`
	StartedAt    time.Time `avro:"started_at"`
	FinishedAt   time.Time `avro:"finished_at"`
	Queries      int       `avro:"queries"`
	Unchanged    int       `avro:"unchanged"`
	Failed       int       `avro:"failed"`
	Deferred     int       `avro:"deferred"`
	Drafts       int       `avro:"drafts"`
	InputTokens  int64     `avro:"input_tokens"`
	OutputTokens int64     `avro:"output_tokens"`
	// Error is why the run failed.
	Error string `avro:"error"`
}

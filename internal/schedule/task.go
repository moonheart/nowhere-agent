// Package schedule implements scheduled tasks (scheduled-tasks capability):
// durable, owner-scoped definitions of recurring agent runs. A task names a
// cron schedule and a prompt source; a trigger (trigger.go) scans for due
// tasks, claims each atomically, and fires it through the same run registry a
// human chat uses.
package schedule

import (
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/robfig/cron/v3"
)

// MultitaskStrategy governs a fire that is due while the target session already
// has an active run (design: concurrency strategy).
type MultitaskStrategy string

const (
	// MultitaskReject skips the fire when the target session is busy (default):
	// unattended runs must not pile up duplicates.
	MultitaskReject MultitaskStrategy = "reject"
	// MultitaskInterrupt cancels the active run and starts the new one.
	MultitaskInterrupt MultitaskStrategy = "interrupt"
	// MultitaskEnqueue waits for the active run to finish before starting.
	MultitaskEnqueue MultitaskStrategy = "enqueue"
)

// OnRunCompleted decides what happens to a freshly-created session once the run
// reaches a terminal state (design: session targeting).
type OnRunCompleted string

const (
	// OnRunKeep retains the session (default): the output is the deliverable.
	OnRunKeep OnRunCompleted = "keep"
	// OnRunDelete removes a freshly-created session after the run, for
	// fire-and-forget maintenance runs whose output is only read via the task.
	OnRunDelete OnRunCompleted = "delete"
)

// Task is one scheduled task definition, mapping the scheduled_task table.
type Task struct {
	ID       string
	UserID   string
	TeamID   string // empty = no team scope
	// Prompt source (design D1). AgentDefName, when set, resolves system prompt
	// and model from the agentdef store at fire time; Prompt is then the kickoff
	// user turn. A standalone Prompt (no AgentDefName) is its own user turn with
	// no system prompt.
	AgentDefName string
	Prompt       string
	// ToolWhitelist is the unattended permission grant (design D3): the run's
	// loop is bound with exactly these tools. Empty = a tool-free run.
	ToolWhitelist []string
	// Cron is a standard 5-field cron expression; Timezone its IANA zone.
	Cron     string
	Timezone string
	// TargetSessionID, when set, makes every fire append to that session;
	// empty means a fresh session per fire (design D2).
	TargetSessionID string
	OnRunCompleted  OnRunCompleted
	Multitask       MultitaskStrategy
	// WebhookURL, when set, receives a POST notification when one of this
	// task's runs reaches a terminal state (run completion → enterprise system).
	// Empty falls back to the global WEBHOOK_URL, and no URL at all disables
	// outbound notifications for the task.
	WebhookURL string
	// EndTime, when non-nil, stops the task from firing after that instant.
	EndTime *time.Time
	Enabled bool
	// NextRunAt is the next fire time; the trigger claims a due task by
	// atomically advancing it (design D4). LastRunAt records the most recent
	// claim (for display and catch-up inspection).
	NextRunAt time.Time
	LastRunAt *time.Time
	// Metadata is open-ended task config (design D7), merged into the run's
	// session metadata at fire time.
	Metadata  map[string]any
	CreatedAt time.Time
	UpdatedAt time.Time
}

// PromptSourceKind distinguishes the two ways a task supplies its prompt.
type PromptSourceKind int

const (
	// SourcePrompt is a standalone free-text prompt (no system prompt).
	SourcePrompt PromptSourceKind = iota
	// SourceAgentDef references an agent definition for system prompt + model.
	SourceAgentDef
)

// PromptSource reports which form the task's prompt takes (design D1).
func (t Task) PromptSource() PromptSourceKind {
	if t.AgentDefName != "" {
		return SourceAgentDef
	}
	return SourcePrompt
}

// ErrInvalid is returned for a task that fails validation.
var ErrInvalid = errors.New("schedule: invalid task")

// cronParser is the standard 5-field parser (no seconds, no descriptors), so a
// task's schedule matches what users know from crontab.
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// Validate checks the fields that must hold before a task is stored: a prompt
// source is present, the cron expression and timezone parse, and the enum
// fields are known. It does not check that the referenced agent definition or
// target session exists — those resolve at fire time.
func (t Task) Validate() error {
	if t.Prompt == "" && t.AgentDefName == "" {
		return fmt.Errorf("%w: a prompt or agent definition is required", ErrInvalid)
	}
	if _, err := t.Schedule(); err != nil {
		return err
	}
	switch t.Multitask {
	case MultitaskReject, MultitaskInterrupt, MultitaskEnqueue:
	default:
		return fmt.Errorf("%w: unknown multitask_strategy %q", ErrInvalid, t.Multitask)
	}
	switch t.OnRunCompleted {
	case OnRunKeep, OnRunDelete:
	default:
		return fmt.Errorf("%w: unknown on_run_completed %q", ErrInvalid, t.OnRunCompleted)
	}
	if t.WebhookURL != "" && !validWebhookURL(t.WebhookURL) {
		return fmt.Errorf("%w: webhook_url must be an absolute http(s) URL", ErrInvalid)
	}
	return nil
}

// validWebhookURL reports whether u is an absolute http(s) URL — the only
// targets the notifier will POST to. Anything else (javascript:, file:, a bare
// hostname) is refused at write time so a task can never point notifications at
// a non-HTTP endpoint.
func validWebhookURL(u string) bool {
	parsed, err := url.Parse(u)
	if err != nil {
		return false
	}
	return (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

// Schedule parses the task's cron expression in its timezone. The returned
// schedule computes fire times in that zone.
func (t Task) Schedule() (cron.Schedule, error) {
	loc, err := t.location()
	if err != nil {
		return nil, err
	}
	spec, err := cronParser.Parse(t.Cron)
	if err != nil {
		return nil, fmt.Errorf("%w: bad cron %q: %v", ErrInvalid, t.Cron, err)
	}
	// cron.SpecSchedule carries the location it should interpret the expression
	// in; wrap so Next() answers in the task's zone.
	return cronSpecIn{spec, loc}, nil
}

// location resolves the task's IANA timezone, defaulting to UTC.
func (t Task) location() (*time.Location, error) {
	tz := t.Timezone
	if tz == "" {
		tz = "UTC"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("%w: bad timezone %q: %v", ErrInvalid, t.Timezone, err)
	}
	return loc, nil
}

// cronSpecIn adapts a parsed cron.Schedule to fire in a specific location:
// it shifts the reference time into the zone, asks the underlying schedule for
// the next fire there, and converts back to an absolute instant.
type cronSpecIn struct {
	sched cron.Schedule
	loc   *time.Location
}

func (c cronSpecIn) Next(t time.Time) time.Time {
	return c.sched.Next(t.In(c.loc))
}

// NextAfter returns the task's next fire time strictly after `from`, in the
// task's timezone. It is used both to seed next_run_at at creation and to
// advance it on each claim. robfig/cron gives up after a five-year search and
// answers time.Time{} for expressions that never match (e.g. "0 0 30 2 *" —
// Feb has no 30th); storing that zero instant would make ListDue match forever
// and burn a real LLM run on every scan, so a zero next is treated as an
// invalid schedule. Create/Update surface it to the user; Claim propagates it
// as a fire failure (a bad row can never be claimed again).
func (t Task) NextAfter(from time.Time) (time.Time, error) {
	s, err := t.Schedule()
	if err != nil {
		return time.Time{}, err
	}
	next := s.Next(from)
	if next.IsZero() {
		return time.Time{}, fmt.Errorf("%w: cron %q has no occurrence within five years", ErrInvalid, t.Cron)
	}
	return next, nil
}

// Due reports whether the task should fire at `now`: enabled, not expired, and
// past its next_run_at. This mirrors the due-scan WHERE clause (design: due
// filter) and is kept here so the scan query and the in-memory store share one
// definition.
func (t Task) Due(now time.Time) bool {
	if !t.Enabled {
		return false
	}
	if t.EndTime != nil && now.After(*t.EndTime) {
		return false
	}
	return !t.NextRunAt.After(now)
}

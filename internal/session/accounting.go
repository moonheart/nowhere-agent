package session

import (
	"time"

	"nowhere-agent/internal/provider"
)

// StepKind classifies run_steps rows. "assistant" intents precede assistant
// provider requests; "tool" intents precede tool execution; "overflow_compact"
// records an overflow recovery attempt (the once-per-input guard).
type StepKind string

const (
	StepAssistant StepKind = "assistant"
	StepTool      StepKind = "tool"
	// StepOverflowCompact records that an overflow recovery compacted the
	// context and retried — the persisted once-per-input guard. The row's
	// attempt field is always 1 (one recovery per conversational input).
	StepOverflowCompact StepKind = "overflow_compact"
)

// RunStep is one durable step intent: written before the effect, carrying the
// pre-provisioned id of the message the effect is expected to produce and the
// durable attempt count for the step. Recovery distinguishes "intent without
// result" (interrupted step) from "result present" (completed step).
type RunStep struct {
	ID              int64
	RunID           string
	Seq             int
	StepKind        StepKind
	Attempt         int
	ResultMessageID *int64 // nil until provisioned (overflow_compact rows carry none)
	ToolCallID      string
	CreatedAt       time.Time
	// ResultExists reports whether a message row with ResultMessageID exists
	// (populated by LatestRunSteps; false for steps without a provisioned id).
	ResultExists bool
}

// UsageCause classifies usage_records rows.
type UsageCause string

const (
	// UsageAssistant is a settled assistant/compaction/branch-summary request.
	UsageAssistant UsageCause = "assistant"
	// UsageTool is usage reported by a finalized tool result (nested LLM work).
	UsageTool UsageCause = "tool"
	// UsageAdjustment is application-supplied accounting (corrections,
	// estimates, reconciliation). Negative values are legal.
	UsageAdjustment UsageCause = "adjustment"
	// UsageOverflow is a DISCARDED response from the recoverable-truncation
	// path: its tokens were consumed (and reported in the live usage frame)
	// but no message ever persisted, so the ledger row is the only durable
	// record. No ResultMessageID (nothing was provisioned into a message).
	UsageOverflow UsageCause = "overflow"
)

// UsageRecord is one durable per-request usage row. Written at settle time,
// before any classification, retry, or discard decision, so spend never
// vanishes with a failed or discarded response. The run's aggregate usage is
// the sum of its records (recomputed, never accumulated).
type UsageRecord struct {
	ID              int64
	RunID           string
	Cause           UsageCause
	ResultMessageID *int64
	Attempt         int
	Usage           provider.Usage
	CreatedAt       time.Time
}

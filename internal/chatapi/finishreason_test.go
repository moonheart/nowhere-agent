package chatapi

import (
	"context"
	"strings"
	"testing"

	"nowhere-agent/internal/agent"
)

// finishReasonOf extracts the finish frame's finishReason from an emitted body.
func finishReasonOf(t *testing.T, body string) string {
	t.Helper()
	for _, reason := range []string{"stop", "length", "content-filter", "tool-calls", "error", "other", "unknown"} {
		if strings.Contains(body, `"finishReason":"`+reason+`"`) {
			return reason
		}
	}
	t.Fatalf("no finishReason found in body\n---\n%s", body)
	return ""
}

// TestFinishReasonDefaultsToStop pins the clean-completion path: a run with no
// error/cancel finishes "stop", which the assistant-ui accumulator maps to a
// "complete" message status.
func TestFinishReasonDefaultsToStop(t *testing.T) {
	e, rec := newTestEmitter()
	e.finish()
	if got := finishReasonOf(t, rec.Body.String()); got != "stop" {
		t.Errorf("finishReason = %q, want stop", got)
	}
}

// TestFinishReasonErrorOnKindError pins G2: a failed run must finish "error"
// (not "stop"), so the client renders the message incomplete rather than as a
// clean completion. The error frame itself must also be present.
func TestFinishReasonErrorOnKindError(t *testing.T) {
	e, rec := newTestEmitter()
	if err := e.Emit(context.Background(), agent.KindError, "boom"); err != nil {
		t.Fatal(err)
	}
	e.finish()
	body := rec.Body.String()
	if !strings.Contains(body, `"type":"error"`) || !strings.Contains(body, `"errorText":"boom"`) {
		t.Errorf("missing error frame\n---\n%s", body)
	}
	if got := finishReasonOf(t, body); got != "error" {
		t.Errorf("finishReason = %q, want error", got)
	}
}

// TestFinishReasonCancelledIsOther pins the cancel mapping: the spec's
// FinishReason union has no "cancelled", so an intentional stop finishes "other"
// (an incomplete-but-not-failure reason), while the cancelled data-run frame
// still carries the precise status for the run panel.
func TestFinishReasonCancelledIsOther(t *testing.T) {
	e, rec := newTestEmitter()
	if err := e.Emit(context.Background(), agent.KindCancelled, nil); err != nil {
		t.Fatal(err)
	}
	e.finish()
	body := rec.Body.String()
	if got := finishReasonOf(t, body); got != "other" {
		t.Errorf("finishReason = %q, want other", got)
	}
	if !strings.Contains(body, `"status":"cancelled"`) {
		t.Errorf("missing cancelled data-run frame\n---\n%s", body)
	}
}

// TestFinishReasonLengthOnTruncatedFinalStep pins the truncation case: when the
// final step hit max_tokens without continuing (a non-continued "length"
// finish-step), the terminal KindError that follows must be classified "length",
// not a generic "error" — the answer was cut off at the token limit.
func TestFinishReasonLengthOnTruncatedFinalStep(t *testing.T) {
	e, rec := newTestEmitter()
	ctx := context.Background()
	if err := e.Emit(ctx, agent.KindStepFinish, agent.StepEvent{FinishReason: "length", IsContinued: false}); err != nil {
		t.Fatal(err)
	}
	if err := e.Emit(ctx, agent.KindError, "response truncated: hit the max_tokens limit"); err != nil {
		t.Fatal(err)
	}
	e.finish()
	if got := finishReasonOf(t, rec.Body.String()); got != "length" {
		t.Errorf("finishReason = %q, want length", got)
	}
}

// TestFinishReasonContinuedLengthStepDoesNotLatch pins that a *continued*
// max_tokens truncation (the re-issuable tool-batch case, where the loop
// continues) does NOT poison the terminal reason: the run recovers and finishes
// "stop".
func TestFinishReasonContinuedLengthStepDoesNotLatch(t *testing.T) {
	e, rec := newTestEmitter()
	ctx := context.Background()
	if err := e.Emit(ctx, agent.KindStepFinish, agent.StepEvent{FinishReason: "length", IsContinued: true}); err != nil {
		t.Fatal(err)
	}
	// Run recovers: a later step finishes stop, then the run ends cleanly.
	if err := e.Emit(ctx, agent.KindStepFinish, agent.StepEvent{FinishReason: "stop", IsContinued: false}); err != nil {
		t.Fatal(err)
	}
	e.finish()
	if got := finishReasonOf(t, rec.Body.String()); got != "stop" {
		t.Errorf("finishReason = %q, want stop", got)
	}
}

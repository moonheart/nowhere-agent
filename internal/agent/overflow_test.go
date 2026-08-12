package agent

import (
	"context"
	"testing"

	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/toolruntime"
)

// TestIsRecoverableLength pins the classification (change durable-run-accounting,
// overflow recovery): a length stop below the intended cap is recoverable, a
// cap fully used is genuine, and unclassifiable inputs (no cap, no usage)
// defer to the legacy error behavior.
func TestIsRecoverableLength(t *testing.T) {
	cases := []struct {
		name   string
		usage  *provider.Usage
		cap    int
		want   bool
	}{
		{name: "output below cap is recoverable", usage: &provider.Usage{OutputTokens: 16}, cap: 128_000, want: true},
		{name: "zero output against large cap is recoverable", usage: &provider.Usage{OutputTokens: 0}, cap: 128_000, want: true},
		{name: "cap fully used is genuine", usage: &provider.Usage{OutputTokens: 1024}, cap: 1024, want: false},
		{name: "output above cap is genuine", usage: &provider.Usage{OutputTokens: 2048}, cap: 1024, want: false},
		{name: "no cap configured is unclassifiable", usage: &provider.Usage{OutputTokens: 10}, cap: 0, want: false},
		{name: "no reported usage is unclassifiable", usage: nil, cap: 1024, want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsRecoverableLength(c.usage, c.cap); got != c.want {
				t.Errorf("IsRecoverableLength(%+v, %d) = %v, want %v", c.usage, c.cap, got, c.want)
			}
		})
	}
}

// lengthRecoverableResponse is a text turn stopped at max_tokens with usage
// far below the configured cap — the recoverable shape.
func lengthRecoverableResponse(text string) []provider.Event {
	return []provider.Event{
		{Type: provider.EventMessageStart},
		{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockText}},
		{Type: provider.EventBlockDelta, Index: 0, Delta: text},
		{Type: provider.EventBlockStop, Index: 0},
		{Type: provider.EventMessageStop, StopReason: provider.StopMaxTokens,
			Usage: &provider.Usage{InputTokens: 50, OutputTokens: 10}},
	}
}

// TestLoopRecoverableLengthDiscardsAndRetries verifies the recovery path: the
// recoverable response is discarded (never enters Produced), the recovery
// guard is emitted, the oldest round is dropped, and the retried request
// succeeds. The history carries two rounds so the drop has something to take.
func TestLoopRecoverableLengthDiscardsAndRetries(t *testing.T) {
	p := &scriptProvider{script: [][]provider.Event{
		lengthRecoverableResponse("partial"),
		textResponse("complete"),
	}}
	emit := &memEmitter{}
	loop := New(p, toolruntime.NewRegistry(), Config{Model: "m", MaxTokens: 100})

	history := []provider.Message{
		provider.TextMessage(provider.RoleUser, "first"),
		provider.TextMessage(provider.RoleUser, "second"),
	}
	produced, err := loop.Run(context.Background(), history, emit)
	if err != nil {
		t.Fatalf("run after recovery: %v", err)
	}
	if emit.count(KindOverflowRecovery) != 1 {
		t.Errorf("KindOverflowRecovery = %d, want 1", emit.count(KindOverflowRecovery))
	}
	if emit.count(KindDone) != 1 {
		t.Errorf("KindDone = %d, want 1", emit.count(KindDone))
	}
	if emit.count(KindError) != 0 {
		t.Errorf("KindError = %d, want 0", emit.count(KindError))
	}
	// The discarded response never entered the run's produced messages.
	for _, m := range produced {
		if len(m.Content) == 1 && m.Content[0].Text == "partial" {
			t.Error("the discarded recoverable response was persisted")
		}
	}
	last := produced[len(produced)-1]
	if len(last.Content) != 1 || last.Content[0].Text != "complete" {
		t.Errorf("last produced = %+v, want the retried answer", last)
	}
}

// TestLoopSecondRecoverableLengthFails pins the once-per-input guard: a second
// recoverable response in the same window fails the run instead of compacting
// again, with neutral truncation wording.
func TestLoopSecondRecoverableLengthFails(t *testing.T) {
	p := &scriptProvider{script: [][]provider.Event{
		lengthRecoverableResponse("partial one"),
		lengthRecoverableResponse("partial two"),
	}}
	emit := &memEmitter{}
	loop := New(p, toolruntime.NewRegistry(), Config{Model: "m", MaxTokens: 100})

	history := []provider.Message{
		provider.TextMessage(provider.RoleUser, "first"),
		provider.TextMessage(provider.RoleUser, "second"),
	}
	produced, err := loop.Run(context.Background(), history, emit)
	if err == nil {
		t.Fatal("second recoverable response must fail the run")
	}
	if err.Error() != "response was truncated before completion" {
		t.Errorf("error = %q, want neutral truncation wording", err.Error())
	}
	if emit.count(KindOverflowRecovery) != 1 {
		t.Errorf("KindOverflowRecovery = %d, want 1 (guard used once)", emit.count(KindOverflowRecovery))
	}
	if emit.count(KindError) != 1 {
		t.Errorf("KindError = %d, want 1", emit.count(KindError))
	}
	if emit.count(KindDone) != 0 {
		t.Errorf("KindDone = %d, want 0", emit.count(KindDone))
	}
	// Both recoverable responses are discarded; nothing of them is produced.
	for _, m := range produced {
		if len(m.Content) == 1 && m.Content[0].Text == "partial one" {
			t.Error("a discarded recoverable response was persisted")
		}
	}
}

// TestLoopGenuineLengthCompletes verifies a max_tokens stop that fully used the
// intended cap completes normally (no compaction can help, the answer is as
// complete as configured).
func TestLoopGenuineLengthCompletes(t *testing.T) {
	p := &scriptProvider{script: [][]provider.Event{{
		{Type: provider.EventMessageStart},
		{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockText}},
		{Type: provider.EventBlockDelta, Index: 0, Delta: "full"},
		{Type: provider.EventBlockStop, Index: 0},
		{Type: provider.EventMessageStop, StopReason: provider.StopMaxTokens,
			Usage: &provider.Usage{InputTokens: 10, OutputTokens: 100}},
	}}}
	emit := &memEmitter{}
	loop := New(p, toolruntime.NewRegistry(), Config{Model: "m", MaxTokens: 100})

	produced, err := loop.Run(context.Background(), nil, emit)
	if err != nil {
		t.Fatalf("genuine output-limit stop must complete normally: %v", err)
	}
	if emit.count(KindDone) != 1 {
		t.Errorf("KindDone = %d, want 1", emit.count(KindDone))
	}
	if emit.count(KindOverflowRecovery) != 0 {
		t.Errorf("KindOverflowRecovery = %d, want 0", emit.count(KindOverflowRecovery))
	}
	if len(produced) != 1 || produced[0].Content[0].Text != "full" {
		t.Errorf("produced = %+v, want the cap-limited answer", produced)
	}
}

// TestLoopRecoverableLengthWithNothingToDrop fails cleanly when the working
// view has nothing safe to drop: without compaction the request cannot fit, so
// the run fails and no recovery guard is recorded (no recovery happened).
func TestLoopRecoverableLengthWithNothingToDrop(t *testing.T) {
	p := &scriptProvider{script: [][]provider.Event{lengthRecoverableResponse("partial")}}
	emit := &memEmitter{}
	loop := New(p, toolruntime.NewRegistry(), Config{Model: "m", MaxTokens: 100})

	produced, err := loop.Run(context.Background(), nil, emit)
	if err == nil {
		t.Fatal("recoverable truncation with nothing to drop must fail the run")
	}
	if emit.count(KindOverflowRecovery) != 0 {
		t.Errorf("KindOverflowRecovery = %d, want 0 (no recovery happened)", emit.count(KindOverflowRecovery))
	}
	if len(produced) != 0 {
		t.Errorf("produced = %+v, want empty", produced)
	}
}

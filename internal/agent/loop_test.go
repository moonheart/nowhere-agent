package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/toolruntime"
)

// scriptProvider emits canned event sequences, one per Stream call.
type scriptProvider struct {
	mu       sync.Mutex
	script   [][]provider.Event
	calls    int
	requests []provider.Request
}

func (p *scriptProvider) Name() string { return "script" }

func (p *scriptProvider) Stream(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.calls >= len(p.script) {
		return nil, errors.New("no more scripted responses")
	}
	p.requests = append(p.requests, req)
	evs := p.script[p.calls]
	p.calls++
	ch := make(chan provider.Event, len(evs))
	for _, e := range evs {
		ch <- e
	}
	close(ch)
	return ch, nil
}

// textResponse builds a script entry producing a single text block.
func textResponse(text string) []provider.Event {
	return []provider.Event{
		{Type: provider.EventMessageStart},
		{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockText}},
		{Type: provider.EventBlockDelta, Index: 0, Delta: text},
		{Type: provider.EventBlockStop, Index: 0},
		{Type: provider.EventMessageStop, StopReason: provider.StopEndTurn, Usage: &provider.Usage{InputTokens: 1, OutputTokens: 1}},
	}
}

// toolUseResponse builds a script entry that requests a tool call.
func toolUseResponse(id, name, jsonArgs string) []provider.Event {
	return []provider.Event{
		{Type: provider.EventMessageStart},
		{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockToolUse, ToolUseID: id, ToolName: name, ToolInput: map[string]any{}}},
		{Type: provider.EventBlockDelta, Index: 0, Delta: jsonArgs},
		{Type: provider.EventBlockStop, Index: 0},
		{Type: provider.EventMessageStop, StopReason: provider.StopToolUse},
	}
}

// memEmitter collects emitted events.
type memEmitter struct {
	mu     sync.Mutex
	events []EventKind
}

func (m *memEmitter) Emit(_ context.Context, kind EventKind, _ any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, kind)
	return nil
}

func (m *memEmitter) count(kind EventKind) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, k := range m.events {
		if k == kind {
			n++
		}
	}
	return n
}

type echoTool struct{}

func (echoTool) Name() string           { return "echo" }
func (echoTool) Description() string    { return "echoes input" }
func (echoTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (echoTool) Risk() toolruntime.Risk { return toolruntime.RiskReadOnly }
func (echoTool) Timeout() time.Duration { return time.Second }
func (echoTool) Call(_ context.Context, args map[string]any) (toolruntime.Result, error) {
	return toolruntime.Result{Content: "echo-result"}, nil
}

func TestLoopSingleTurn(t *testing.T) {
	p := &scriptProvider{script: [][]provider.Event{textResponse("hello world")}}
	reg := toolruntime.NewRegistry()
	emit := &memEmitter{}
	loop := New(p, reg, Config{Model: "m", MaxTokens: 100})

	produced, err := loop.Run(context.Background(), []provider.Message{provider.TextMessage(provider.RoleUser, "hi")}, emit)
	if err != nil {
		t.Fatal(err)
	}
	if len(produced) != 1 {
		t.Fatalf("expected 1 assistant message, got %d", len(produced))
	}
	if produced[0].Content[0].Text != "hello world" {
		t.Errorf("text = %q", produced[0].Content[0].Text)
	}
	if emit.count(KindDone) != 1 {
		t.Error("expected done event")
	}
	if emit.count(KindText) == 0 {
		t.Error("expected streamed text events")
	}
}

func TestLoopToolRoundTrip(t *testing.T) {
	p := &scriptProvider{script: [][]provider.Event{
		toolUseResponse("tu1", "echo", `{"x":1}`),
		textResponse("final answer"),
	}}
	reg := toolruntime.NewRegistry()
	reg.Register(echoTool{})
	emit := &memEmitter{}
	loop := New(p, reg, Config{Model: "m", MaxTokens: 100})

	produced, err := loop.Run(context.Background(), []provider.Message{provider.TextMessage(provider.RoleUser, "hi")}, emit)
	if err != nil {
		t.Fatal(err)
	}
	// assistant(tool_use) + user(tool_result) + assistant(final)
	if len(produced) != 3 {
		t.Fatalf("expected 3 messages, got %d: %+v", len(produced), produced)
	}
	if produced[0].Content[0].Type != provider.BlockToolUse {
		t.Errorf("msg0 type = %q", produced[0].Content[0].Type)
	}
	if produced[1].Role != provider.RoleUser || produced[1].Content[0].Type != provider.BlockToolResult {
		t.Errorf("msg1 should be tool_result, got %+v", produced[1])
	}
	if produced[1].Content[0].ToolContent != "echo-result" {
		t.Errorf("tool result content = %q", produced[1].Content[0].ToolContent)
	}
	if produced[2].Content[0].Text != "final answer" {
		t.Errorf("final = %q", produced[2].Content[0].Text)
	}
	if emit.count(KindToolUse) != 1 || emit.count(KindToolResult) != 1 {
		t.Errorf("tool events = use %d result %d", emit.count(KindToolUse), emit.count(KindToolResult))
	}
	if p.calls != 2 {
		t.Errorf("provider called %d times want 2", p.calls)
	}
}

// argsCapture records the delta payloads of KindToolArgs events in order.
type argsCapture struct {
	memEmitter
	mu     sync.Mutex
	deltas []string
}

func (a *argsCapture) Emit(ctx context.Context, kind EventKind, payload any) error {
	if kind == KindToolArgs {
		if m, ok := payload.(map[string]any); ok {
			d, _ := m["delta"].(string)
			a.mu.Lock()
			a.deltas = append(a.deltas, d)
			a.mu.Unlock()
		}
	}
	return a.memEmitter.Emit(ctx, kind, payload)
}

// TestLoopStreamsToolArgs pins incremental argument streaming: a tool_use block
// must surface as a sequence of KindToolArgs events — an opening marker (empty
// delta) at block start, then one event per argument fragment as the model
// streams it — rather than appearing whole only at block stop. The concatenated
// deltas must reconstruct the full argument JSON.
func TestLoopStreamsToolArgs(t *testing.T) {
	fragments := []string{`{"content":"`, `# big file`, ` ...`, `"}`}
	evs := []provider.Event{{Type: provider.EventMessageStart},
		{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockToolUse, ToolUseID: "tu1", ToolName: "echo", ToolInput: map[string]any{}}}}
	for _, f := range fragments {
		evs = append(evs, provider.Event{Type: provider.EventBlockDelta, Index: 0, Delta: f})
	}
	evs = append(evs,
		provider.Event{Type: provider.EventBlockStop, Index: 0},
		provider.Event{Type: provider.EventMessageStop})

	p := &scriptProvider{script: [][]provider.Event{evs, textResponse("done")}}
	reg := toolruntime.NewRegistry()
	reg.Register(echoTool{})
	emit := &argsCapture{}
	loop := New(p, reg, Config{Model: "m", MaxTokens: 100})

	if _, err := loop.Run(context.Background(), []provider.Message{provider.TextMessage(provider.RoleUser, "hi")}, emit); err != nil {
		t.Fatal(err)
	}

	// 1 opening marker ("") + one event per fragment.
	want := append([]string{""}, fragments...)
	if len(emit.deltas) != len(want) {
		t.Fatalf("KindToolArgs deltas = %v, want %v", emit.deltas, want)
	}
	var joined string
	for i, d := range emit.deltas {
		if d != want[i] {
			t.Errorf("delta[%d] = %q, want %q", i, d, want[i])
		}
		joined += d
	}
	if joined != `{"content":"# big file ..."}` {
		t.Errorf("joined deltas = %q", joined)
	}
}

func TestLoopMaxIterationsGuard(t *testing.T) {
	// Provider always requests a tool → loop must stop at the guard.
	p := &scriptProvider{script: [][]provider.Event{
		toolUseResponse("tu1", "echo", `{}`),
		toolUseResponse("tu2", "echo", `{}`),
		toolUseResponse("tu3", "echo", `{}`),
	}}
	reg := toolruntime.NewRegistry()
	reg.Register(echoTool{})
	loop := New(p, reg, Config{Model: "m", MaxTokens: 100, MaxIterations: 2})

	_, err := loop.Run(context.Background(), nil, &memEmitter{})
	if err == nil {
		t.Fatal("expected max-iteration error")
	}
}

// TestLoopMaxIterationsEmitsError pins O4: hitting the iteration guard must emit
// a terminal KindError, not stop silently. Before the fix the loop returned an
// error with no emit, so the run flipped to failed with no explanatory frame and
// the client saw the stream simply end.
func TestLoopMaxIterationsEmitsError(t *testing.T) {
	p := &scriptProvider{script: [][]provider.Event{
		toolUseResponse("tu1", "echo", `{}`),
		toolUseResponse("tu2", "echo", `{}`),
		toolUseResponse("tu3", "echo", `{}`),
	}}
	reg := toolruntime.NewRegistry()
	reg.Register(echoTool{})
	emit := &memEmitter{}
	loop := New(p, reg, Config{Model: "m", MaxTokens: 100, MaxIterations: 2})

	if _, err := loop.Run(context.Background(), nil, emit); err == nil {
		t.Fatal("expected max-iteration error")
	}
	if emit.count(KindError) != 1 {
		t.Errorf("KindError = %d, want 1 (terminal frame at the guard)", emit.count(KindError))
	}
	if emit.count(KindDone) != 0 {
		t.Error("guard exit must not emit done")
	}
}

// truncatedResponse builds a text turn whose message stop signals a max_tokens
// truncation and carries no tool call — a cut-off final answer.
func truncatedResponse(text string) []provider.Event {
	return []provider.Event{
		{Type: provider.EventMessageStart},
		{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockText}},
		{Type: provider.EventBlockDelta, Index: 0, Delta: text},
		{Type: provider.EventBlockStop, Index: 0},
		{Type: provider.EventMessageStop, StopReason: provider.StopMaxTokens},
	}
}

// TestLoopTruncationSurfacedAsError pins L1: a turn truncated at max_tokens with
// no tool calls is a cut-off answer, so the loop surfaces it as an error, not a
// clean done. Before the fix the loop inferred completion from len(calls)==0 and
// a truncated reply looked like success.
func TestLoopTruncationSurfacedAsError(t *testing.T) {
	p := &scriptProvider{script: [][]provider.Event{truncatedResponse("partial ans")}}
	emit := &memEmitter{}
	loop := New(p, toolruntime.NewRegistry(), Config{Model: "m", MaxTokens: 100})

	produced, err := loop.Run(context.Background(), nil, emit)
	if err == nil {
		t.Fatal("truncation must surface as an error, not a clean finish")
	}
	if emit.count(KindError) != 1 {
		t.Errorf("KindError = %d, want 1", emit.count(KindError))
	}
	if emit.count(KindDone) != 0 {
		t.Error("a truncated turn must NOT emit done")
	}
	// The partial text is still assembled (streamed before the stop), so it is
	// persisted/visible even though the run is marked failed.
	if len(produced) != 1 || produced[0].Content[0].Text != "partial ans" {
		t.Errorf("partial content not preserved: %+v", produced)
	}
}

// thinkingWithSignatureResponse builds a script entry with a thinking block whose
// signature streams as a separate SignatureDelta, then a text block.
func thinkingWithSignatureResponse(think, sig, text string) []provider.Event {
	return []provider.Event{
		{Type: provider.EventMessageStart},
		{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockThinking}},
		{Type: provider.EventBlockDelta, Index: 0, Delta: think},
		{Type: provider.EventBlockDelta, Index: 0, SignatureDelta: sig},
		{Type: provider.EventBlockStop, Index: 0},
		{Type: provider.EventBlockStart, Index: 1, Block: &provider.Block{Type: provider.BlockText}},
		{Type: provider.EventBlockDelta, Index: 1, Delta: text},
		{Type: provider.EventBlockStop, Index: 1},
		{Type: provider.EventMessageStop, StopReason: provider.StopEndTurn},
	}
}

// TestLoopThinkingSignatureCaptured verifies the signature lands on
// ThinkingSignature and is NOT concatenated into the thinking text.
func TestLoopThinkingSignatureCaptured(t *testing.T) {
	p := &scriptProvider{script: [][]provider.Event{
		thinkingWithSignatureResponse("let me think", "sig-xyz", "answer"),
	}}
	loop := New(p, toolruntime.NewRegistry(), Config{Model: "m", MaxTokens: 100})

	produced, err := loop.Run(context.Background(), nil, &memEmitter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(produced) != 1 || len(produced[0].Content) != 2 {
		t.Fatalf("expected 1 msg with 2 blocks, got %+v", produced)
	}
	th := produced[0].Content[0]
	if th.Type != provider.BlockThinking {
		t.Fatalf("block0 type = %q", th.Type)
	}
	if th.Thinking != "let me think" {
		t.Errorf("Thinking = %q, want pure text (no signature)", th.Thinking)
	}
	if th.ThinkingSignature != "sig-xyz" {
		t.Errorf("ThinkingSignature = %q, want sig-xyz", th.ThinkingSignature)
	}
}

func TestLoopToolInputParsed(t *testing.T) {
	p := &scriptProvider{script: [][]provider.Event{
		toolUseResponse("tu1", "echo", `{"path":"/tmp/x"}`),
		textResponse("done"),
	}}
	reg := toolruntime.NewRegistry()
	reg.Register(echoTool{})
	loop := New(p, reg, Config{Model: "m", MaxTokens: 100})

	produced, err := loop.Run(context.Background(), nil, &memEmitter{})
	if err != nil {
		t.Fatal(err)
	}
	toolUse := produced[0].Content[0]
	if toolUse.ToolInput["path"] != "/tmp/x" {
		t.Errorf("tool input = %+v", toolUse.ToolInput)
	}
}

// cancelOnMessageEmitter cancels the run ctx the moment the assistant message
// (with its tool_use batch) is emitted — landing the cancellation exactly in
// the window between message persistence and batch dispatch.
type cancelOnMessageEmitter struct {
	memEmitter
	cancel context.CancelFunc
}

func (e *cancelOnMessageEmitter) Emit(ctx context.Context, kind EventKind, payload any) error {
	if kind == KindMessage {
		e.cancel()
	}
	return e.memEmitter.Emit(ctx, kind, payload)
}

func TestLoopCancelBetweenMessageAndBatch(t *testing.T) {
	// Cancel lands after the assistant tool_use message is produced but before
	// the tool batch starts. The batch must still be ANSWERED with synthetic
	// cancelled results — the assistant message is already durable-bound, and
	// leaving its tool_use unpaired would force every later send to paper over
	// the gap with the transient EnsurePairing repair.
	p := &scriptProvider{script: [][]provider.Event{toolUseResponse("tu1", "echo", `{}`)}}
	reg := toolruntime.NewRegistry()
	reg.Register(echoTool{})
	ctx, cancel := context.WithCancel(context.Background())
	emit := &cancelOnMessageEmitter{cancel: cancel}
	loop := New(p, reg, Config{Model: "m", MaxTokens: 100})

	produced, err := loop.Run(ctx, nil, emit)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v want context.Canceled", err)
	}
	if emit.count(KindCancelled) != 1 {
		t.Errorf("expected 1 cancelled event, got %d", emit.count(KindCancelled))
	}
	if len(produced) != 2 {
		t.Fatalf("produced %d messages, want [assistant tool_use, tool_result]", len(produced))
	}
	res := produced[1]
	if len(res.Content) != 1 || res.Content[0].Type != provider.BlockToolResult || res.Content[0].ToolResultID != "tu1" {
		t.Fatalf("tool_result = %+v, want one block answering tu1", res.Content)
	}
	if !res.Content[0].IsError || !strings.Contains(res.Content[0].ToolContent, "cancelled") {
		t.Errorf("result = %+v, want an is_error 'cancelled' result (the call never ran)", res.Content[0])
	}
	if strings.Contains(res.Content[0].ToolContent, "echo-result") {
		t.Error("the cancelled batch must not have executed")
	}
}

func TestLoopCancelOnFinalAnswerReportsCancelled(t *testing.T) {
	// Cancel lands after the final assistant message (no tool calls) is
	// persisted but before the terminal done frame. The run must report
	// cancelled — symmetric with the pre-dispatch cancel guard — not done.
	p := &scriptProvider{script: [][]provider.Event{textResponse("final answer")}}
	reg := toolruntime.NewRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	emit := &cancelOnMessageEmitter{cancel: cancel}
	loop := New(p, reg, Config{Model: "m", MaxTokens: 100})

	produced, err := loop.Run(ctx, nil, emit)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v want context.Canceled", err)
	}
	if emit.count(KindCancelled) != 1 {
		t.Errorf("expected 1 cancelled event, got %d", emit.count(KindCancelled))
	}
	if emit.count(KindDone) != 0 {
		t.Error("a run cancelled before its terminal frame must not emit done")
	}
	if len(produced) != 1 || produced[0].Content[0].Text != "final answer" {
		t.Errorf("produced = %+v, want the completed assistant message (it is durable)", produced)
	}
}

// cancelOnMessageUsageEmitter cancels the run ctx when the assistant message
// is emitted, and captures the terminal KindUsage payload.
type cancelOnMessageUsageEmitter struct {
	usageEmitter
	cancel context.CancelFunc
}

func (e *cancelOnMessageUsageEmitter) Emit(ctx context.Context, kind EventKind, payload any) error {
	if kind == KindMessage {
		e.cancel()
	}
	return e.usageEmitter.Emit(ctx, kind, payload)
}

func TestLoopCancelBetweenMessageAndBatchReportsUsage(t *testing.T) {
	// A cancelled run still consumed tokens: the usage accumulated up to the
	// cancellation must be reported (KindUsage → the runs-row usage record),
	// not silently dropped with the run.
	p := &scriptProvider{script: [][]provider.Event{toolUseResponseUsage("tu1", "echo", `{}`, 12, 5)}}
	reg := toolruntime.NewRegistry()
	reg.Register(echoTool{})
	ctx, cancel := context.WithCancel(context.Background())
	emit := &cancelOnMessageUsageEmitter{cancel: cancel}
	loop := New(p, reg, Config{Model: "m", MaxTokens: 100})

	_, err := loop.Run(ctx, nil, emit)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v want context.Canceled", err)
	}
	if emit.count(KindUsage) != 1 {
		t.Fatalf("KindUsage count = %d, want 1 (cancelled runs still report usage)", emit.count(KindUsage))
	}
	if emit.usage == nil || emit.usage.InputTokens != 12 || emit.usage.OutputTokens != 5 {
		t.Errorf("usage = %+v, want {12 5}", emit.usage)
	}
}

// guardedEmitter mimics the registry emitter's write path: an emit on a
// cancelled ctx is dropped (its ctx.Err() guard), so a persistence emit must
// arrive on a live ctx to land.
type guardedEmitter struct {
	memEmitter
}

func (e *guardedEmitter) Emit(ctx context.Context, kind EventKind, payload any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return e.memEmitter.Emit(ctx, kind, payload)
}

// cancelBeforeAssistantEmitter cancels the run ctx the instant the assistant
// KindMessage arrives — BEFORE the guarded write path checks the ctx —
// simulating a cancel that races into the window between the provider stream
// ending and the assistant message being persisted.
type cancelBeforeAssistantEmitter struct {
	guardedEmitter
	cancel context.CancelFunc
}

func (e *cancelBeforeAssistantEmitter) Emit(ctx context.Context, kind EventKind, payload any) error {
	if kind == KindMessage {
		if _, ok := payload.(MessageWithUsage); ok {
			e.cancel()
		}
	}
	return e.guardedEmitter.Emit(ctx, kind, payload)
}

func TestLoopCancelRacingAssistantPersistKeepsPairing(t *testing.T) {
	// Regression: the assistant KindMessage used to go out on the live ctx, so
	// a cancel landing in this window dropped it while the batch's synthetic
	// cancelled tool_results (detached ctx) still persisted — leaving orphaned
	// tool_results in the durable record. Both must now land.
	p := &scriptProvider{script: [][]provider.Event{toolUseResponse("tu1", "echo", `{}`)}}
	reg := toolruntime.NewRegistry()
	reg.Register(echoTool{})
	ctx, cancel := context.WithCancel(context.Background())
	emit := &cancelBeforeAssistantEmitter{cancel: cancel}
	loop := New(p, reg, Config{Model: "m", MaxTokens: 100})

	produced, err := loop.Run(ctx, nil, emit)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v want context.Canceled", err)
	}
	if emit.count(KindMessage) != 2 {
		t.Fatalf("persisted messages = %d, want 2 (assistant tool_use + tool_result); a dropped assistant message orphans the tool_result", emit.count(KindMessage))
	}
	if len(produced) != 2 {
		t.Fatalf("produced %d messages, want [assistant tool_use, tool_result]", len(produced))
	}
}

// mutateViewMW rewrites a block in place on the per-attempt view, the way
// image materialization fills ImageData.
type mutateViewMW struct{}

func (mutateViewMW) MiddlewareName() string { return "mutate-view" }

func (mutateViewMW) WrapModelCall(ctx context.Context, c *ModelCall, next ModelHandler) (ModelResult, error) {
	if len(c.View) > 0 && len(c.View[0].Content) > 0 {
		c.View[0].Content[0].Text = "MUTATED"
	}
	return next(ctx, c)
}

func TestAttemptViewCopyShieldsDurableHistory(t *testing.T) {
	// The per-attempt view is a block-granularity copy: middleware mutating a
	// block in place must not reach the caller's (durable) history, whose
	// messages otherwise share Content slices with the view.
	p := &scriptProvider{script: [][]provider.Event{textResponse("hi")}}
	loop := New(p, toolruntime.NewRegistry(), Config{Model: "m", MaxTokens: 100})
	loop.Use(mutateViewMW{})

	history := []provider.Message{{Role: provider.RoleUser, Content: []provider.Block{{Type: provider.BlockText, Text: "original"}}}}
	if _, err := loop.Run(context.Background(), history, &memEmitter{}); err != nil {
		t.Fatal(err)
	}
	if history[0].Content[0].Text != "original" {
		t.Errorf("durable history mutated by middleware: got %q, want %q", history[0].Content[0].Text, "original")
	}
}

func TestLoopUnknownStopReasonFailsRun(t *testing.T) {
	// A no-tool-calls turn whose stream closes without a finish reason is
	// indistinguishable from a truncation the adapter failed to flag: the run
	// must fail loudly, not complete silently (which would defeat the
	// max_tokens truncation guard).
	p := &scriptProvider{script: [][]provider.Event{{
		{Type: provider.EventMessageStart},
		{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockText}},
		{Type: provider.EventBlockDelta, Index: 0, Delta: "maybe truncated"},
		{Type: provider.EventBlockStop, Index: 0},
		{Type: provider.EventMessageStop}, // no StopReason reported
	}}}
	emit := &memEmitter{}
	loop := New(p, toolruntime.NewRegistry(), Config{Model: "m", MaxTokens: 100})

	_, err := loop.Run(context.Background(), nil, emit)
	if err == nil || !strings.Contains(err.Error(), "finish reason") {
		t.Fatalf("err = %v, want an unreported-finish-reason error", err)
	}
	if emit.count(KindDone) != 0 {
		t.Error("an unreported stop reason must not complete the run")
	}
	if emit.count(KindError) != 1 {
		t.Errorf("expected 1 error event, got %d", emit.count(KindError))
	}
}

func TestLoopEarlyStopReasonFailsRun(t *testing.T) {
	// A no-tool-calls turn that ends on any reason other than end_turn —
	// stop_sequence here; content_filter and provider passthroughs behave the
	// same — ended before the model chose to stop. Completing the run would
	// pass a cut-off answer off as done: the same silent-truncation class the
	// max_tokens and unreported-reason guards catch.
	p := &scriptProvider{script: [][]provider.Event{{
		{Type: provider.EventMessageStart},
		{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockText}},
		{Type: provider.EventBlockDelta, Index: 0, Delta: "cut off at the stop token"},
		{Type: provider.EventBlockStop, Index: 0},
		{Type: provider.EventMessageStop, StopReason: provider.StopStopSequence},
	}}}
	emit := &memEmitter{}
	loop := New(p, toolruntime.NewRegistry(), Config{Model: "m", MaxTokens: 100})

	_, err := loop.Run(context.Background(), nil, emit)
	if err == nil || !strings.Contains(err.Error(), "stop_sequence") {
		t.Fatalf("err = %v, want an early-end error naming the stop reason", err)
	}
	if emit.count(KindDone) != 0 {
		t.Error("an early-end stop reason must not complete the run")
	}
	if emit.count(KindError) != 1 {
		t.Errorf("expected 1 error event, got %d", emit.count(KindError))
	}
}

func TestLoopEmptyResponseFailsRun(t *testing.T) {
	// A turn that reports end_turn but assembled zero content blocks and no
	// tool calls carries nothing. Persisting it would write an assistant
	// message with empty content, which OpenAI-compatible gateways reject with
	// a 400 on the next send — the EnsurePairing repair patches the send view,
	// but the empty row stays durable. Fail loudly instead.
	p := &scriptProvider{script: [][]provider.Event{{
		{Type: provider.EventMessageStart},
		{Type: provider.EventMessageStop, StopReason: provider.StopEndTurn},
	}}}
	emit := &memEmitter{}
	loop := New(p, toolruntime.NewRegistry(), Config{Model: "m", MaxTokens: 100})

	produced, err := loop.Run(context.Background(), nil, emit)
	if err == nil || !strings.Contains(err.Error(), "empty response") {
		t.Fatalf("err = %v, want an empty-response error", err)
	}
	if len(produced) != 0 {
		t.Errorf("produced %d messages, want 0 — an empty assistant message must not be recorded", len(produced))
	}
	if emit.count(KindMessage) != 0 {
		t.Errorf("KindMessage = %d, want 0 — an empty assistant message must not be persisted", emit.count(KindMessage))
	}
	if emit.count(KindDone) != 0 {
		t.Error("an empty response must not complete the run")
	}
}

func TestLoopEmptyTextBlockFailsRun(t *testing.T) {
	// A turn that reports end_turn with a single text block that streamed ZERO
	// deltas is the empty-response case wearing a 1-block disguise: the block
	// assembles to Text:"" — content the provider rejects with a 400 on the
	// next send. The empty-response guard must still fire (dropEmptyBlocks
	// strips the hollow block before the 0-length check).
	p := &scriptProvider{script: [][]provider.Event{{
		{Type: provider.EventMessageStart},
		{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockText}},
		{Type: provider.EventBlockStop, Index: 0},
		{Type: provider.EventMessageStop, StopReason: provider.StopEndTurn},
	}}}
	emit := &memEmitter{}
	loop := New(p, toolruntime.NewRegistry(), Config{Model: "m", MaxTokens: 100})

	produced, err := loop.Run(context.Background(), nil, emit)
	if err == nil || !strings.Contains(err.Error(), "empty response") {
		t.Fatalf("err = %v, want an empty-response error", err)
	}
	if len(produced) != 0 {
		t.Errorf("produced %d messages, want 0 — a hollow assistant message must not be recorded", len(produced))
	}
	if emit.count(KindMessage) != 0 {
		t.Errorf("KindMessage = %d, want 0 — a hollow assistant message must not be persisted", emit.count(KindMessage))
	}
	if emit.count(KindDone) != 0 {
		t.Error("an empty-text-block response must not complete the run")
	}
}

func TestDropEmptyBlocks(t *testing.T) {
	blocks := []provider.Block{
		{Type: provider.BlockText, Text: ""},                                    // hollow: drop
		{Type: provider.BlockThinking},                                          // hollow: drop
		{Type: provider.BlockThinking, ThinkingSignature: "sig"},                // signed: keep (round-trip)
		{Type: provider.BlockText, CachePoint: true},                            // cache boundary: keep
		{Type: provider.BlockText, Text: "hi"},                                  // content: keep
		{Type: provider.BlockToolUse, ToolUseID: "t1", ToolName: "echo"},        // tool call: keep
	}
	kept := dropEmptyBlocks(blocks)
	if len(kept) != 4 {
		t.Fatalf("kept %d blocks, want 4: %+v", len(kept), kept)
	}
	if kept[0].ThinkingSignature != "sig" || !kept[1].CachePoint || kept[2].Text != "hi" || kept[3].ToolUseID != "t1" {
		t.Errorf("kept the wrong blocks: %+v", kept)
	}
}

func TestLoopFinalizesUnclosedBlocksInStreamOrder(t *testing.T) {
	// A provider that closes the stream WITHOUT block-stop events: the open
	// blocks must be finalized in provider index order. The natural-close
	// path used to range over a map, randomizing the persisted block order.
	p := &scriptProvider{script: [][]provider.Event{
		{
			{Type: provider.EventMessageStart},
			{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockText}},
			{Type: provider.EventBlockDelta, Index: 0, Delta: "lead-in"},
			{Type: provider.EventBlockStart, Index: 1, Block: &provider.Block{Type: provider.BlockToolUse, ToolUseID: "tu1", ToolName: "echo", ToolInput: map[string]any{}}},
			{Type: provider.EventBlockDelta, Index: 1, Delta: `{}`},
			// No block-stops, no message-stop: the channel just closes.
		},
		textResponse("done"),
	}}
	reg := toolruntime.NewRegistry()
	reg.Register(echoTool{})
	loop := New(p, reg, Config{Model: "m", MaxTokens: 100})

	produced, err := loop.Run(context.Background(), nil, &memEmitter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(produced) != 3 {
		t.Fatalf("produced %d messages, want [assistant, tool_result, assistant]", len(produced))
	}
	content := produced[0].Content
	if len(content) != 2 || content[0].Type != provider.BlockText || content[1].Type != provider.BlockToolUse {
		t.Fatalf("assistant blocks = %+v, want [text, tool_use] in stream order", content)
	}
	if content[0].Text != "lead-in" || content[1].ToolUseID != "tu1" {
		t.Errorf("assistant blocks = %+v, want text %q then tool_use tu1", content, "lead-in")
	}
}

func TestLoopCancelBeforeStream(t *testing.T) {
	// A pre-cancelled context must abort the loop at the iteration guard and
	// emit a cancelled event, not a done/error.
	p := &scriptProvider{script: [][]provider.Event{textResponse("never")}}
	reg := toolruntime.NewRegistry()
	emit := &memEmitter{}
	loop := New(p, reg, Config{Model: "m", MaxTokens: 100})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before Run

	_, err := loop.Run(ctx, nil, emit)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v want context.Canceled", err)
	}
	if emit.count(KindCancelled) != 1 {
		t.Errorf("expected 1 cancelled event, got %d", emit.count(KindCancelled))
	}
	if emit.count(KindDone) != 0 {
		t.Error("cancelled run must not emit done")
	}
}

// slowProvider emits a couple of events then parks until the test closes
// release, so the loop exercises the mid-stream cancel path in consume().
type slowProvider struct {
	started chan struct{}
	release chan struct{}
}

func (p *slowProvider) Name() string { return "slow" }

func (p *slowProvider) Stream(context.Context, provider.Request) (<-chan provider.Event, error) {
	ch := make(chan provider.Event)
	go func() {
		defer close(ch)
		close(p.started)
		ch <- provider.Event{Type: provider.EventMessageStart}
		ch <- provider.Event{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockText}}
		// Park mid-stream: the loop should interrupt consume() via ctx cancel
		// before this would ever deliver more content.
		<-p.release
	}()
	return ch, nil
}

func TestLoopCancelMidStream(t *testing.T) {
	p := &slowProvider{started: make(chan struct{}), release: make(chan struct{})}
	reg := toolruntime.NewRegistry()
	emit := &memEmitter{}
	loop := New(p, reg, Config{Model: "m", MaxTokens: 100})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := loop.Run(ctx, nil, emit)
		done <- err
	}()

	<-p.started // wait until the stream is producing
	cancel()    // interrupt mid-stream
	err := <-done
	close(p.release) // let the parked provider goroutine exit
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v want context.Canceled", err)
	}
	if emit.count(KindCancelled) != 1 {
		t.Errorf("expected 1 cancelled event, got %d", emit.count(KindCancelled))
	}
}

// toolUseNoStop emits a tool_use block that is never closed with an
// EventBlockStop before the stream ends — the shape the OpenAI adapter produced
// before it closed tool blocks in finish(). The loop must still emit KindToolUse
// (from its channel-close finalization) so the client sees a tool-call before
// the tool-result.
func toolUseNoStop(id, name, jsonArgs string) []provider.Event {
	return []provider.Event{
		{Type: provider.EventMessageStart},
		{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockToolUse, ToolUseID: id, ToolName: name, ToolInput: map[string]any{}}},
		{Type: provider.EventBlockDelta, Index: 0, Delta: jsonArgs},
		// No EventBlockStop / EventMessageStop: the channel just closes.
	}
}

func TestLoopEmitsToolUseWhenBlockLeftOpen(t *testing.T) {
	p := &scriptProvider{script: [][]provider.Event{
		toolUseNoStop("c1", "echo", `{}`),
		textResponse("done"),
	}}
	reg := toolruntime.NewRegistry()
	reg.Register(echoTool{})
	emit := &memEmitter{}
	loop := New(p, reg, Config{Model: "m", MaxTokens: 100})

	produced, err := loop.Run(context.Background(), []provider.Message{provider.TextMessage(provider.RoleUser, "hi")}, emit)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Every dispatched tool call must have a preceding tool-use event, even when
	// the provider left the block open at stream close.
	if got := emit.count(KindToolUse); got != 1 {
		t.Errorf("KindToolUse count = %d, want 1", got)
	}
	if got := emit.count(KindToolResult); got != 1 {
		t.Errorf("KindToolResult count = %d, want 1", got)
	}
	// And the run completes with the final text after the tool round-trip.
	if len(produced) == 0 || produced[len(produced)-1].Content[0].Text != "done" {
		t.Errorf("final produced = %+v", produced)
	}
}

// TestToolResultMessageFillsEmpty pins fix A: a tool returning empty content
// (e.g. list_dir on an empty workspace) must still produce a tool_result block
// with non-empty ToolContent. An empty one serializes to a `role:"tool"`
// message with no content field, which the OpenAI/deepseek gateway 400s on,
// aborting the run right after the tool batch.
func TestToolResultMessageFillsEmpty(t *testing.T) {
	calls := []toolruntime.Call{
		{ID: "c1", Name: "list_dir", Args: map[string]any{}},
		{ID: "c2", Name: "read_file", Args: map[string]any{}},
	}
	results := []toolruntime.Result{
		{Content: ""},          // empty → placeholder
		{Content: "real data"}, // non-empty → untouched
	}

	msg := toolResultMessage(calls, results)
	if msg.Role != provider.RoleUser {
		t.Errorf("role = %q", msg.Role)
	}
	if len(msg.Content) != 2 {
		t.Fatalf("blocks = %d, want 2", len(msg.Content))
	}
	if got := msg.Content[0].ToolContent; got != emptyToolResultPlaceholder {
		t.Errorf("empty result content = %q, want placeholder %q", got, emptyToolResultPlaceholder)
	}
	if got := msg.Content[1].ToolContent; got != "real data" {
		t.Errorf("non-empty result content = %q, want preserved", got)
	}
	for i, b := range msg.Content {
		if b.Type != provider.BlockToolResult || b.ToolResultID != calls[i].ID {
			t.Errorf("block %d = %+v", i, b)
		}
	}
}

// emptyTool returns empty content, like list_dir on an empty workspace.
type emptyTool struct{}

func (emptyTool) Name() string           { return "empty" }
func (emptyTool) Description() string    { return "returns nothing" }
func (emptyTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (emptyTool) Risk() toolruntime.Risk { return toolruntime.RiskReadOnly }
func (emptyTool) Timeout() time.Duration { return time.Second }
func (emptyTool) Call(context.Context, map[string]any) (toolruntime.Result, error) {
	return toolruntime.Result{Content: ""}, nil
}

// TestLoopEmptyToolResultRoundTrip is the end-to-end regression for the live
// 400: a tool with empty output goes through a full loop, and the SECOND
// request sent to the provider must carry a non-empty tool_result content.
// Before fix A the block had ToolContent="", which the OpenAI adapter drops via
// omitempty → a `role:"tool"` message with no content field → gateway 400.
func TestLoopEmptyToolResultRoundTrip(t *testing.T) {
	p := &scriptProvider{script: [][]provider.Event{
		toolUseResponse("tu1", "empty", `{}`),
		textResponse("done"),
	}}
	reg := toolruntime.NewRegistry()
	reg.Register(emptyTool{})
	loop := New(p, reg, Config{Model: "m", MaxTokens: 100})

	if _, err := loop.Run(context.Background(), []provider.Message{provider.TextMessage(provider.RoleUser, "hi")}, &memEmitter{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(p.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2 (tool round-trip)", len(p.requests))
	}
	// The second request's messages end with the tool_result message.
	second := p.requests[1]
	last := second.Messages[len(second.Messages)-1]
	if last.Role != provider.RoleUser || len(last.Content) == 0 {
		t.Fatalf("second request last message = %+v", last)
	}
	blk := last.Content[0]
	if blk.Type != provider.BlockToolResult {
		t.Fatalf("block type = %q, want tool_result", blk.Type)
	}
	if blk.ToolContent == "" {
		t.Error("tool_result content sent to provider is empty — would 400 on OpenAI/deepseek")
	}
	if blk.ToolContent != emptyToolResultPlaceholder {
		t.Errorf("tool_result content = %q, want placeholder", blk.ToolContent)
	}
}

// usageEmitter captures the payload of the terminal KindUsage event.
type usageEmitter struct {
	memEmitter
	usage *provider.Usage
}

func (u *usageEmitter) Emit(ctx context.Context, kind EventKind, payload any) error {
	if kind == KindUsage {
		if got, ok := payload.(provider.Usage); ok {
			g := got
			u.usage = &g
		}
	}
	return u.memEmitter.Emit(ctx, kind, payload)
}

// usageTextResponse is textResponse with explicit token usage on its stop event.
func usageTextResponse(text string, in, out int) []provider.Event {
	return []provider.Event{
		{Type: provider.EventMessageStart},
		{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockText}},
		{Type: provider.EventBlockDelta, Index: 0, Delta: text},
		{Type: provider.EventBlockStop, Index: 0},
		{Type: provider.EventMessageStop, StopReason: provider.StopEndTurn, Usage: &provider.Usage{InputTokens: in, OutputTokens: out}},
	}
}

// toolUseResponseUsage is toolUseResponse with token usage on its stop event.
func toolUseResponseUsage(id, name, jsonArgs string, in, out int) []provider.Event {
	evs := toolUseResponse(id, name, jsonArgs)
	evs[len(evs)-1].Usage = &provider.Usage{InputTokens: in, OutputTokens: out}
	return evs
}

// TestLoopEmitsUsage pins L2: the loop reports token usage as a terminal
// KindUsage event so the transport can surface real counts (finish() previously
// hardcoded zeros).
func TestLoopEmitsUsage(t *testing.T) {
	p := &scriptProvider{script: [][]provider.Event{usageTextResponse("hi", 12, 5)}}
	emit := &usageEmitter{}
	loop := New(p, toolruntime.NewRegistry(), Config{Model: "m", MaxTokens: 100})

	if _, err := loop.Run(context.Background(), nil, emit); err != nil {
		t.Fatal(err)
	}
	if emit.count(KindUsage) != 1 {
		t.Fatalf("KindUsage count = %d, want 1", emit.count(KindUsage))
	}
	if emit.usage == nil || emit.usage.InputTokens != 12 || emit.usage.OutputTokens != 5 {
		t.Errorf("usage = %+v, want {12 5}", emit.usage)
	}
}

// TestLoopAccumulatesUsageAcrossTurns verifies usage sums over every provider
// call in a run (a tool round-trip is two calls).
func TestLoopAccumulatesUsageAcrossTurns(t *testing.T) {
	p := &scriptProvider{script: [][]provider.Event{
		toolUseResponseUsage("tu1", "echo", `{}`, 5, 3),
		usageTextResponse("done", 7, 4),
	}}
	reg := toolruntime.NewRegistry()
	reg.Register(echoTool{})
	emit := &usageEmitter{}
	loop := New(p, reg, Config{Model: "m", MaxTokens: 100})

	if _, err := loop.Run(context.Background(), nil, emit); err != nil {
		t.Fatal(err)
	}
	if emit.usage == nil || emit.usage.InputTokens != 12 || emit.usage.OutputTokens != 7 {
		t.Errorf("accumulated usage = %+v, want {12 7}", emit.usage)
	}
}

// TestCacheHitPctDeepSeek pins the DeepSeek/OpenAI semantics: input_tokens
// already includes the cached prefix, so 3584/3604 ≈ 99% (not the ~50% the
// double-counting formula gives).
func TestCacheHitPctDeepSeek(t *testing.T) {
	if got := cacheHitPct(provider.Usage{InputTokens: 3604, CacheReadTokens: 3584}); got != 99 {
		t.Errorf("cacheHitPct = %d want 99", got)
	}
	if got := cacheHitPct(provider.Usage{InputTokens: 0, CacheReadTokens: 0}); got != 0 {
		t.Errorf("empty = %d want 0", got)
	}
	// Anthropic-style (cache not in input) clamps to 100 rather than nonsense.
	if got := cacheHitPct(provider.Usage{InputTokens: 100, CacheReadTokens: 3456}); got != 100 {
		t.Errorf("clamp = %d want 100", got)
	}
}

// TestMergeUsageKeepsMostInformative pins that a later placeholder usage chunk
// (a proxy-injected one with shrunken counts and cache cleared to 0) never
// clobbers the real model's usage, including the cache-hit count.
func TestMergeUsageKeepsMostInformative(t *testing.T) {
	real := &provider.Usage{InputTokens: 3535, OutputTokens: 44, CacheReadTokens: 3456}
	placeholder := &provider.Usage{InputTokens: 3060, OutputTokens: 42, CacheReadTokens: 0}

	got := mergeUsage(real, placeholder)
	if got.InputTokens != 3535 || got.CacheReadTokens != 3456 {
		t.Errorf("placeholder clobbered real usage: %+v", got)
	}
	// Order independence: placeholder first, real later still yields the real max.
	got = mergeUsage(placeholder, real)
	if got.InputTokens != 3535 || got.CacheReadTokens != 3456 {
		t.Errorf("order-dependent merge: %+v", got)
	}
	// Nil handling.
	if mergeUsage(nil, real) != real || mergeUsage(real, nil) != real {
		t.Error("nil merge should pass through the non-nil side")
	}
	// ReasoningTokens merges by the same max rule.
	a := &provider.Usage{ReasoningTokens: 0}
	b := &provider.Usage{ReasoningTokens: 30}
	if got := mergeUsage(a, b); got.ReasoningTokens != 30 {
		t.Errorf("ReasoningTokens = %d want 30", got.ReasoningTokens)
	}
	if got := mergeUsage(b, a); got.ReasoningTokens != 30 {
		t.Errorf("order-dependent ReasoningTokens merge: %d", got.ReasoningTokens)
	}
}

// payloadEmitter captures every payload, for asserting on KindMessage contents.
type payloadEmitter struct {
	memEmitter
	payloads []any
}

func (p *payloadEmitter) Emit(ctx context.Context, kind EventKind, payload any) error {
	p.payloads = append(p.payloads, payload)
	return p.memEmitter.Emit(ctx, kind, payload)
}

// cancelOnStepStartEmitter cancels the run ctx when the iter>0 KindStepStart
// frame goes out, landing the cancellation inside the NEXT provider attempt —
// the mid-stream cancel path.
type cancelOnStepStartEmitter struct {
	stepCapture
	cancel context.CancelFunc
}

func (e *cancelOnStepStartEmitter) Emit(ctx context.Context, kind EventKind, payload any) error {
	if kind == KindStepStart {
		e.cancel()
	}
	return e.stepCapture.Emit(ctx, kind, payload)
}

func TestLoopCancelMidStreamClosesStep(t *testing.T) {
	// A step opened at iter>0 must not dangle when the run is cancelled
	// mid-attempt: the cancel path closes it (reason "other", the transport's
	// cancelled mapping) before the terminal cancelled frame, symmetric with
	// the error path.
	p := &scriptProvider{script: [][]provider.Event{
		toolUseResponse("tu1", "echo", `{}`),
		textResponse("never reached"),
	}}
	reg := toolruntime.NewRegistry()
	reg.Register(echoTool{})
	ctx, cancel := context.WithCancel(context.Background())
	emit := &cancelOnStepStartEmitter{cancel: cancel}
	loop := New(p, reg, Config{Model: "m", MaxTokens: 100})

	_, err := loop.Run(ctx, nil, emit)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v want context.Canceled", err)
	}
	if emit.count(KindStepStart) != 1 {
		t.Errorf("KindStepStart = %d, want 1", emit.count(KindStepStart))
	}
	if len(emit.finishes) != 2 {
		t.Fatalf("KindStepFinish = %d, want 2 (one per opened step): %+v", len(emit.finishes), emit.finishes)
	}
	if emit.finishes[0].FinishReason != "tool-calls" || !emit.finishes[0].IsContinued {
		t.Errorf("step0 = %+v, want tool-calls/continued", emit.finishes[0])
	}
	if emit.finishes[1].FinishReason != "other" || emit.finishes[1].IsContinued {
		t.Errorf("step1 = %+v, want other/not-continued (the step opened at iter 1 must close)", emit.finishes[1])
	}
	if emit.count(KindCancelled) != 1 {
		t.Errorf("KindCancelled = %d, want 1", emit.count(KindCancelled))
	}
}

// cancelOnNthAssistantEmitter cancels the run ctx when the n-th assembled
// assistant message (MessageWithUsage payload) is emitted — landing the
// cancellation in the window between message persistence and batch dispatch.
type cancelOnNthAssistantEmitter struct {
	stepCapture
	cancel context.CancelFunc
	n      int
	seen   int
}

func (e *cancelOnNthAssistantEmitter) Emit(ctx context.Context, kind EventKind, payload any) error {
	if kind == KindMessage {
		if _, ok := payload.(MessageWithUsage); ok {
			e.seen++
			if e.seen == e.n {
				e.cancel()
			}
		}
	}
	return e.stepCapture.Emit(ctx, kind, payload)
}

func TestLoopCancelBeforeBatchClosesStep(t *testing.T) {
	// The pre-dispatch cancel path at iter>0 must also close the step it
	// opened: record the synthetic cancelled results, close the step
	// ("other"), then emit the terminal cancelled frame.
	p := &scriptProvider{script: [][]provider.Event{
		toolUseResponse("tu1", "echo", `{}`),
		toolUseResponse("tu2", "echo", `{}`),
	}}
	reg := toolruntime.NewRegistry()
	reg.Register(echoTool{})
	ctx, cancel := context.WithCancel(context.Background())
	emit := &cancelOnNthAssistantEmitter{cancel: cancel, n: 2}
	loop := New(p, reg, Config{Model: "m", MaxTokens: 100})

	produced, err := loop.Run(ctx, nil, emit)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v want context.Canceled", err)
	}
	if len(emit.finishes) != 2 {
		t.Fatalf("KindStepFinish = %d, want 2: %+v", len(emit.finishes), emit.finishes)
	}
	if emit.finishes[1].FinishReason != "other" || emit.finishes[1].IsContinued {
		t.Errorf("step1 = %+v, want other/not-continued", emit.finishes[1])
	}
	if emit.count(KindCancelled) != 1 {
		t.Errorf("KindCancelled = %d, want 1", emit.count(KindCancelled))
	}
	// Both batches answered: [assistant, tool_result, assistant, tool_result].
	if len(produced) != 4 {
		t.Fatalf("produced %d messages, want 4", len(produced))
	}
	last := produced[3]
	if len(last.Content) != 1 || last.Content[0].Type != provider.BlockToolResult ||
		!last.Content[0].IsError || !strings.Contains(last.Content[0].ToolContent, "cancelled") {
		t.Errorf("last tool_result = %+v, want an is_error 'cancelled' result", last.Content)
	}
}

func TestLoopUnknownStopReasonWithToolCallsFailsBatch(t *testing.T) {
	// A tool-call turn whose stream closes WITHOUT a finish reason gets the
	// same treatment as a max_tokens truncation: the batch may be incomplete,
	// so every call fails for re-issue (not dispatched) and the loop continues.
	called := false
	turn := []provider.Event{
		{Type: provider.EventMessageStart},
		{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockToolUse, ToolUseID: "tu1", ToolName: "echo", ToolInput: map[string]any{}}},
		{Type: provider.EventBlockDelta, Index: 0, Delta: `{}`},
		{Type: provider.EventBlockStop, Index: 0},
		{Type: provider.EventMessageStop}, // no StopReason reported
	}
	p := &scriptProvider{script: [][]provider.Event{turn, textResponse("done")}}
	reg := toolruntime.NewRegistry()
	reg.Register(riskTool{name: "echo", risk: toolruntime.RiskReadOnly, called: &called})
	emit := &stepCapture{}
	loop := New(p, reg, Config{Model: "m", MaxTokens: 100})

	produced, err := loop.Run(context.Background(), nil, emit)
	if err != nil {
		t.Fatalf("the run must continue after a re-issued batch, got %v", err)
	}
	if called {
		t.Error("a possibly-truncated batch must NOT be dispatched")
	}
	if p.calls != 2 {
		t.Errorf("provider calls = %d, want 2 (batch failed, model re-issued)", p.calls)
	}
	if emit.count(KindDone) != 1 {
		t.Errorf("KindDone = %d, want 1", emit.count(KindDone))
	}
	if len(emit.finishes) == 0 || emit.finishes[0].FinishReason != "error" || !emit.finishes[0].IsContinued {
		t.Errorf("step0 = %+v, want error/continued", emit.finishes)
	}
	// The failed batch is answered with an explanatory is_error result.
	if len(produced) < 2 {
		t.Fatalf("produced = %d messages, want at least [assistant, tool_result]", len(produced))
	}
	res := produced[1]
	if len(res.Content) != 1 || !res.Content[0].IsError || !strings.Contains(res.Content[0].ToolContent, "finish reason") {
		t.Errorf("tool_result = %+v, want is_error naming the missing finish reason", res.Content)
	}
}

func TestLoopDuplicateBlockStartFails(t *testing.T) {
	// Adapter contract violation: a second block start on an open index would
	// silently discard the deltas already accumulated. The loop must fail
	// loudly instead of truncating output without a trace.
	p := &scriptProvider{script: [][]provider.Event{{
		{Type: provider.EventMessageStart},
		{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockText}},
		{Type: provider.EventBlockDelta, Index: 0, Delta: "partial"},
		{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockText}}, // duplicate
		{Type: provider.EventBlockDelta, Index: 0, Delta: "other"},
		{Type: provider.EventBlockStop, Index: 0},
		{Type: provider.EventMessageStop, StopReason: provider.StopEndTurn},
	}}}
	emit := &memEmitter{}
	loop := New(p, toolruntime.NewRegistry(), Config{Model: "m", MaxTokens: 100})

	_, err := loop.Run(context.Background(), nil, emit)
	if err == nil || !strings.Contains(err.Error(), "contract violation") {
		t.Fatalf("err = %v, want a contract-violation error", err)
	}
	if emit.count(KindError) != 1 || emit.count(KindDone) != 0 {
		t.Errorf("events: KindError = %d (want 1), KindDone = %d (want 0)", emit.count(KindError), emit.count(KindDone))
	}
}

func TestLoopDeltaAtUnopenedIndexFails(t *testing.T) {
	// Adapter contract violation: a delta for a block that never started would
	// be silently dropped. Fail loudly.
	p := &scriptProvider{script: [][]provider.Event{{
		{Type: provider.EventMessageStart},
		{Type: provider.EventBlockDelta, Index: 1, Delta: "orphan"},
		{Type: provider.EventMessageStop, StopReason: provider.StopEndTurn},
	}}}
	emit := &memEmitter{}
	loop := New(p, toolruntime.NewRegistry(), Config{Model: "m", MaxTokens: 100})

	_, err := loop.Run(context.Background(), nil, emit)
	if err == nil || !strings.Contains(err.Error(), "contract violation") {
		t.Fatalf("err = %v, want a contract-violation error", err)
	}
	if emit.count(KindDone) != 0 {
		t.Error("a contract-violating stream must not complete the run")
	}
}

func TestLoopRepairsUnpairedHistoryAcrossIterations(t *testing.T) {
	// Pairing repair happens once on the incoming history; the repaired view
	// must stay the base of EVERY provider request across iterations (the
	// loop's own produced tail is paired by construction).
	p := &scriptProvider{script: [][]provider.Event{
		toolUseResponse("tu-new", "echo", `{}`),
		textResponse("done"),
	}}
	reg := toolruntime.NewRegistry()
	reg.Register(echoTool{})
	loop := New(p, reg, Config{Model: "m", MaxTokens: 100})

	// History left unpaired by a prior run: an assistant tool_use with no
	// tool_result following it.
	history := []provider.Message{
		provider.TextMessage(provider.RoleUser, "hi"),
		{Role: provider.RoleAssistant, Content: []provider.Block{{Type: provider.BlockToolUse, ToolUseID: "tu-old", ToolName: "echo", ToolInput: map[string]any{}}}},
	}
	if _, err := loop.Run(context.Background(), history, &memEmitter{}); err != nil {
		t.Fatal(err)
	}
	if len(p.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(p.requests))
	}
	for i, req := range p.requests {
		var found bool
		for _, m := range req.Messages {
			for _, b := range m.Content {
				if b.Type == provider.BlockToolResult && b.ToolResultID == "tu-old" {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("request %d lacks the synthesized result answering tu-old — the repaired pairing did not reach the provider", i)
		}
	}
}

// TestLoopEmitsAssistantUsageWithMessage verifies each assistant KindMessage
// carries the usage of the single LLM call that produced it (MessageWithUsage),
// so the persistence path can record per-call usage on that row.
func TestLoopEmitsAssistantUsageWithMessage(t *testing.T) {
	p := &scriptProvider{script: [][]provider.Event{usageTextResponse("hi", 12, 5)}}
	emit := &payloadEmitter{}
	loop := New(p, toolruntime.NewRegistry(), Config{Model: "m", MaxTokens: 100})

	if _, err := loop.Run(context.Background(), nil, emit); err != nil {
		t.Fatal(err)
	}
	var got *MessageWithUsage
	for _, pl := range emit.payloads {
		if mwu, ok := pl.(MessageWithUsage); ok {
			mwu := mwu
			got = &mwu
		}
	}
	if got == nil {
		t.Fatal("no MessageWithUsage payload emitted for the assistant message")
	}
	if got.Usage == nil || got.Usage.InputTokens != 12 || got.Usage.OutputTokens != 5 {
		t.Errorf("assistant message usage = %+v, want {12 5}", got.Usage)
	}
	if len(got.Message.Content) == 0 {
		t.Error("MessageWithUsage lost the assistant content")
	}
}

package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/toolruntime"
)

// scriptProvider emits canned event sequences, one per Stream call.
type scriptProvider struct {
	mu     sync.Mutex
	script [][]provider.Event
	calls  int
}

func (p *scriptProvider) Name() string { return "script" }

func (p *scriptProvider) Stream(context.Context, provider.Request) (<-chan provider.Event, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.calls >= len(p.script) {
		return nil, errors.New("no more scripted responses")
	}
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
		{Type: provider.EventMessageStop, Usage: &provider.Usage{InputTokens: 1, OutputTokens: 1}},
	}
}

// toolUseResponse builds a script entry that requests a tool call.
func toolUseResponse(id, name, jsonArgs string) []provider.Event {
	return []provider.Event{
		{Type: provider.EventMessageStart},
		{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockToolUse, ToolUseID: id, ToolName: name, ToolInput: map[string]any{}}},
		{Type: provider.EventBlockDelta, Index: 0, Delta: jsonArgs},
		{Type: provider.EventBlockStop, Index: 0},
		{Type: provider.EventMessageStop},
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
		{Type: provider.EventMessageStop},
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

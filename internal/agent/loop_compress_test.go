package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"nowhere-agent/internal/contextmgmt"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/toolruntime"
)

// recordingProvider answers every Stream with a fixed text reply and records
// each request so tests can inspect what the loop actually sent.
type recordingProvider struct {
	mu       sync.Mutex
	requests []provider.Request
	reply    string
}

func (p *recordingProvider) Name() string { return "rec" }

func (p *recordingProvider) Stream(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	p.mu.Unlock()
	ch := make(chan provider.Event, 5)
	for _, e := range textResponse(p.reply) {
		ch <- e
	}
	close(ch)
	return ch, nil
}

func (p *recordingProvider) last() provider.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests[len(p.requests)-1]
}

// stubCompressor yields a fixed summary and counts invocations.
type stubCompressor struct {
	calls int
	err   error
	got   [][]provider.Message
}

func (s *stubCompressor) Summarize(_ context.Context, dropped []provider.Message) (string, error) {
	s.calls++
	s.got = append(s.got, dropped)
	if s.err != nil {
		return "", s.err
	}
	return "SUMMARY-OF-OLDER-TURNS", nil
}

// bigConversation builds a history whose estimated tokens exceed the budget.
func bigConversation(turns int, size int) []provider.Message {
	msgs := make([]provider.Message, 0, turns*2)
	for i := 0; i < turns; i++ {
		msgs = append(msgs,
			provider.TextMessage(provider.RoleUser, strings.Repeat("u", size)),
			provider.TextMessage(provider.RoleAssistant, strings.Repeat("a", size)),
		)
	}
	return msgs
}

func TestLoopCompressesViewOverBudget(t *testing.T) {
	rp := &recordingProvider{reply: "final"}
	comp := &stubCompressor{}
	loop := New(rp, toolruntime.NewRegistry(), Config{Model: "m", MaxTokens: 100})
	loop.Use(&CompressMW{Compressor: comp, Window: 200, MaxTokens: 100})

	// ~big history: estimated tokens far above (200-100)*0.8 = 80.
	history := bigConversation(6, 400)
	if _, err := loop.Run(context.Background(), history, &memEmitter{}); err != nil {
		t.Fatal(err)
	}
	if comp.calls == 0 {
		t.Fatal("compressor should have run over budget")
	}
	// The request the provider actually received must be the compressed view:
	// it carries the summary, not the full verbatim history.
	sent := rp.last()
	var sawSummary bool
	for _, m := range sent.Messages {
		for _, b := range m.Content {
			if strings.Contains(b.Text, "SUMMARY-OF-OLDER-TURNS") {
				sawSummary = true
			}
		}
	}
	if !sawSummary {
		t.Error("provider request should carry the compression summary")
	}
	// And the compressed view is smaller than the input history.
	if len(sent.Messages) >= len(history) {
		t.Errorf("compressed view (%d msgs) should be smaller than history (%d msgs)", len(sent.Messages), len(history))
	}
}

func TestLoopSkipsCompressionWhenUnconfigured(t *testing.T) {
	rp := &recordingProvider{reply: "ok"}
	// No Compressor / no ContextWindow: compression disabled.
	loop := New(rp, toolruntime.NewRegistry(), Config{Model: "m", MaxTokens: 100})
	history := bigConversation(6, 400)
	if _, err := loop.Run(context.Background(), history, &memEmitter{}); err != nil {
		t.Fatal(err)
	}
	sent := rp.last()
	// The full history is sent verbatim (plus nothing dropped).
	if len(sent.Messages) != len(history) {
		t.Errorf("expected full history sent (%d), got %d", len(history), len(sent.Messages))
	}
}

func TestLoopCompressionCircuitBreaker(t *testing.T) {
	// One run, four model calls (3 tool iterations + final answer), compressor
	// down throughout: the breaker (MaxFailures=2) caps summarizer calls at 2
	// for the run, and later iterations skip it entirely.
	sp := &scriptProvider{script: [][]provider.Event{
		toolUseResponse("t1", "echo", "{}"),
		toolUseResponse("t2", "echo", "{}"),
		toolUseResponse("t3", "echo", "{}"),
		textResponse("done"),
	}}
	comp := &stubCompressor{err: errors.New("summarizer down")}
	reg := toolruntime.NewRegistry()
	reg.Register(echoTool{})
	loop := New(sp, reg, Config{Model: "m", MaxTokens: 100})
	loop.Use(&CompressMW{Compressor: comp, Window: 200, MaxTokens: 100, MaxFailures: 2})

	history := bigConversation(6, 400)
	if _, err := loop.Run(context.Background(), history, &memEmitter{}); err != nil {
		t.Fatal(err)
	}
	if sp.calls != 4 {
		t.Fatalf("provider calls = %d, want 4 (3 tool iterations + final)", sp.calls)
	}
	if comp.calls != 2 {
		t.Errorf("compressor called %d times, breaker should cap at 2 within a run", comp.calls)
	}

	// The breaker is per-RUN: a fresh run on the same Loop gets a fresh count
	// and tries the summarizer again (no cross-run failure leak).
	sp2 := &scriptProvider{script: [][]provider.Event{
		toolUseResponse("t4", "echo", "{}"),
		textResponse("done"),
	}}
	loop.provider = sp2
	if _, err := loop.Run(context.Background(), history, &memEmitter{}); err != nil {
		t.Fatal(err)
	}
	if comp.calls != 4 {
		t.Errorf("compressor calls = %d, want 4 (2 more attempts in the fresh run)", comp.calls)
	}
}

// TestLoopCompressionReusesSummaryAcrossIterations drives a multi-iteration
// tool loop over an over-budget history: the summary must be computed ONCE and
// reused (byte-stably) while the growing tail still fits the budget, instead of
// re-summarizing the whole history every iteration.
func TestLoopCompressionReusesSummaryAcrossIterations(t *testing.T) {
	sp := &scriptProvider{script: [][]provider.Event{
		toolUseResponse("t1", "echo", "{}"),
		toolUseResponse("t2", "echo", "{}"),
		textResponse("done"),
	}}
	comp := &stubCompressor{}
	reg := toolruntime.NewRegistry()
	reg.Register(echoTool{})
	loop := New(sp, reg, Config{Model: "m", MaxTokens: 100})
	loop.Use(&CompressMW{Compressor: comp, Window: 200, MaxTokens: 100})

	// 12 small msgs: est 120 > (200-100)*0.8 = 80 → compress; the compressed
	// view and its growing tail stay under the 100 budget across iterations.
	history := bigConversation(6, 40)
	if _, err := loop.Run(context.Background(), history, &memEmitter{}); err != nil {
		t.Fatal(err)
	}
	if sp.calls != 3 {
		t.Fatalf("provider calls = %d, want 3 (2 tool iterations + final)", sp.calls)
	}
	if comp.calls != 1 {
		t.Errorf("summarizer calls = %d, want 1 (summary reused across iterations)", comp.calls)
	}
	for i, req := range sp.requests {
		if len(req.Messages) == 0 || !contextmgmt.IsSummary(req.Messages[0]) {
			t.Errorf("request %d must lead with the compression summary", i)
		}
	}
}

// TestLoopCompressionExtendsSummaryIncrementally: once the growing tail pushes
// the reused view past the full budget, the summarizer runs again — but on the
// previous summary plus only the newly dropped rounds, not the whole prefix.
func TestLoopCompressionExtendsSummaryIncrementally(t *testing.T) {
	sp := &scriptProvider{script: [][]provider.Event{
		toolUseResponse("t1", "echo", `{"payload":"`+strings.Repeat("x", 400)+`"}`),
		textResponse("done"),
	}}
	comp := &stubCompressor{}
	reg := toolruntime.NewRegistry()
	reg.Register(echoTool{})
	loop := New(sp, reg, Config{Model: "m", MaxTokens: 100})
	loop.Use(&CompressMW{Compressor: comp, Window: 200, MaxTokens: 100})

	// est 360 > 80 → compress; the big tool-call args appended in iter 1 push
	// the reuse candidate past the full 100 budget by iter 2.
	history := bigConversation(6, 120)
	if _, err := loop.Run(context.Background(), history, &memEmitter{}); err != nil {
		t.Fatal(err)
	}
	if comp.calls != 2 {
		t.Fatalf("summarizer calls = %d, want 2 (initial + one incremental extension)", comp.calls)
	}
	if len(comp.got) < 2 || len(comp.got[1]) == 0 || !contextmgmt.IsSummary(comp.got[1][0]) {
		t.Error("incremental re-summarization must lead with the previous summary")
	}
}

func TestLoopRepairsUnpairedHistoryBeforeSend(t *testing.T) {
	rp := &recordingProvider{reply: "final"}
	// History with a dangling tool_use (cancelled before the result): the loop
	// must synthesize a result so the provider gets a valid conversation.
	dangling := provider.Message{
		Role: provider.RoleAssistant,
		Content: []provider.Block{
			{Type: provider.BlockToolUse, ToolUseID: "t1", ToolName: "search", ToolInput: map[string]any{}},
		},
	}
	history := []provider.Message{
		provider.TextMessage(provider.RoleUser, "hi"),
		dangling,
	}
	loop := New(rp, toolruntime.NewRegistry(), Config{Model: "m", MaxTokens: 100})
	if _, err := loop.Run(context.Background(), history, &memEmitter{}); err != nil {
		t.Fatal(err)
	}
	sent := rp.last()
	// Every tool_use in the sent view must have a matching tool_result.
	uses, results := map[string]bool{}, map[string]bool{}
	for _, m := range sent.Messages {
		for _, b := range m.Content {
			switch b.Type {
			case provider.BlockToolUse:
				uses[b.ToolUseID] = true
			case provider.BlockToolResult:
				results[b.ToolResultID] = true
			}
		}
	}
	for id := range uses {
		if !results[id] {
			t.Errorf("sent view has dangling tool_use %q", id)
		}
	}
}

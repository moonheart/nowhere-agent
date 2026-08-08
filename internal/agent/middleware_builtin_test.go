package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"nowhere-agent/internal/contextmgmt"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/toolruntime"
)

// These tests exercise each built-in middleware in isolation, through the wrap
// chain — no full Loop required. They pin the transient-view contract: a
// middleware may rewrite ModelCall.View/Request but the change is local to the
// chain invocation.

// okHandler is an innermost ModelHandler that records the call it received.
func okHandler(got **ModelCall) ModelHandler {
	return func(_ context.Context, c *ModelCall) (ModelResult, error) {
		*got = c
		return ModelResult{Assistant: provider.TextMessage(provider.RoleAssistant, "ok")}, nil
	}
}

func TestCompressMWRewritesTransientView(t *testing.T) {
	comp := &stubCompressor{}
	mw := &CompressMW{Compressor: comp, Window: 200, MaxTokens: 100}
	var got *ModelCall
	call := &ModelCall{
		View: bigConversation(6, 400), // over budget
	}
	call.Request.Messages = call.View

	if _, err := mw.WrapModelCall(context.Background(), call, okHandler(&got)); err != nil {
		t.Fatalf("WrapModelCall: %v", err)
	}
	if comp.calls == 0 {
		t.Fatal("compressor should run over budget")
	}
	// The handler saw the compressed view (summary present), smaller than input.
	if len(got.View) >= 12 {
		t.Errorf("compressed view = %d msgs, want smaller than 12", len(got.View))
	}
	var sawSummary bool
	for _, m := range got.Request.Messages {
		for _, b := range m.Content {
			if strings.Contains(b.Text, "SUMMARY-OF-OLDER-TURNS") {
				sawSummary = true
			}
		}
	}
	if !sawSummary {
		t.Error("handler request should carry the compression summary")
	}
}

func TestCompressMWDisabledWithoutCompressor(t *testing.T) {
	mw := &CompressMW{Window: 200, MaxTokens: 100} // no Compressor
	var got *ModelCall
	call := &ModelCall{View: bigConversation(6, 400)}
	call.Request.Messages = call.View
	if _, err := mw.WrapModelCall(context.Background(), call, okHandler(&got)); err != nil {
		t.Fatal(err)
	}
	if len(got.View) != 12 {
		t.Errorf("view should pass through unchanged, got %d msgs", len(got.View))
	}
}

func TestCompressMWCircuitBreaker(t *testing.T) {
	comp := &stubCompressor{err: errors.New("down")}
	mw := &CompressMW{Compressor: comp, Window: 200, MaxTokens: 100, MaxFailures: 2}
	okH := func(_ context.Context, _ *ModelCall) (ModelResult, error) {
		return ModelResult{Assistant: provider.TextMessage(provider.RoleAssistant, "ok")}, nil
	}
	// Each call is a fresh over-budget view; the breaker caps summarizer calls.
	for i := 0; i < 5; i++ {
		call := &ModelCall{View: bigConversation(6, 400)}
		call.Request.Messages = call.View
		if _, err := mw.WrapModelCall(context.Background(), call, okH); err != nil {
			t.Fatal(err)
		}
	}
	if comp.calls > 2 {
		t.Errorf("compressor called %d times, breaker should cap at 2", comp.calls)
	}
}

// TestCompressMWSharedBreakerTripsAcrossInstances mirrors production wiring:
// the loop (and its CompressMW) is rebuilt per run, so a per-instance failure
// count would reset every run and never trip. A shared CircuitBreaker keeps a
// persistently failing summarizer tripped across instances.
func TestCompressMWSharedBreakerTripsAcrossInstances(t *testing.T) {
	comp := &stubCompressor{err: errors.New("down")}
	br := &CircuitBreaker{}
	okH := func(_ context.Context, _ *ModelCall) (ModelResult, error) {
		return ModelResult{Assistant: provider.TextMessage(provider.RoleAssistant, "ok")}, nil
	}
	// Five "runs", each with a FRESH CompressMW instance sharing one breaker.
	for i := 0; i < 5; i++ {
		mw := &CompressMW{Compressor: comp, Window: 200, MaxTokens: 100, MaxFailures: 2, Breaker: br}
		call := &ModelCall{View: bigConversation(6, 400)}
		call.Request.Messages = call.View
		if _, err := mw.WrapModelCall(context.Background(), call, okH); err != nil {
			t.Fatal(err)
		}
	}
	if comp.calls > 2 {
		t.Errorf("compressor called %d times across instances, shared breaker should cap at 2", comp.calls)
	}
}

// TestCompressMWCountsSystemAndToolsAgainstBudget: the request envelope
// (system prompt + tool schemas) rides on every provider call but lives
// outside the message view; it must shrink the compression budget so the
// trigger fires when the envelope + view approach the window.
func TestCompressMWCountsSystemAndToolsAgainstBudget(t *testing.T) {
	comp := &stubCompressor{}
	// Budget = 200-100 = 100; a ~300-char system prompt (~75 tokens) leaves
	// ~25. A 24-token view fits 100 but not 25, so compression must fire ONLY
	// when the envelope is present.
	mkCall := func(withEnvelope bool) *ModelCall {
		call := &ModelCall{View: bigConversation(4, 12)} // est ≈ 24 tokens
		call.Request.Messages = call.View
		if withEnvelope {
			call.Request.System = strings.Repeat("s", 300)
		}
		return call
	}
	okH := func(_ context.Context, _ *ModelCall) (ModelResult, error) {
		return ModelResult{Assistant: provider.TextMessage(provider.RoleAssistant, "ok")}, nil
	}
	if _, err := compMW(comp).WrapModelCall(context.Background(), mkCall(false), okH); err != nil {
		t.Fatal(err)
	}
	if comp.calls != 0 {
		t.Fatalf("no envelope: compressor called %d times, want 0 (view fits the full budget)", comp.calls)
	}
	if _, err := compMW(comp).WrapModelCall(context.Background(), mkCall(true), okH); err != nil {
		t.Fatal(err)
	}
	if comp.calls != 1 {
		t.Errorf("with envelope: compressor called %d times, want 1 (system counts against the budget)", comp.calls)
	}
}

func compMW(comp *stubCompressor) *CompressMW {
	return &CompressMW{Compressor: comp, Window: 200, MaxTokens: 100}
}

func TestOverflowMWDropsRoundAndRetries(t *testing.T) {
	mw := &OverflowMW{MaxRetries: 3}
	calls := 0
	var sizes []int
	handler := func(_ context.Context, c *ModelCall) (ModelResult, error) {
		calls++
		sizes = append(sizes, len(c.View))
		if calls < 3 {
			return ModelResult{}, &provider.ContextOverflowError{StatusCode: 413, Body: "too large"}
		}
		return ModelResult{Assistant: provider.TextMessage(provider.RoleAssistant, "ok")}, nil
	}
	call := &ModelCall{View: bigConversation(8, 200)}
	call.Request.Messages = call.View
	res, err := mw.WrapModelCall(context.Background(), call, handler)
	if err != nil {
		t.Fatalf("overflow should be retried to success: %v", err)
	}
	if calls != 3 {
		t.Fatalf("handler called %d times, want 3 (2 overflow + 1 success)", calls)
	}
	if res.Assistant.Content[0].Text != "ok" {
		t.Errorf("result = %q, want ok", res.Assistant.Content[0].Text)
	}
	for i := 1; i < len(sizes); i++ {
		if sizes[i] >= sizes[i-1] {
			t.Errorf("view should shrink between retries: %v", sizes)
		}
	}
}

func TestOverflowMWPreservesSummaryOnRetry(t *testing.T) {
	mw := &OverflowMW{MaxRetries: 3}
	calls := 0
	var views [][]provider.Message
	handler := func(_ context.Context, c *ModelCall) (ModelResult, error) {
		calls++
		views = append(views, append([]provider.Message{}, c.View...))
		if calls < 3 {
			return ModelResult{}, &provider.ContextOverflowError{StatusCode: 413, Body: "too large"}
		}
		return ModelResult{Assistant: provider.TextMessage(provider.RoleAssistant, "ok")}, nil
	}
	// A compressed view: summary + 3 verbatim rounds. The retries must shrink
	// by dropping the oldest REAL round, never the summary.
	call := &ModelCall{View: []provider.Message{
		contextmgmt.SummaryMessage("old context"),
		provider.TextMessage(provider.RoleUser, strings.Repeat("a", 200)),
		provider.TextMessage(provider.RoleUser, strings.Repeat("b", 200)),
		provider.TextMessage(provider.RoleUser, strings.Repeat("c", 200)),
	}}
	call.Request.Messages = call.View
	if _, err := mw.WrapModelCall(context.Background(), call, handler); err != nil {
		t.Fatalf("overflow should be retried to success: %v", err)
	}
	if calls != 3 {
		t.Fatalf("handler called %d times, want 3", calls)
	}
	for i, v := range views {
		if len(v) == 0 || !contextmgmt.IsSummary(v[0]) {
			t.Errorf("attempt %d: summary must survive overflow retries", i)
		}
	}
	if len(views[1]) != 3 || len(views[2]) != 2 {
		t.Errorf("view sizes = %d/%d/%d, want 4/3/2 (one real round dropped per retry)",
			len(views[0]), len(views[1]), len(views[2]))
	}
}

// TestOverflowMWAdvancesCompressionCache pins the cache/drop interaction: the
// rounds the overflow retry drops from an already-compressed view must advance
// the run's compression cache, or the next iteration's hysteresis rebuild
// would resurrect them from durable history.
func TestOverflowMWAdvancesCompressionCache(t *testing.T) {
	mw := &OverflowMW{MaxRetries: 3}
	calls := 0
	handler := func(_ context.Context, c *ModelCall) (ModelResult, error) {
		calls++
		if calls < 3 {
			return ModelResult{}, &provider.ContextOverflowError{StatusCode: 413, Body: "too large"}
		}
		return ModelResult{Assistant: provider.TextMessage(provider.RoleAssistant, "ok")}, nil
	}
	state := &RunState{compressCache: &contextmgmt.CompressionCache{Covered: 1, CoveredBytes: 1, Summary: "S"}}
	call := &ModelCall{
		View: []provider.Message{
			contextmgmt.SummaryMessage("S"),
			provider.TextMessage(provider.RoleUser, strings.Repeat("a", 200)),
			provider.TextMessage(provider.RoleUser, strings.Repeat("b", 200)),
			provider.TextMessage(provider.RoleUser, strings.Repeat("c", 200)),
		},
		State: state,
	}
	call.Request.Messages = call.View
	if _, err := mw.WrapModelCall(context.Background(), call, handler); err != nil {
		t.Fatalf("overflow should be retried to success: %v", err)
	}
	if calls != 3 {
		t.Fatalf("handler called %d times, want 3", calls)
	}
	// Two rounds dropped across the two retries: coverage 1 → 3.
	if state.compressCache.Covered != 3 {
		t.Errorf("cache.Covered = %d, want 3 (1 + 2 dropped rounds)", state.compressCache.Covered)
	}
}

// TestOverflowMWInvalidatesCacheWhenSummaryDropped: the last-resort plain drop
// takes the summary itself — the cache no longer describes the view and must
// be rebuilt from scratch next iteration.
func TestOverflowMWInvalidatesCacheWhenSummaryDropped(t *testing.T) {
	mw := &OverflowMW{MaxRetries: 3}
	calls := 0
	handler := func(_ context.Context, c *ModelCall) (ModelResult, error) {
		calls++
		if calls < 2 {
			return ModelResult{}, &provider.ContextOverflowError{StatusCode: 413, Body: "too large"}
		}
		return ModelResult{Assistant: provider.TextMessage(provider.RoleAssistant, "ok")}, nil
	}
	state := &RunState{compressCache: &contextmgmt.CompressionCache{Covered: 1, CoveredBytes: 1, Summary: "S"}}
	call := &ModelCall{
		View: []provider.Message{
			contextmgmt.SummaryMessage("S"),
			provider.TextMessage(provider.RoleUser, strings.Repeat("a", 200)),
		},
		State: state,
	}
	call.Request.Messages = call.View
	if _, err := mw.WrapModelCall(context.Background(), call, handler); err != nil {
		t.Fatalf("overflow should be retried to success: %v", err)
	}
	if state.compressCache.Covered != 0 || state.compressCache.Summary != "" {
		t.Errorf("cache = %+v, want invalidated after the summary was dropped", state.compressCache)
	}
}

func TestOverflowMWBounded(t *testing.T) {
	mw := &OverflowMW{MaxRetries: 3}
	calls := 0
	handler := func(_ context.Context, _ *ModelCall) (ModelResult, error) {
		calls++
		return ModelResult{}, &provider.ContextOverflowError{StatusCode: 413}
	}
	call := &ModelCall{View: bigConversation(8, 200)}
	call.Request.Messages = call.View
	if _, err := mw.WrapModelCall(context.Background(), call, handler); err == nil {
		t.Fatal("should fail after exhausting retries")
	}
	if calls > 4 { // 1 initial + 3 retries
		t.Errorf("handler called %d times, bound should cap at 4", calls)
	}
}

func TestMemoryInjectMWAppendsOnlyToView(t *testing.T) {
	inj := &fakeInjector{extra: []provider.Message{provider.TextMessage(provider.RoleUser, "[mem] dark mode")}}
	mw := &MemoryInjectMW{Injector: inj, SessionID: "sess-1"}
	base := []provider.Message{provider.TextMessage(provider.RoleUser, "hello")}
	state := &RunState{View: append([]provider.Message{}, base...)}

	if err := mw.BeforeModel(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	if !inj.injected || inj.gotSess != "sess-1" {
		t.Fatalf("injector not called with sess-1: %+v", inj)
	}
	if len(state.View) != 2 {
		t.Fatalf("view = %d msgs, want 2 (base + injected)", len(state.View))
	}
	if state.View[len(state.View)-1].Content[0].Text != "[mem] dark mode" {
		t.Errorf("last view msg = %q, want injected memory", state.View[len(state.View)-1].Content[0].Text)
	}
	// The caller's original base slice is untouched (copy-on-write preserved).
	if len(base) != 1 {
		t.Errorf("base slice mutated to %d msgs", len(base))
	}
}

func TestMemoryInjectMWEmptyIsNoop(t *testing.T) {
	mw := &MemoryInjectMW{Injector: &fakeInjector{extra: nil}, SessionID: "s"}
	state := &RunState{View: []provider.Message{provider.TextMessage(provider.RoleUser, "hi")}}
	if err := mw.BeforeModel(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	if len(state.View) != 1 {
		t.Errorf("empty injection must leave view unchanged, got %d", len(state.View))
	}
}

func TestUsageMWEmitsTotalAtRunEnd(t *testing.T) {
	emit := &usageEmitter{}
	mw := &UsageMW{}
	state := &RunState{Emit: emit, Usage: provider.Usage{InputTokens: 12, OutputTokens: 7}}
	if err := mw.AfterRun(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	if emit.usage == nil || emit.usage.InputTokens != 12 || emit.usage.OutputTokens != 7 {
		t.Errorf("usage = %+v, want {12 7}", emit.usage)
	}
}

func TestUsageMWSkipsZeroTotal(t *testing.T) {
	emit := &usageEmitter{}
	mw := &UsageMW{}
	if err := mw.AfterRun(context.Background(), &RunState{Emit: emit}); err != nil {
		t.Fatal(err)
	}
	if emit.count(KindUsage) != 0 {
		t.Errorf("zero usage must not emit KindUsage, got %d", emit.count(KindUsage))
	}
}

// TestPermissionMWExposesGate verifies PermissionMW surfaces its policy to the
// loop via the single GateFuncProvider hook (used at both gate points).
func TestPermissionMWExposesGate(t *testing.T) {
	mw := &PermissionMW{Check: denyNetwork}
	var _ GateFuncProvider = mw

	gate := mw.GateCheck()
	if gate == nil {
		t.Fatal("GateCheck returned nil")
	}
	netTool := riskTool{name: "net", risk: toolruntime.RiskNetwork}
	readTool := riskTool{name: "r", risk: toolruntime.RiskReadOnly}
	if deny, _ := gate(context.Background(), netTool); !deny {
		t.Error("gate should deny the network tool")
	}
	if deny, _ := gate(context.Background(), readTool); deny {
		t.Error("gate should allow the read-only tool")
	}
}

// TestLoopUseFirstGateWins verifies that when two middleware register the same
// gate hook, the first registered is the one the loop consults.
func TestLoopUseFirstGateWins(t *testing.T) {
	loop := New(&scriptProvider{}, toolruntime.NewRegistry(), Config{Model: "m", MaxTokens: 100})
	loop.Use(&PermissionMW{Check: denyNetwork})
	loop.Use(&PermissionMW{Check: askAll}) // ignored: first registration wins
	if loop.gateInteraction == nil || loop.gateExecute == nil {
		t.Fatal("gates should be wired from the first PermissionMW")
	}
	if deny, reason := loop.gateExecute(context.Background(), riskTool{name: "net", risk: toolruntime.RiskNetwork}); !deny || IsApprovalReason(reason) {
		t.Errorf("execute gate = (%v, %q), want the first (denyNetwork) policy, not the approval marker", deny, reason)
	}
}

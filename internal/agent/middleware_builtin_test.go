package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

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
	var got *ModelCall
	base := []provider.Message{provider.TextMessage(provider.RoleUser, "hello")}
	call := &ModelCall{View: append([]provider.Message{}, base...)}
	call.Request.Messages = call.View

	if _, err := mw.WrapModelCall(context.Background(), call, okHandler(&got)); err != nil {
		t.Fatal(err)
	}
	if !inj.injected || inj.gotSess != "sess-1" {
		t.Fatalf("injector not called with sess-1: %+v", inj)
	}
	if len(got.View) != 2 {
		t.Fatalf("view = %d msgs, want 2 (base + injected)", len(got.View))
	}
	if got.View[len(got.View)-1].Content[0].Text != "[mem] dark mode" {
		t.Errorf("last view msg = %q, want injected memory", got.View[len(got.View)-1].Content[0].Text)
	}
	// The caller's original base slice is untouched (copy-on-write preserved).
	if len(base) != 1 {
		t.Errorf("base slice mutated to %d msgs", len(base))
	}
}

func TestMemoryInjectMWEmptyIsNoop(t *testing.T) {
	mw := &MemoryInjectMW{Injector: &fakeInjector{extra: nil}, SessionID: "s"}
	var got *ModelCall
	call := &ModelCall{View: []provider.Message{provider.TextMessage(provider.RoleUser, "hi")}}
	call.Request.Messages = call.View
	if _, err := mw.WrapModelCall(context.Background(), call, okHandler(&got)); err != nil {
		t.Fatal(err)
	}
	if len(got.View) != 1 {
		t.Errorf("empty injection must leave view unchanged, got %d", len(got.View))
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
	if deny, _ := gate(netTool); !deny {
		t.Error("gate should deny the network tool")
	}
	if deny, _ := gate(readTool); deny {
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
	if deny, reason := loop.gateExecute(riskTool{name: "net", risk: toolruntime.RiskNetwork}); !deny || IsApprovalReason(reason) {
		t.Errorf("execute gate = (%v, %q), want the first (denyNetwork) policy, not the approval marker", deny, reason)
	}
}

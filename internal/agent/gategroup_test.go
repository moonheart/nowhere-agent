package agent

import (
	"context"
	"testing"

	"nowhere-agent/internal/toolruntime"
)

// TestGateGroupFirstDenyWins pins the composition semantics the assembly point
// relies on: gates are consulted in registration order, the FIRST deny ends
// the consultation with that gate's reason, and later gates are not consulted.
func TestGateGroupFirstDenyWins(t *testing.T) {
	tool := riskTool{name: "danger", risk: toolruntime.RiskNetwork}

	var consultOrder []string
	record := func(name string, deny bool, reason string) GateFunc {
		return func(context.Context, toolruntime.Tool) (bool, string) {
			consultOrder = append(consultOrder, name)
			return deny, reason
		}
	}

	g := NewGateGroup().
		Use(record("first", true, "first reason")).
		Use(record("second", true, "second reason"))
	gotDeny, gotReason := g.GateCheck()(context.Background(), tool)
	if !gotDeny || gotReason != "first reason" {
		t.Errorf("deny=%v reason=%q, want the FIRST gate's deny (true, %q)", gotDeny, gotReason, "first reason")
	}
	if len(consultOrder) != 1 || consultOrder[0] != "first" {
		t.Errorf("consulted %v, want only [first] — a deny must short-circuit", consultOrder)
	}
}

// TestGateGroupAllAllowPasses pins the allow path: when every gate allows, the
// composed func allows, and every gate was consulted (no short-circuit on
// allow).
func TestGateGroupAllAllowPasses(t *testing.T) {
	tool := riskTool{name: "safe", risk: toolruntime.RiskReadOnly}

	consulted := 0
	allow := func(context.Context, toolruntime.Tool) (bool, string) {
		consulted++
		return false, ""
	}

	g := NewGateGroup().Use(allow).Use(allow).Use(allow)
	deny, reason := g.GateCheck()(context.Background(), tool)
	if deny || reason != "" {
		t.Errorf("deny=%v reason=%q, want allow (false, %q)", deny, reason, "")
	}
	if consulted != 3 {
		t.Errorf("consulted %d gates, want all 3", consulted)
	}
}

// TestGateGroupSkipsNilAndEmpty pins the defensive edges: a nil gate is
// skipped, and an empty group allows.
func TestGateGroupSkipsNilAndEmpty(t *testing.T) {
	tool := riskTool{name: "t", risk: toolruntime.RiskReadOnly}

	if deny, _ := NewGateGroup().GateCheck()(context.Background(), tool); deny {
		t.Error("empty group denied — want allow")
	}

	g := NewGateGroup().Use(nil).Use(denyNetwork)
	deny, reason := g.GateCheck()(context.Background(), riskTool{name: "net", risk: toolruntime.RiskNetwork})
	if !deny || reason == "" {
		t.Errorf("nil gate broke the chain: deny=%v reason=%q, want the network deny", deny, reason)
	}
}

// TestGateGroupIsGateFuncProvider pins the marker contract: a GateGroup
// registers into a loop anywhere a GateFuncProvider or named middleware does.
func TestGateGroupIsGateFuncProvider(t *testing.T) {
	var _ GateFuncProvider = NewGateGroup()
	var _ Middleware = NewGateGroup()
	if NewGateGroup().MiddlewareName() != "gate-group" {
		t.Error("MiddlewareName drifted")
	}
}

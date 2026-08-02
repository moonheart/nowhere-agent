package agent

import (
	"context"
	"testing"

	"nowhere-agent/internal/toolruntime"
)

// TestGateFuncResolvesSessionModeFromContext pins the per-session permission
// toggle at the middleware seam: a single GateFunc, registered once, reads the
// owning session's mode from the run CONTEXT at call time — so flipping the
// session's mode changes the verdict with no loop rebuild and no re-registration,
// and it lifts ONLY the approval gate (an env deny still blocks).
func TestGateFuncResolvesSessionModeFromContext(t *testing.T) {
	// Per-session mode lookup, keyed by session id — the production reader is the
	// session state store; here a map stands in.
	modes := map[string]string{"sess-a": "allow_all", "sess-b": "auto"}
	// Base policy: ask every tool (approval marker) except a hard-denied one.
	permit := func(ctx context.Context, tool toolruntime.Tool) (bool, string) {
		if tool.Name() == "denied" {
			return true, "denied by policy"
		}
		if modes[SessionIDFromContext(ctx)] == "allow_all" {
			return false, "" // allow_all lifts the approval gate
		}
		return true, ApprovalReasonPrefix + "ask"
	}

	tool := riskTool{name: "edit", risk: toolruntime.RiskExternalWrite}

	// allow_all session: the approval gate is lifted (no prompt).
	if deny, _ := permit(ContextWithSessionID(context.Background(), "sess-a"), tool); deny {
		t.Error("allow_all session should bypass the approval gate")
	}
	// auto session: the approval gate still prompts.
	if deny, reason := permit(ContextWithSessionID(context.Background(), "sess-b"), tool); !deny || !IsApprovalReason(reason) {
		t.Errorf("auto session should still gate for approval, got (%v, %q)", deny, reason)
	}
	// No session on the context: falls back to the safe default (gated).
	if deny, reason := permit(context.Background(), tool); !deny || !IsApprovalReason(reason) {
		t.Errorf("no session id should fall back to gated, got (%v, %q)", deny, reason)
	}
	// A hard deny is NOT lifted by allow_all.
	denied := riskTool{name: "denied", risk: toolruntime.RiskExternalWrite}
	if deny, reason := permit(ContextWithSessionID(context.Background(), "sess-a"), denied); !deny || IsApprovalReason(reason) {
		t.Errorf("allow_all must not lift a hard deny, got (%v, %q)", deny, reason)
	}
}

// TestContextWithSessionIDRoundTrip pins the context carrier the run registry
// uses to tag a run's context with its owning session id.
func TestContextWithSessionIDRoundTrip(t *testing.T) {
	if got := SessionIDFromContext(context.Background()); got != "" {
		t.Errorf("empty context session id = %q, want empty", got)
	}
	ctx := ContextWithSessionID(context.Background(), "sess-123")
	if got := SessionIDFromContext(ctx); got != "sess-123" {
		t.Errorf("session id = %q, want sess-123", got)
	}
}

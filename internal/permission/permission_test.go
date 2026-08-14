package permission

import (
	"testing"
	"time"

	"context"
	"nowhere-agent/internal/toolruntime"
)

type stubTool struct{ risk toolruntime.Risk }

func (s stubTool) Name() string           { return "stub" }
func (s stubTool) Description() string    { return "" }
func (s stubTool) Schema() map[string]any { return nil }
func (s stubTool) Risk() toolruntime.Risk { return s.risk }
func (s stubTool) Timeout() time.Duration { return 0 }
func (s stubTool) Call(context.Context, map[string]any) (toolruntime.Result, error) {
	return toolruntime.Result{}, nil
}

// DefaultPolicy mirrors the config defaults (PERMISSION_*): read-only,
// sandbox-write and network are allowed (the wired web-search tool runs over
// network); only external-write asks.
func TestDefaultPolicyPermissiveInsideSandbox(t *testing.T) {
	c := NewChecker(DefaultPolicy())
	if v := c.Check(stubTool{toolruntime.RiskReadOnly}); v != Allow {
		t.Errorf("read_only -> %v want allow", v)
	}
	if v := c.Check(stubTool{toolruntime.RiskSandboxWrite}); v != Allow {
		t.Errorf("sandbox_write -> %v want allow", v)
	}
	if v := c.Check(stubTool{toolruntime.RiskNetwork}); v != Allow {
		t.Errorf("network -> %v want allow (matches PERMISSION_NETWORK default)", v)
	}
}

func TestDefaultPolicyGatesExternalWrite(t *testing.T) {
	c := NewChecker(DefaultPolicy())
	if v := c.Check(stubTool{toolruntime.RiskExternalWrite}); v != Ask {
		t.Errorf("external_write -> %v want ask", v)
	}
}

func TestCustomPolicy(t *testing.T) {
	p := Policy{
		ReadOnly:      DecisionAllow,
		SandboxWrite:  DecisionDeny,
		Network:       DecisionDeny,
		ExternalWrite: DecisionAllow,
	}
	c := NewChecker(p)
	if v := c.Check(stubTool{toolruntime.RiskSandboxWrite}); v != Deny {
		t.Errorf("sandbox_write -> %v want deny", v)
	}
	if v := c.Check(stubTool{toolruntime.RiskExternalWrite}); v != Allow {
		t.Errorf("external_write -> %v want allow", v)
	}
}

func TestNewApprovalUniqueIDs(t *testing.T) {
	a := NewApproval("run1", "tool", "detail")
	b := NewApproval("run1", "tool", "detail")
	if a.ID == b.ID {
		t.Error("approval IDs should be unique")
	}
	if a.RunID != "run1" || a.ToolName != "tool" {
		t.Errorf("approval fields wrong: %+v", a)
	}
}

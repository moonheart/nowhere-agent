// Package permission implements execution-permission (design D10): runtime
// authorization of agent actions. It is permissive inside the sandbox and
// gates sandbox-escaping actions behind a configurable allow/ask/deny policy.
// This is distinct from resource permission (identity-scope), which governs
// data ownership.
package permission

import (
	"github.com/google/uuid"

	"nowhere-agent/internal/toolruntime"
)

// Verdict is the outcome of a permission check.
type Verdict string

const (
	// Allow lets the action proceed without user involvement.
	Allow Verdict = "allow"
	// Ask requires explicit user approval before proceeding.
	Ask Verdict = "ask"
	// Deny blocks the action.
	Deny Verdict = "deny"
)

// Decision is a policy verdict for a risk level.
type Decision string

const (
	DecisionAllow Decision = "allow"
	DecisionAsk   Decision = "ask"
	DecisionDeny  Decision = "deny"
)

// Policy maps a tool's risk level to a decision. Sandbox-contained risks
// (read-only, sandbox-write) default to allow; escaping risks default to ask
// unless configured otherwise.
type Policy struct {
	ReadOnly      Decision
	SandboxWrite  Decision
	Network       Decision
	ExternalWrite Decision
}

// DefaultPolicy is permissive inside the sandbox and asks for escaping actions.
func DefaultPolicy() Policy {
	return Policy{
		ReadOnly:      DecisionAllow,
		SandboxWrite:  DecisionAllow,
		Network:       DecisionAsk,
		ExternalWrite: DecisionAsk,
	}
}

// decisionFor resolves the decision for a risk level.
func (p Policy) decisionFor(r toolruntime.Risk) Decision {
	switch r {
	case toolruntime.RiskReadOnly:
		return p.ReadOnly
	case toolruntime.RiskSandboxWrite:
		return p.SandboxWrite
	case toolruntime.RiskNetwork:
		return p.Network
	case toolruntime.RiskExternalWrite:
		return p.ExternalWrite
	default:
		return DecisionAsk
	}
}

// Checker evaluates whether a tool call may proceed under a policy.
type Checker struct {
	policy Policy
}

// NewChecker creates a Checker with the given policy.
func NewChecker(p Policy) *Checker { return &Checker{policy: p} }

// Check evaluates a tool by its risk level.
func (c *Checker) Check(t toolruntime.Tool) Verdict {
	return Verdict(c.policy.decisionFor(t.Risk()))
}

// Approval is a pending request for the user to authorize an action.
type Approval struct {
	ID       string
	RunID    string
	ToolName string
	Detail   string
}

// NewApproval builds an Approval for a gated tool call.
func NewApproval(runID, toolName, detail string) Approval {
	return Approval{
		ID:       uuid.NewString(),
		RunID:    runID,
		ToolName: toolName,
		Detail:   detail,
	}
}

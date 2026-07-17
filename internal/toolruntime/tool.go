// Package toolruntime implements the tool-runtime capability (design D9): all
// tools — built-in, skill scripts, or external MCP servers — are exposed to the
// agent loop through one Tool interface. The loop never depends on a tool's
// origin.
package toolruntime

import (
	"context"
	"time"
)

// Risk classifies a tool's danger level; execution-permission (D10) uses it to
// decide whether an action needs approval.
type Risk string

const (
	// RiskReadOnly makes no external change (read file, list dir).
	RiskReadOnly Risk = "read_only"
	// RiskSandboxWrite mutates only inside the session sandbox.
	RiskSandboxWrite Risk = "sandbox_write"
	// RiskNetwork reaches outside the sandbox (egress).
	RiskNetwork Risk = "network"
	// RiskExternalWrite mutates state outside the session workspace.
	RiskExternalWrite Risk = "external_write"
)

// Result is the outcome of a tool call. Content and Error are fed back to the
// model as a tool-result block so it can self-correct.
type Result struct {
	Content string
	// IsError marks the result as a failure (model sees it as an error).
	IsError bool
}

// Tool is the unified contract. Implementations must be safe for concurrent use.
type Tool interface {
	// Name is the tool's unique identifier (used in function-calling).
	Name() string
	// Description tells the model what the tool does.
	Description() string
	// Schema is the JSON Schema for the tool's input.
	Schema() map[string]any
	// Risk classifies the tool for permission checks.
	Risk() Risk
	// Timeout bounds a single call; zero uses a default.
	Timeout() time.Duration
	// Call executes the tool with JSON-decoded args.
	Call(ctx context.Context, args map[string]any) (Result, error)
}

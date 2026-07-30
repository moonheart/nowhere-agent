// Package toolruntime implements the tool-runtime capability (design D9): all
// tools — built-in, skill scripts, or external MCP servers — are exposed to the
// agent loop through one Tool interface. The loop never depends on a tool's
// origin.
package toolruntime

import (
	"context"
	"time"

	"nowhere-agent/internal/provider"
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
	// Nested carries a tool call's nested content blocks (a subagent's thinking /
	// text / tool-call conversation), for spawn_agent results. It is persisted
	// and replayed to the UI as the sub-conversation; it is NEVER fed back to the
	// model (the model only sees the collapsed Content).
	Nested []provider.Block
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

// ClientTool is a Tool that executes in the client (browser), not on the server
// (general interrupt). The loop detects it via this OPTIONAL interface — the
// base Tool contract is untouched — and suspends the run instead of dispatching:
// the call is handed to the client, which executes it and returns the output,
// folded back as the tool result on resume. Call is never reached in the gated
// path (the loop intercepts first), mirroring ask_user.
type ClientTool interface {
	Tool
	// ClientSide reports that this tool runs in the client. true → suspend.
	ClientSide() bool
	// OutputSchema, when non-nil, declares the shape of the output the client
	// returns. The server validates the returned output against it before folding
	// (declare + validate), so client output is never trusted blindly. Nil accepts
	// any output.
	OutputSchema() map[string]any
}

// IsClientTool reports whether t is a client-side tool (implements ClientTool
// with ClientSide() == true). The loop uses it at the unified suspend point.
func IsClientTool(t Tool) bool {
	ct, ok := t.(ClientTool)
	return ok && ct.ClientSide()
}

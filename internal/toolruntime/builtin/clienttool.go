package builtin

import (
	"context"
	"fmt"
	"time"

	"nowhere-agent/internal/toolruntime"
)

// clientTool is a client-side tool (general interrupt): the model calls it, the
// loop SUSPENDS the run (like an approval / ask_user) and hands the call to the
// client, which executes it and returns the output — folded back as the tool
// result on resume. The server never runs it. Call is unreachable in the gated
// path (the loop intercepts before dispatch), so it only runs if a server wires
// the tool without the interaction gate, where it explains the misconfiguration.
type clientTool struct {
	name         string
	description  string
	inputSchema  map[string]any
	outputSchema map[string]any // nil → accept any client output
}

// NewClientTool declares a client-side tool: name/description/inputSchema are
// shown to the model (the tool's calling contract); outputSchema, when non-nil,
// declares the shape of the output the client must return, which the server
// validates before folding (declare + validate). RiskReadOnly so the permission
// gate leaves the suspend to the interaction gate. It implements
// toolruntime.ClientTool.
func NewClientTool(name, description string, inputSchema, outputSchema map[string]any) toolruntime.Tool {
	return clientTool{name: name, description: description, inputSchema: inputSchema, outputSchema: outputSchema}
}

func (c clientTool) Name() string           { return c.name }
func (c clientTool) Description() string    { return c.description }
func (c clientTool) Schema() map[string]any { return c.inputSchema }
func (c clientTool) Risk() toolruntime.Risk { return toolruntime.RiskReadOnly }
func (c clientTool) Timeout() time.Duration { return 0 } // no execution timeout: it runs in the client

// ClientSide marks the tool as client-executed: the loop suspends on it.
func (c clientTool) ClientSide() bool { return true }

// OutputSchema declares the client output's shape for server-side validation.
func (c clientTool) OutputSchema() map[string]any { return c.outputSchema }

// Call should never run in the gated path (the loop suspends first). If reached,
// the server wired the tool without the interaction gate.
func (c clientTool) Call(context.Context, map[string]any) (toolruntime.Result, error) {
	return toolruntime.Result{
		Content: fmt.Sprintf("%s is a client-side tool: it must be driven by the agent loop's interaction gate (the run suspends and the client executes it); it cannot execute on the server", c.name),
		IsError: true,
	}, nil
}

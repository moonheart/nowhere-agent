package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"nowhere-agent/internal/toolruntime"
)

// InteractionHandler folds a resolved Interaction into the tool_result a fresh
// run appends (general interrupt resume). One handler per Kind, registered on
// the RunRegistry; the per-kind knowledge (how to turn the client's Result into
// a tool_result) lives here, not in an inline switch on the resume path, so a
// new interaction kind is added by registering a handler — the resume path never
// changes.
type InteractionHandler interface {
	// Fold turns the client's Result into the tool_result for the suspended call.
	// tools is the session-bound registry, needed when a handler must execute an
	// approved call; it may be nil for handlers that never execute. approve is the
	// verdict's boolean arm (approved vs rejected/skipped) for kinds that need it.
	Fold(ctx context.Context, in Interaction, approve bool, tools *toolruntime.Registry) (toolruntime.Result, error)
}

// interactionHandlers is the registry's kind → handler map, with the three
// built-in kinds wired by default.
func defaultInteractionHandlers() map[string]InteractionHandler {
	return map[string]InteractionHandler{
		KindToolApproval: toolApprovalHandler{},
		KindAskUser:      askUserHandler{},
		KindClientTool:   clientToolHandler{},
	}
}

// foldInteraction resolves an interaction's kind and delegates to its handler.
// An unregistered kind is a clear error (a handler was never wired for it).
func (rg *RunRegistry) foldInteraction(ctx context.Context, in Interaction, approve bool, tools *toolruntime.Registry) (toolruntime.Result, error) {
	kind := in.Kind
	if kind == "" {
		kind = KindToolApproval
	}
	rg.mu.Lock()
	h, ok := rg.interactionHandlers[kind]
	rg.mu.Unlock()
	if !ok {
		return toolruntime.Result{}, fmt.Errorf("no interaction handler registered for kind %q", kind)
	}
	return h.Fold(ctx, in, approve, tools)
}

// --- tool_approval: execute the gated call on approve, deny on reject --------

type toolApprovalHandler struct{}

func (toolApprovalHandler) Fold(ctx context.Context, in Interaction, approve bool, tools *toolruntime.Registry) (toolruntime.Result, error) {
	if !approve {
		return toolruntime.Result{Content: "the user denied permission to run " + in.ToolName, IsError: true}, nil
	}
	// Approved: execute the gated tool now (the approval is its authorization).
	if tools == nil {
		return toolruntime.Result{}, errors.New("approved call needs a tool registry to execute")
	}
	var input map[string]any
	_ = json.Unmarshal(in.Payload, &input)
	return tools.CallAll(ctx, []toolruntime.Call{{ID: in.ToolCallID, Name: in.ToolName, Args: input}})[0], nil
}

// --- ask_user: fold the structured answers, or a skip note --------------------

type askUserHandler struct{}

func (askUserHandler) Fold(_ context.Context, in Interaction, approve bool, _ *toolruntime.Registry) (toolruntime.Result, error) {
	if !approve {
		// Skipped: the run continues without the input; the model decides next.
		return toolruntime.Result{Content: "the user skipped these questions (no answer given)"}, nil
	}
	if len(in.Result) > 0 {
		if data, err := json.Marshal(in.Result); err == nil {
			return toolruntime.Result{Content: string(data)}, nil
		}
	}
	return toolruntime.Result{Content: "the user answered (unparseable response)", IsError: true}, nil
}

// --- client_tool: fold the client's output (validated) or an error ------------
// The server never executes a client-side tool; it folds what the client
// returned. Output is validated against the tool's declared output schema before
// folding so the server does not blindly trust client output (design: declare +
// validate). A mismatch or a client-reported error becomes an is_error result so
// the model self-corrects — the same malformed→error pattern the loop uses for
// bad tool args.

type clientToolHandler struct{}

func (clientToolHandler) Fold(_ context.Context, in Interaction, _ bool, tools *toolruntime.Registry) (toolruntime.Result, error) {
	var res struct {
		Output any    `json:"output"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(in.Result, &res); err != nil {
		return toolruntime.Result{Content: "client returned an unparseable result for " + in.ToolName, IsError: true}, nil
	}
	if res.Error != "" {
		return toolruntime.Result{Content: "client-side tool " + in.ToolName + " failed: " + res.Error, IsError: true}, nil
	}
	// Validate the output against the tool's declared output schema when the tool
	// is available and declares one. A client tool that can't be looked up (or
	// declares no schema) is accepted as-is.
	if tools != nil {
		if tool, ok := tools.Get(in.ToolName); ok {
			if ct, isClient := tool.(interface{ OutputSchema() map[string]any }); isClient {
				if schema := ct.OutputSchema(); schema != nil {
					if verr := toolruntime.ValidateOutput(schema, res.Output); verr != nil {
						return toolruntime.Result{Content: "client output for " + in.ToolName + " does not match its declared output schema: " + verr.Error(), IsError: true}, nil
					}
				}
			}
		}
	}
	data, err := json.Marshal(res.Output)
	if err != nil {
		return toolruntime.Result{Content: "client output for " + in.ToolName + " could not be encoded", IsError: true}, nil
	}
	if len(data) == 0 || string(data) == "null" {
		return toolruntime.Result{Content: "(no output)"}, nil
	}
	return toolruntime.Result{Content: string(data)}, nil
}

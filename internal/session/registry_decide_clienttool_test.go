package session

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"nowhere-agent/internal/toolruntime"
)

// clientSideTool is a toolruntime.ClientTool for the fold tests: it declares an
// output schema the client_tool handler validates against.
type clientSideTool struct {
	name string
}

func (c clientSideTool) Name() string           { return c.name }
func (c clientSideTool) Description() string    { return "client tool" }
func (c clientSideTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (c clientSideTool) Risk() toolruntime.Risk { return toolruntime.RiskReadOnly }
func (c clientSideTool) Timeout() time.Duration { return 0 }
func (c clientSideTool) ClientSide() bool       { return true }
func (c clientSideTool) OutputSchema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"text": map[string]any{"type": "string"}},
		"required":   []string{"text"},
	}
}
func (c clientSideTool) Call(context.Context, map[string]any) (toolruntime.Result, error) {
	return toolruntime.Result{Content: "server-ran (should not happen)", IsError: true}, nil
}

// seedClientToolRun persists a user + assistant(client_tool tool_use) pair and a
// pending client_tool interaction, returning the interaction and the registry
// holding the client tool (for output-schema validation).
func seedClientToolRun(t *testing.T, rg *RunRegistry, ms MessageStore, sess Session) (Interaction, *toolruntime.Registry) {
	t.Helper()
	run, _ := rg.rt.store.CreateRun(context.Background(), sess.ID, 1)
	seedGatedConversation(t, rg, ms, sess.ID, run.ID, "tu1", "get_clipboard", map[string]any{})
	ap := createSuspendedInteraction(t, rg, []string{"tu1"}, Interaction{
		RunID: run.ID, SessionID: sess.ID, ToolCallID: "tu1", ToolName: "get_clipboard",
		Kind: KindClientTool,
	})
	reg := toolruntime.NewRegistry()
	reg.Register(clientSideTool{name: "get_clipboard"})
	return ap, reg
}

// TestDecideClientToolValidOutput: a conforming client output is folded as the
// (non-error) tool_result on resume.
func TestDecideClientToolValidOutput(t *testing.T) {
	rg, ms, sess := newDecideRegistry(t)
	ap, reg := seedClientToolRun(t, rg, ms, sess)

	result := json.RawMessage(`{"output":{"text":"copied text"}}`)
	_, history, err := rg.Decide(context.Background(), ap.ID, true, result, reg, nil, nil)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	tr := history[len(history)-1].Content[0]
	if tr.IsError {
		t.Fatalf("valid client output should not be an error, got %+v", tr)
	}
	if tr.ToolResultID != "tu1" || tr.ToolContent != `{"text":"copied text"}` {
		t.Errorf("tool_result = %+v, want the folded client output", tr)
	}
}

// TestDecideClientToolInvalidOutput: output violating the declared output schema
// becomes an is_error result (not trusted blindly) so the model self-corrects.
func TestDecideClientToolInvalidOutput(t *testing.T) {
	rg, ms, sess := newDecideRegistry(t)
	ap, reg := seedClientToolRun(t, rg, ms, sess)

	// Missing the required "text" property.
	result := json.RawMessage(`{"output":{"wrong":123}}`)
	_, history, err := rg.Decide(context.Background(), ap.ID, true, result, reg, nil, nil)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	tr := history[len(history)-1].Content[0]
	if !tr.IsError {
		t.Errorf("schema-violating output should be is_error, got %+v", tr)
	}
}

// TestDecideClientToolReportedError: a client-reported error folds as is_error.
func TestDecideClientToolReportedError(t *testing.T) {
	rg, ms, sess := newDecideRegistry(t)
	ap, reg := seedClientToolRun(t, rg, ms, sess)

	result := json.RawMessage(`{"error":"clipboard access denied"}`)
	_, history, err := rg.Decide(context.Background(), ap.ID, true, result, reg, nil, nil)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	tr := history[len(history)-1].Content[0]
	if !tr.IsError {
		t.Errorf("client-reported error should be is_error, got %+v", tr)
	}
}

// TestDecideUnknownKindErrors: a kind with no registered handler is a clear error.
func TestDecideUnknownKindErrors(t *testing.T) {
	rg, ms, sess := newDecideRegistry(t)
	run, _ := rg.rt.store.CreateRun(context.Background(), sess.ID, 1)
	seedGatedConversation(t, rg, ms, sess.ID, run.ID, "tu1", "mystery", map[string]any{})
	ap := createSuspendedInteraction(t, rg, []string{"tu1"}, Interaction{
		RunID: run.ID, SessionID: sess.ID, ToolCallID: "tu1", ToolName: "mystery", Kind: "mystery_kind",
	})
	if _, _, err := rg.Decide(context.Background(), ap.ID, true, nil, nil, nil, nil); err == nil {
		t.Fatal("an unregistered interaction kind should error, not silently fold")
	}
}

// TestRegisterInteractionHandlerOverride: a custom handler replaces the default
// for a kind, and Fold receives the interaction + verdict.
func TestRegisterInteractionHandlerOverride(t *testing.T) {
	rg, ms, sess := newDecideRegistry(t)
	run, _ := rg.rt.store.CreateRun(context.Background(), sess.ID, 1)
	seedGatedConversation(t, rg, ms, sess.ID, run.ID, "tu1", "ask_user", map[string]any{})
	ap := createSuspendedInteraction(t, rg, []string{"tu1"}, Interaction{
		RunID: run.ID, SessionID: sess.ID, ToolCallID: "tu1", ToolName: "ask_user", Kind: KindAskUser,
	})

	rg.RegisterInteractionHandler(KindAskUser, foldFunc(func(_ context.Context, in Interaction, approve bool, _ *toolruntime.Registry, _ ToolExecutor) (toolruntime.Result, error) {
		return toolruntime.Result{Content: "custom fold for " + in.ToolName}, nil
	}))

	_, history, err := rg.Decide(context.Background(), ap.ID, true, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	tr := history[len(history)-1].Content[0]
	if tr.ToolContent != "custom fold for ask_user" {
		t.Errorf("custom handler not used, got %q", tr.ToolContent)
	}
}

// foldFunc adapts a function to InteractionHandler for tests.
type foldFunc func(context.Context, Interaction, bool, *toolruntime.Registry, ToolExecutor) (toolruntime.Result, error)

func (f foldFunc) Fold(ctx context.Context, in Interaction, approve bool, tools *toolruntime.Registry, exec ToolExecutor) (toolruntime.Result, error) {
	return f(ctx, in, approve, tools, exec)
}

package chatapi

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/session"
)

// TestEmitStreamEventInterruptFrame pins the translator half of the interaction
// suspend: a broker StreamEvent of kind interrupt must render a transient
// data-interaction frame carrying the interaction id, kind, tool-call id, tool
// name, and args — the frame the client turns into the approval / ask_user /
// client-tool card. The routing half (interrupt reaches the broker at all) is
// covered in package session by TestIsContentKindRoutesInterruptToBroker; a
// regression in either half makes a suspended run "just end" with no prompt.
func TestEmitStreamEventInterruptFrame(t *testing.T) {
	rec := httptest.NewRecorder()
	emitter := &sseEmitter{w: rec, flusher: rec, msgID: "m", textID: "text-1", thinkID: "reasoning-1"}

	payload, err := json.Marshal(agent.Interaction{
		ID:         "int-1",
		Kind:       "client_tool",
		ToolCallID: "tc-1",
		ToolName:   "sleep",
		Input:      map[string]any{"seconds": 3},
	})
	if err != nil {
		t.Fatalf("marshal interaction: %v", err)
	}
	req := httptest.NewRequest("POST", "/api/chat/resume", nil)
	emitStreamEvent(req, emitter, session.StreamEvent{
		RunID:   "r1",
		Kind:    string(agent.KindInterrupt),
		Payload: payload,
	})

	body := rec.Body.String()
	for _, want := range []string{
		`"type":"data-interaction"`,
		`"interactionId":"int-1"`,
		`"kind":"client_tool"`,
		`"toolCallId":"tc-1"`,
		`"toolName":"sleep"`,
		`"transient":true`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("data-interaction frame missing %s\n---\n%s", want, body)
		}
	}
}

// TestEmitInterruptFrameDirect pins the direct-path half of the same contract:
// the loop hands sseEmitter.Emit the agent.Interaction STRUCT (serveChatDirect,
// the no-runtime path), and the type assertion must not silently drop the
// frame. Without the struct branch the run "just ends" with no prompt.
func TestEmitInterruptFrameDirect(t *testing.T) {
	rec := httptest.NewRecorder()
	emitter := &sseEmitter{w: rec, flusher: rec, msgID: "m", textID: "text-1", thinkID: "reasoning-1"}

	if err := emitter.Emit(t.Context(), agent.KindInterrupt, agent.Interaction{
		ID:         "int-2",
		Kind:       "ask_user",
		ToolCallID: "tc-2",
		ToolName:   "ask_user",
		Input:      map[string]any{"questions": []any{"how?"}},
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}

	body := rec.Body.String()
	for _, want := range []string{
		`"type":"data-interaction"`,
		`"interactionId":"int-2"`,
		`"kind":"ask_user"`,
		`"toolCallId":"tc-2"`,
		`"toolName":"ask_user"`,
		`"transient":true`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("data-interaction frame missing %s\n---\n%s", want, body)
		}
	}
}

// TestEmitInterruptFrameDirectLowercaseKeys pins the map branch's tolerance for
// a JSON round-trip with lowercase keys, so a payload that was lowercased by
// storage never silently disables the interaction card.
func TestEmitInterruptFrameDirectLowercaseKeys(t *testing.T) {
	rec := httptest.NewRecorder()
	emitter := &sseEmitter{w: rec, flusher: rec, msgID: "m", textID: "text-1", thinkID: "reasoning-1"}

	if err := emitter.Emit(t.Context(), agent.KindInterrupt, map[string]any{
		"id":         "int-3",
		"kind":       "",
		"toolCallID": "tc-3",
		"toolName":   "sleep",
		"input":      map[string]any{"seconds": float64(3)},
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}

	body := rec.Body.String()
	for _, want := range []string{
		`"type":"data-interaction"`,
		`"interactionId":"int-3"`,
		`"kind":"approval"`,
		`"toolCallId":"tc-3"`,
		`"toolName":"sleep"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("data-interaction frame missing %s\n---\n%s", want, body)
		}
	}
}

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

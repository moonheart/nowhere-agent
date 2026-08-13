// Package chatapi implements the /api/chat endpoint: it bridges the
// assistant-ui data-stream runtime (HTTP POST + SSE) to the agent loop,
// speaking the AI SDK v6 "ui-message-stream" SSE protocol back to the browser.
package chatapi

import (
	"encoding/json"
)

// ui-message-stream chunk types (subset we emit).
type chunk map[string]any

// sseFrame renders one chunk as an SSE "data:" frame.
func sseFrame(c chunk) string {
	b, _ := json.Marshal(c)
	return "data: " + string(b) + "\n\n"
}

// decodeTextPayload extracts a string from a persisted event payload, tolerating
// both a JSON-encoded string and a raw byte blob.
func decodeTextPayload(payload []byte) string {
	var s string
	if err := json.Unmarshal(payload, &s); err != nil {
		return string(payload)
	}
	return s
}

// decodeMapPayload extracts an object from a persisted event payload.
func decodeMapPayload(payload []byte) (map[string]any, bool) {
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		return nil, false
	}
	return m, true
}

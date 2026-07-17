// Package chatapi implements the /api/chat endpoint: it bridges the
// assistant-ui data-stream runtime (HTTP POST + SSE) to the agent loop,
// speaking the AI SDK v6 "ui-message-stream" SSE protocol back to the browser.
package chatapi

import (
	"encoding/json"
	"fmt"
)

// ui-message-stream chunk types (subset we emit).
type chunk map[string]any

// sseFrame renders one chunk as an SSE "data:" frame.
func sseFrame(c chunk) string {
	b, _ := json.Marshal(c)
	return "data: " + string(b) + "\n\n"
}

// streamWriter accumulates the SSE body for a run.
type streamWriter struct {
	buf       []byte
	messageID string
}

func newStreamWriter(messageID string) *streamWriter {
	return &streamWriter{messageID: messageID}
}

func (w *streamWriter) start() {
	w.buf = append(w.buf, sseFrame(chunk{"type": "start", "messageId": w.messageID})...)
}

func (w *streamWriter) textStart(id string) {
	w.buf = append(w.buf, sseFrame(chunk{"type": "text-start", "id": id})...)
}

func (w *streamWriter) textDelta(id, delta string) {
	w.buf = append(w.buf, sseFrame(chunk{"type": "text-delta", "id": id, "delta": delta})...)
}

func (w *streamWriter) textEnd(id string) {
	w.buf = append(w.buf, sseFrame(chunk{"type": "text-end", "id": id})...)
}

func (w *streamWriter) toolCallStart(toolCallID, toolName string) {
	w.buf = append(w.buf, sseFrame(chunk{"type": "tool-call-start", "toolCallId": toolCallID, "toolName": toolName})...)
}

func (w *streamWriter) toolCallDelta(toolCallID, argsText string) {
	w.buf = append(w.buf, sseFrame(chunk{"type": "tool-call-delta", "toolCallId": toolCallID, "argsText": argsText})...)
}

func (w *streamWriter) toolCallEnd(toolCallID string) {
	w.buf = append(w.buf, sseFrame(chunk{"type": "tool-call-end", "toolCallId": toolCallID})...)
}

func (w *streamWriter) toolResult(toolCallID string, result any, isError bool) {
	w.buf = append(w.buf, sseFrame(chunk{"type": "tool-result", "toolCallId": toolCallID, "result": result, "isError": isError})...)
}

func (w *streamWriter) error(text string) {
	w.buf = append(w.buf, sseFrame(chunk{"type": "error", "errorText": text})...)
}

func (w *streamWriter) finish() {
	w.buf = append(w.buf, sseFrame(chunk{
		"type":         "finish",
		"finishReason": "stop",
		"usage":        map[string]any{"inputTokens": 0, "outputTokens": 0},
	})...)
}

// done appends the terminal [DONE] marker the decoder requires.
func (w *streamWriter) done() {
	w.buf = append(w.buf, "data: [DONE]\n\n"...)
}

// String renders the accumulated body.
func (w *streamWriter) String() string {
	return string(w.buf)
}

var _ = fmt.Sprintf // keep fmt imported for future use

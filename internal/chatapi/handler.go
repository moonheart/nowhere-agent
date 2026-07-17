package chatapi

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"

	"github.com/google/uuid"

	"nowhere-agent/internal/agent"
)

// LoopFactory builds an agent loop for a chat request (provider + tools wired
// by the server). Keeping it a factory lets the handler stay transport-only.
type LoopFactory func(ctx context.Context) *agent.Loop

// Handler serves POST /api/chat.
type Handler struct {
	newLoop LoopFactory
	system  string
}

// NewHandler creates a chat Handler.
func NewHandler(newLoop LoopFactory, systemPrompt string) *Handler {
	return &Handler{newLoop: newLoop, system: systemPrompt}
}

// Register mounts the route.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/chat", h.serveChat)
}

// sseEmitter adapts agent.Emitter to write ui-message-stream frames live.
type sseEmitter struct {
	w           http.ResponseWriter
	flusher     http.Flusher
	mu          sync.Mutex
	msgID       string
	textID      string
	textStarted bool
}

func (h *Handler) serveChat(w http.ResponseWriter, r *http.Request) {
	var req dataStreamRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	// SSE headers for ui-message-stream.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("x-vercel-ai-ui-message-stream", "v1")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	emitter := &sseEmitter{w: w, flusher: flusher, msgID: uuid.NewString(), textID: "text-1"}
	emitter.write(chunk{"type": "start", "messageId": emitter.msgID})

	loop := h.newLoop(r.Context())
	history := toHistory(req)

	_, err := loop.Run(r.Context(), history, emitter)
	if err != nil {
		emitter.Emit(r.Context(), agent.KindError, err.Error())
	}

	emitter.finish()
}

// Emit implements agent.Emitter, streaming frames as the loop produces them.
func (e *sseEmitter) Emit(_ context.Context, kind agent.EventKind, payload any) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	switch kind {
	case agent.KindText:
		if !e.textStarted {
			e.write(chunk{"type": "text-start", "id": e.textID})
			e.textStarted = true
		}
		if s, ok := payload.(string); ok {
			e.write(chunk{"type": "text-delta", "id": e.textID, "delta": s})
		}
	case agent.KindToolUse:
		if m, ok := payload.(map[string]any); ok {
			id, _ := m["id"].(string)
			name, _ := m["name"].(string)
			e.write(chunk{"type": "tool-call-start", "toolCallId": id, "toolName": name})
			e.write(chunk{"type": "tool-call-end", "toolCallId": id})
		}
	case agent.KindToolResult:
		if m, ok := payload.(map[string]any); ok {
			id, _ := m["tool_use_id"].(string)
			isErr, _ := m["is_error"].(bool)
			e.write(chunk{"type": "tool-result", "toolCallId": id, "result": m["content"], "isError": isErr})
		}
	case agent.KindError:
		if s, ok := payload.(string); ok {
			e.write(chunk{"type": "error", "errorText": s})
		}
	}
	return nil
}

// finish closes the text block, sends finish + [DONE].
func (e *sseEmitter) finish() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.textStarted {
		e.write(chunk{"type": "text-end", "id": e.textID})
	}
	e.write(chunk{
		"type":         "finish",
		"finishReason": "stop",
		"usage":        map[string]any{"inputTokens": 0, "outputTokens": 0},
	})
	e.writeRaw("data: [DONE]\n\n")
	e.flusher.Flush()
}

func (e *sseEmitter) write(c chunk) {
	e.writeRaw(sseFrame(c))
	e.flusher.Flush()
}

func (e *sseEmitter) writeRaw(s string) {
	_, _ = e.w.Write([]byte(s))
}

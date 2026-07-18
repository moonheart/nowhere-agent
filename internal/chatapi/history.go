package chatapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/session"
)

// historyMessage is the assistant-ui ThreadMessageLike shape the web client's
// history.load() consumes (via ExportedMessageRepository.fromArray).
type historyMessage struct {
	ID      string        `json:"id"`
	Role    string        `json:"role"`
	Content []historyPart `json:"content"`
}

type historyPart struct {
	Type string `json:"type"` // "text" | "reasoning"
	Text string `json:"text"`
}

// serveHistory handles GET /api/chat/history?threadId=<id>: it rebuilds the
// conversation from the session's durable run_events so a reloading client can
// restore prior messages (user text + assistant reasoning/text).
func (h *Handler) serveHistory(w http.ResponseWriter, r *http.Request) {
	threadID := r.URL.Query().Get("threadId")
	if threadID == "" {
		http.Error(w, `{"error":"threadId required"}`, http.StatusBadRequest)
		return
	}
	if h.runtime == nil {
		http.Error(w, `{"error":"history unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	if _, ok := h.authorizeSession(w, r, threadID); !ok {
		return
	}

	msgs, err := h.buildHistory(r, threadID)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"messages": msgs})
}

// buildHistory reads every run's events for the session and folds them into an
// ordered message list. Runs are sequenced; events within a run are offset-ordered.
func (h *Handler) buildHistory(r *http.Request, sessionID string) ([]historyMessage, error) {
	runs, err := h.runtime.RunsForSession(r.Context(), sessionID)
	if err != nil {
		return nil, err
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].Seq < runs[j].Seq })

	var msgs []historyMessage
	for _, run := range runs {
		events, err := h.runtime.Replay(r.Context(), run.ID, 0)
		if err != nil {
			return nil, err
		}
		msgs = appendRunMessages(msgs, run, events)
	}
	return msgs, nil
}

// appendRunMessages folds one run's events into messages: a leading user event
// becomes a user message; the assistant's thinking/text/tool output becomes one
// assistant message with ordered content parts.
func appendRunMessages(msgs []historyMessage, run session.Run, events []session.Event) []historyMessage {
	var assistant *historyMessage
	// Assistant message id is stable per run so the client's repository import
	// keys off it.
	assistantID := "run-" + run.ID

	for _, e := range events {
		switch agent.EventKind(e.Kind) {
		case agent.KindUser:
			text := decodeTextPayload(e.Payload)
			if text == "" {
				continue
			}
			msgs = append(msgs, historyMessage{
				ID:      fmt.Sprintf("%s-user-%d", assistantID, e.Offset),
				Role:    "user",
				Content: []historyPart{{Type: "text", Text: text}},
			})

		case agent.KindThinking:
			a := ensureAssistant(&assistant, assistantID)
			appendPartText(a, "reasoning", e.Payload)

		case agent.KindText:
			a := ensureAssistant(&assistant, assistantID)
			appendPartText(a, "text", e.Payload)
		}
	}
	if assistant != nil {
		msgs = append(msgs, *assistant)
	}
	return msgs
}

// ensureAssistant lazily creates the run's assistant message on first output.
func ensureAssistant(a **historyMessage, id string) *historyMessage {
	if *a == nil {
		*a = &historyMessage{ID: id, Role: "assistant"}
	}
	return *a
}

// appendPartText appends a delta to the message's trailing part of the same
// type, starting a new part when the type changes (preserves think/text order).
func appendPartText(m *historyMessage, partType string, payload []byte) {
	delta := decodeTextPayload(payload)
	if delta == "" {
		return
	}
	if n := len(m.Content); n > 0 && m.Content[n-1].Type == partType {
		m.Content[n-1].Text += delta
		return
	}
	m.Content = append(m.Content, historyPart{Type: partType, Text: delta})
}

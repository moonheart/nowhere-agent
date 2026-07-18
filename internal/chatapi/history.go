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

	// active reports whether a run is still in flight. The client sets
	// unstable_resume only then; resuming a completed run would start a NEW
	// assistant message after the loaded one (duplicate), because resume is a
	// continuation of an unfinished run, not a re-read of history.
	_, active, err := h.runtime.ActiveRun(r.Context(), threadID)
	if err != nil {
		active = false
	}

	// after is the highest event offset the client already has (the newest run's
	// persisted max). resume() passes it back so the server streams only events
	// that landed after this snapshot — not the whole run again (which would
	// duplicate the assistant reply load() just restored).
	after := 0
	if len(msgs) > 0 {
		if n, err := h.latestRunMaxOffset(r, threadID); err == nil {
			after = n
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"messages": msgs, "active": active, "after": after})
}

// latestRunMaxOffset returns the newest run's max persisted event offset — the
// point the client's history snapshot already covers.
func (h *Handler) latestRunMaxOffset(r *http.Request, sessionID string) (int, error) {
	run, ok := h.latestRun(r, sessionID)
	if !ok {
		return 0, nil
	}
	events, err := h.runtime.Replay(r.Context(), run.ID, 0)
	if err != nil {
		return 0, err
	}
	max := 0
	for _, e := range events {
		if e.Offset > max {
			max = e.Offset
		}
	}
	return max, nil
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

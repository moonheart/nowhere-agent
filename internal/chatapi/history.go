package chatapi

import (
	"encoding/json"
	"fmt"
	"net/http"

	"nowhere-agent/internal/provider"
)

// historyMessage is the assistant-ui ThreadMessageLike shape the web client's
// history.load() consumes (via ExportedMessageRepository.fromArray).
type historyMessage struct {
	ID      string        `json:"id"`
	Role    string        `json:"role"`
	Content []historyPart `json:"content"`
}

type historyPart struct {
	Type string `json:"type"`           // "text" | "reasoning" | "tool-call"
	Text string `json:"text,omitempty"` // text/reasoning payload

	// tool-call fields (assistant-ui ToolCallMessagePart)
	ToolCallID string `json:"toolCallId,omitempty"`
	ToolName   string `json:"toolName,omitempty"`
	ArgsText   string `json:"argsText,omitempty"`
	Result     any    `json:"result,omitempty"`
	IsError    bool   `json:"isError,omitempty"`
}

// serveHistory handles GET /api/chat/history?threadId=<id>: it rebuilds the
// conversation from the session's durable messages (the authoritative content
// record, persist-raw-messages) so a reloading client can restore prior
// messages (user text + assistant reasoning/text). Content deltas no longer
// live in run_events (redis-stream-live), so this reads the message store.
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

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"messages": msgs, "active": active})
}

// buildHistory reads the session's persisted messages and folds them into an
// ordered message list. Each stored message becomes one history message; the
// assistant's thinking/text blocks become ordered content parts. Tool calls are
// rendered as tool-call parts: a tool_use block starts a call and the matching
// tool_result block (keyed by id) fills in its result, so a reloaded client sees
// the tool activity, not just the prose.
func (h *Handler) buildHistory(r *http.Request, sessionID string) ([]historyMessage, error) {
	if h.msgStore == nil {
		return nil, nil
	}
	stored, err := h.msgStore.MessagesFor(r.Context(), sessionID)
	if err != nil {
		return nil, err
	}

	var msgs []historyMessage
	// calls indexes the tool-call parts already appended, keyed by tool_use id,
	// so a tool_result can be merged back onto its call.
	calls := map[string]*historyPart{}
	for _, m := range stored {
		hm := historyMessage{ID: fmt.Sprintf("msg-%d", m.ID), Role: string(m.Role)}
		for _, b := range m.Content {
			switch b.Type {
			case provider.BlockText:
				appendPartText(&hm, "text", b.Text)
			case provider.BlockThinking:
				appendPartText(&hm, "reasoning", b.Thinking)
			case provider.BlockToolUse:
				argsText := "{}"
				if b.ToolInput != nil {
					if data, err := json.Marshal(b.ToolInput); err == nil {
						argsText = string(data)
					}
				}
				hm.Content = append(hm.Content, historyPart{
					Type:       "tool-call",
					ToolCallID: b.ToolUseID,
					ToolName:   b.ToolName,
					ArgsText:   argsText,
				})
				calls[b.ToolUseID] = &hm.Content[len(hm.Content)-1]
			case provider.BlockToolResult:
				if call, ok := calls[b.ToolResultID]; ok {
					call.Result = b.ToolContent
					call.IsError = b.IsError
				}
			}
		}
		if len(hm.Content) > 0 {
			msgs = append(msgs, hm)
		}
	}
	return msgs, nil
}

// appendPartText appends text to the message's trailing part of the same type,
// starting a new part when the type changes (preserves think/text order).
func appendPartText(m *historyMessage, partType, text string) {
	if text == "" {
		return
	}
	if n := len(m.Content); n > 0 && m.Content[n-1].Type == partType {
		m.Content[n-1].Text += text
		return
	}
	m.Content = append(m.Content, historyPart{Type: partType, Text: text})
}

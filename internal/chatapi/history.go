package chatapi

import (
	"encoding/json"
	"fmt"
	"net/http"

	"nowhere-agent/internal/provider"
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
	Type string `json:"type"`           // "text" | "reasoning" | "tool-call"
	Text string `json:"text,omitempty"` // text/reasoning payload

	// tool-call fields (assistant-ui ToolCallMessagePart)
	ToolCallID string `json:"toolCallId,omitempty"`
	ToolName   string `json:"toolName,omitempty"`
	ArgsText   string `json:"argsText,omitempty"`
	Result     any    `json:"result,omitempty"`
	IsError    bool   `json:"isError,omitempty"`
	// Messages is a tool call's nested sub-conversation (a spawn_agent child's
	// turns), rendered inline by the client. Rebuilt from the persisted
	// ToolResult block's ToolMessages.
	Messages []historyMessage `json:"messages,omitempty"`
}

// nestedFromBlocks converts a subagent's persisted content blocks into one
// nested assistant message the client renders under the spawn_agent card. A
// subagent's thinking spans every turn (the provider re-sends it each round), so
// all reasoning is folded into a single leading part; the final answer text is a
// single trailing part; tool calls keep their order with results merged back
// (recursing for a nested spawn_agent).
func nestedFromBlocks(blocks []provider.Block) []historyMessage {
	var thinking, answer string
	var tools []historyPart
	calls := map[string]*historyPart{}
	for _, b := range blocks {
		switch b.Type {
		case provider.BlockThinking:
			thinking += b.Thinking
		case provider.BlockText:
			answer += b.Text
		case provider.BlockToolUse:
			argsText := "{}"
			if b.ToolInput != nil {
				if data, err := json.Marshal(b.ToolInput); err == nil {
					argsText = string(data)
				}
			}
			tools = append(tools, historyPart{
				Type: "tool-call", ToolCallID: b.ToolUseID, ToolName: b.ToolName, ArgsText: argsText,
			})
			calls[b.ToolUseID] = &tools[len(tools)-1]
		case provider.BlockToolResult:
			if call, ok := calls[b.ToolResultID]; ok {
				call.Result = b.ToolContent
				call.IsError = b.IsError
				if len(b.ToolMessages) > 0 {
					call.Messages = nestedFromBlocks(b.ToolMessages)
				}
			}
		}
	}

	cur := &historyMessage{ID: "sub-0", Role: "assistant"}
	if thinking != "" {
		cur.Content = append(cur.Content, historyPart{Type: "reasoning", Text: thinking})
	}
	cur.Content = append(cur.Content, tools...)
	if answer != "" {
		cur.Content = append(cur.Content, historyPart{Type: "text", Text: answer})
	}
	if len(cur.Content) == 0 {
		return nil
	}
	return []historyMessage{*cur}
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

// isToolResultOnly reports whether a stored message is a user-role message that
// carries only tool_result blocks (the agent's tool feedback). These merge into
// the surrounding assistant turn rather than appearing as their own bubble.
func isToolResultOnly(m session.StoredMessage) bool {
	if m.Role != provider.RoleUser || len(m.Content) == 0 {
		return false
	}
	for _, b := range m.Content {
		if b.Type != provider.BlockToolResult {
			return false
		}
	}
	return true
}

// buildHistory reads the session's persisted messages and folds them into an
// ordered message list. Consecutive assistant rounds of one logical turn —
// assistant → tool_result → assistant → … — merge into a SINGLE assistant
// message, so a reloaded client renders the reply as one bubble (matching the
// live stream) instead of one bubble per round. Tool calls are rendered as
// tool-call parts: a tool_use block starts a call and the matching tool_result
// block (keyed by id) fills in its result.
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
	var cur *historyMessage // the open assistant turn being accumulated
	flush := func() {
		if cur != nil && len(cur.Content) > 0 {
			msgs = append(msgs, *cur)
		}
		cur = nil
	}
	for _, m := range stored {
		switch {
		case m.Role == provider.RoleAssistant:
			if cur == nil {
				cur = &historyMessage{ID: fmt.Sprintf("msg-%d", m.ID), Role: "assistant"}
			}
			for _, b := range m.Content {
				switch b.Type {
				case provider.BlockText:
					appendPartText(cur, "text", b.Text)
				case provider.BlockThinking:
					appendPartText(cur, "reasoning", b.Thinking)
				case provider.BlockToolUse:
					argsText := "{}"
					if b.ToolInput != nil {
						if data, err := json.Marshal(b.ToolInput); err == nil {
							argsText = string(data)
						}
					}
					cur.Content = append(cur.Content, historyPart{
						Type:       "tool-call",
						ToolCallID: b.ToolUseID,
						ToolName:   b.ToolName,
						ArgsText:   argsText,
					})
					calls[b.ToolUseID] = &cur.Content[len(cur.Content)-1]
				}
			}
		case isToolResultOnly(m):
			// Tool feedback merges into the open assistant turn's tool-call parts.
			for _, b := range m.Content {
				if call, ok := calls[b.ToolResultID]; ok {
					call.Result = b.ToolContent
					call.IsError = b.IsError
					// A spawn_agent result carries the child's sub-conversation.
					if len(b.ToolMessages) > 0 {
						call.Messages = nestedFromBlocks(b.ToolMessages)
					}
				}
			}
		default:
			// A real user text (or anything else) ends the current assistant turn.
			flush()
			hm := historyMessage{ID: fmt.Sprintf("msg-%d", m.ID), Role: string(m.Role)}
			for _, b := range m.Content {
				if b.Type == provider.BlockText {
					appendPartText(&hm, "text", b.Text)
				}
			}
			if len(hm.Content) > 0 {
				msgs = append(msgs, hm)
			}
		}
	}
	flush()
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

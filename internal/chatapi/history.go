package chatapi

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"nowhere-agent/internal/httpx"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/session"
)

// historyMessage is the assistant-ui ThreadMessageLike shape the web client's
// history.load() consumes (via ExportedMessageRepository.fromArray).
type historyMessage struct {
	ID      string        `json:"id"`
	Role    string        `json:"role"`
	Content []historyPart `json:"content"`
	// Metadata carries the run's token usage as an unstable_data entry, so a
	// reloaded client renders it exactly like the live data-usage frame did.
	// Nil for messages with no usage (user turns).
	Metadata *historyMetadata `json:"metadata,omitempty"`
}

// historyMetadata is the subset of ThreadMessageLike.metadata we populate.
type historyMetadata struct {
	UnstableData []historyUsageData `json:"unstable_data,omitempty"`
	// Error carries the turn's run terminal error text when that run failed
	// (change failed-run-retry): the client renders the failure notice and a
	// retry affordance from it. Empty on successful turns.
	Error string `json:"error,omitempty"`
	// Timing carries the turn's wall-clock duration so a reloaded client can
	// show “耗时 Xs” without client-side localStorage (server-persisted, A).
	Timing *historyTiming `json:"timing,omitempty"`
}

// historyTiming is the subset of MessageTiming we populate for history reloads.
// Only totalStreamTime is needed for the turn header; the live stream still
// drives streamStartTime/firstTokenTime.
type historyTiming struct {
	TotalStreamTime int `json:"totalStreamTime,omitempty"`
}

// historyUsageData is one unstable_data entry: {name:"usage", data:{...}}.
type historyUsageData struct {
	Name string         `json:"name"`
	Data map[string]int `json:"data"`
}

type historyPart struct {
	Type string `json:"type"`           // "text" | "reasoning" | "tool-call" | "image" | "data"
	Text string `json:"text,omitempty"` // text/reasoning payload

	// data fields: agent-driven UI declared by a tool result. Shape mirrors the
	// live data-generative-ui frame's data part: {type:"data", name,
	// data:{spec}} — the client's mapPart passes it straight through to the
	// same data-part renderer the live stream uses.
	Name string          `json:"name,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`

	// image fields (image-input capability): a user attachment referenced by its
	// session-relative workspace path. The client renders it via the
	// authenticated file endpoint (GET .../sessions/{id}/files/{path...}).
	MediaType string `json:"mediaType,omitempty"`
	Path      string `json:"path,omitempty"`

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
// maxHistoryPage caps one history tail page (limit param).
const maxHistoryPage = 500

// historyPage reads the session's messages for /history: the FULL conversation
// when no limit is given (legacy contract — every caller keeps working), else
// the newest messages bounded by limit, keyset-paged backwards via `before`
// (the previous page's first message id; the limit+1 probe makes hasMore exact
// without a second query). A bounded page is the deliberate tradeoff for long
// sessions: the client renders the tail and shows a truncation hint instead of
// loading the whole conversation.
func (h *Handler) historyPage(r *http.Request, sessionID string) (stored []session.StoredMessage, hasMore bool, err error) {
	if h.msgStore == nil {
		return nil, false, nil
	}
	limit := 0
	if lv := r.URL.Query().Get("limit"); lv != "" {
		if n, perr := strconv.Atoi(lv); perr == nil && n > 0 {
			limit = min(n, maxHistoryPage)
		}
	}
	if limit <= 0 {
		stored, err = h.msgStore.MessagesFor(r.Context(), sessionID)
		return stored, false, err
	}
	var before int64
	if bv := r.URL.Query().Get("before"); bv != "" {
		if n, perr := strconv.ParseInt(bv, 10, 64); perr == nil && n > 0 {
			before = n
		}
	}
	page, err := h.msgStore.MessagesTail(r.Context(), sessionID, before, limit+1)
	if err != nil {
		return nil, false, err
	}
	// The page is ascending; the limit+1'th (oldest) row proves truncation.
	// Keep the NEWEST limit rows — the oldest extra one is at the front.
	if len(page) > limit {
		page = page[len(page)-limit:]
		hasMore = true
	}
	return page, hasMore, nil
}

func (h *Handler) serveHistory(w http.ResponseWriter, r *http.Request) {
	threadID := r.URL.Query().Get("threadId")
	if threadID == "" {
		httpx.Error(w, http.StatusBadRequest, "threadId required")
		return
	}
	if h.runtime == nil {
		httpx.Error(w, http.StatusServiceUnavailable, "history unavailable")
		return
	}
	if _, ok := h.authorizeSession(w, r, threadID); !ok {
		return
	}

	stored, hasMore, err := h.historyPage(r, threadID)
	if err != nil {
		slog.Warn("history rebuild failed", "thread", threadID, "err", err)
		httpx.ErrorFrom(w, err)
		return
	}
	msgs, err := h.buildHistoryFrom(r, threadID, stored)
	if err != nil {
		slog.Warn("history rebuild failed", "thread", threadID, "err", err)
		httpx.ErrorFrom(w, err)
		return
	}

	// active reports whether a run is genuinely in flight (queued/running). Runs
	// are stateless and terminal on completion, so there is no suspended state to
	// special-case.
	_, active, err := h.runtime.ActiveRun(r.Context(), threadID)
	if err != nil {
		active = false
	}

	// A run parked in waiting_approval has durable pending interactions the
	// transient data-interaction frames showed live but a refresh dropped. Echo
	// the WHOLE queue here (a gated batch parks one interaction per gated call) so
	// the reloading client re-renders every card (approve/deny or the ask_user
	// questions). Shape matches that frame's payload; the array is in queue order.
	var pendingList []map[string]any
	if h.registry != nil {
		if list, err := h.registry.PendingApprovalsForSession(r.Context(), threadID); err == nil {
			for _, ap := range list {
				var args any
				if err := json.Unmarshal(ap.Payload, &args); err != nil {
					args = map[string]any{}
				}
				kind := ap.Kind
				if kind == "" {
					kind = "approval"
				}
				pendingList = append(pendingList, map[string]any{
					"approvalId": ap.ID,
					"kind":       kind,
					"toolCallId": ap.ToolCallID,
					"toolName":   ap.ToolName,
					"args":       args,
				})
			}
		}
	}
	// pendingApproval keeps the legacy singular shape for older clients (the head
	// of the queue); pendingInteractions carries the whole batch.
	var pending map[string]any
	if len(pendingList) > 0 {
		pending = pendingList[0]
	}

	// Session state (capability-gap O1): the session's generic key/value store,
	// echoed so a reloading client restores session-level UI (the plan panel)
	// that the live data-session-state frames drove before the refresh. Shape is
	// the raw dictionary {key: value}; absent when empty.
	var sessionState map[string]json.RawMessage
	if h.runtime != nil {
		if st, err := h.runtime.SessionState(r.Context(), threadID); err == nil && len(st) > 0 {
			sessionState = st
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"messages": msgs, "active": active, "pendingApproval": pending, "pendingInteractions": pendingList, "sessionState": sessionState, "hasMore": hasMore})
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

// buildHistory reads the session's persisted messages (the full conversation)
// and folds them into an ordered message list. Consecutive assistant rounds of
// one logical turn — assistant → tool_result → assistant → … — merge into a
// SINGLE assistant message, so a reloaded client renders the reply as one
// bubble (matching the live stream) instead of one bubble per round. Tool calls
// are rendered as tool-call parts: a tool_use block starts a call and the
// matching tool_result block (keyed by id) fills in its result.
//
// The exception is a HITL gate: an ask_user / permission tool_use whose
// tool_result arrives via a SEPARATE verdict run (capability-gap O2), not inline
// in the same run. That gate ENDS the turn — the live client shows the gated
// message and the verdict's reply as two bubbles — so buildHistory flushes the
// turn at a gated (unanswered) tool_use instead of folding the reply in.
//
// serveHistory's paginated path folds a bounded tail page instead (see
// buildHistoryFrom); a boundary cut inside a merged turn renders that turn's
// visible part as its own bubble, and a page head may carry orphaned
// tool_result rows (whose call lives on the previous page) that are dropped —
// the documented cost of rendering only the tail.
func (h *Handler) buildHistory(r *http.Request, sessionID string) ([]historyMessage, error) {
	if h.msgStore == nil {
		return nil, nil
	}
	stored, err := h.msgStore.MessagesFor(r.Context(), sessionID)
	if err != nil {
		return nil, err
	}
	return h.buildHistoryFrom(r, sessionID, stored)
}

// buildHistoryFrom folds an already-read stored-message slice into the ordered
// message list (see buildHistory for the folding semantics). The slice is
// either the full conversation (buildHistory) or a bounded tail page
// (serveHistory's limit path).
func (h *Handler) buildHistoryFrom(r *http.Request, sessionID string, stored []session.StoredMessage) ([]historyMessage, error) {

	var msgs []historyMessage
	// calls indexes the tool-call parts already appended, keyed by tool_use id,
	// so a tool_result can be merged back onto its call.
	calls := map[string]*historyPart{}
	var cur *historyMessage // the open assistant turn being accumulated
	var curUsage provider.Usage
	var curHasUsage bool
	// curError is the failed-run error carried by the turn's last assistant
	// message's metadata; echoed on the merged turn so a reloaded client can
	// show why the run stopped (change failed-run-retry).
	var curError string
	// Server-persisted turn duration (A): runID -> totalStreamTime ms from runs.finished_at.
	// Lets a reloaded client show “耗时 Xs” without localStorage.
	runDurations := map[string]int{}
	var curRunID string
	if h.runtime != nil {
		if runs, err := h.runtime.RunsForSession(r.Context(), sessionID); err == nil {
			for _, run := range runs {
				if run.FinishedAt != nil && !run.CreatedAt.IsZero() {
					ms := int(run.FinishedAt.Sub(run.CreatedAt).Milliseconds())
					if ms > 0 && ms < int(24*time.Hour.Milliseconds()) {
						runDurations[run.ID] = ms
					}
				}
			}
		}
	}
	flush := func() {
		if cur != nil && len(cur.Content) > 0 {
			// Attach the turn's accumulated LLM usage as an unstable_data entry
			// so the client renders it like the live data-usage frame, the
			// failed-run error (if any) so the client can offer a retry, and the
			// server-persisted timing so refresh keeps “耗时”.
			hasTiming := curRunID != "" && runDurations[curRunID] > 0
			if curHasUsage || curError != "" || hasTiming {
				md := &historyMetadata{}
				if curHasUsage {
					md.UnstableData = []historyUsageData{{
						Name: "usage",
						Data: map[string]int{
							"inputTokens":      curUsage.InputTokens,
							"outputTokens":     curUsage.OutputTokens,
							"cacheReadTokens":  curUsage.CacheReadTokens,
							"cacheWriteTokens": curUsage.CacheWriteTokens,
						},
					}}
				}
				if curError != "" {
					md.Error = curError
				}
				if hasTiming {
					md.Timing = &historyTiming{TotalStreamTime: runDurations[curRunID]}
				}
				cur.Metadata = md
			}
			msgs = append(msgs, *cur)
		}
		cur = nil
		curRunID = ""
		curUsage = provider.Usage{}
		curHasUsage = false
		curError = ""
	}
	for i, m := range stored {
		switch {
		case m.Role == provider.RoleAssistant:
			if cur == nil {
				cur = &historyMessage{ID: fmt.Sprintf("msg-%d", m.ID), Role: "assistant"}
			}
			if curRunID == "" {
				curRunID = m.RunID
			}
			// A failed run's last assistant message carries its terminal error
			// as metadata; the turn it belongs to echoes it (later messages of
			// the same merged turn win — only the last can be the failed one).
			if err := storedMessageError(m, &curError); err != nil {
				return nil, fmt.Errorf("read message metadata: %w", err)
			}
			// Accumulate each assistant round's LLM-call usage into the turn total.
			if m.Usage != nil {
				curUsage.InputTokens += m.Usage.InputTokens
				curUsage.OutputTokens += m.Usage.OutputTokens
				curUsage.CacheReadTokens += m.Usage.CacheReadTokens
				curUsage.CacheWriteTokens += m.Usage.CacheWriteTokens
				curHasUsage = true
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
			// A HITL gate (ask_user / permission approval) ends its run on a bare
			// tool_use: no further assistant round follows IN THE SAME RUN, and the
			// verdict's reply arrives via a separate verdict run. Close the turn so
			// that reply becomes a fresh bubble — unlike a normal tool loop, whose
			// same-run continuation rounds fold into this one.
			if endsOnGatedCall(m) && isLastAssistantOfRun(stored, i) {
				flush()
			}
		case isToolResultOnly(m):
			// Tool feedback merges into the open assistant turn's tool-call parts.
			// A gated call's result comes from a later verdict run whose turn was
			// already flushed; it still resolves the parked call's part via `calls`.
			for _, b := range m.Content {
				if call, ok := calls[b.ToolResultID]; ok {
					call.Result = b.ToolContent
					call.IsError = b.IsError
					// A spawn_agent result carries the child's sub-conversation.
					if len(b.ToolMessages) > 0 {
						call.Messages = nestedFromBlocks(b.ToolMessages)
					}
				}
				// Agent-driven UI declared by the tool result: re-render it in the
				// turn exactly where the live data-generative-ui frame landed it.
				if b.GenerativeUI != nil {
					if raw, err := json.Marshal(map[string]any{"spec": b.GenerativeUI}); err == nil {
						cur.Content = append(cur.Content, historyPart{Type: "data", Name: "generative-ui", Data: raw})
					}
				}
			}
		default:
			// A real user text (or anything else) ends the current assistant turn.
			flush()
			hm := historyMessage{ID: fmt.Sprintf("msg-%d", m.ID), Role: string(m.Role)}
			for _, b := range m.Content {
				switch b.Type {
				case provider.BlockText:
					appendPartText(&hm, "text", b.Text)
				case provider.BlockImage:
					// An attached image (image-input capability): carried by path
					// so the client renders it via the authenticated /files/ route.
					if b.ImagePath != "" {
						hm.Content = append(hm.Content, historyPart{Type: "image", MediaType: b.MediaType, Path: b.ImagePath})
					}
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

// endsOnGatedCall reports whether an assistant message's last block is a bare
// tool_use — i.e. the model emitted a call and produced no trailing text. Both
// a HITL gate and a normal mid-loop tool round look like this; the run boundary
// (isLastAssistantOfRun) tells them apart.
func endsOnGatedCall(m session.StoredMessage) bool {
	if len(m.Content) == 0 {
		return false
	}
	return m.Content[len(m.Content)-1].Type == provider.BlockToolUse
}

// isLastAssistantOfRun reports whether stored[i] is the final assistant message
// of the contiguous run-segment it belongs to — i.e. no LATER assistant message
// shares its RunID. A HITL gate is the last assistant of its (ended) run; a
// normal tool loop's mid round is followed by more assistant messages in the
// same run. Messages are appended in run order, so a later same-run assistant
// can only appear after i.
func isLastAssistantOfRun(stored []session.StoredMessage, i int) bool {
	runID := stored[i].RunID
	for j := i + 1; j < len(stored); j++ {
		if stored[j].Role == provider.RoleAssistant && stored[j].RunID == runID {
			return false
		}
	}
	return true
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

// messageErrorMeta is the metadata shape a failed run attaches to its last
// assistant message (registry.attachRunError).
type messageErrorMeta struct {
	Error string `json:"error"`
}

// storedMessageError reads a stored message's metadata and, when it carries the
// failed-run error key, writes the text into *out. Malformed metadata JSON is a
// hard error (it would silently swallow a failure marker); the error key being
// absent is not.
func storedMessageError(m session.StoredMessage, out *string) error {
	if len(m.Metadata) == 0 {
		return nil
	}
	var meta messageErrorMeta
	if err := json.Unmarshal(m.Metadata, &meta); err != nil {
		return fmt.Errorf("message %d metadata: %w", m.ID, err)
	}
	if meta.Error != "" {
		*out = meta.Error
	}
	return nil
}

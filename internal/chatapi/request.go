package chatapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/session"
)

// dataStreamRequest mirrors the body the assistant-ui data-stream runtime POSTs.
type dataStreamRequest struct {
	System   string            `json:"system"`
	Messages []incomingMessage `json:"messages"`
	// Tools carries client-declared tool definitions (general interrupt): tools
	// the CLIENT executes. The server registers them for the run; the loop
	// suspends on a call to one and hands it to the client, which executes and
	// returns the output as the tool result. Keyed by tool name.
	Tools    map[string]clientToolDecl `json:"tools,omitempty"`
	ThreadID string                    `json:"threadId"`
	// Approval, when set, turns this POST into a verdict on a parked run rather
	// than a new turn: the handler resumes the run and streams its continuation
	// over the same ui-message-stream response (reusing the chat attach path).
	Approval *approvalRequest `json:"approval,omitempty"`
}

// clientToolDecl is one client-declared tool definition from the request body.
// The client owns execution; these fields are the calling contract shown to the
// model plus the optional output contract the server validates against.
type clientToolDecl struct {
	Description  string         `json:"description"`
	InputSchema  map[string]any `json:"inputSchema,omitempty"`
	Parameters   map[string]any `json:"parameters,omitempty"` // AI-SDK alias for inputSchema
	OutputSchema map[string]any `json:"outputSchema,omitempty"`
	// ClientSide marks the tool as client-executed. When omitted, a tool declared
	// in the body is assumed client-side (the only kind a client can declare).
	ClientSide *bool `json:"clientSide,omitempty"`
}

// approvalRequest carries the human verdict for a parked tool-approval /
// ask_user interaction (capability-gap O2).
type approvalRequest struct {
	ApprovalID string          `json:"approvalId"`
	Approved   bool            `json:"approved"`
	Answer     json.RawMessage `json:"answer,omitempty"` // ask_user: the structured response
}

type incomingMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Parts   []incomingPart  `json:"parts"`
}

type incomingPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// toHistory converts incoming messages into canonical provider messages.
// The data-stream runtime sends AI-SDK-style messages; we extract text.
func toHistory(req dataStreamRequest) []provider.Message {
	var history []provider.Message
	for _, m := range req.Messages {
		role := provider.RoleUser
		if m.Role == "assistant" {
			role = provider.RoleAssistant
		}
		if m.Role == "system" {
			continue // system handled separately
		}
		text := extractText(m)
		if text == "" {
			continue
		}
		history = append(history, provider.TextMessage(role, text))
	}
	return history
}

// extractText pulls text from either content (string or array) or parts.
func extractText(m incomingMessage) string {
	if len(m.Parts) > 0 {
		var b strings.Builder
		for _, p := range m.Parts {
			if p.Type == "text" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	if len(m.Content) == 0 {
		return ""
	}
	// content may be a plain string or an array of parts.
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}
	var parts []incomingPart
	if err := json.Unmarshal(m.Content, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "text" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return ""
}

// lastUserText returns the text of the most recent user message.
func lastUserText(req dataStreamRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			return extractText(req.Messages[i])
		}
	}
	return ""
}

// storedMessagesToProvider converts durable StoredMessages back into canonical
// provider messages for the loop, preserving full blocks (thinking+signature,
// tool_use, tool_result).
func storedMessagesToProvider(stored []session.StoredMessage) []provider.Message {
	out := make([]provider.Message, 0, len(stored))
	for _, m := range stored {
		out = append(out, provider.Message{Role: m.Role, Content: m.Content})
	}
	return out
}

var _ = http.MethodPost // referenced by handler registration elsewhere

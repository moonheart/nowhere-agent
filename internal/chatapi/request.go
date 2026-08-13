package chatapi

import (
	"encoding/json"
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
	// Images carries the images attached to the CURRENT user turn (image-input
	// capability). The frontend uploads each attachment to the session first and
	// includes the returned path + media type here; they append as BlockImage
	// blocks to the most recent user message. (The data-stream runtime mangles
	// per-message image parts into opaque file parts, so images travel top-level.)
	Images []incomingImagePart `json:"images,omitempty"`
	// Approval, when set, turns this POST into a verdict on a parked run rather
	// than a new turn: the handler resumes the run and streams its continuation
	// over the same ui-message-stream response (reusing the chat attach path).
	Approval *approvalRequest `json:"approval,omitempty"`
}

// incomingImagePart is one attached image already stored in the session's
// workspace, referenced by its session-relative path (the upload endpoint's
// response). MediaType is optional; it defaults to image/webp (the store's
// normalized output) when empty.
type incomingImagePart struct {
	MediaType string `json:"mediaType"`
	Path      string `json:"path"`
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
	// Image part (image-input): MediaType + Path of an attached image, uploaded
	// via the session's image endpoint first. The path is workspace-relative.
	MediaType string `json:"mediaType"`
	Path      string `json:"path"`
}

// toHistory converts incoming messages into canonical provider messages,
// preserving both text and image parts (image-input capability). The data-stream
// runtime sends AI-SDK-style messages; we extract the blocks they carry.
// Top-level attached images (req.Images) append to the most recent user message.
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
		blocks := extractBlocks(m)
		if len(blocks) == 0 {
			continue
		}
		history = append(history, provider.Message{Role: role, Content: blocks})
	}
	if imgs := requestImages(req); len(imgs) > 0 {
		if n := len(history); n > 0 && history[n-1].Role == provider.RoleUser {
			history[n-1].Content = append(history[n-1].Content, imgs...)
		} else {
			// Image-only turn (no user text, or the last user message carried
			// nothing extractable): still surface the images.
			history = append(history, provider.Message{Role: provider.RoleUser, Content: imgs})
		}
	}
	return history
}

// requestImages converts the request's top-level attached images (image-input
// capability) into provider image blocks. Paths are workspace-relative and were
// already validated at upload time.
func requestImages(req dataStreamRequest) []provider.Block {
	var out []provider.Block
	for _, img := range req.Images {
		if img.Path == "" {
			continue
		}
		mediaType := img.MediaType
		if mediaType == "" {
			mediaType = "image/webp"
		}
		out = append(out, provider.Block{Type: provider.BlockImage, MediaType: mediaType, ImagePath: img.Path})
	}
	return out
}

// extractBlocks pulls the content blocks from either content (string or array)
// or parts: text parts become text blocks, image parts become BlockImage blocks.
func extractBlocks(m incomingMessage) []provider.Block {
	var parts []incomingPart
	switch {
	case len(m.Parts) > 0:
		parts = m.Parts
	case len(m.Content) == 0:
		return nil
	default:
		// content may be a plain string or an array of parts.
		var s string
		if err := json.Unmarshal(m.Content, &s); err == nil {
			if s != "" {
				return []provider.Block{{Type: provider.BlockText, Text: s}}
			}
			return nil
		}
		if err := json.Unmarshal(m.Content, &parts); err != nil {
			return nil
		}
	}
	var out []provider.Block
	for _, p := range parts {
		switch p.Type {
		case "text":
			if p.Text != "" {
				out = append(out, provider.Block{Type: provider.BlockText, Text: p.Text})
			}
		case "image":
			if p.Path != "" {
				mediaType := p.MediaType
				if mediaType == "" {
					mediaType = "image/webp"
				}
				out = append(out, provider.Block{Type: provider.BlockImage, MediaType: mediaType, ImagePath: p.Path})
			}
		}
	}
	return out
}

// extractText pulls plain text from either content (string or array) or parts.
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

// userTurnBlocks returns the content blocks of the most recent user message
// (text + any attached images, image-input capability). It is the full-block
// user turn the run worker persists, mirroring toHistory but scoped to the last
// user message. Top-level attached images (req.Images) append even when that
// message is otherwise empty (an image-only turn), so the durable record
// persists the image path pointers.
func userTurnBlocks(req dataStreamRequest) []provider.Block {
	var blocks []provider.Block
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			blocks = extractBlocks(req.Messages[i])
			break
		}
	}
	return append(blocks, requestImages(req)...)
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

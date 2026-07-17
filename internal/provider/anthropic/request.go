// Package anthropic translates between the canonical provider model and the
// Anthropic Messages API. Request building and SSE stream decoding are kept
// pure/separable so they can be unit-tested without network access.
package anthropic

import (
	"nowhere-agent/internal/provider"
)

// apiRequest is the Anthropic Messages API request body.
type apiRequest struct {
	Model     string        `json:"model"`
	MaxTokens int           `json:"max_tokens"`
	System    any           `json:"system,omitempty"`
	Messages  []apiMessage  `json:"messages"`
	Tools     []apiTool     `json:"tools,omitempty"`
	Stream    bool          `json:"stream"`
}

type systemBlock struct {
	Type      string       `json:"type"` // "text"
	Text      string       `json:"text"`
	CacheCtl  *cacheControl `json:"cache_control,omitempty"`
}

type cacheControl struct {
	Type string `json:"type"` // "ephemeral"
}

type apiMessage struct {
	Role    string     `json:"role"`
	Content []apiBlock `json:"content"`
}

type apiBlock struct {
	Type string `json:"type"`

	Text string `json:"text,omitempty"`

	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`

	ID    string         `json:"id,omitempty"`
	Name  string         `json:"name,omitempty"`
	Input map[string]any `json:"input,omitempty"`

	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`

	CacheCtl *cacheControl `json:"cache_control,omitempty"`
}

type apiTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

// buildRequest converts a canonical Request into the Anthropic API shape.
// Pure: no I/O. Applies prompt caching when CacheablePrefix is set.
func buildRequest(r provider.Request) apiRequest {
	req := apiRequest{
		Model:     r.Model,
		MaxTokens: r.MaxTokens,
		Stream:    true,
	}

	// System with optional cache point.
	if r.System != "" {
		sb := systemBlock{Type: "text", Text: r.System}
		if r.CacheablePrefix {
			sb.CacheCtl = &cacheControl{Type: "ephemeral"}
		}
		req.System = []systemBlock{sb}
	}

	// Tools; mark the last tool cacheable as part of the stable prefix.
	if len(r.Tools) > 0 {
		req.Tools = make([]apiTool, len(r.Tools))
		for i, t := range r.Tools {
			req.Tools[i] = apiTool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema}
		}
	}

	// Messages.
	req.Messages = make([]apiMessage, 0, len(r.Messages))
	for _, m := range r.Messages {
		req.Messages = append(req.Messages, apiMessage{
			Role:    string(m.Role),
			Content: convertBlocks(m.Content),
		})
	}
	return req
}

// convertBlocks maps canonical blocks to Anthropic blocks, preserving thinking
// round-trip and tool use/result correlation.
func convertBlocks(blocks []provider.Block) []apiBlock {
	out := make([]apiBlock, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case provider.BlockText:
			ab := apiBlock{Type: "text", Text: b.Text}
			if b.CachePoint {
				ab.CacheCtl = &cacheControl{Type: "ephemeral"}
			}
			out = append(out, ab)
		case provider.BlockThinking:
			out = append(out, apiBlock{Type: "thinking", Thinking: b.Thinking, Signature: b.ThinkingSignature})
		case provider.BlockToolUse:
			out = append(out, apiBlock{Type: "tool_use", ID: b.ToolUseID, Name: b.ToolName, Input: b.ToolInput})
		case provider.BlockToolResult:
			out = append(out, apiBlock{Type: "tool_result", ToolUseID: b.ToolResultID, Content: b.ToolContent, IsError: b.IsError})
		}
	}
	return out
}

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
	// ToolChoice forces a specific tool when set (used for structured output).
	ToolChoice *apiToolChoice `json:"tool_choice,omitempty"`
	Stream    bool          `json:"stream"`
}

// apiToolChoice forces the model to call one named tool (type "tool").
type apiToolChoice struct {
	Type string `json:"type"` // "tool"
	Name string `json:"name"`
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

	// Source carries an image payload (base64) for type=="image".
	Source *apiImageSource `json:"source,omitempty"`

	CacheCtl *cacheControl `json:"cache_control,omitempty"`
}

// apiImageSource is the Anthropic image source shape: base64 data + media type.
type apiImageSource struct {
	Type      string `json:"type"`       // "base64"
	MediaType string `json:"media_type"` // e.g. "image/webp"
	Data      string `json:"data"`       // base64 bytes
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

	// Structured output (L3): append the synthetic response tool and force it.
	if jr := r.JSONResponse; jr != nil {
		req.Tools = append(req.Tools, apiTool{Name: jr.Name, Description: jr.Description, InputSchema: jr.Schema})
		req.ToolChoice = &apiToolChoice{Type: "tool", Name: jr.Name}
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
		case provider.BlockImage:
			// Materialized blocks carry base64 in ImageData; emit the native
			// image source. Unmaterialized blocks (no data) degrade to a text
			// placeholder so the request still sends.
			if b.ImageData != "" {
				out = append(out, apiBlock{
					Type:   "image",
					Source: &apiImageSource{Type: "base64", MediaType: b.MediaType, Data: b.ImageData},
				})
			} else {
				out = append(out, apiBlock{Type: "text", Text: "[image unavailable: " + b.ImagePath + "]"})
			}
		}
	}
	return out
}

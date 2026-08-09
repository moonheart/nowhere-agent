// Package anthropic translates between the canonical provider model and the
// Anthropic Messages API. Request building and SSE stream decoding are kept
// pure/separable so they can be unit-tested without network access.
package anthropic

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"

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

	// Sampling controls; omitted entirely when nil (provider default).
	Temperature   *float64 `json:"temperature,omitempty"`
	TopP          *float64 `json:"top_p,omitempty"`
	StopSequences []string `json:"stop_sequences,omitempty"`

	// Thinking enables extended reasoning with a token budget.
	Thinking *apiThinking `json:"thinking,omitempty"`
}

// apiThinking is the Anthropic extended-thinking request block.
type apiThinking struct {
	Type         string `json:"type"` // "enabled"
	BudgetTokens int    `json:"budget_tokens"`
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
// Pure: no I/O. Applies prompt caching when CacheablePrefix is set, gates
// parameters on the model's capability profile, and enables extended thinking
// when requested.
func buildRequest(r provider.Request) apiRequest {
	req := apiRequest{
		Model:     r.Model,
		MaxTokens: r.MaxTokens,
		Stream:    true,
	}
	profile, profileKnown := provider.LookupProfile("anthropic", r.Model)

	// Extended thinking. The API requires max_tokens > thinking.budget_tokens
	// and forbids temperature/top_p alongside it, so those are enlarged /
	// dropped respectively.
	if r.Thinking != nil && r.Thinking.BudgetTokens > 0 {
		req.Thinking = &apiThinking{Type: "enabled", BudgetTokens: r.Thinking.BudgetTokens}
		if req.MaxTokens <= r.Thinking.BudgetTokens {
			req.MaxTokens = r.Thinking.BudgetTokens + 4096
		}
	} else {
		// Sampling controls, gated by profile: a model known to reject them
		// gets none. Unknown models get whatever the caller asked for.
		if !profileKnown || profile.Sampling {
			req.Temperature = r.Temperature
			req.TopP = r.TopP
		}
		req.StopSequences = r.StopSequences
	}

	// System with optional cache point.
	if r.System != "" {
		sb := systemBlock{Type: "text", Text: r.System}
		if r.CacheablePrefix {
			sb.CacheCtl = &cacheControl{Type: "ephemeral"}
		}
		req.System = []systemBlock{sb}
	}

	// Tools; mark the last tool cacheable as part of the stable prefix. A
	// model profiled without tool calling gets no tools (sending them would
	// be a 400).
	tools := r.Tools
	jsonResp := r.JSONResponse
	if profileKnown && !profile.ToolCalling {
		tools = nil
		jsonResp = nil
	}
	if len(tools) > 0 {
		req.Tools = make([]apiTool, len(tools))
		for i, t := range tools {
			req.Tools[i] = apiTool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema}
		}
	}

	// Structured output (L3): append the synthetic response tool and force it.
	if jr := jsonResp; jr != nil {
		req.Tools = append(req.Tools, apiTool{Name: jr.Name, Description: jr.Description, InputSchema: jr.Schema})
		req.ToolChoice = &apiToolChoice{Type: "tool", Name: jr.Name}
	}

	// Messages. Consecutive user messages are merged into one (their content
	// blocks concatenate): compression summaries and memory injection can both
	// produce adjacent user-role messages, which Anthropic-compatible endpoints
	// may reject as a malformed user/assistant alternation. Assistant messages
	// are never merged — a thinking block must stay the first block of its
	// message.
	req.Messages = make([]apiMessage, 0, len(r.Messages))
	for _, m := range r.Messages {
		role := string(m.Role)
		blocks := convertBlocks(m.Content)
		if n := len(req.Messages); n > 0 && role == string(provider.RoleUser) && req.Messages[n-1].Role == role {
			req.Messages[n-1].Content = append(req.Messages[n-1].Content, blocks...)
			continue
		}
		req.Messages = append(req.Messages, apiMessage{Role: role, Content: blocks})
	}
	return req
}

// toolUseIDPattern is Anthropic's accepted tool_use id shape (documented as
// ^[a-zA-Z0-9_-]+$, bounded in practice at 64 chars).
var toolUseIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// normalizeToolUseID makes a tool_use/tool_result id acceptable to Anthropic.
// IDs produced by other providers (cross-provider continuation) can carry
// characters Anthropic rejects — e.g. Fireworks/Kimi's "functions.xxx:0" —
// which fails the whole request with a 400. An illegal id is replaced by a
// DETERMINISTIC hash so the tool_use and its matching tool_result (and every
// later turn replaying them) normalize to the same value and stay paired.
func normalizeToolUseID(id string) string {
	if toolUseIDPattern.MatchString(id) {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return "toolu_" + hex.EncodeToString(sum[:])[:24]
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
			out = append(out, apiBlock{Type: "tool_use", ID: normalizeToolUseID(b.ToolUseID), Name: b.ToolName, Input: b.ToolInput})
		case provider.BlockToolResult:
			out = append(out, apiBlock{Type: "tool_result", ToolUseID: normalizeToolUseID(b.ToolResultID), Content: b.ToolContent, IsError: b.IsError})
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

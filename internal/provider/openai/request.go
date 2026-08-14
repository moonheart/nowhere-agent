// Package openai translates between the canonical provider model and the
// OpenAI chat.completions API. Request building and SSE decoding are kept
// pure/separable so they can be unit-tested without network access.
package openai

import (
	"encoding/json"
	"fmt"

	"nowhere-agent/internal/provider"
)

// apiRequest is the OpenAI chat.completions request body.
type apiRequest struct {
	Model         string         `json:"model"`
	Messages      []apiMessage   `json:"messages"`
	Tools         []apiTool      `json:"tools,omitempty"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
	Stream        bool           `json:"stream"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`

	// Sampling controls; omitted entirely when nil (provider default).
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	Stop        []string `json:"stop,omitempty"`
}

// streamOptions requests a final usage chunk in the SSE stream. Without
// include_usage the gateway omits usage entirely for streamed responses, so the
// loop would have no token counts to report.
type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type apiMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCalls  []apiCall       `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

// apiContentPart is one element of OpenAI's array content form
// (image-input capability): text and image_url parts. Content is serialized as
// either a plain JSON string (legacy) or an array of these when a message
// carries images.
type apiContentPart struct {
	Type     string       `json:"type"`
	Text     string       `json:"text,omitempty"`
	ImageURL *apiImageURL `json:"image_url,omitempty"`
}

type apiImageURL struct {
	URL string `json:"url"`
}

type apiCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // "function"
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type apiTool struct {
	Type     string `json:"type"` // "function"
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description,omitempty"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

// buildRequest converts a canonical Request into the OpenAI API shape.
// Thinking blocks cannot be represented in chat.completions and are dropped;
// consecutive blocks of one message are flattened per OpenAI's message model.
// The extended-thinking request spec (Request.Thinking) is likewise
// unrepresentable — no OpenAI-compatible gateway accepts a thinking token
// budget — and is dropped here; Adapter.Stream logs a warning when a budget
// was configured. Sampling parameters are gated on the model's capability
// profile (reasoning models reject temperature/top_p). Pure: no I/O.
func buildRequest(r provider.Request) (apiRequest, error) {
	req := apiRequest{Model: r.Model, MaxTokens: r.MaxTokens, Stream: true, StreamOptions: &streamOptions{IncludeUsage: true}}

	if profile, known := provider.LookupProfile("openai", r.Model); !known || profile.Sampling {
		req.Temperature = r.Temperature
		req.TopP = r.TopP
		req.Stop = r.StopSequences
	}

	if r.System != "" {
		req.Messages = append(req.Messages, apiMessage{Role: "system", Content: rawString(r.System)})
	}

	imageInput := false
	if profile, known := provider.LookupProfile("openai", r.Model); known {
		imageInput = profile.ImageInput
	}
	// An explicit request override wins: the view_image tool forces image input
	// for vision models whose name is not in the capability table (self-hosted
	// vLLM deployments), so their image blocks are not degraded to text.
	if r.ImageInput != nil {
		imageInput = *r.ImageInput
	}

	for _, m := range r.Messages {
		msgs, err := convertMessage(m, imageInput)
		if err != nil {
			return apiRequest{}, err
		}
		req.Messages = append(req.Messages, msgs...)
	}

	tools := r.Tools
	jsonResp := r.JSONResponse
	if profile, known := provider.LookupProfile("openai", r.Model); known && !profile.ToolCalling {
		tools = nil
		jsonResp = nil
	}
	for _, t := range tools {
		var at apiTool
		at.Type = "function"
		at.Function.Name = t.Name
		at.Function.Description = t.Description
		at.Function.Parameters = t.InputSchema
		req.Tools = append(req.Tools, at)
	}

	// Structured output (L3): append the synthetic response function. We do NOT
	// send tool_choice: the OpenAI-compatible gateway in use rejects the
	// object form of tool_choice (400 invalid_request_error), though it accepts
	// tools and the string form "auto". Instead the prompt instructs the model
	// to call the function (soft-forcing); reasoning models reliably do, and
	// the answer still arrives as a tool_call's JSON arguments — isolated from
	// any prose. The reader tolerates a missing tool call as an error.
	if jr := jsonResp; jr != nil {
		var at apiTool
		at.Type = "function"
		at.Function.Name = jr.Name
		at.Function.Description = jr.Description
		at.Function.Parameters = jr.Schema
		req.Tools = append(req.Tools, at)
	}
	return req, nil
}

// rawString encodes a plain string as JSON for apiMessage.Content (the legacy
// string content form).
func rawString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

// convertMessage flattens one canonical message into one or more OpenAI
// messages. Tool results become role=tool messages keyed by tool_call_id.
// When imageInput is true and a BlockImage has materialized data, the message
// content becomes an array of parts (text + image_url); otherwise images
// degrade to a text reference so the request still sends.
func convertMessage(m provider.Message, imageInput bool) ([]apiMessage, error) {
	var text string
	var calls []apiCall
	var results []apiMessage
	var images []apiContentPart

	for _, b := range m.Content {
		switch b.Type {
		case provider.BlockText:
			text += b.Text
		case provider.BlockToolUse:
			// A tool call whose arguments were never parsed (ArgsError) or
			// streamed as JSON null persists with a nil ToolInput; the OpenAI
			// API rejects any non-object arguments value ("null" included), so
			// an unparseable historical call is re-sent with an empty object.
			toolInput := b.ToolInput
			if toolInput == nil {
				toolInput = map[string]any{}
			}
			args, err := json.Marshal(toolInput)
			if err != nil {
				return nil, fmt.Errorf("marshal tool input: %w", err)
			}
			var c apiCall
			c.ID = b.ToolUseID
			c.Type = "function"
			c.Function.Name = b.ToolName
			c.Function.Arguments = string(args)
			calls = append(calls, c)
		case provider.BlockToolResult:
			results = append(results, apiMessage{
				Role:       "tool",
				Content:    rawString(b.ToolContent),
				ToolCallID: b.ToolResultID,
			})
		case provider.BlockThinking:
			// Not representable; intentionally dropped.
		case provider.BlockImage:
			if imageInput && b.ImageData != "" {
				mediaType := b.MediaType
				if mediaType == "" {
					mediaType = "image/webp"
				}
				images = append(images, apiContentPart{
					Type: "image_url",
					ImageURL: &apiImageURL{
						URL: "data:" + mediaType + ";base64," + b.ImageData,
					},
				})
			} else {
				text += "[image: " + b.ImagePath + "]"
			}
		}
	}

	content := contentJSON(text, images)

	var out []apiMessage
	if m.Role == provider.RoleAssistant && len(calls) > 0 {
		out = append(out, apiMessage{Role: "assistant", Content: content, ToolCalls: calls})
	} else if len(results) > 0 && len(images) == 0 && text == "" {
		// pure tool-result message(s)
		out = append(out, results...)
		return out, nil
	} else {
		out = append(out, apiMessage{Role: string(m.Role), Content: content})
	}
	out = append(out, results...)
	return out, nil
}

// contentJSON serializes a message's content: a plain string when there are no
// image parts (legacy form), otherwise an array of text + image_url parts.
func contentJSON(text string, images []apiContentPart) json.RawMessage {
	if len(images) == 0 {
		return rawString(text)
	}
	parts := make([]apiContentPart, 0, len(images)+1)
	if text != "" {
		parts = append(parts, apiContentPart{Type: "text", Text: text})
	}
	parts = append(parts, images...)
	b, _ := json.Marshal(parts)
	return b
}

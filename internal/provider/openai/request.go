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
	// ToolChoice forces a named function when set (used for structured output).
	ToolChoice    *apiToolChoice `json:"tool_choice,omitempty"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
	Stream        bool           `json:"stream"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
}

// apiToolChoice forces the model to call one named function.
type apiToolChoice struct {
	Type     string `json:"type"` // "function"
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
}

// streamOptions requests a final usage chunk in the SSE stream. Without
// include_usage the gateway omits usage entirely for streamed responses, so the
// loop would have no token counts to report.
type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type apiMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []apiCall  `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
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
// Pure: no I/O.
func buildRequest(r provider.Request) (apiRequest, error) {
	req := apiRequest{Model: r.Model, MaxTokens: r.MaxTokens, Stream: true, StreamOptions: &streamOptions{IncludeUsage: true}}

	if r.System != "" {
		req.Messages = append(req.Messages, apiMessage{Role: "system", Content: r.System})
	}

	for _, m := range r.Messages {
		msgs, err := convertMessage(m)
		if err != nil {
			return apiRequest{}, err
		}
		req.Messages = append(req.Messages, msgs...)
	}

	for _, t := range r.Tools {
		var at apiTool
		at.Type = "function"
		at.Function.Name = t.Name
		at.Function.Description = t.Description
		at.Function.Parameters = t.InputSchema
		req.Tools = append(req.Tools, at)
	}

	// Structured output (L3): append the synthetic response function and force it.
	if jr := r.JSONResponse; jr != nil {
		var at apiTool
		at.Type = "function"
		at.Function.Name = jr.Name
		at.Function.Description = jr.Description
		at.Function.Parameters = jr.Schema
		req.Tools = append(req.Tools, at)
		tc := &apiToolChoice{Type: "function"}
		tc.Function.Name = jr.Name
		req.ToolChoice = tc
	}
	return req, nil
}

// convertMessage flattens one canonical message into one or more OpenAI
// messages. Tool results become role=tool messages keyed by tool_call_id.
func convertMessage(m provider.Message) ([]apiMessage, error) {
	var text string
	var calls []apiCall
	var results []apiMessage

	for _, b := range m.Content {
		switch b.Type {
		case provider.BlockText:
			text += b.Text
		case provider.BlockToolUse:
			args, err := json.Marshal(b.ToolInput)
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
				Content:    b.ToolContent,
				ToolCallID: b.ToolResultID,
			})
		case provider.BlockThinking:
			// Not representable; intentionally dropped.
		case provider.BlockImage:
			// OpenAI content here is a plain string; degrade images to a text
			// reference. (The OpenAI-compat gateway in use does not accept image
			// parts on this path; Anthropic is the image-capable provider.)
			text += "[image: " + b.ImagePath + "]"
		}
	}

	var out []apiMessage
	if m.Role == provider.RoleAssistant && len(calls) > 0 {
		out = append(out, apiMessage{Role: "assistant", Content: text, ToolCalls: calls})
	} else if len(results) > 0 && text == "" {
		// pure tool-result message(s)
		out = append(out, results...)
		return out, nil
	} else {
		out = append(out, apiMessage{Role: string(m.Role), Content: text})
	}
	out = append(out, results...)
	return out, nil
}

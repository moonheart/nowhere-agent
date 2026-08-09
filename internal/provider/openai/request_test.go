package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"nowhere-agent/internal/provider"
)

// contentString decodes a legacy string-form Content for assertions.
func contentString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return string(raw)
	}
	return s
}

func TestBuildRequestSystemAndUser(t *testing.T) {
	req, err := buildRequest(provider.Request{
		Model:    "gpt-test",
		System:   "be nice",
		Messages: []provider.Message{provider.TextMessage(provider.RoleUser, "hi")},
	})
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("expected system+user, got %d", len(req.Messages))
	}
	if req.Messages[0].Role != "system" || contentString(req.Messages[0].Content) != "be nice" {
		t.Errorf("system message wrong: %+v", req.Messages[0])
	}
	if req.Messages[1].Role != "user" {
		t.Errorf("user message wrong: %+v", req.Messages[1])
	}
}

func TestBuildRequestToolUseAndResult(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleAssistant, Content: []provider.Block{
			{Type: provider.BlockToolUse, ToolUseID: "c1", ToolName: "read", ToolInput: map[string]any{"path": "/x"}},
		}},
		{Role: provider.RoleUser, Content: []provider.Block{
			{Type: provider.BlockToolResult, ToolResultID: "c1", ToolContent: "data"},
		}},
	}
	req, err := buildRequest(provider.Request{Model: "m", Messages: msgs})
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	var assistant, tool *apiMessage
	for i := range req.Messages {
		m := &req.Messages[i]
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			assistant = m
		}
		if m.Role == "tool" {
			tool = m
		}
	}
	if assistant == nil || assistant.ToolCalls[0].Function.Name != "read" {
		t.Fatalf("assistant tool_call missing: %+v", req.Messages)
	}
	if assistant.ToolCalls[0].Function.Arguments == "" {
		t.Error("tool args not serialized")
	}
	if tool == nil || tool.ToolCallID != "c1" || contentString(tool.Content) != "data" {
		t.Fatalf("tool result message wrong: %+v", tool)
	}
}

func TestBuildRequestDropsThinking(t *testing.T) {
	req, err := buildRequest(provider.Request{
		Model: "m",
		Messages: []provider.Message{
			{Role: provider.RoleAssistant, Content: []provider.Block{
				{Type: provider.BlockThinking, Thinking: "hmm"},
				{Type: provider.BlockText, Text: "answer"},
			}},
		},
	})
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	// Only the text should survive.
	if contentString(req.Messages[0].Content) != "answer" {
		t.Errorf("thinking not dropped / text wrong: %+v", req.Messages[0])
	}
}

// TestBuildRequestIncludesUsage pins L2: streamed requests must ask for a final
// usage chunk, else the gateway omits usage entirely and the loop has nothing to
// report.
func TestBuildRequestIncludesUsage(t *testing.T) {
	req, err := buildRequest(provider.Request{
		Model:    "m",
		Messages: []provider.Message{provider.TextMessage(provider.RoleUser, "hi")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if req.StreamOptions == nil || !req.StreamOptions.IncludeUsage {
		t.Error("stream_options.include_usage must be set so streamed responses return usage")
	}
}

func TestBuildRequestTools(t *testing.T) {
	req, err := buildRequest(provider.Request{
		Model: "m",
		Tools: []provider.ToolDefinition{{Name: "read", Description: "d", InputSchema: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if len(req.Tools) != 1 || req.Tools[0].Function.Name != "read" {
		t.Fatalf("tools wrong: %+v", req.Tools)
	}
}

// TestBuildRequestJSONResponseToolNoForce pins L3 on the OpenAI-compatible
// gateway: the synthetic response function is appended, but tool_choice is NOT
// sent (the gateway rejects the object form of tool_choice with a 400). The
// model is soft-forced via the prompt instead.
func TestBuildRequestJSONResponseToolNoForce(t *testing.T) {
	req, err := buildRequest(provider.Request{
		Model:    "m",
		Messages: []provider.Message{provider.TextMessage(provider.RoleUser, "hi")},
		JSONResponse: &provider.JSONResponseSpec{
			Name:        "record_facts",
			Description: "d",
			Schema:      map[string]any{"type": "object"},
		},
	})
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	last := req.Tools[len(req.Tools)-1]
	if last.Function.Name != "record_facts" || last.Function.Parameters["type"] != "object" {
		t.Errorf("response tool wrong: %+v", last)
	}
	// Marshal to confirm no tool_choice key is emitted.
	b, _ := json.Marshal(req)
	if strings.Contains(string(b), "tool_choice") {
		t.Errorf("tool_choice must not be sent (gateway 400s on it): %s", b)
	}
}

// Sampling params are forwarded for models whose profile allows them.
func TestBuildRequestSamplingPassThrough(t *testing.T) {
	temp, topP := 0.3, 0.8
	req, err := buildRequest(provider.Request{
		Model: "gpt-4o", Temperature: &temp, TopP: &topP, StopSequences: []string{"END"},
	})
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if req.Temperature == nil || *req.Temperature != 0.3 {
		t.Errorf("temperature not forwarded: %+v", req.Temperature)
	}
	if req.TopP == nil || *req.TopP != 0.8 {
		t.Errorf("top_p not forwarded: %+v", req.TopP)
	}
	if len(req.Stop) != 1 || req.Stop[0] != "END" {
		t.Errorf("stop not forwarded: %+v", req.Stop)
	}
}

// Reasoning models reject temperature/top_p with a 400; the profile gates
// them out before the wire.
func TestBuildRequestSamplingGatedForReasoners(t *testing.T) {
	temp := 0.3
	for _, model := range []string{"o3", "o3-mini", "gpt-5", "deepseek-reasoner"} {
		req, err := buildRequest(provider.Request{Model: model, Temperature: &temp})
		if err != nil {
			t.Fatalf("buildRequest(%s): %v", model, err)
		}
		if req.Temperature != nil {
			t.Errorf("%s: temperature must be dropped (profile forbids sampling)", model)
		}
	}
	// Unknown model: no profile, no gating — caller's value passes through.
	req, err := buildRequest(provider.Request{Model: "my-fine-tune", Temperature: &temp})
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if req.Temperature == nil {
		t.Error("unknown model: temperature must pass through (no profile, no gating)")
	}
}

// A model profiled without tool calling gets no tools on the wire.
func TestBuildRequestToolsGatedForNonToolModel(t *testing.T) {
	req, err := buildRequest(provider.Request{
		Model: "o1-mini",
		Tools: []provider.ToolDefinition{{Name: "read", Description: "d", InputSchema: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if len(req.Tools) != 0 {
		t.Errorf("o1-mini must get no tools: %+v", req.Tools)
	}
}

// imageBlock builds a canonical image block with materialized base64.
func imageBlock(path, mediaType, data string) provider.Block {
	return provider.Block{Type: provider.BlockImage, MediaType: mediaType, ImagePath: path, ImageData: data}
}

// contentParts decodes an array-form Content into parts.
func contentParts(raw json.RawMessage) []apiContentPart {
	var parts []apiContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil
	}
	return parts
}

// TestBuildRequestVisionModelEmitsImageURLParts: a vision-capable model gets
// its materialized image serialized as an image_url content part.
func TestBuildRequestVisionModelEmitsImageURLParts(t *testing.T) {
	req, err := buildRequest(provider.Request{
		Model: "gpt-4o",
		Messages: []provider.Message{{
			Role: provider.RoleUser,
			Content: []provider.Block{
				{Type: provider.BlockText, Text: "what is this"},
				imageBlock("img/a.webp", "image/webp", "AAAA"),
			},
		}},
	})
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	msg := req.Messages[len(req.Messages)-1]
	parts := contentParts(msg.Content)
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want 2 (text + image_url): %s", len(parts), msg.Content)
	}
	if parts[0].Type != "text" || parts[0].Text != "what is this" {
		t.Errorf("part[0] = %+v, want the text part", parts[0])
	}
	if parts[1].Type != "image_url" || parts[1].ImageURL == nil {
		t.Fatalf("part[1] = %+v, want an image_url part", parts[1])
	}
	want := "data:image/webp;base64,AAAA"
	if parts[1].ImageURL.URL != want {
		t.Errorf("image_url = %q, want %q", parts[1].ImageURL.URL, want)
	}
}

// TestBuildRequestNonVisionModelDegrades: a model without native image input
// still gets the legacy text degradation, not image parts.
func TestBuildRequestNonVisionModelDegrades(t *testing.T) {
	req, err := buildRequest(provider.Request{
		Model: "deepseek-chat",
		Messages: []provider.Message{{
			Role:    provider.RoleUser,
			Content: []provider.Block{imageBlock("img/a.webp", "image/webp", "AAAA")},
		}},
	})
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	msg := req.Messages[len(req.Messages)-1]
	if parts := contentParts(msg.Content); len(parts) > 0 {
		t.Fatalf("non-vision model must not emit image parts, got %s", msg.Content)
	}
	if !strings.Contains(contentString(msg.Content), "[image: img/a.webp]") {
		t.Errorf("degraded content = %q, want an [image: path] reference", contentString(msg.Content))
	}
}

// TestBuildRequestUnmaterializedImageDegradesEvenForVisionModel: a vision model
// cannot serialize an image with no materialized data, so it degrades too.
func TestBuildRequestUnmaterializedImageDegradesEvenForVisionModel(t *testing.T) {
	req, err := buildRequest(provider.Request{
		Model: "gpt-4o",
		Messages: []provider.Message{{
			Role:    provider.RoleUser,
			Content: []provider.Block{imageBlock("img/a.webp", "image/webp", "")},
		}},
	})
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	msg := req.Messages[len(req.Messages)-1]
	if parts := contentParts(msg.Content); len(parts) > 0 {
		t.Fatalf("unmaterialized image must degrade, got %s", msg.Content)
	}
	if !strings.Contains(contentString(msg.Content), "[image: img/a.webp]") {
		t.Errorf("degraded content = %q, want an [image: path] reference", contentString(msg.Content))
	}
}

// TestBuildRequestForcedImageInputOverridesProfile: a model outside the
// capability table (self-hosted vLLM) still emits image_url parts when the
// request forces image input — the view_image tool's contract.
func TestBuildRequestForcedImageInputOverridesProfile(t *testing.T) {
	force := true
	req, err := buildRequest(provider.Request{
		Model:      "mimo-v2.5",
		ImageInput: &force,
		Messages: []provider.Message{{
			Role:    provider.RoleUser,
			Content: []provider.Block{imageBlock("img/a.webp", "image/webp", "AAAA")},
		}},
	})
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	msg := req.Messages[len(req.Messages)-1]
	parts := contentParts(msg.Content)
	if len(parts) != 1 || parts[0].Type != "image_url" || parts[0].ImageURL == nil {
		t.Fatalf("forced image input must emit image_url, got %s", msg.Content)
	}
	want := "data:image/webp;base64,AAAA"
	if parts[0].ImageURL.URL != want {
		t.Errorf("image_url = %q, want %q", parts[0].ImageURL.URL, want)
	}
}

// TestBuildRequestMixedMessageFlattensImageParts: text + image in one message
// flatten into one parts array; image-only messages still send.
func TestBuildRequestMixedMessageFlattensImageParts(t *testing.T) {
	req, err := buildRequest(provider.Request{
		Model: "gpt-4o-mini",
		Messages: []provider.Message{{
			Role:    provider.RoleUser,
			Content: []provider.Block{imageBlock("img/a.webp", "image/webp", "AAA")},
		}},
	})
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	msg := req.Messages[len(req.Messages)-1]
	parts := contentParts(msg.Content)
	if len(parts) != 1 || parts[0].Type != "image_url" {
		t.Fatalf("image-only message parts = %+v, want one image_url part", parts)
	}
}

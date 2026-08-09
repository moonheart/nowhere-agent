package anthropic

import (
	"testing"

	"nowhere-agent/internal/provider"
)

func TestBuildRequestBasics(t *testing.T) {
	req := buildRequest(provider.Request{
		Model:     "claude-test",
		MaxTokens: 1024,
		System:    "you are helpful",
		Messages:  []provider.Message{provider.TextMessage(provider.RoleUser, "hi")},
	})
	if req.Model != "claude-test" || req.MaxTokens != 1024 || !req.Stream {
		t.Fatalf("unexpected request: %+v", req)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != "user" {
		t.Fatalf("unexpected messages: %+v", req.Messages)
	}
}

func TestBuildRequestMergesConsecutiveUserMessages(t *testing.T) {
	req := buildRequest(provider.Request{
		Model: "m", MaxTokens: 1,
		Messages: []provider.Message{
			provider.TextMessage(provider.RoleUser, "[Earlier conversation summarized]\nS"),
			provider.TextMessage(provider.RoleUser, "first"),
			provider.TextMessage(provider.RoleAssistant, "reply"),
			provider.TextMessage(provider.RoleUser, "second"),
		},
	})
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %d, want 3 (leading user pair merged): %+v", len(req.Messages), req.Messages)
	}
	if req.Messages[0].Role != "user" || len(req.Messages[0].Content) != 2 {
		t.Errorf("merged user message must carry both text blocks: %+v", req.Messages[0])
	}
	if req.Messages[1].Role != "assistant" || req.Messages[2].Role != "user" {
		t.Errorf("roles = %q/%q/%q, want user/assistant/user",
			req.Messages[0].Role, req.Messages[1].Role, req.Messages[2].Role)
	}

	// Assistant messages are never merged (thinking must stay first).
	two := buildRequest(provider.Request{
		Model: "m", MaxTokens: 1,
		Messages: []provider.Message{
			provider.TextMessage(provider.RoleAssistant, "a"),
			provider.TextMessage(provider.RoleAssistant, "b"),
		},
	})
	if len(two.Messages) != 2 {
		t.Errorf("assistant messages = %d, want 2 (never merged)", len(two.Messages))
	}
}

func TestBuildRequestSystemCachePoint(t *testing.T) {
	withCache := buildRequest(provider.Request{
		Model: "m", MaxTokens: 1, System: "sys", CacheablePrefix: true,
	})
	blocks, ok := withCache.System.([]systemBlock)
	if !ok || len(blocks) != 1 {
		t.Fatalf("expected system blocks, got %T", withCache.System)
	}
	if blocks[0].CacheCtl == nil {
		t.Error("expected cache_control on system when CacheablePrefix set")
	}

	noCache := buildRequest(provider.Request{Model: "m", MaxTokens: 1, System: "sys"})
	blocks2, _ := noCache.System.([]systemBlock)
	if blocks2[0].CacheCtl != nil {
		t.Error("did not expect cache_control without CacheablePrefix")
	}
}

func TestBuildRequestTools(t *testing.T) {
	req := buildRequest(provider.Request{
		Model: "m", MaxTokens: 1,
		Tools: []provider.ToolDefinition{
			{Name: "read", Description: "read file", InputSchema: map[string]any{"type": "object"}},
		},
	})
	if len(req.Tools) != 1 || req.Tools[0].Name != "read" {
		t.Fatalf("unexpected tools: %+v", req.Tools)
	}
	if req.Tools[0].InputSchema["type"] != "object" {
		t.Errorf("schema not preserved: %+v", req.Tools[0].InputSchema)
	}
}

func TestConvertBlocksRoundTrip(t *testing.T) {
	blocks := []provider.Block{
		{Type: provider.BlockText, Text: "hello"},
		{Type: provider.BlockThinking, Thinking: "hmm", ThinkingSignature: "sig"},
		{Type: provider.BlockToolUse, ToolUseID: "tu1", ToolName: "read", ToolInput: map[string]any{"path": "/x"}},
		{Type: provider.BlockToolResult, ToolResultID: "tu1", ToolContent: "data", IsError: false},
	}
	out := convertBlocks(blocks)
	if len(out) != 4 {
		t.Fatalf("expected 4 blocks, got %d", len(out))
	}
	if out[0].Type != "text" || out[0].Text != "hello" {
		t.Errorf("text block wrong: %+v", out[0])
	}
	if out[1].Type != "thinking" || out[1].Thinking != "hmm" || out[1].Signature != "sig" {
		t.Errorf("thinking block wrong (must round-trip signature): %+v", out[1])
	}
	if out[2].Type != "tool_use" || out[2].ID != "tu1" || out[2].Name != "read" {
		t.Errorf("tool_use block wrong: %+v", out[2])
	}
	if out[3].Type != "tool_result" || out[3].ToolUseID != "tu1" || out[3].Content != "data" {
		t.Errorf("tool_result block wrong: %+v", out[3])
	}
}

func TestConvertBlocksTextCachePoint(t *testing.T) {
	out := convertBlocks([]provider.Block{{Type: provider.BlockText, Text: "x", CachePoint: true}})
	if out[0].CacheCtl == nil {
		t.Error("expected cache_control on text block with CachePoint")
	}
}

func TestConvertBlocksImageMaterialized(t *testing.T) {
	out := convertBlocks([]provider.Block{
		{Type: provider.BlockImage, MediaType: "image/webp", ImagePath: "img/a.webp", ImageData: "QUJD"},
	})
	if len(out) != 1 || out[0].Type != "image" {
		t.Fatalf("expected image block, got %+v", out)
	}
	if out[0].Source == nil || out[0].Source.Type != "base64" || out[0].Source.MediaType != "image/webp" || out[0].Source.Data != "QUJD" {
		t.Errorf("image source wrong: %+v", out[0].Source)
	}
}

func TestConvertBlocksImageUnmaterialized(t *testing.T) {
	// No ImageData (e.g. dangling path) → text placeholder, request still sends.
	out := convertBlocks([]provider.Block{
		{Type: provider.BlockImage, MediaType: "image/webp", ImagePath: "img/gone.webp"},
	})
	if len(out) != 1 || out[0].Type != "text" {
		t.Fatalf("expected text placeholder, got %+v", out)
	}
	if out[0].Text == "" || out[0].Source != nil {
		t.Errorf("placeholder wrong: %+v", out[0])
	}
}

// TestBuildRequestJSONResponseForced pins L3: a JSONResponse request appends
// the synthetic tool and forces it via tool_choice, so the model must emit the
// object as a tool call.
func TestBuildRequestJSONResponseForced(t *testing.T) {
	req := buildRequest(provider.Request{
		Model:    "m",
		Messages: []provider.Message{provider.TextMessage(provider.RoleUser, "hi")},
		JSONResponse: &provider.JSONResponseSpec{
			Name:        "record_facts",
			Description: "d",
			Schema:      map[string]any{"type": "object"},
		},
	})
	if req.ToolChoice == nil || req.ToolChoice.Type != "tool" || req.ToolChoice.Name != "record_facts" {
		t.Fatalf("tool_choice wrong: %+v", req.ToolChoice)
	}
	last := req.Tools[len(req.Tools)-1]
	if last.Name != "record_facts" || last.InputSchema["type"] != "object" {
		t.Errorf("response tool wrong: %+v", last)
	}
}

func TestBuildRequestSampling(t *testing.T) {
	temp, topP := 0.2, 0.9
	req := buildRequest(provider.Request{
		Model: "claude-sonnet-4", MaxTokens: 1,
		Temperature: &temp, TopP: &topP, StopSequences: []string{"END"},
	})
	if req.Temperature == nil || *req.Temperature != 0.2 {
		t.Errorf("temperature not forwarded: %+v", req.Temperature)
	}
	if req.TopP == nil || *req.TopP != 0.9 {
		t.Errorf("top_p not forwarded: %+v", req.TopP)
	}
	if len(req.StopSequences) != 1 || req.StopSequences[0] != "END" {
		t.Errorf("stop_sequences not forwarded: %+v", req.StopSequences)
	}
	if req.Thinking != nil {
		t.Error("thinking must be absent when not requested")
	}
}

// Extended thinking: the API forbids temperature alongside it and requires
// max_tokens > budget, so the builder drops sampling and enlarges max_tokens.
func TestBuildRequestThinking(t *testing.T) {
	temp := 0.2
	req := buildRequest(provider.Request{
		Model: "claude-sonnet-4", MaxTokens: 1024,
		Temperature: &temp,
		Thinking:    &provider.ThinkingSpec{BudgetTokens: 4096},
	})
	if req.Thinking == nil || req.Thinking.Type != "enabled" || req.Thinking.BudgetTokens != 4096 {
		t.Fatalf("thinking wrong: %+v", req.Thinking)
	}
	if req.Temperature != nil {
		t.Error("temperature must be dropped when thinking is enabled")
	}
	if req.MaxTokens <= 4096 {
		t.Errorf("max_tokens = %d, want > budget (4096)", req.MaxTokens)
	}

	roomy := buildRequest(provider.Request{
		Model: "claude-sonnet-4", MaxTokens: 8192,
		Thinking: &provider.ThinkingSpec{BudgetTokens: 4096},
	})
	if roomy.MaxTokens != 8192 {
		t.Errorf("max_tokens = %d, want unchanged 8192 (already exceeds budget)", roomy.MaxTokens)
	}
}

func TestNormalizeToolUseID(t *testing.T) {
	if got := normalizeToolUseID("toolu_01ABC_xyz-"); got != "toolu_01ABC_xyz-" {
		t.Errorf("valid id rewritten: %q", got)
	}
	// Cross-provider ids (Fireworks/Kimi style) are illegal for Anthropic.
	a := normalizeToolUseID("functions.write_todos:0")
	if a == "functions.write_todos:0" {
		t.Error("illegal id passed through")
	}
	if !toolUseIDPattern.MatchString(a) {
		t.Errorf("normalized id %q still violates the Anthropic pattern", a)
	}
	// Deterministic: the tool_use and its tool_result must normalize alike.
	if b := normalizeToolUseID("functions.write_todos:0"); b != a {
		t.Errorf("not deterministic: %q vs %q", a, b)
	}
	// Overlong ids (legal charset, >64) are also rejected by the API.
	long := make([]byte, 100)
	for i := range long {
		long[i] = 'a'
	}
	if got := normalizeToolUseID(string(long)); !toolUseIDPattern.MatchString(got) {
		t.Errorf("overlong id normalized to %q, still invalid", got)
	}
}

// convertBlocks applies the SAME normalization to tool_use and tool_result,
// keeping the pair correlated after rewriting.
func TestConvertBlocksNormalizesIDsConsistently(t *testing.T) {
	out := convertBlocks([]provider.Block{
		{Type: provider.BlockToolUse, ToolUseID: "functions.read:0", ToolName: "read", ToolInput: map[string]any{}},
		{Type: provider.BlockToolResult, ToolResultID: "functions.read:0", ToolContent: "ok"},
	})
	if out[0].ID == "functions.read:0" {
		t.Fatal("illegal id not normalized")
	}
	if out[0].ID != out[1].ToolUseID {
		t.Errorf("pairing broken: tool_use id %q != tool_result id %q", out[0].ID, out[1].ToolUseID)
	}
}

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

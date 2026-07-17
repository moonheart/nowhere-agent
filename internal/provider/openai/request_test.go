package openai

import (
	"testing"

	"nowhere-agent/internal/provider"
)

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
	if req.Messages[0].Role != "system" || req.Messages[0].Content != "be nice" {
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
	if tool == nil || tool.ToolCallID != "c1" || tool.Content != "data" {
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
	if req.Messages[0].Content != "answer" {
		t.Errorf("thinking not dropped / text wrong: %+v", req.Messages[0])
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

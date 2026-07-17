package provider

import "testing"

func TestTextMessage(t *testing.T) {
	m := TextMessage(RoleUser, "hello")
	if m.Role != RoleUser {
		t.Errorf("got role %q", m.Role)
	}
	if len(m.Content) != 1 || m.Content[0].Type != BlockText || m.Content[0].Text != "hello" {
		t.Errorf("unexpected content: %+v", m.Content)
	}
}

func TestBlockTypesDistinct(t *testing.T) {
	types := []BlockType{BlockText, BlockToolUse, BlockToolResult, BlockThinking}
	seen := map[BlockType]bool{}
	for _, bt := range types {
		if seen[bt] {
			t.Fatalf("duplicate block type %q", bt)
		}
		seen[bt] = true
	}
}

func TestEventTypesDistinct(t *testing.T) {
	types := []EventType{EventMessageStart, EventBlockStart, EventBlockDelta, EventBlockStop, EventMessageStop, EventError}
	seen := map[EventType]bool{}
	for _, et := range types {
		if seen[et] {
			t.Fatalf("duplicate event type %q", et)
		}
		seen[et] = true
	}
}

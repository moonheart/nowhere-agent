package contextmgmt

import (
	"testing"

	"nowhere-agent/internal/provider"
)

func TestEnsurePairingWellFormedUnchanged(t *testing.T) {
	msgs := []provider.Message{
		provider.TextMessage(provider.RoleUser, "hi"),
		toolUse("t1", "search"),
		toolResult("t1"),
		provider.TextMessage(provider.RoleAssistant, "done"),
	}
	out := EnsurePairing(msgs)
	if len(out) != len(msgs) {
		t.Fatalf("well-formed input changed length: %d -> %d", len(msgs), len(out))
	}
	assertPaired(t, out)
	// No synthetic error results introduced.
	for _, m := range out {
		for _, b := range m.Content {
			if b.Type == provider.BlockToolResult && b.IsError {
				t.Error("well-formed input should not gain synthetic error results")
			}
		}
	}
}

func TestEnsurePairingDanglingToolUseGetsErrorResult(t *testing.T) {
	// A cancelled run persisted the tool_use but the result never ran.
	msgs := []provider.Message{
		provider.TextMessage(provider.RoleUser, "hi"),
		toolUse("t1", "search"),
		provider.TextMessage(provider.RoleAssistant, "never mind"),
	}
	out := EnsurePairing(msgs)
	assertPaired(t, out)
	// A synthetic is_error result for t1 must appear right after the tool_use.
	var found bool
	for _, m := range out {
		for _, b := range m.Content {
			if b.Type == provider.BlockToolResult && b.ToolResultID == "t1" {
				found = true
				if !b.IsError {
					t.Error("synthesized result for dangling tool_use should be is_error")
				}
			}
		}
	}
	if !found {
		t.Error("dangling tool_use t1 was not answered")
	}
}

func TestEnsurePairingOrphanResultStripped(t *testing.T) {
	// Compression dropped the assistant's tool_use but the result survived.
	msgs := []provider.Message{
		provider.TextMessage(provider.RoleUser, "hi"),
		toolResult("t-ghost"), // orphan: no matching tool_use
		provider.TextMessage(provider.RoleAssistant, "ok"),
	}
	out := EnsurePairing(msgs)
	assertPaired(t, out)
	for _, m := range out {
		for _, b := range m.Content {
			if b.Type == provider.BlockToolResult && b.ToolResultID == "t-ghost" {
				t.Error("orphan tool_result should have been stripped")
			}
		}
	}
}

func TestEnsurePairingOrphanOnlyMessageGetsPlaceholder(t *testing.T) {
	// A message that is ONLY an orphan tool_result must not become empty.
	msgs := []provider.Message{
		toolResult("t-ghost"),
	}
	out := EnsurePairing(msgs)
	if len(out) != 1 {
		t.Fatalf("expected 1 message, got %d", len(out))
	}
	if len(out[0].Content) == 0 {
		t.Fatal("message stripped to empty content")
	}
	if out[0].Content[0].Type != provider.BlockText {
		t.Errorf("placeholder should be a text block, got %v", out[0].Content[0].Type)
	}
}

func TestEnsurePairingDeduplicatesIDs(t *testing.T) {
	dup := toolUse("t1", "search")
	msgs := []provider.Message{
		dup,
		dup, // duplicate tool_use id
		toolResult("t1"),
		toolResult("t1"), // duplicate result
	}
	out := EnsurePairing(msgs)
	assertPaired(t, out)
	var useCount, resultCount int
	for _, m := range out {
		for _, b := range m.Content {
			switch b.Type {
			case provider.BlockToolUse:
				if b.ToolUseID == "t1" {
					useCount++
				}
			case provider.BlockToolResult:
				if b.ToolResultID == "t1" {
					resultCount++
				}
			}
		}
	}
	if useCount != 1 || resultCount != 1 {
		t.Errorf("expected 1 use + 1 result after dedupe, got %d use %d result", useCount, resultCount)
	}
}

func TestEnsurePairingEndsWithDanglingToolUse(t *testing.T) {
	// History ends on a tool_use with no following message at all.
	msgs := []provider.Message{
		provider.TextMessage(provider.RoleUser, "hi"),
		toolUse("t1", "search"),
	}
	out := EnsurePairing(msgs)
	assertPaired(t, out)
}

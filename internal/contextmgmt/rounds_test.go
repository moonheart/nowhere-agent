package contextmgmt

import (
	"context"
	"strings"
	"testing"

	"nowhere-agent/internal/provider"
)

// toolUse builds an assistant message carrying one tool_use block.
func toolUse(id, name string) provider.Message {
	return provider.Message{
		Role: provider.RoleAssistant,
		Content: []provider.Block{
			{Type: provider.BlockText, Text: "let me check"},
			{Type: provider.BlockToolUse, ToolUseID: id, ToolName: name, ToolInput: map[string]any{"q": "x"}},
		},
	}
}

// toolResult builds a user message answering the given tool_use ids.
func toolResult(ids ...string) provider.Message {
	m := provider.Message{Role: provider.RoleUser}
	for _, id := range ids {
		m.Content = append(m.Content, provider.Block{
			Type:         provider.BlockToolResult,
			ToolResultID: id,
			ToolContent:  "result for " + id,
		})
	}
	return m
}

func TestGroupRoundsKeepsToolExchangeTogether(t *testing.T) {
	msgs := []provider.Message{
		provider.TextMessage(provider.RoleUser, "hi"),
		toolUse("t1", "search"),
		toolResult("t1"),
		provider.TextMessage(provider.RoleAssistant, "done"),
	}
	rounds := groupRounds(msgs)
	if len(rounds) != 3 {
		t.Fatalf("expected 3 rounds, got %d: %+v", len(rounds), rounds)
	}
	// The tool exchange (assistant toolUse + its result) must be ONE round.
	if rounds[1].start != 1 || rounds[1].end != 3 {
		t.Errorf("tool exchange round = [%d,%d), want [1,3)", rounds[1].start, rounds[1].end)
	}
}

func TestGroupRoundsMultipleResultsOneRound(t *testing.T) {
	m := provider.Message{
		Role: provider.RoleAssistant,
		Content: []provider.Block{
			{Type: provider.BlockToolUse, ToolUseID: "a", ToolName: "x"},
			{Type: provider.BlockToolUse, ToolUseID: "b", ToolName: "y"},
		},
	}
	msgs := []provider.Message{m, toolResult("a", "b")}
	rounds := groupRounds(msgs)
	if len(rounds) != 1 {
		t.Fatalf("multi-call exchange should be one round, got %d", len(rounds))
	}
}

func TestCompressNeverSeversToolPair(t *testing.T) {
	// KeepRecent=1 message-count would have split toolUse from toolResult;
	// round-based keeps them together.
	msgs := []provider.Message{
		provider.TextMessage(provider.RoleUser, strings.Repeat("x", 400)),
		toolUse("t1", "search"),
		toolResult("t1"),
		provider.TextMessage(provider.RoleAssistant, strings.Repeat("y", 400)),
	}
	p := Policy{MaxTokens: 10, Threshold: 0.8, KeepRecent: 1}
	c := &stubCompressor{}
	out, err := Compress(context.Background(), msgs, p, c)
	if err != nil {
		t.Fatal(err)
	}
	// The kept round is the final assistant text; everything older is summarized.
	// Critically the tool_use and its result are either both summarized or both
	// kept — never split. Assert the surviving messages are all paired.
	assertPaired(t, out)
	// And the summary replaced the older content.
	if !strings.Contains(out[0].Content[0].Text, "SUMMARY") {
		t.Errorf("expected summary first, got %q", out[0].Content[0].Text)
	}
}

func TestCompressKeepsRecentRoundVerbatim(t *testing.T) {
	msgs := []provider.Message{
		provider.TextMessage(provider.RoleUser, strings.Repeat("x", 400)),
		provider.TextMessage(provider.RoleAssistant, "old answer"),
		provider.TextMessage(provider.RoleUser, "recent question"),
		toolUse("t9", "lookup"),
		toolResult("t9"),
	}
	p := Policy{MaxTokens: 100, Threshold: 0.8, KeepRecent: 2}
	c := &stubCompressor{}
	out, err := Compress(context.Background(), msgs, p, c)
	if err != nil {
		t.Fatal(err)
	}
	assertPaired(t, out)
	// KeepRecent=2 rounds: [recent question] + [toolUse+result]. The kept
	// tool_use must survive verbatim (not summarized). The budget is large
	// enough that the post-check hard-drop does not trigger — this test pins
	// the split boundary, not the oversize fallback.
	foundToolUse := false
	for _, m := range out {
		for _, b := range m.Content {
			if b.Type == provider.BlockToolUse && b.ToolUseID == "t9" {
				foundToolUse = true
			}
		}
	}
	if !foundToolUse {
		t.Error("recent round's tool_use should be kept verbatim")
	}
}

func TestDropOldestRoundPreservingSummaryKeepsSummary(t *testing.T) {
	msgs := []provider.Message{
		SummaryMessage("old stuff"),
		provider.TextMessage(provider.RoleUser, "u1"),
		provider.TextMessage(provider.RoleAssistant, "a1"),
		provider.TextMessage(provider.RoleUser, "u2"),
	}
	out, ok := DropOldestRoundPreservingSummary(msgs)
	if !ok {
		t.Fatal("should drop a round")
	}
	if !IsSummary(out[0]) {
		t.Error("summary must be preserved")
	}
	if len(out) != 3 {
		t.Errorf("out = %d msgs, want 3 (summary kept, oldest real round dropped)", len(out))
	}
	for _, m := range out {
		for _, b := range m.Content {
			if b.Text == "u1" {
				t.Error("the oldest real round should have been dropped")
			}
		}
	}
}

func TestDropOldestRoundPreservingSummaryRefusesAtSummaryPlusOne(t *testing.T) {
	msgs := []provider.Message{
		SummaryMessage("old stuff"),
		provider.TextMessage(provider.RoleUser, "u2"),
	}
	if _, ok := DropOldestRoundPreservingSummary(msgs); ok {
		t.Error("must refuse to drop the summary when it is the last shrinkable context")
	}
	// Plain DropOldestRound remains the last-resort escape.
	out, ok := DropOldestRound(msgs)
	if !ok || len(out) != 1 || IsSummary(out[0]) {
		t.Errorf("last-resort drop should take the summary: ok=%v out=%v", ok, out)
	}
}

func TestDropOldestKeepingSummaryDropsDownToSummaryAlone(t *testing.T) {
	msgs := []provider.Message{
		SummaryMessage("old stuff"),
		provider.TextMessage(provider.RoleUser, "u1"),
		provider.TextMessage(provider.RoleAssistant, "a1"),
	}
	out, ok := DropOldestKeepingSummary(msgs)
	if !ok {
		t.Fatal("should drop the oldest kept round")
	}
	if len(out) != 2 || !IsSummary(out[0]) || out[1].Content[0].Text != "a1" {
		t.Errorf("out = %v, want summary + newest round", out)
	}
	// At summary+1 round — where DropOldestRoundPreservingSummary refuses —
	// it drops the last real round, leaving the summary alone.
	out2, ok := DropOldestKeepingSummary(out)
	if !ok {
		t.Fatal("should drop down to the summary alone")
	}
	if len(out2) != 1 || !IsSummary(out2[0]) {
		t.Errorf("out = %v, want the summary alone", out2)
	}
	// Nothing but the summary remains: refuse. Non-summary-led: refuse.
	if _, ok := DropOldestKeepingSummary(out2); ok {
		t.Error("must refuse when only the summary remains")
	}
	if _, ok := DropOldestKeepingSummary([]provider.Message{
		provider.TextMessage(provider.RoleUser, "u1"),
		provider.TextMessage(provider.RoleAssistant, "a1"),
	}); ok {
		t.Error("must refuse on a view with no leading summary")
	}
}

// assertPaired fails the test if any tool_use lacks a result or vice versa.
func assertPaired(t *testing.T, msgs []provider.Message) {
	t.Helper()
	uses := map[string]bool{}
	results := map[string]bool{}
	for _, m := range msgs {
		for _, b := range m.Content {
			switch b.Type {
			case provider.BlockToolUse:
				uses[b.ToolUseID] = true
			case provider.BlockToolResult:
				results[b.ToolResultID] = true
			}
		}
	}
	for id := range uses {
		if !results[id] {
			t.Errorf("tool_use %q has no matching tool_result", id)
		}
	}
	for id := range results {
		if !uses[id] {
			t.Errorf("tool_result %q has no matching tool_use", id)
		}
	}
}

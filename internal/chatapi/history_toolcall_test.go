package chatapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/session"
)

// TestBuildHistoryIncludesToolCalls verifies buildHistory folds persisted
// tool_use/tool_result blocks into tool-call parts so a reloaded client sees
// the tool activity (not just the prose), with the result merged onto its call.
func TestBuildHistoryIncludesToolCalls(t *testing.T) {
	ms := session.NewMemMessageStore()
	sess := "sess-1"
	append := func(role provider.Role, blocks ...provider.Block) {
		if _, err := ms.AppendMessage(context.Background(), session.StoredMessage{
			SessionID: sess, RunID: "r1", Role: role, Content: blocks,
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	append(provider.RoleUser, provider.Block{Type: provider.BlockText, Text: "save a note"})
	append(provider.RoleAssistant,
		provider.Block{Type: provider.BlockToolUse, ToolUseID: "tu1", ToolName: "write_file", ToolInput: map[string]any{"path": "note.txt", "content": "hi"}},
	)
	append(provider.RoleUser,
		provider.Block{Type: provider.BlockToolResult, ToolResultID: "tu1", ToolContent: "wrote 2 bytes", IsError: false},
	)
	append(provider.RoleAssistant, provider.Block{Type: provider.BlockText, Text: "done"})

	h := NewHandler(newTestLoop, "sys").WithMessageStore(ms)
	req := httptest.NewRequest("GET", "/api/chat/history?threadId="+sess, nil)
	msgs, err := h.buildHistory(req, sess)
	if err != nil {
		t.Fatalf("buildHistory: %v", err)
	}

	// Find the tool-call part and confirm its result was merged on.
	var found *historyPart
	for i := range msgs {
		for j := range msgs[i].Content {
			if msgs[i].Content[j].Type == "tool-call" {
				found = &msgs[i].Content[j]
			}
		}
	}
	if found == nil {
		t.Fatalf("no tool-call part in history: %+v", msgs)
	}
	if found.ToolCallID != "tu1" || found.ToolName != "write_file" {
		t.Errorf("tool-call id/name = %q/%q", found.ToolCallID, found.ToolName)
	}
	if !strings.Contains(found.ArgsText, "note.txt") {
		t.Errorf("argsText = %q, want it to carry the path", found.ArgsText)
	}
	if found.Result != "wrote 2 bytes" {
		t.Errorf("result = %v, want merged tool_result content", found.Result)
	}
	if found.IsError {
		t.Error("isError should be false")
	}
}

// TestBuildHistoryNestsSubagentConversation verifies a spawn_agent tool_result's
// persisted ToolMessages become a nested sub-conversation on the tool-call part,
// so a reloaded client renders the child's thinking/text/tools — not just the
// collapsed result.
func TestBuildHistoryNestsSubagentConversation(t *testing.T) {
	ms := session.NewMemMessageStore()
	sess := "sess-1"
	append := func(role provider.Role, blocks ...provider.Block) {
		if _, err := ms.AppendMessage(context.Background(), session.StoredMessage{
			SessionID: sess, RunID: "r1", Role: role, Content: blocks,
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	append(provider.RoleUser, provider.Block{Type: provider.BlockText, Text: "delegate"})
	append(provider.RoleAssistant,
		provider.Block{Type: provider.BlockToolUse, ToolUseID: "sp1", ToolName: "spawn_agent", ToolInput: map[string]any{"prompt": "x"}},
	)
	// The child's tool_result carries its sub-conversation in ToolMessages.
	append(provider.RoleUser,
		provider.Block{
			Type:         provider.BlockToolResult,
			ToolResultID: "sp1",
			ToolContent:  "child findings",
			ToolMessages: []provider.Block{
				{Type: provider.BlockThinking, Thinking: "let me look"},
				{Type: provider.BlockToolUse, ToolUseID: "c1", ToolName: "read_file", ToolInput: map[string]any{"path": "a.txt"}},
				{Type: provider.BlockToolResult, ToolResultID: "c1", ToolContent: "file body"},
				{Type: provider.BlockThinking, Thinking: "now verify"},
				{Type: provider.BlockText, Text: "child findings"},
			},
		},
	)
	append(provider.RoleAssistant, provider.Block{Type: provider.BlockText, Text: "done"})

	h := NewHandler(newTestLoop, "sys").WithMessageStore(ms)
	req := httptest.NewRequest("GET", "/api/chat/history?threadId="+sess, nil)
	msgs, err := h.buildHistory(req, sess)
	if err != nil {
		t.Fatalf("buildHistory: %v", err)
	}

	var found *historyPart
	for i := range msgs {
		for j := range msgs[i].Content {
			if msgs[i].Content[j].Type == "tool-call" && msgs[i].Content[j].ToolName == "spawn_agent" {
				found = &msgs[i].Content[j]
			}
		}
	}
	if found == nil {
		t.Fatalf("no spawn_agent tool-call part: %+v", msgs)
	}
	if found.Result != "child findings" {
		t.Errorf("collapsed result = %v", found.Result)
	}
	if len(found.Messages) != 1 {
		t.Fatalf("expected 1 nested assistant message, got %d: %+v", len(found.Messages), found.Messages)
	}
	nested := found.Messages[0]
	// Reasoning across turns folds into a single leading part (not one per turn).
	var reasoningParts, textParts, toolParts int
	var reasoningText string
	for _, p := range nested.Content {
		switch p.Type {
		case "reasoning":
			reasoningParts++
			reasoningText = p.Text
		case "text":
			textParts++
			if p.Text != "child findings" {
				t.Errorf("nested text = %q", p.Text)
			}
		case "tool-call":
			toolParts++
			if p.ToolName != "read_file" || p.Result != "file body" {
				t.Errorf("nested tool = %+v", p)
			}
		}
	}
	if reasoningParts != 1 {
		t.Errorf("reasoning parts = %d, want 1 (folded across turns)", reasoningParts)
	}
	if !strings.Contains(reasoningText, "let me look") || !strings.Contains(reasoningText, "now verify") {
		t.Errorf("folded reasoning = %q, want both turns' thinking", reasoningText)
	}
	if textParts != 1 || toolParts != 1 {
		t.Errorf("parts: text=%d tool=%d, want 1/1", textParts, toolParts)
	}
}

// TestBuildHistoryMergesOneTurnIntoOneBubble reproduces the reported regression:
// a single logical reply that spawns a subagent then verifies (spawn_agent →
// tool_result → read_file → tool_result → final text) must reload as ONE
// assistant message, not one bubble per round. This is what made a live run look
// like a single block but its reload look "split into 3".
func TestBuildHistoryMergesOneTurnIntoOneBubble(t *testing.T) {
	ms := session.NewMemMessageStore()
	sess := "sess-1"
	append := func(role provider.Role, blocks ...provider.Block) {
		if _, err := ms.AppendMessage(context.Background(), session.StoredMessage{
			SessionID: sess, RunID: "r1", Role: role, Content: blocks,
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	append(provider.RoleUser, provider.Block{Type: provider.BlockText, Text: "测试子代理, 里面写一个文件"})
	append(provider.RoleAssistant,
		provider.Block{Type: provider.BlockThinking, Thinking: "spawn a subagent"},
		provider.Block{Type: provider.BlockToolUse, ToolUseID: "sp1", ToolName: "spawn_agent", ToolInput: map[string]any{"prompt": "x"}},
	)
	append(provider.RoleUser,
		provider.Block{Type: provider.BlockToolResult, ToolResultID: "sp1", ToolContent: "child output"},
	)
	append(provider.RoleAssistant,
		provider.Block{Type: provider.BlockThinking, Thinking: "verify the file"},
		provider.Block{Type: provider.BlockText, Text: "子代理测试成功！让我确认一下文件内容:"},
		provider.Block{Type: provider.BlockToolUse, ToolUseID: "rd1", ToolName: "read_file", ToolInput: map[string]any{"path": "f.txt"}},
	)
	append(provider.RoleUser,
		provider.Block{Type: provider.BlockToolResult, ToolResultID: "rd1", ToolContent: "file body"},
	)
	append(provider.RoleAssistant,
		provider.Block{Type: provider.BlockThinking, Thinking: "test passed"},
		provider.Block{Type: provider.BlockText, Text: "✅ 子代理测试成功!"},
	)

	h := NewHandler(newTestLoop, "sys").WithMessageStore(ms)
	req := httptest.NewRequest("GET", "/api/chat/history?threadId="+sess, nil)
	msgs, err := h.buildHistory(req, sess)
	if err != nil {
		t.Fatalf("buildHistory: %v", err)
	}

	// Expect exactly 2 messages: the user's prompt + ONE merged assistant reply.
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages (user + 1 merged assistant), got %d: %+v", len(msgs), roles(msgs))
	}
	if msgs[0].Role != "user" || msgs[1].Role != "assistant" {
		t.Fatalf("roles = %v, want [user assistant]", roles(msgs))
	}
	asst := msgs[1]
	// The merged reply must carry both tool calls and all three thinking/text beats.
	var sawSpawn, sawRead bool
	var textRunes int
	for _, p := range asst.Content {
		switch p.Type {
		case "tool-call":
			if p.ToolName == "spawn_agent" {
				sawSpawn = true
				if p.Result != "child output" {
					t.Errorf("spawn_agent result = %v", p.Result)
				}
			}
			if p.ToolName == "read_file" {
				sawRead = true
				if p.Result != "file body" {
					t.Errorf("read_file result = %v", p.Result)
				}
			}
		case "text":
			textRunes += len(p.Text)
		}
	}
	if !sawSpawn || !sawRead {
		t.Errorf("merged reply missing tool calls: spawn=%v read=%v\n%+v", sawSpawn, sawRead, asst.Content)
	}
	if textRunes == 0 {
		t.Error("merged reply has no text parts")
	}
}

func roles(msgs []historyMessage) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Role
	}
	return out
}

// TestBuildHistorySplitsOnHITLGate pins the gate boundary: a run that ends on
// an ask_user/permission tool_use (no trailing text) folds its OWN rounds, but
// the verdict run's reply (a different RunID) must become a SEPARATE bubble —
// matching the live client, which shows the gated message and the verdict reply
// as two bubbles. Regression: they used to merge into one on reload.
func TestBuildHistorySplitsOnHITLGate(t *testing.T) {
	ms := session.NewMemMessageStore()
	sess := "sess-1"
	append := func(runID string, role provider.Role, blocks ...provider.Block) {
		if _, err := ms.AppendMessage(context.Background(), session.StoredMessage{
			SessionID: sess, RunID: runID, Role: role, Content: blocks,
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	append("run-user", provider.RoleUser, provider.Block{Type: provider.BlockText, Text: "测试 ask_user"})
	// Run 1 ends on the ask_user gate (bare tool_use, no trailing text).
	append("run-1", provider.RoleAssistant,
		provider.Block{Type: provider.BlockThinking, Thinking: "ask a question"},
		provider.Block{Type: provider.BlockText, Text: "让我问你一个问题。"},
		provider.Block{Type: provider.BlockToolUse, ToolUseID: "q1", ToolName: "ask_user", ToolInput: map[string]any{"questions": []any{}}},
	)
	// The verdict run injects the answer as a tool_result (user-role), then replies.
	append("run-2", provider.RoleUser,
		provider.Block{Type: provider.BlockToolResult, ToolResultID: "q1", ToolContent: `{"answers":{"q":"JavaScript"}}`},
	)
	append("run-2", provider.RoleAssistant,
		provider.Block{Type: provider.BlockThinking, Thinking: "they answered"},
		provider.Block{Type: provider.BlockText, Text: "你选择了 JavaScript。"},
	)

	h := NewHandler(newTestLoop, "sys").WithMessageStore(ms)
	req := httptest.NewRequest("GET", "/api/chat/history?threadId="+sess, nil)
	msgs, err := h.buildHistory(req, sess)
	if err != nil {
		t.Fatalf("buildHistory: %v", err)
	}

	// Expect 3 messages: user, gated assistant (run-1), verdict reply (run-2).
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages (user + gated + verdict reply), got %d: %v", len(msgs), roles(msgs))
	}
	if msgs[0].Role != "user" || msgs[1].Role != "assistant" || msgs[2].Role != "assistant" {
		t.Fatalf("roles = %v, want [user assistant assistant]", roles(msgs))
	}
	// The gated bubble carries the ask_user call; the verdict bubble is the reply.
	var sawAsk bool
	for _, p := range msgs[1].Content {
		if p.Type == "tool-call" && p.ToolName == "ask_user" {
			sawAsk = true
		}
	}
	if !sawAsk {
		t.Errorf("gated bubble missing ask_user call: %+v", msgs[1].Content)
	}
	var replyText string
	for _, p := range msgs[2].Content {
		if p.Type == "text" {
			replyText += p.Text
		}
	}
	if !strings.Contains(replyText, "JavaScript") {
		t.Errorf("verdict reply text = %q, want the answer", replyText)
	}
}

// TestBuildHistoryEmitsGenerativeUIDataPart verifies a tool_result block that
// carries a GenerativeUI spec becomes a data part named generative-ui in the
// assistant turn, mirroring the live data-generative-ui frame so a reloaded
// client renders the same card.
func TestBuildHistoryEmitsGenerativeUIDataPart(t *testing.T) {
	ms := session.NewMemMessageStore()
	sess := "sess-1"
	append := func(role provider.Role, blocks ...provider.Block) {
		if _, err := ms.AppendMessage(context.Background(), session.StoredMessage{
			SessionID: sess, RunID: "r1", Role: role, Content: blocks,
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	append(provider.RoleUser, provider.Block{Type: provider.BlockText, Text: "test the ui"})
	append(provider.RoleAssistant,
		provider.Block{Type: provider.BlockToolUse, ToolUseID: "tu1", ToolName: "test_ui", ToolInput: map[string]any{}},
	)
	append(provider.RoleUser,
		provider.Block{
			Type:         provider.BlockToolResult,
			ToolResultID: "tu1",
			ToolContent:  "test UI card pushed",
			GenerativeUI: &provider.GenerativeUISpec{Root: []provider.GenerativeUINode{
				{Component: "test-ui-card", Props: map[string]any{"title": "Generative UI works"}},
			}},
		},
	)
	append(provider.RoleAssistant, provider.Block{Type: provider.BlockText, Text: "done"})

	h := NewHandler(newTestLoop, "sys").WithMessageStore(ms)
	req := httptest.NewRequest("GET", "/api/chat/history?threadId="+sess, nil)
	msgs, err := h.buildHistory(req, sess)
	if err != nil {
		t.Fatalf("buildHistory: %v", err)
	}

	// The turn folds into ONE assistant bubble carrying the data part.
	if len(msgs) != 2 || msgs[1].Role != "assistant" {
		t.Fatalf("expected [user assistant], got roles %v", roles(msgs))
	}
	var found *historyPart
	for i := range msgs[1].Content {
		if msgs[1].Content[i].Type == "data" {
			found = &msgs[1].Content[i]
		}
	}
	if found == nil {
		t.Fatalf("no data part in history: %+v", msgs[1].Content)
	}
	if found.Name != "generative-ui" {
		t.Errorf("data part name = %q want generative-ui", found.Name)
	}
	var payload struct {
		Spec struct {
			Root []struct {
				Component string `json:"component"`
			} `json:"root"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(found.Data, &payload); err != nil {
		t.Fatalf("data part is not the spec envelope: %v (%s)", err, found.Data)
	}
	if len(payload.Spec.Root) != 1 || payload.Spec.Root[0].Component != "test-ui-card" {
		t.Errorf("spec envelope root = %+v", payload.Spec.Root)
	}
}

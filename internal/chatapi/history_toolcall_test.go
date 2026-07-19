package chatapi

import (
	"context"
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

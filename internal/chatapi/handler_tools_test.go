package chatapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/sandbox"
	"nowhere-agent/internal/session"
	"nowhere-agent/internal/toolruntime"
	"nowhere-agent/internal/toolruntime/builtin"
)

// toolScriptProvider requests a write_file then a read_file, then answers.
type toolScriptProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *toolScriptProvider) Name() string { return "tool-script" }

func (p *toolScriptProvider) Stream(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	ch := make(chan provider.Event, 8)
	toolUse := func(id, name, args string) {
		ch <- provider.Event{Type: provider.EventMessageStart}
		ch <- provider.Event{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockToolUse, ToolUseID: id, ToolName: name, ToolInput: map[string]any{}}}
		ch <- provider.Event{Type: provider.EventBlockDelta, Index: 0, Delta: args}
		ch <- provider.Event{Type: provider.EventBlockStop, Index: 0}
		ch <- provider.Event{Type: provider.EventMessageStop}
	}
	switch p.calls {
	case 1:
		toolUse("tu1", "write_file", `{"path":"note.txt","content":"from-the-agent"}`)
	case 2:
		toolUse("tu2", "read_file", `{"path":"note.txt"}`)
	default:
		ch <- provider.Event{Type: provider.EventMessageStart}
		ch <- provider.Event{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockText}}
		ch <- provider.Event{Type: provider.EventBlockDelta, Index: 0, Delta: "all done"}
		ch <- provider.Event{Type: provider.EventBlockStop, Index: 0}
		ch <- provider.Event{Type: provider.EventMessageStop}
	}
	close(ch)
	return ch, nil
}

// TestChatToolBinderDrivesFileTools wires a ToolBinder that attaches the real
// file tools over a mem sandbox, then drives a run whose provider calls
// write_file then read_file. It asserts the tool_use/tool_result frames stream
// and the file actually lands in the session sandbox — the whole think→tool→
// think loop exercised end to end.
func TestChatToolBinderDrivesFileTools(t *testing.T) {
	store := session.NewMemStore()
	rt := session.NewRuntime(store)

	sb := sandbox.NewMemPort()
	mgr := sandbox.NewManager(sb)
	msgStore := session.NewMemMessageStore()

	h := NewHandler(func(ctx context.Context, system string) *agent.Loop {
		return agent.New(&toolScriptProvider{}, toolruntime.NewRegistry(), agent.Config{Model: "m", System: system, MaxTokens: 100})
	}, "sys").WithRuntime(rt).WithMessageStore(msgStore).WithToolBinder(func(ctx context.Context, loop *agent.Loop, sessionID string) {
		handle, err := mgr.Ensure(ctx, sessionID, sandbox.Options{})
		if err != nil {
			t.Errorf("ensure sandbox: %v", err)
			return
		}
		reg := toolruntime.NewRegistry()
		for _, tl := range builtin.FileTools(sb, handle) {
			reg.Register(tl)
		}
		loop.WithTools(reg)
	})

	mux := http.NewServeMux()
	h.Register(mux)

	body := `{"messages":[{"role":"user","content":"save then read a note"}]}`
	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	// The live stream must at least show the run started and the first tool
	// executed. (Later frames may be dropped from the in-memory broker's live
	// fan-out when the fast in-process run outruns the attach — content is the
	// broker's concern, not this test's; see TestChatPersistsRunWithRuntime.)
	for _, want := range []string{
		`"type":"tool-call-start"`, `"toolName":"write_file"`,
		`"type":"tool-result"`, `wrote 14 bytes to note.txt`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stream missing %q\n---\n%s", want, out)
		}
	}

	// The file the agent wrote must actually exist in the session sandbox.
	if len(store.Sessions()) != 1 {
		t.Fatalf("expected 1 session, got %d", len(store.Sessions()))
	}
	sessID := store.Sessions()[0].ID
	handle, ok := mgr.Get(sessID)
	if !ok {
		t.Fatal("no sandbox for session")
	}
	rc, err := sb.ReadFile(context.Background(), handle, "note.txt")
	if err != nil {
		t.Fatalf("read back note.txt: %v", err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	if string(b) != "from-the-agent" {
		t.Errorf("file content = %q", string(b))
	}

	// The whole think→tool→think loop must have run: write_file, then read_file,
	// then the final text answer. The durable message record is authoritative for
	// what the loop produced (the live stream above may lag the fast in-process
	// run), so assert the persisted messages carry both tool calls and the final
	// answer.
	runs := store.RunsFor(sessID)
	if len(runs) != 1 || runs[0].Status != session.RunDone {
		t.Fatalf("run = %+v", runs)
	}
	msgs, err := msgStore.MessagesFor(context.Background(), sessID)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	var toolNames, resultContents []string
	var finalText string
	for _, m := range msgs {
		for _, blk := range m.Content {
			switch blk.Type {
			case provider.BlockToolUse:
				toolNames = append(toolNames, blk.ToolName)
			case provider.BlockToolResult:
				resultContents = append(resultContents, blk.ToolContent)
			case provider.BlockText:
				finalText = blk.Text
			}
		}
	}
	joined := strings.Join(toolNames, ",")
	if !strings.Contains(joined, "write_file") || !strings.Contains(joined, "read_file") {
		t.Errorf("tool uses persisted = %v, want write_file + read_file", toolNames)
	}
	if finalText != "all done" {
		t.Errorf("final text = %q, want %q", finalText, "all done")
	}
	// The read_file result must echo the content write_file persisted.
	var sawReadBack bool
	for _, c := range resultContents {
		if strings.Contains(c, "from-the-agent") {
			sawReadBack = true
		}
	}
	if !sawReadBack {
		t.Errorf("no tool result echoed the written file content; results = %v", resultContents)
	}
}

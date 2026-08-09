package subagent

import (
	"context"
	"sync"
	"testing"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/agentdef"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/toolruntime"
)

func TestActivityEmitterForwards(t *testing.T) {
	var got []Activity
	e := activityEmitter{sink: func(a Activity) { got = append(got, a) }, agentType: "x", depth: 1, toolCallID: "tc-1"}

	_ = e.Emit(context.Background(), agent.KindToolUse, map[string]any{"name": "read_file"})
	_ = e.Emit(context.Background(), agent.KindText, "hello ")
	_ = e.Emit(context.Background(), agent.KindThinking, "hmm")
	_ = e.Emit(context.Background(), agent.KindDone, nil)

	// tool + stream(text) + stream(thinking) + done
	if len(got) != 4 {
		t.Fatalf("expected 4 activities, got %+v", got)
	}
	if got[0].Phase != "tool" || got[0].Tool != "read_file" || got[0].Depth != 1 {
		t.Fatalf("tool activity: %+v", got[0])
	}
	if got[1].Phase != "stream" || got[1].Kind != "text" || got[1].Text != "hello " {
		t.Fatalf("text stream activity: %+v", got[1])
	}
	if got[2].Phase != "stream" || got[2].Kind != "thinking" || got[2].Text != "hmm" {
		t.Fatalf("thinking stream activity: %+v", got[2])
	}
	if got[3].Phase != "done" {
		t.Fatalf("done activity: %+v", got[3])
	}
	// Every signal must carry the tool-call id so the UI can nest it.
	for _, a := range got {
		if a.ToolCallID != "tc-1" {
			t.Fatalf("activity missing toolCallID: %+v", a)
		}
	}
}

func TestActivityEmitterStreamTagsCallID(t *testing.T) {
	var got []Activity
	e := activityEmitter{sink: func(a Activity) { got = append(got, a) }, agentType: "x", depth: 2, toolCallID: "tc-9"}
	_ = e.Emit(context.Background(), agent.KindText, "chunk")
	if len(got) != 1 || got[0].Phase != "stream" || got[0].Kind != "text" || got[0].Text != "chunk" || got[0].ToolCallID != "tc-9" || got[0].Depth != 2 {
		t.Fatalf("stream activity wrong: %+v", got)
	}
}

// TestActivityEmitterInterruptSuppressesDone pins that a child's KindInterrupt
// is forwarded as an "interrupted" activity (naming the gated tool) and the
// loop's trailing KindDone after it is suppressed — a gated child stalled, it
// did not finish.
func TestActivityEmitterInterruptSuppressesDone(t *testing.T) {
	var got []Activity
	e := &activityEmitter{sink: func(a Activity) { got = append(got, a) }, agentType: "x", depth: 1, toolCallID: "tc-1"}

	_ = e.Emit(context.Background(), agent.KindInterrupt, agent.Interaction{Kind: "approval", ToolCallID: "t1", ToolName: "run_command"})
	_ = e.Emit(context.Background(), agent.KindDone, nil)

	if len(got) != 1 {
		t.Fatalf("expected exactly the interrupted activity (done suppressed), got %+v", got)
	}
	if got[0].Phase != "interrupted" || got[0].Tool != "run_command" || got[0].SubToolCallID != "t1" || got[0].Kind != "approval" {
		t.Fatalf("interrupted activity: %+v", got[0])
	}
}

func TestActivityEmitterNilSink(t *testing.T) {
	e := activityEmitter{sink: nil, agentType: "x", depth: 1}
	if err := e.Emit(context.Background(), agent.KindToolUse, map[string]any{"name": "x"}); err != nil {
		t.Fatalf("nil sink should be a no-op, got %v", err)
	}
}

func TestSpawnForwardsActivity(t *testing.T) {
	store := agentdef.NewStore()
	reg := toolruntime.NewRegistry()

	// The child calls a tool, then answers — so the run emits tool-use then done.
	childProv := &scriptProvider{script: [][]provider.Event{
		toolUseEvents("t1", "read_file", `{}`),
		textEvents("done"),
	}}
	factory := func(context.Context, agentdef.AgentDef, int) (*agent.Loop, error) {
		return agent.New(childProv, toolruntime.NewRegistry(), childCfg()), nil
	}
	tool := NewSpawnTool(testResolver(store), reg, factory, 3)
	reg.Register(tool)

	var mu sync.Mutex
	var got []Activity
	ctx := WithSink(context.Background(), func(a Activity) {
		mu.Lock()
		got = append(got, a)
		mu.Unlock()
	})

	res, err := tool.Call(ctx, map[string]any{"prompt": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %+v", res)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) == 0 || got[0].Phase != "start" || got[0].AgentType != agentdef.GeneralPurpose {
		t.Fatalf("expected leading start activity, got %+v", got)
	}
	var sawTool, sawDone bool
	for _, a := range got {
		if a.Depth != 1 {
			t.Fatalf("activity at wrong depth: %+v", a)
		}
		if a.Phase == "tool" && a.Tool == "read_file" {
			sawTool = true
		}
		if a.Phase == "done" {
			sawDone = true
		}
	}
	if !sawTool || !sawDone {
		t.Fatalf("expected tool(read_file) + done, got %+v", got)
	}
}

func TestSpawnNoSinkStillWorks(t *testing.T) {
	// Without a sink installed, the child runs black-box and the result still
	// collapses normally (discardEmitter path).
	store := agentdef.NewStore()
	reg := toolruntime.NewRegistry()
	factory := func(context.Context, agentdef.AgentDef, int) (*agent.Loop, error) {
		return agent.New(echoProvider{"ok"}, toolruntime.NewRegistry(), childCfg()), nil
	}
	tool := NewSpawnTool(testResolver(store), reg, factory, 3)
	reg.Register(tool)

	res, err := tool.Call(context.Background(), map[string]any{"prompt": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || res.Content != "ok" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

// TestSpawnTagsActivitiesWithCallID drives the spawn tool through the real
// dispatch path (Registry.CallAll), which injects the tool-call id into ctx.
// Every activity the child emits must carry that id so the chat UI can nest
// this child's streamed output under the right spawn_agent card — essential
// when several subagents run in parallel.
func TestSpawnTagsActivitiesWithCallID(t *testing.T) {
	store := agentdef.NewStore()
	reg := toolruntime.NewRegistry()
	childProv := &scriptProvider{script: [][]provider.Event{
		textEvents("child working…"),
	}}
	factory := func(context.Context, agentdef.AgentDef, int) (*agent.Loop, error) {
		return agent.New(childProv, toolruntime.NewRegistry(), childCfg()), nil
	}
	reg.Register(NewSpawnTool(testResolver(store), reg, factory, 3))

	var mu sync.Mutex
	var got []Activity
	ctx := WithSink(context.Background(), func(a Activity) {
		mu.Lock()
		got = append(got, a)
		mu.Unlock()
	})

	results := reg.CallAll(ctx, []toolruntime.Call{{ID: "tc-42", Name: ToolName, Args: map[string]any{"prompt": "x"}}})
	if len(results) != 1 || results[0].IsError {
		t.Fatalf("dispatch result: %+v", results)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) == 0 {
		t.Fatal("no activities forwarded")
	}
	var sawStream bool
	for _, a := range got {
		if a.ToolCallID != "tc-42" {
			t.Fatalf("activity missing/mismatched toolCallID: %+v", a)
		}
		if a.Phase == "stream" && a.Kind == "text" {
			sawStream = true
		}
	}
	if !sawStream {
		t.Fatalf("expected a streamed text activity, got %+v", got)
	}
}

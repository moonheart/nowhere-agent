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
	e := activityEmitter{sink: func(a Activity) { got = append(got, a) }, agentType: "x", depth: 1}

	_ = e.Emit(context.Background(), agent.KindToolUse, map[string]any{"name": "read_file"})
	_ = e.Emit(context.Background(), agent.KindText, "ignored")
	_ = e.Emit(context.Background(), agent.KindDone, nil)

	if len(got) != 2 {
		t.Fatalf("expected tool+done, got %+v", got)
	}
	if got[0].Phase != "tool" || got[0].Tool != "read_file" || got[0].Depth != 1 {
		t.Fatalf("tool activity: %+v", got[0])
	}
	if got[1].Phase != "done" {
		t.Fatalf("done activity: %+v", got[1])
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
	factory := func(context.Context, agentdef.AgentDef, int) *agent.Loop {
		return agent.New(childProv, toolruntime.NewRegistry(), childCfg())
	}
	tool := NewSpawnTool(store, reg, factory, 3)
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
	factory := func(context.Context, agentdef.AgentDef, int) *agent.Loop {
		return agent.New(echoProvider{"ok"}, toolruntime.NewRegistry(), childCfg())
	}
	tool := NewSpawnTool(store, reg, factory, 3)
	reg.Register(tool)

	res, err := tool.Call(context.Background(), map[string]any{"prompt": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || res.Content != "ok" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

package subagent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/agentdef"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/toolruntime"
)

// TestSpawnBudgetTotalCap verifies the per-run total-spawn budget: once the cap
// is reached, further spawns are rejected as is_error rather than launched.
func TestSpawnBudgetTotalCap(t *testing.T) {
	store := agentdef.NewStore()
	reg := toolruntime.NewRegistry()
	factory := func(context.Context, agentdef.AgentDef, int) (*agent.Loop, error) {
		return agent.New(echoProvider{"ok"}, toolruntime.NewRegistry(), childCfg()), nil
	}
	tool := NewSpawnTool(testResolver(store), reg, factory, 3).WithBudget(2, 4)
	reg.Register(tool)

	for i := 0; i < 2; i++ {
		res, err := tool.Call(context.Background(), map[string]any{"prompt": "go"})
		if err != nil || res.IsError {
			t.Fatalf("spawn %d unexpectedly failed: %+v err=%v", i, res, err)
		}
	}
	res, err := tool.Call(context.Background(), map[string]any{"prompt": "go"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content, "budget") {
		t.Errorf("third spawn = %+v, want an is_error budget message", res)
	}
}

// gateProvider blocks each child in Stream until released, tracking the peak
// number running concurrently so a test can assert the semaphore cap.
type gateProvider struct {
	running    *int32
	maxRunning *int32
	release    <-chan struct{}
}

func (gateProvider) Name() string { return "gate" }
func (p gateProvider) Stream(context.Context, provider.Request) (<-chan provider.Event, error) {
	n := atomic.AddInt32(p.running, 1)
	for {
		old := atomic.LoadInt32(p.maxRunning)
		if n <= old || atomic.CompareAndSwapInt32(p.maxRunning, old, n) {
			break
		}
	}
	<-p.release
	atomic.AddInt32(p.running, -1)
	evs := textEvents("done")
	ch := make(chan provider.Event, len(evs))
	for _, e := range evs {
		ch <- e
	}
	close(ch)
	return ch, nil
}

// TestSpawnBudgetConcurrencyCap verifies no more than maxConcurrent child runs
// execute at once.
func TestSpawnBudgetConcurrencyCap(t *testing.T) {
	var running, maxRunning int32
	release := make(chan struct{})
	store := agentdef.NewStore()
	reg := toolruntime.NewRegistry()
	factory := func(context.Context, agentdef.AgentDef, int) (*agent.Loop, error) {
		return agent.New(gateProvider{running: &running, maxRunning: &maxRunning, release: release}, toolruntime.NewRegistry(), childCfg()), nil
	}
	tool := NewSpawnTool(testResolver(store), reg, factory, 3).WithBudget(10, 2) // at most 2 concurrent
	reg.Register(tool)

	const n = 5
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = tool.Call(context.Background(), map[string]any{"prompt": "go"})
		}()
	}
	// Let the goroutines contend for slots and block in Stream.
	time.Sleep(200 * time.Millisecond)
	if m := atomic.LoadInt32(&maxRunning); m > 2 {
		t.Errorf("peak concurrent children = %d, want <= 2 (semaphore cap)", m)
	}
	close(release)
	wg.Wait()
}

// TestNestedSpawnRejectsSaturatedConcurrency verifies a nested spawn (depth >
// 0) does NOT block on a saturated semaphore: with the root's fan-out holding
// every slot, a depth-1 spawn must fail immediately with an is_error result,
// not stall until the 5-minute tool timeout (which would freeze the whole run
// tree). Root-level spawns keep waiting.
func TestNestedSpawnRejectsSaturatedConcurrency(t *testing.T) {
	var running, maxRunning int32
	release := make(chan struct{})
	store := agentdef.NewStore()
	reg := toolruntime.NewRegistry()
	factory := func(context.Context, agentdef.AgentDef, int) (*agent.Loop, error) {
		return agent.New(gateProvider{running: &running, maxRunning: &maxRunning, release: release}, toolruntime.NewRegistry(), childCfg()), nil
	}
	tool := NewSpawnTool(testResolver(store), reg, factory, 3).WithBudget(10, 2) // at most 2 concurrent
	reg.Register(tool)

	// Two root-level spawns grab both slots and block in Stream.
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = tool.Call(context.Background(), map[string]any{"prompt": "go"})
		}()
	}
	// Let the root spawns contend for (and hold) both slots.
	time.Sleep(200 * time.Millisecond)
	if m := atomic.LoadInt32(&maxRunning); m != 2 {
		t.Fatalf("root spawns should hold both slots, peak = %d", m)
	}

	// A nested spawn at depth 1 must fail immediately, not block for a slot.
	start := time.Now()
	res, err := tool.Call(withDepth(context.Background(), 1), map[string]any{"prompt": "go"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content, "saturated") {
		t.Errorf("nested spawn on saturated semaphore = %+v, want an is_error saturation message", res)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("nested spawn took %v, want an immediate rejection (no slot wait)", d)
	}

	close(release)
	wg.Wait()
}

// TestFailedChildBuildDoesNotConsumeBudget: a spawn whose child could not be
// built returns an error result but must NOT count against the total budget —
// the model retries after an error result, and counting attempts that never
// ran would exhaust the cap on tries, not runs.
func TestFailedChildBuildDoesNotConsumeBudget(t *testing.T) {
	store := agentdef.NewStore()
	reg := toolruntime.NewRegistry()
	failBuild := true
	factory := func(context.Context, agentdef.AgentDef, int) (*agent.Loop, error) {
		if failBuild {
			return nil, errors.New("boom")
		}
		return agent.New(echoProvider{"ok"}, toolruntime.NewRegistry(), childCfg()), nil
	}
	tool := NewSpawnTool(testResolver(store), reg, factory, 3).WithBudget(1, 2)
	reg.Register(tool)

	res, err := tool.Call(context.Background(), map[string]any{"prompt": "go"})
	if err != nil || !res.IsError {
		t.Fatalf("build-failed spawn = %+v err=%v, want an error result", res, err)
	}
	failBuild = false
	res, err = tool.Call(context.Background(), map[string]any{"prompt": "go"})
	if err != nil || res.IsError {
		t.Fatalf("retry after a build failure = %+v err=%v, want success (failed builds must not consume budget)", res, err)
	}
}

// TestSaturatedNestedSpawnRejectedAtSemaphoreNotBudget: a nested spawn on a
// saturated semaphore is rejected THERE (before the budget counter), so the
// attempt neither blocks nor consumes a budget slot. With a budget of 1 held
// by the running root spawn, a budget-first implementation would answer
// "budget exhausted" instead of "saturated".
func TestSaturatedNestedSpawnRejectedAtSemaphoreNotBudget(t *testing.T) {
	var running, maxRunning int32
	release := make(chan struct{})
	store := agentdef.NewStore()
	reg := toolruntime.NewRegistry()
	factory := func(context.Context, agentdef.AgentDef, int) (*agent.Loop, error) {
		return agent.New(gateProvider{running: &running, maxRunning: &maxRunning, release: release}, toolruntime.NewRegistry(), childCfg()), nil
	}
	tool := NewSpawnTool(testResolver(store), reg, factory, 3).WithBudget(1, 1) // 1 total spawn, 1 concurrent
	reg.Register(tool)

	// One root spawn holds the sole semaphore slot AND the sole budget slot.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = tool.Call(context.Background(), map[string]any{"prompt": "go"})
	}()
	time.Sleep(200 * time.Millisecond)
	if m := atomic.LoadInt32(&maxRunning); m != 1 {
		t.Fatalf("root spawn should hold the slot, peak = %d", m)
	}

	res, err := tool.Call(withDepth(context.Background(), 1), map[string]any{"prompt": "go"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "saturated") {
		t.Errorf("nested spawn on saturated semaphore = %+v, want a saturation error (not a budget error)", res)
	}

	close(release)
	wg.Wait()
}

package subagent

import (
	"context"
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
	factory := func(context.Context, agentdef.AgentDef, int) *agent.Loop {
		return agent.New(echoProvider{"ok"}, toolruntime.NewRegistry(), childCfg())
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
	factory := func(context.Context, agentdef.AgentDef, int) *agent.Loop {
		return agent.New(gateProvider{running: &running, maxRunning: &maxRunning, release: release}, toolruntime.NewRegistry(), childCfg())
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

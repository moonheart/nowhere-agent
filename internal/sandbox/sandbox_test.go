package sandbox

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// failingDestroyPort wraps a MemPort whose Destroy fails ONCE for the named
// sessions, to exercise the Manager's sweep-failure handling and retry.
type failingDestroyPort struct {
	*MemPort
	fail map[string]bool
	done map[string]bool
}

func (p *failingDestroyPort) Destroy(ctx context.Context, h Handle) error {
	if p.fail[h.SessionID] && !p.done[h.SessionID] {
		p.done[h.SessionID] = true
		return fmt.Errorf("destroy blocked for %s", h.SessionID)
	}
	return p.MemPort.Destroy(ctx, h)
}

func TestManagerEnsureCreatesOnce(t *testing.T) {
	m := NewManager(NewMemPort())
	ctx := context.Background()

	h1, err := m.Ensure(ctx, "s1", Options{})
	if err != nil {
		t.Fatal(err)
	}
	h2, err := m.Ensure(ctx, "s1", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if h1.ID != h2.ID {
		t.Errorf("Ensure returned different handles: %s vs %s", h1.ID, h2.ID)
	}
}

func TestManagerEnsureIsolatesSessions(t *testing.T) {
	m := NewManager(NewMemPort())
	ctx := context.Background()
	h1, _ := m.Ensure(ctx, "s1", Options{})
	h2, _ := m.Ensure(ctx, "s2", Options{})
	if h1.ID == h2.ID {
		t.Error("sessions share a sandbox id")
	}
}

func TestManagerDeferredStopAndSweep(t *testing.T) {
	m := NewManager(NewMemPort())
	ctx := context.Background()
	h, _ := m.Ensure(ctx, "s1", Options{})

	// Not yet ended: sweep should not destroy.
	m.MarkSessionEnded("s1", 50*time.Millisecond)
	if got, _ := m.Sweep(ctx, time.Now()); len(got) != 0 {
		t.Errorf("sweep destroyed too early: %v", got)
	}
	if m.StateOf("s1") != StateStopped {
		t.Errorf("state = %q want stopped", m.StateOf("s1"))
	}

	// After the delay elapses, sweep destroys.
	got, err := m.Sweep(ctx, time.Now().Add(100*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "s1" {
		t.Errorf("sweep destroyed %v want [s1]", got)
	}
	if m.StateOf("s1") != StateDestroyed {
		t.Errorf("state = %q want destroyed", m.StateOf("s1"))
	}
	_ = h
}

func TestManagerGetOnlyRunning(t *testing.T) {
	m := NewManager(NewMemPort())
	ctx := context.Background()
	m.Ensure(ctx, "s1", Options{})
	if _, ok := m.Get("s1"); !ok {
		t.Error("expected running sandbox")
	}
	m.MarkSessionEnded("s1", time.Minute)
	if _, ok := m.Get("s1"); ok {
		t.Error("stopped sandbox should not be returned by Get")
	}
}

func TestManagerSweepAggregatesDestroyFailures(t *testing.T) {
	m := NewManager(&failingDestroyPort{MemPort: NewMemPort(), fail: map[string]bool{"s2": true}, done: map[string]bool{}})
	ctx := context.Background()
	m.Ensure(ctx, "s1", Options{})
	m.Ensure(ctx, "s2", Options{})
	m.MarkSessionEnded("s1", 0)
	m.MarkSessionEnded("s2", 0)

	destroyed, err := m.Sweep(ctx, time.Now().Add(time.Minute))
	if err == nil {
		t.Fatal("expected aggregated destroy error")
	}
	// The healthy sandbox is still swept despite s2's failure.
	if len(destroyed) != 1 || destroyed[0] != "s1" {
		t.Errorf("destroyed = %v, want [s1]", destroyed)
	}
	if !strings.Contains(err.Error(), "s2") {
		t.Errorf("error does not name the failed session: %v", err)
	}
	// The failed session stays retryable, and a second sweep succeeds.
	if m.StateOf("s2") != StateStopped {
		t.Errorf("state = %q, want stopped (not destroyed) after failed sweep", m.StateOf("s2"))
	}
	destroyed, err = m.Sweep(ctx, time.Now().Add(2*time.Minute))
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if len(destroyed) != 1 || destroyed[0] != "s2" {
		t.Errorf("second sweep destroyed = %v, want [s2]", destroyed)
	}
}

func TestMemPortFileRoundTrip(t *testing.T) {
	p := NewMemPort()
	ctx := context.Background()
	h, _ := p.Create(ctx, "s1", Options{})

	if err := p.WriteFile(ctx, h, "/work/a.txt", strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	rc, err := p.ReadFile(ctx, h, "/work/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	if string(b) != "hello" {
		t.Errorf("read %q want hello", b)
	}

	paths, err := p.ListDir(ctx, h, "/work")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "/work/a.txt" {
		t.Errorf("list = %v", paths)
	}
}

func TestMemPortDestroyRemoves(t *testing.T) {
	p := NewMemPort()
	ctx := context.Background()
	h, _ := p.Create(ctx, "s1", Options{})
	if err := p.Destroy(ctx, h); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, h, []string{"ls"}); err == nil {
		t.Error("expected error after destroy")
	}
}

func TestNetworkPolicyModesDistinct(t *testing.T) {
	modes := []NetworkMode{NetworkOpen, NetworkAllowlist, NetworkDeny}
	seen := map[NetworkMode]bool{}
	for _, m := range modes {
		if seen[m] {
			t.Fatalf("dup mode %q", m)
		}
		seen[m] = true
	}
}

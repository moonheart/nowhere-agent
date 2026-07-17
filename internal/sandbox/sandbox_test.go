package sandbox

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

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

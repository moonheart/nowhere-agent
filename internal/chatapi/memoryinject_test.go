package chatapi

import (
	"context"
	"strings"
	"testing"
	"time"

	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/memory"
	"nowhere-agent/internal/session"
)

// newInjectorRig wires a mem-backed injector with one session for the user.
func newInjectorRig(t *testing.T) (*sessionMemoryInjector, *memory.MemPort, *session.Runtime, session.Session) {
	t.Helper()
	rt := session.NewRuntime(session.NewMemStore())
	mem := memory.NewMemPort()
	user := identity.User{ID: "u1"}
	sess, err := rt.CreateSession(context.Background(), user.ID, "t")
	if err != nil {
		t.Fatal(err)
	}
	inj := &sessionMemoryInjector{
		mem:     mem,
		scopes:  staticScopes{scopes: []identity.ScopeRef{identity.UserScope(user.ID)}},
		runtime: rt,
		user:    user,
		limit:   8,
		now:     time.Now,
	}
	return inj, mem, rt, sess
}

// storeMemAt stores a memory then backdates/forwards its CreatedAt so tests can
// order memories deterministically against the injection watermark (MemPort.Store
// would otherwise set now, tying the watermark's clock tick).
func storeMemAt(t *testing.T, mem *memory.MemPort, m memory.Memory, at time.Time) memory.Memory {
	t.Helper()
	got, err := mem.Store(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	mem.Backdate(got.ID, at)
	got.CreatedAt = at
	return got
}

// TestInjectFirstTurnInjectsPreferenceAndFact: a session with no watermark gets
// the preference/fact set — NOT summary/insight — as one user-role message with
// a date anchor per memory.
func TestInjectFirstTurnInjectsPreferenceAndFact(t *testing.T) {
	inj, mem, _, sess := newInjectorRig(t)
	ctx := context.Background()
	scope := identity.UserScope("u1")

	for _, m := range []memory.Memory{
		{Scope: scope, Kind: memory.KindPreference, Content: "素食者"},
		{Scope: scope, Kind: memory.KindFact, Content: "住在旧金山"},
		{Scope: scope, Kind: memory.KindSummary, Content: "上次聊了旅行"},
		{Scope: scope, Kind: memory.KindInsight, Content: "跨会话模式"},
	} {
		if _, err := mem.Store(ctx, m); err != nil {
			t.Fatal(err)
		}
	}

	got, err := inj.Inject(ctx, sess.ID, nil)
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("injected %d messages want 1", len(got))
	}
	if got[0].Role != "user" {
		t.Errorf("injected role = %q want user", got[0].Role)
	}
	text := got[0].Content[0].Text
	if !strings.Contains(text, "素食者") || !strings.Contains(text, "住在旧金山") {
		t.Errorf("injected text missing preference/fact: %q", text)
	}
	if strings.Contains(text, "上次聊了旅行") || strings.Contains(text, "跨会话模式") {
		t.Errorf("summary/insight must NOT be auto-injected: %q", text)
	}
	// Date anchor present per memory.
	if !strings.Contains(text, time.Now().Format("2006-01-02")) {
		t.Errorf("injected text missing date anchor: %q", text)
	}
}

// TestInjectIncrementalOnlyNewMemories: after the first injection advances the
// watermark, only memories created later are surfaced.
func TestInjectIncrementalOnlyNewMemories(t *testing.T) {
	inj, mem, rt, sess := newInjectorRig(t)
	ctx := context.Background()
	scope := identity.UserScope("u1")
	base := time.Now()

	// first fact created before the watermark; the watermark lands at base.
	storeMemAt(t, mem, memory.Memory{Scope: scope, Kind: memory.KindFact, Content: "first fact"}, base.Add(-time.Minute))
	inj.now = func() time.Time { return base }
	if _, err := inj.Inject(ctx, sess.ID, nil); err != nil {
		t.Fatal(err)
	}

	// No new memory → nothing injected.
	got, err := inj.Inject(ctx, sess.ID, nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("second inject with no new memory = %v err %v want empty", got, err)
	}

	// A new memory created after the watermark → only it is injected.
	storeMemAt(t, mem, memory.Memory{Scope: scope, Kind: memory.KindPreference, Content: "new pref"}, base.Add(time.Minute))
	got, err = inj.Inject(ctx, sess.ID, nil)
	if err != nil || len(got) != 1 {
		t.Fatalf("incremental inject = %d err %v want 1", len(got), err)
	}
	text := got[0].Content[0].Text
	if !strings.Contains(text, "new pref") {
		t.Errorf("incremental inject missing new memory: %q", text)
	}
	if strings.Contains(text, "first fact") {
		t.Errorf("already-injected memory must not be re-injected: %q", text)
	}

	// Watermark advanced to base.
	if at, _ := rt.MemoryInjectedAt(ctx, sess.ID); at.IsZero() {
		t.Error("watermark not advanced after injection")
	}
}

// TestInjectEmptyDoesNotAdvanceWatermark: with nothing to inject, the watermark
// stays put (so a memory written just after the empty check isn't skipped).
func TestInjectEmptyDoesNotAdvanceWatermark(t *testing.T) {
	inj, _, rt, sess := newInjectorRig(t)
	ctx := context.Background()

	got, err := inj.Inject(ctx, sess.ID, nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty inject = %v err %v", got, err)
	}
	if at, _ := rt.MemoryInjectedAt(ctx, sess.ID); !at.IsZero() {
		t.Errorf("watermark advanced on empty inject: %v", at)
	}
}

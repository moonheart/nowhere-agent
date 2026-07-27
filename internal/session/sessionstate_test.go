package session

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TestMemStoreSessionStateKV verifies single-key upsert preserves sibling keys,
// values round-trip, and a missing key reports false.
func TestMemStoreSessionStateKV(t *testing.T) {
	store := NewMemStore()
	ctx := context.Background()
	sess, err := store.CreateSession(ctx, "u1", "t")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := store.SetSessionStateKV(ctx, sess.ID, "plan", json.RawMessage(`{"items":["a"]}`)); err != nil {
		t.Fatalf("set plan: %v", err)
	}
	if err := store.SetSessionStateKV(ctx, sess.ID, "progress", json.RawMessage(`{"n":2}`)); err != nil {
		t.Fatalf("set progress: %v", err)
	}

	// A sibling-key write must not clobber the first key.
	v, ok, err := store.SessionStateKV(ctx, sess.ID, "plan")
	if err != nil || !ok {
		t.Fatalf("get plan: ok=%v err=%v", ok, err)
	}
	if string(v) != `{"items":["a"]}` {
		t.Errorf("plan = %s", v)
	}
	v, ok, _ = store.SessionStateKV(ctx, sess.ID, "progress")
	if !ok || string(v) != `{"n":2}` {
		t.Errorf("progress = %s ok=%v", v, ok)
	}

	// Overwrite one key; the other is untouched.
	if err := store.SetSessionStateKV(ctx, sess.ID, "plan", json.RawMessage(`{"items":["b"]}`)); err != nil {
		t.Fatalf("overwrite plan: %v", err)
	}
	v, _, _ = store.SessionStateKV(ctx, sess.ID, "plan")
	if string(v) != `{"items":["b"]}` {
		t.Errorf("plan after overwrite = %s", v)
	}

	if _, ok, _ := store.SessionStateKV(ctx, sess.ID, "missing"); ok {
		t.Error("missing key should report false")
	}

	// Whole-dictionary read returns every key.
	all, err := store.SessionState(ctx, sess.ID)
	if err != nil {
		t.Fatalf("SessionState: %v", err)
	}
	if len(all) != 2 || string(all["plan"]) != `{"items":["b"]}` || string(all["progress"]) != `{"n":2}` {
		t.Errorf("SessionState = %v", all)
	}
}

// TestRuntimeSetSessionStateKVFanOut verifies the Runtime persists the key AND
// publishes a live session_state frame on the broker (reconnect-readable).
func TestRuntimeSetSessionStateKVFanOut(t *testing.T) {
	store := NewMemStore()
	rt := NewRuntime(store)
	ctx := context.Background()
	sess, err := rt.CreateSession(ctx, "u1", "t")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Subscribe before publishing so we catch the live frame.
	ch, unsub := rt.Broker().Subscribe(sess.ID, 8)
	defer unsub()

	if err := rt.SetSessionStateKV(ctx, sess.ID, "plan", json.RawMessage(`{"items":["a"]}`)); err != nil {
		t.Fatalf("SetSessionStateKV: %v", err)
	}

	// Persisted.
	v, ok, err := store.SessionStateKV(ctx, sess.ID, "plan")
	if err != nil || !ok || string(v) != `{"items":["a"]}` {
		t.Fatalf("persisted plan = %s ok=%v err=%v", v, ok, err)
	}

	// Fanned out live on the broker.
	select {
	case ev := <-ch:
		if ev.Kind != "session_state" {
			t.Errorf("kind = %q want session_state", ev.Kind)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(ev.Payload, &m); err != nil {
			t.Fatalf("payload decode: %v", err)
		}
		if string(m["key"]) != `"plan"` {
			t.Errorf("payload key = %s", m["key"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no session_state frame published")
	}
}

// TestPGStoreSessionStateKV is the Postgres counterpart: jsonb_set single-key
// upsert preserves siblings and round-trips through the state column.
func TestPGStoreSessionStateKV(t *testing.T) {
	db := pgTestDB(t)
	store := NewPGStore(db)
	ctx := context.Background()
	userID := pgNewUser(t, db)

	sess, err := store.CreateSession(ctx, userID, "pg state")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM sessions WHERE id = $1`, sess.ID) })

	if err := store.SetSessionStateKV(ctx, sess.ID, "plan", json.RawMessage(`{"items":["a"]}`)); err != nil {
		t.Fatalf("set plan: %v", err)
	}
	if err := store.SetSessionStateKV(ctx, sess.ID, "progress", json.RawMessage(`{"n":2}`)); err != nil {
		t.Fatalf("set progress: %v", err)
	}

	v, ok, err := store.SessionStateKV(ctx, sess.ID, "plan")
	if err != nil || !ok {
		t.Fatalf("get plan: ok=%v err=%v", ok, err)
	}
	// jsonb normalizes whitespace, so compare decoded values, not raw text.
	var plan struct {
		Items []string `json:"items"`
	}
	if err := json.Unmarshal(v, &plan); err != nil || len(plan.Items) != 1 || plan.Items[0] != "a" {
		t.Errorf("plan = %s (decoded %+v)", v, plan)
	}

	// Overwrite one key; sibling preserved.
	if err := store.SetSessionStateKV(ctx, sess.ID, "plan", json.RawMessage(`{"items":["b"]}`)); err != nil {
		t.Fatalf("overwrite plan: %v", err)
	}
	v, _, _ = store.SessionStateKV(ctx, sess.ID, "progress")
	var prog struct {
		N int `json:"n"`
	}
	if err := json.Unmarshal(v, &prog); err != nil || prog.N != 2 {
		t.Errorf("progress clobbered = %s", v)
	}

	if _, ok, _ := store.SessionStateKV(ctx, sess.ID, "missing"); ok {
		t.Error("missing key should report false")
	}

	all, err := store.SessionState(ctx, sess.ID)
	if err != nil {
		t.Fatalf("SessionState: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("SessionState has %d keys, want 2: %v", len(all), all)
	}
	if err := json.Unmarshal(all["plan"], &plan); err != nil || len(plan.Items) != 1 || plan.Items[0] != "b" {
		t.Errorf("SessionState plan = %s", all["plan"])
	}
}

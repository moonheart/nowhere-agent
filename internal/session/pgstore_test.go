package session

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func getenvOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func randSuffix() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// pgTestDB opens a connection to the dev database and skips when unreachable.
// Set SESSION_PG_TEST_DSN to point at a test database; defaults to the local
// docker dev instance. Each test cleans up the rows it creates.
func pgTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := getenvOr("SESSION_PG_TEST_DSN", "postgres://postgres:postgres@localhost:5432/nowhere?sslmode=disable")
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Skipf("open db: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		t.Skipf("no postgres reachable: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// pgNewUser creates a throwaway user to satisfy the sessions FK.
func pgNewUser(t *testing.T, db *sql.DB) string {
	t.Helper()
	var id string
	err := db.QueryRow(`
		INSERT INTO users (email, password_hash, display_name)
		VALUES ($1, 'x', 'pgtest') RETURNING id`,
		"pgtest-"+randSuffix()+"@example.com").Scan(&id)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

func TestPGStoreSessionLifecycle(t *testing.T) {
	db := pgTestDB(t)
	store := NewPGStore(db)
	ctx := context.Background()
	userID := pgNewUser(t, db)

	sess, err := store.CreateSession(ctx, userID, "pg test")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if sess.Status != SessionActive {
		t.Errorf("status = %q want active", sess.Status)
	}

	got, err := store.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Title != "pg test" || got.UserID != userID {
		t.Errorf("got %+v", got)
	}

	if err := store.EndSession(ctx, sess.ID); err != nil {
		t.Fatalf("EndSession: %v", err)
	}
	ended, _ := store.GetSession(ctx, sess.ID)
	if ended.Status != SessionEnded {
		t.Errorf("status after end = %q want ended", ended.Status)
	}
}

func TestPGStoreRunLifecycle(t *testing.T) {
	db := pgTestDB(t)
	store := NewPGStore(db)
	ctx := context.Background()
	userID := pgNewUser(t, db)
	sess, err := store.CreateSession(ctx, userID, "runs")
	if err != nil {
		t.Fatal(err)
	}

	// No active run initially.
	if _, ok, err := store.ActiveRun(ctx, sess.ID); err != nil || ok {
		t.Fatalf("ActiveRun before any run = ok %v err %v", ok, err)
	}

	seq, err := store.NextRunSeq(ctx, sess.ID)
	if err != nil || seq != 1 {
		t.Fatalf("NextRunSeq = %d err %v want 1", seq, err)
	}
	run, err := store.CreateRun(ctx, sess.ID, seq)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if run.Status != RunQueued {
		t.Errorf("run status = %q want queued", run.Status)
	}

	// Queued counts as active.
	if _, ok, _ := store.ActiveRun(ctx, sess.ID); !ok {
		t.Error("expected queued run to be active")
	}

	if err := store.UpdateRunStatus(ctx, run.ID, RunRunning); err != nil {
		t.Fatalf("UpdateRunStatus: %v", err)
	}
	active, ok, _ := store.ActiveRun(ctx, sess.ID)
	if !ok || active.Status != RunRunning {
		t.Errorf("active run = %+v ok %v", active, ok)
	}

	// Complete it; next seq should advance and no active run remains.
	if err := store.UpdateRunStatus(ctx, run.ID, RunDone); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := store.ActiveRun(ctx, sess.ID); ok {
		t.Error("expected no active run after done")
	}
	if seq, _ := store.NextRunSeq(ctx, sess.ID); seq != 2 {
		t.Errorf("NextRunSeq after first run = %d want 2", seq)
	}
}

func TestPGStoreEventsAndReplay(t *testing.T) {
	db := pgTestDB(t)
	store := NewPGStore(db)
	ctx := context.Background()
	userID := pgNewUser(t, db)
	sess, _ := store.CreateSession(ctx, userID, "events")
	run, _ := store.CreateRun(ctx, sess.ID, 1)

	for i := 1; i <= 3; i++ {
		e := Event{RunID: run.ID, SessionID: sess.ID, Offset: i, Kind: "message", Payload: []byte(`"x"`)}
		if err := store.AppendEvent(ctx, e); err != nil {
			t.Fatalf("AppendEvent %d: %v", i, err)
		}
	}

	events, err := store.EventsAfter(ctx, run.ID, 1)
	if err != nil {
		t.Fatalf("EventsAfter: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("EventsAfter returned %d events want 2", len(events))
	}
	if events[0].Offset != 2 || events[1].Offset != 3 {
		t.Errorf("replayed offsets = %d,%d", events[0].Offset, events[1].Offset)
	}
	if string(events[0].Payload) != `"x"` {
		t.Errorf("payload = %s", events[0].Payload)
	}
}

func TestPGStoreListIdleSessions(t *testing.T) {
	db := pgTestDB(t)
	store := NewPGStore(db)
	ctx := context.Background()
	userID := pgNewUser(t, db)
	sess, _ := store.CreateSession(ctx, userID, "idle")

	// Force the session to look stale.
	if _, err := db.Exec(`UPDATE sessions SET updated_at = now() - interval '2 hours' WHERE id = $1`, sess.ID); err != nil {
		t.Fatal(err)
	}

	idle, err := store.ListIdleSessions(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("ListIdleSessions: %v", err)
	}
	found := false
	for _, s := range idle {
		if s.ID == sess.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("expected session %s in idle list", sess.ID)
	}

	// A recently-active session must not appear.
	if _, err := db.Exec(`UPDATE sessions SET updated_at = now() WHERE id = $1`, sess.ID); err != nil {
		t.Fatal(err)
	}
	idle, _ = store.ListIdleSessions(ctx, time.Now().Add(-time.Hour))
	for _, s := range idle {
		if s.ID == sess.ID {
			t.Errorf("recently-active session %s should not be idle", sess.ID)
		}
	}
}

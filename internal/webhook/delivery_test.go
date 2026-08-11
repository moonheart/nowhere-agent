package webhook

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// outboxTestDB opens the dev database. The shared dev PG can hold rows from
// earlier interrupted runs, so each test cleans up by its own run_id prefix
// (rows the test created — never anyone else's) and ClaimNext assertions stay
// deterministic.
func outboxTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("DB_DSN")
	if dsn == "" {
		dsn = "postgres://postgres:postgres@localhost:5432/nowhere?sslmode=disable"
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Skipf("open db: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3_000_000_000)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		t.Skipf("no postgres reachable: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// cleanTestDeliveries removes rows whose run_id matches the given prefixes —
// only rows this package's tests created.
func cleanTestDeliveries(t *testing.T, db *sql.DB, prefixes ...string) {
	t.Helper()
	for _, p := range prefixes {
		if _, err := db.Exec(`DELETE FROM webhook_deliveries WHERE run_id LIKE $1`, p+"%"); err != nil {
			t.Fatalf("clean deliveries %q: %v", p, err)
		}
	}
}

func TestDeliveryEnqueueClaimDeliver(t *testing.T) {
	store := NewDeliveryStore(outboxTestDB(t))
	ctx := context.Background()
	now := time.Now().UTC()
	runID := "run-" + time.Now().Format("150405.000000000")
	cleanTestDeliveries(t, store.db, runID)

	d, err := store.Enqueue(ctx, runID, "sess-1", "https://hooks.example.com/x", "",
		RunCompletedPayload{Event: "run.completed", RunID: runID, Status: "done"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	t.Cleanup(func() { store.db.Exec(`DELETE FROM webhook_deliveries WHERE id = $1`, d.ID) })

	if d.Status != DeliveryPending {
		t.Fatalf("status = %q, want pending", d.Status)
	}

	claimed, err := store.ClaimNext(ctx, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed.ID != d.ID || claimed.Attempts != 1 {
		t.Fatalf("claimed = %+v", claimed)
	}
	// The claim carries a lease: an immediate second claim finds nothing due
	// (the row's next_attempt_at moved out by the lease).
	if _, err := store.ClaimNext(ctx, now.Add(time.Minute)); !errors.Is(err, ErrNoPending) {
		t.Fatalf("second claim within lease: %v, want ErrNoPending", err)
	}
	// After the lease expires the row is claimable again (attempts bumped).
	claimed2, err := store.ClaimNext(ctx, now.Add(time.Minute).Add(claimLease+time.Second))
	if err != nil {
		t.Fatalf("claim after lease: %v", err)
	}
	if claimed2.Attempts != 2 {
		t.Fatalf("attempts after lease reclaim = %d, want 2", claimed2.Attempts)
	}

	if err := store.MarkDelivered(ctx, d.ID, now); err != nil {
		t.Fatalf("mark delivered: %v", err)
	}
	got, _, err := store.List(ctx, DeliveryDelivered, 10, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, row := range got {
		if row.ID == d.ID {
			found = true
			if row.Status != DeliveryDelivered || row.DeliveredAt == nil {
				t.Fatalf("delivered row bad: %+v", row)
			}
		}
	}
	if !found {
		t.Fatal("delivered row not listed")
	}
}

func TestDeliveryFailureBackoffAndRequeue(t *testing.T) {
	store := NewDeliveryStore(outboxTestDB(t))
	ctx := context.Background()
	now := time.Now().UTC()
	runID := "run-fail-" + time.Now().Format("150405.000000000")
	cleanTestDeliveries(t, store.db, runID)

	d, err := store.Enqueue(ctx, runID, "sess-2", "https://hooks.example.com/x", "", RunCompletedPayload{Event: "run.completed", RunID: runID})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	t.Cleanup(func() { store.db.Exec(`DELETE FROM webhook_deliveries WHERE id = $1`, d.ID) })

	// First failure schedules a retry in the future (backoff) — still pending.
	if err := store.MarkFailed(ctx, d.ID, now.Add(time.Minute), "boom"); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	// Not due yet → nothing to claim now.
	if _, err := store.ClaimNext(ctx, now.Add(30*time.Second)); !errors.Is(err, ErrNoPending) {
		t.Fatalf("claim before backoff: %v, want ErrNoPending", err)
	}
	// Due → claimable again. The pre-backoff claim was refused (ErrNoPending)
	// and never consumed an attempt, so this first successful claim sees 1.
	claimed, err := store.ClaimNext(ctx, now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("claim after backoff: %v", err)
	}
	if claimed.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (the refused pre-backoff claim did not count)", claimed.Attempts)
	}
	// A failure with no future window dead-letters the row.
	if err := store.MarkFailed(ctx, d.ID, now.Add(-time.Minute), "final"); err != nil {
		t.Fatalf("mark failed final: %v", err)
	}
	got, _, err := store.List(ctx, DeliveryFailed, 10, 0)
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	found := false
	for _, row := range got {
		if row.ID == d.ID {
			found = true
			if row.LastError != "final" {
				t.Fatalf("dead-letter error not recorded: %+v", row)
			}
		}
	}
	if !found {
		t.Fatal("dead-lettered row not listed")
	}
	// Manual requeue resets it to pending and claimable.
	if err := store.Requeue(ctx, d.ID); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	got, _, _ = store.List(ctx, DeliveryPending, 10, 0)
	found = false
	for _, row := range got {
		if row.ID == d.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("requeued row not pending")
	}
}

// TestDeliveryCascadesWithUser pins the PIPL §47 tie-in: a delivery row linked
// to a user disappears when the account is deleted (ON DELETE CASCADE), so no
// conversation summary survives the erasure.
func TestDeliveryCascadesWithUser(t *testing.T) {
	store := NewDeliveryStore(outboxTestDB(t))
	ctx := context.Background()
	cleanTestDeliveries(t, store.db, "run-cascade")

	var userID string
	if err := store.db.QueryRow(
		`INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id`,
		"dlv-"+time.Now().Format("150405.000000000")+"@test.dev").Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() { store.db.Exec(`DELETE FROM users WHERE id = $1`, userID) })

	d, err := store.Enqueue(ctx, "run-cascade", "sess-c", "https://hooks.example.com/x", userID,
		RunCompletedPayload{Event: "run.completed", RunID: "run-cascade", UserID: userID, Summary: "secret summary"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Delete the account; the delivery row must vanish with it.
	if _, err := store.db.Exec(`DELETE FROM users WHERE id = $1`, userID); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	var n int
	if err := store.db.QueryRow(`SELECT count(*) FROM webhook_deliveries WHERE id = $1`, d.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("delivery row survived account deletion (n=%d err=%v)", n, err)
	}
}

// TestPurgeExpired removes old dead letters AND old delivered rows, keeping
// the table bounded.
func TestPurgeExpired(t *testing.T) {
	store := NewDeliveryStore(outboxTestDB(t))
	ctx := context.Background()
	now := time.Now().UTC()
	cleanTestDeliveries(t, store.db, "run-old", "run-fresh", "run-dlv")

	oldFailed := mustEnqueue(t, store, ctx, "run-old-failed")
	oldDelivered := mustEnqueue(t, store, ctx, "run-old-delivered")
	freshFailed := mustEnqueue(t, store, ctx, "run-fresh-failed")
	freshDelivered := mustEnqueue(t, store, ctx, "run-fresh-delivered")
	t.Cleanup(func() {
		for _, id := range []string{oldFailed, oldDelivered, freshFailed, freshDelivered} {
			store.db.Exec(`DELETE FROM webhook_deliveries WHERE id = $1`, id)
		}
	})
	if _, err := store.db.Exec(`
		UPDATE webhook_deliveries SET status = 'failed', created_at = $2 WHERE id = $1`,
		oldFailed, now.Add(-40*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`
		UPDATE webhook_deliveries SET status = 'delivered', created_at = $2, delivered_at = now() WHERE id = $1`,
		oldDelivered, now.Add(-100*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE webhook_deliveries SET status = 'failed' WHERE id = $1`, freshFailed); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE webhook_deliveries SET status = 'delivered', delivered_at = now() WHERE id = $1`, freshDelivered); err != nil {
		t.Fatal(err)
	}

	n, err := store.PurgeExpired(ctx, now)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 2 {
		t.Fatalf("purged %d rows, want 2 (old failed + old delivered)", n)
	}
	got, _, _ := store.List(ctx, "", 10, 0)
	remaining := map[string]bool{}
	for _, row := range got {
		remaining[row.ID] = true
	}
	if !remaining[freshFailed] || !remaining[freshDelivered] {
		t.Fatal("fresh rows were purged")
	}
	if remaining[oldFailed] || remaining[oldDelivered] {
		t.Fatal("old rows survived the purge")
	}
}

func mustEnqueue(t *testing.T, store *DeliveryStore, ctx context.Context, runID string) string {
	t.Helper()
	d, err := store.Enqueue(ctx, runID, "sess-p", "https://hooks.example.com/x", "", RunCompletedPayload{Event: "run.completed", RunID: runID})
	if err != nil {
		t.Fatalf("enqueue %s: %v", runID, err)
	}
	return d.ID
}

package webhook

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"
)

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

func TestDeliveryEnqueueClaimDeliver(t *testing.T) {
	store := NewDeliveryStore(outboxTestDB(t))
	ctx := context.Background()
	now := time.Now().UTC()
	runID := "run-" + time.Now().Format("150405.000000000")

	d, err := store.Enqueue(ctx, runID, "sess-1", "https://hooks.example.com/x",
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
	// The claim is atomic: a second claim finds nothing due.
	if _, err := store.ClaimNext(ctx, now.Add(time.Minute)); !errors.Is(err, ErrNoPending) {
		t.Fatalf("second claim: %v, want ErrNoPending", err)
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

	d, err := store.Enqueue(ctx, runID, "sess-2", "https://hooks.example.com/x", RunCompletedPayload{Event: "run.completed", RunID: runID})
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
	// Due → claimable again, attempts incremented.
	claimed, err := store.ClaimNext(ctx, now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("claim after backoff: %v", err)
	}
	if claimed.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", claimed.Attempts)
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

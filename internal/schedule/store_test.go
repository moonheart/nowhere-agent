package schedule

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func pgDSN() string {
	if v := os.Getenv("SCHEDULE_PG_TEST_DSN"); v != "" {
		return v
	}
	if v := os.Getenv("DB_DSN"); v != "" {
		return v
	}
	return "postgres://postgres:postgres@localhost:5432/nowhere?sslmode=disable"
}

func pgTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", pgDSN())
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

func randSuffix() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// pgNewUser creates a throwaway user to satisfy the task FK; cleanup removes it.
func pgNewUser(t *testing.T, db *sql.DB) string {
	t.Helper()
	var id string
	err := db.QueryRow(`
		INSERT INTO users (email, password_hash, display_name)
		VALUES ($1, 'x', 'schedtest') RETURNING id`,
		"schedtest-"+randSuffix()+"@example.com").Scan(&id)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM users WHERE id = $1`, id) })
	return id
}

func validTask(userID string) Task {
	return Task{
		UserID:         userID,
		Prompt:         "summarize yesterday",
		ToolWhitelist:  []string{"read_file"},
		Cron:           "0 9 * * *",
		Timezone:       "UTC",
		Enabled:        true,
		OnRunCompleted: OnRunKeep,
		Multitask:      MultitaskReject,
	}
}

func TestPGStoreCRUD(t *testing.T) {
	db := pgTestDB(t)
	store := NewPGStore(db)
	ctx := context.Background()
	userID := pgNewUser(t, db)

	created, err := store.Create(ctx, validTask(userID))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { store.Delete(context.Background(), created.ID) })

	if created.ID == "" {
		t.Fatal("create returned empty id")
	}
	if created.NextRunAt.IsZero() {
		t.Fatal("create did not seed next_run_at")
	}
	// 9am UTC daily: next fire must be at minute 0 hour 9 UTC.
	if n := created.NextRunAt.UTC(); n.Hour() != 9 || n.Minute() != 0 {
		t.Fatalf("next_run_at not at 9:00 UTC: %v", n)
	}

	got, err := store.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Prompt != "summarize yesterday" || got.Cron != "0 9 * * *" {
		t.Fatalf("get round-trip mismatch: %+v", got)
	}
	if len(got.ToolWhitelist) != 1 || got.ToolWhitelist[0] != "read_file" {
		t.Fatalf("whitelist round-trip: %v", got.ToolWhitelist)
	}

	// Update changes the schedule and recomputes next_run_at.
	got.Cron = "30 14 * * *"
	updated, err := store.Update(ctx, got)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if n := updated.NextRunAt.UTC(); n.Hour() != 14 || n.Minute() != 30 {
		t.Fatalf("update did not recompute next_run_at to 14:30: %v", n)
	}

	// SetEnabled toggles.
	if err := store.SetEnabled(ctx, created.ID, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	dis, _ := store.Get(ctx, created.ID)
	if dis.Enabled {
		t.Fatal("SetEnabled(false) did not persist")
	}

	// Delete removes; a second delete is ErrNotFound.
	if err := store.Delete(ctx, created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := store.Delete(ctx, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete should be ErrNotFound, got %v", err)
	}
	if _, err := store.Get(ctx, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete should be ErrNotFound, got %v", err)
	}
}

func TestPGStoreMalformedID(t *testing.T) {
	db := pgTestDB(t)
	store := NewPGStore(db)
	ctx := context.Background()
	if _, err := store.Get(ctx, "not-a-uuid"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("malformed id should map to ErrNotFound, got %v", err)
	}
}

func TestPGStoreListDue(t *testing.T) {
	db := pgTestDB(t)
	store := NewPGStore(db)
	ctx := context.Background()
	userID := pgNewUser(t, db)

	mk := func(mutate func(*Task)) Task {
		task := validTask(userID)
		if mutate != nil {
			mutate(&task)
		}
		created, err := store.Create(ctx, task)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		t.Cleanup(func() { store.Delete(context.Background(), created.ID) })
		return created
	}

	now := time.Now()
	due := mk(nil)
	notDue := mk(nil)
	expired := mk(func(t *Task) { past := now.Add(-time.Hour); t.EndTime = &past })
	disabled := mk(func(t *Task) { t.Enabled = false })

	// Force next_run_at into the past for the due + expired + disabled tasks so
	// only the due filter separates them; notDue stays in the future.
	for _, id := range []string{due.ID, expired.ID, disabled.ID} {
		db.Exec(`UPDATE scheduled_task SET next_run_at = $1 WHERE id = $2`, now.Add(-time.Minute), id)
	}
	db.Exec(`UPDATE scheduled_task SET next_run_at = $1 WHERE id = $2`, now.Add(time.Hour), notDue.ID)

	got, err := store.ListDue(ctx, now)
	if err != nil {
		t.Fatalf("listdue: %v", err)
	}
	ids := map[string]bool{}
	for _, task := range got {
		ids[task.ID] = true
	}
	if !ids[due.ID] {
		t.Error("due task missing from scan set")
	}
	if ids[notDue.ID] {
		t.Error("not-yet-due task leaked into scan set")
	}
	if ids[expired.ID] {
		t.Error("expired task leaked into scan set")
	}
	if ids[disabled.ID] {
		t.Error("disabled task leaked into scan set")
	}
}

// TestPGStoreClaimRace is the multi-instance safety check (design D4): two
// concurrent claims of one due task must yield exactly one winner — the loser's
// WHERE next_run_at <= now() matches zero rows after the winner commits.
func TestPGStoreClaimRace(t *testing.T) {
	db := pgTestDB(t)
	ctx := context.Background()
	userID := pgNewUser(t, db)

	store := NewPGStore(db)
	created, err := store.Create(ctx, validTask(userID))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { store.Delete(context.Background(), created.ID) })

	// Make it due right now.
	now := time.Now()
	db.Exec(`UPDATE scheduled_task SET next_run_at = $1 WHERE id = $2`, now.Add(-time.Minute), created.ID)

	const racers = 8
	var wg sync.WaitGroup
	wins := make(chan string, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each racer uses its own store/connection, as separate instances would.
			c, err := store.Claim(ctx, created.ID, now)
			if err == nil {
				wins <- c.ID
			} else if !errors.Is(err, ErrNotFound) {
				t.Errorf("unexpected claim error: %v", err)
			}
		}()
	}
	wg.Wait()
	close(wins)

	n := 0
	for range wins {
		n++
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 winning claim across %d racers, got %d", racers, n)
	}

	// The winner advanced next_run_at into the future.
	after, _ := store.Get(ctx, created.ID)
	if !after.NextRunAt.After(now) {
		t.Fatalf("claim did not advance next_run_at past now: %v", after.NextRunAt)
	}
	if after.LastRunAt == nil {
		t.Fatal("claim did not record last_run_at")
	}
}

// TestPGStoreClaimCatchUp documents design D6: an overdue-by-many-slots task
// claims once and lands on the next FUTURE slot, not one fire per missed slot.
func TestPGStoreClaimCatchUp(t *testing.T) {
	db := pgTestDB(t)
	ctx := context.Background()
	userID := pgNewUser(t, db)
	store := NewPGStore(db)

	created, err := store.Create(ctx, validTask(userID)) // 9am UTC daily
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { store.Delete(context.Background(), created.ID) })

	// Simulate being down for ~3 days: next_run_at is 3 days in the past.
	now := time.Now()
	db.Exec(`UPDATE scheduled_task SET next_run_at = $1 WHERE id = $2`, now.Add(-72*time.Hour), created.ID)

	claimed, err := store.Claim(ctx, created.ID, now)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	// Advanced to the next future 9am, not merely one day forward.
	if !claimed.NextRunAt.After(now) {
		t.Fatalf("catch-up should land on a future slot, got %v (now %v)", claimed.NextRunAt, now)
	}
	// And now it's no longer due — a second claim right after is a skip.
	if _, err := store.Claim(ctx, created.ID, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second immediate claim should skip (ErrNotFound), got %v", err)
	}
}

func TestPGStoreListSessions(t *testing.T) {
	db := pgTestDB(t)
	ctx := context.Background()
	userID := pgNewUser(t, db)
	store := NewPGStore(db)

	created, err := store.Create(ctx, validTask(userID))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { store.Delete(context.Background(), created.ID) })

	// Two sessions tagged to this task, one to another owner (unrelated).
	var s1, s2 string
	db.QueryRow(`INSERT INTO sessions (user_id, title, task_id, source) VALUES ($1,'a',$2,'scheduled') RETURNING id`, userID, created.ID).Scan(&s1)
	db.QueryRow(`INSERT INTO sessions (user_id, title, task_id, source) VALUES ($1,'b',$2,'scheduled') RETURNING id`, userID, created.ID).Scan(&s2)
	t.Cleanup(func() {
		db.Exec(`DELETE FROM sessions WHERE id IN ($1,$2)`, s1, s2)
	})

	ids, err := store.ListSessions(ctx, created.ID)
	if err != nil {
		t.Fatalf("listsessions: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 sessions, got %v", ids)
	}
}

// A task that has fired nothing must still yield a non-nil slice, so the JSON
// form is [] — a null would break a client reading sessions.length.
func TestPGStoreListSessionsEmptyIsNonNil(t *testing.T) {
	db := pgTestDB(t)
	ctx := context.Background()
	userID := pgNewUser(t, db)
	store := NewPGStore(db)

	created, err := store.Create(ctx, validTask(userID))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { store.Delete(context.Background(), created.ID) })

	ids, err := store.ListSessions(ctx, created.ID)
	if err != nil {
		t.Fatalf("listsessions: %v", err)
	}
	if ids == nil {
		t.Fatal("empty ListSessions returned nil slice; JSON would encode null")
	}
	if len(ids) != 0 {
		t.Fatalf("expected 0 sessions, got %v", ids)
	}
}

// A user with no tasks must also get a non-nil slice from ListForUser.
func TestPGStoreListForUserEmptyIsNonNil(t *testing.T) {
	db := pgTestDB(t)
	ctx := context.Background()
	userID := pgNewUser(t, db)
	store := NewPGStore(db)

	tasks, err := store.ListForUser(ctx, userID)
	if err != nil {
		t.Fatalf("listforuser: %v", err)
	}
	if tasks == nil {
		t.Fatal("empty ListForUser returned nil slice; JSON would encode null")
	}
}

// EndSessions soft-deletes the task's active sessions: they vanish from
// ListSessions (status → ended) but their rows remain, and a repeat clear is a
// no-op (0). Sessions of another task are untouched.
func TestPGStoreEndSessions(t *testing.T) {
	db := pgTestDB(t)
	ctx := context.Background()
	userID := pgNewUser(t, db)
	store := NewPGStore(db)

	task, err := store.Create(ctx, validTask(userID))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { store.Delete(context.Background(), task.ID) })
	other, err := store.Create(ctx, validTask(userID))
	if err != nil {
		t.Fatalf("create other: %v", err)
	}
	t.Cleanup(func() { store.Delete(context.Background(), other.ID) })

	// Two sessions on the task, one on the other.
	var s1, s2, s3 string
	db.QueryRow(`INSERT INTO sessions (user_id, title, task_id, source) VALUES ($1,'a',$2,'scheduled') RETURNING id`, userID, task.ID).Scan(&s1)
	db.QueryRow(`INSERT INTO sessions (user_id, title, task_id, source) VALUES ($1,'b',$2,'scheduled') RETURNING id`, userID, task.ID).Scan(&s2)
	db.QueryRow(`INSERT INTO sessions (user_id, title, task_id, source) VALUES ($1,'c',$2,'scheduled') RETURNING id`, userID, other.ID).Scan(&s3)
	t.Cleanup(func() { db.Exec(`DELETE FROM sessions WHERE id IN ($1,$2,$3)`, s1, s2, s3) })

	cleared, err := store.EndSessions(ctx, task.ID)
	if err != nil {
		t.Fatalf("endsessions: %v", err)
	}
	if cleared != 2 {
		t.Fatalf("expected 2 cleared, got %d", cleared)
	}

	// Cleared sessions are hidden from ListSessions but their rows persist.
	ids, err := store.ListSessions(ctx, task.ID)
	if err != nil {
		t.Fatalf("listsessions after clear: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("expected list empty after clear, got %v", ids)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM sessions WHERE id = $1`, s1).Scan(&status); err != nil {
		t.Fatalf("read cleared session row: %v", err)
	}
	if status != "ended" {
		t.Fatalf("expected cleared session status=ended, got %q", status)
	}

	// The other task's session is untouched and still listed.
	otherIDs, err := store.ListSessions(ctx, other.ID)
	if err != nil {
		t.Fatalf("listsessions other: %v", err)
	}
	if len(otherIDs) != 1 || otherIDs[0] != s3 {
		t.Fatalf("expected other task session intact, got %v", otherIDs)
	}

	// Repeat clear is a no-op.
	again, err := store.EndSessions(ctx, task.ID)
	if err != nil {
		t.Fatalf("repeat endsessions: %v", err)
	}
	if again != 0 {
		t.Fatalf("expected repeat clear = 0, got %d", again)
	}
}

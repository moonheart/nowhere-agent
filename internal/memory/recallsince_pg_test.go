package memory

import (
	"context"
	"testing"
	"time"

	"nowhere-agent/internal/identity"
)

// TestPGPortRecallSince exercises the incremental read against Postgres: time
// lower bound, kind filter, and deprecated exclusion. Skips without a database.
func TestPGPortRecallSince(t *testing.T) {
	db := pgTestDB(t)
	p := NewPGPort(db)
	ctx := context.Background()
	scope := identity.UserScope("user-recallsince")

	old, err := p.Store(ctx, Memory{Scope: scope, Kind: KindFact, Content: "recallsince old golang"})
	if err != nil {
		t.Fatal(err)
	}
	// Use the DB's own clock for the watermark (Go's time.Now and Postgres's
	// now() can skew, which made this boundary flaky).
	var mid time.Time
	if err := db.QueryRow(`SELECT now()`).Scan(&mid); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond) // ensure the next inserts land after mid
	recent, err := p.Store(ctx, Memory{Scope: scope, Kind: KindFact, Content: "recallsince new golang"})
	if err != nil {
		t.Fatal(err)
	}
	summ, err := p.Store(ctx, Memory{Scope: scope, Kind: KindSummary, Content: "recallsince golang summary"})
	if err != nil {
		t.Fatal(err)
	}
	cleanup(t, db, old.ID, recent.ID, summ.ID)

	// Zero since, all kinds → all three.
	got, err := p.RecallSince(ctx, time.Time{}, "golang", []identity.ScopeRef{scope}, nil, 10)
	if err != nil || len(got) != 3 {
		t.Fatalf("zero-since recall = %d err %v want 3", len(got), err)
	}

	// since=mid → only memories created after mid (recent + summary, not old).
	got, err = p.RecallSince(ctx, mid, "golang", []identity.ScopeRef{scope}, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range got {
		if m.ID == old.ID {
			t.Errorf("old memory must be excluded by the watermark: %+v", got)
		}
	}
	if len(got) != 2 {
		t.Errorf("incremental recall = %d want 2, got %+v", len(got), got)
	}

	// Kind filter → only the summary.
	got, err = p.RecallSince(ctx, time.Time{}, "golang", []identity.ScopeRef{scope}, []Kind{KindSummary}, 10)
	if err != nil || len(got) != 1 || got[0].ID != summ.ID {
		t.Errorf("kind-filtered recall = %+v want only the summary", got)
	}

	// Deprecated excluded.
	if err := p.Deprecate(ctx, recent.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = p.RecallSince(ctx, time.Time{}, "golang", []identity.ScopeRef{scope}, []Kind{KindFact}, 10)
	for _, m := range got {
		if m.ID == recent.ID {
			t.Errorf("deprecated memory must not be recalled: %+v", got)
		}
	}
}

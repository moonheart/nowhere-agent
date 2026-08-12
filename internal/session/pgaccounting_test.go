package session

import (
	"context"
	"database/sql"
	"testing"

	"nowhere-agent/internal/provider"
)

// pgAccountingTablesReady reports whether migration 000041 is applied to the
// dev database; PG accounting tests skip (with a hint) when it is not, instead
// of failing against an unmigrated shared DB.
func pgAccountingTablesReady(t *testing.T, db *sql.DB) bool {
	t.Helper()
	var n int
	err := db.QueryRow(`
		SELECT count(*) FROM information_schema.tables
		WHERE table_name IN ('run_steps', 'usage_records')`).Scan(&n)
	if err != nil {
		t.Fatalf("check accounting tables: %v", err)
	}
	if n < 2 {
		t.Log("migration 000041 (run_steps, usage_records) not applied; run `go run ./cmd/migrate` to exercise PG accounting tests")
		return false
	}
	return true
}

// TestPGAppendRunStepAndMessageBinding verifies the store-level intent
// contract: nextval-provisioned message ids, explicit-id message inserts, and
// LatestRunSteps' ResultExists join — the crash-site decider.
func TestPGAppendRunStepAndMessageBinding(t *testing.T) {
	db := pgTestDB(t)
	if !pgAccountingTablesReady(t, db) {
		t.Skip("migration 000041 not applied")
	}
	store := NewPGStore(db)
	ms := NewPGMessageStore(db)
	ctx := context.Background()
	sessID, runID := setupMessageSession(t, ctx, db, store)

	// Assistant intent provisions an id that does not exist yet.
	st, err := store.AppendRunStep(ctx, runID, StepAssistant, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.ResultMessageID == nil {
		t.Fatal("assistant intent without provisioned result id")
	}
	steps, err := store.LatestRunSteps(ctx, runID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].ResultExists {
		t.Fatalf("pre-result step: len=%d resultExists=%v, want 1/false", len(steps), steps[0].ResultExists)
	}

	// The message lands with exactly the provisioned id.
	if _, err := ms.AppendMessage(ctx, StoredMessage{
		ID: *st.ResultMessageID, SessionID: sessID, RunID: runID,
		Role:    provider.RoleAssistant,
		Content: []provider.Block{{Type: provider.BlockText, Text: "hi"}},
		Usage:   &provider.Usage{InputTokens: 4, OutputTokens: 2},
	}); err != nil {
		t.Fatal(err)
	}
	steps, err = store.LatestRunSteps(ctx, runID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !steps[0].ResultExists {
		t.Fatal("step with a persisted result must read ResultExists=true")
	}

	// Tool intents in a parallel batch share the first call's id.
	shared := st.ResultMessageID
	for _, tc := range []string{"tu-1", "tu-2"} {
		ts, err := store.AppendRunStep(ctx, runID, StepTool, tc, shared)
		if err != nil {
			t.Fatal(err)
		}
		if ts.ResultMessageID == nil || *ts.ResultMessageID != *shared {
			t.Fatalf("shared batch id broken: %v vs %v", ts.ResultMessageID, shared)
		}
	}

	// overflow_compact rows are excluded from the newest-step view.
	if _, err := store.AppendRunStep(ctx, runID, StepOverflowCompact, "", nil); err != nil {
		t.Fatal(err)
	}
	steps, err = store.LatestRunSteps(ctx, runID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].StepKind != StepTool {
		t.Fatalf("overflow_compact leaked into newest-step view: %+v", steps)
	}
}

// TestPGUsageLedgerSumAndRecovery verifies ledger summation and the
// step-inspecting startup recovery against Postgres.
func TestPGUsageLedgerSumAndRecovery(t *testing.T) {
	db := pgTestDB(t)
	if !pgAccountingTablesReady(t, db) {
		t.Skip("migration 000041 not applied")
	}
	store := NewPGStore(db)
	ctx := context.Background()
	sessID, runID := setupMessageSession(t, ctx, db, store)

	if err := store.AppendUsageRecord(ctx, UsageRecord{
		RunID: runID, Cause: UsageAssistant,
		Usage: provider.Usage{InputTokens: 10, OutputTokens: 5},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendUsageRecord(ctx, UsageRecord{
		RunID: runID, Cause: UsageAdjustment,
		Usage: provider.Usage{InputTokens: -2},
	}); err != nil {
		t.Fatal(err)
	}
	sum, err := store.SumUsage(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if sum.InputTokens != 8 || sum.OutputTokens != 5 {
		t.Errorf("ledger sum = %+v, want input=8 output=5", sum)
	}

	// An interrupted intent (no result message) is exactly what recovery reads
	// for a crashed run; the store answers it without a full history scan.
	rt := NewRuntime(store)
	if err := store.UpdateRunStatus(ctx, runID, RunRunning); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendRunStep(ctx, runID, StepAssistant, "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.RecoverStrandedRuns(ctx); err != nil {
		t.Fatal(err)
	}
	runs, err := store.RunsForSession(ctx, sessID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		if r.Status != RunFailed {
			t.Errorf("stranded run status = %v, want failed", r.Status)
		}
	}
}

package session

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// TestMemStoreApprovalLifecycle covers create → pending lookup → decide, plus
// the one-pending-per-run invariant and the double-decide error.
func TestMemStoreApprovalLifecycle(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	sess, _ := s.CreateSession(ctx, "u1", "t")
	run, _ := s.CreateRun(ctx, sess.ID, 1)

	// Create a pending approval.
	a, err := s.CreateApproval(ctx, Approval{
		RunID: run.ID, SessionID: sess.ID,
		ToolCallID: "tc1", ToolName: "read_file",
		ToolInput: json.RawMessage(`{"path":"/x"}`),
	})
	if err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}
	if a.ID == "" || a.Status != ApprovalPending {
		t.Fatalf("new approval wrong: %+v", a)
	}

	// Pending lookup by run.
	got, ok, err := s.PendingApprovalForRun(ctx, run.ID)
	if err != nil || !ok || got.ID != a.ID {
		t.Fatalf("PendingApprovalForRun: ok=%v got=%+v err=%v", ok, got, err)
	}

	// One pending per run.
	if _, err := s.CreateApproval(ctx, Approval{RunID: run.ID, SessionID: sess.ID, ToolCallID: "tc2", ToolName: "y"}); err == nil {
		t.Fatal("expected error creating a second pending approval for the same run")
	}

	// Approve it.
	dec, err := s.DecideApproval(ctx, a.ID, true)
	if err != nil {
		t.Fatalf("DecideApproval: %v", err)
	}
	if dec.Status != ApprovalApproved || dec.DecidedAt == nil {
		t.Fatalf("decided approval wrong: %+v", dec)
	}

	// No longer pending for the run; double-decide errors.
	if _, ok, _ := s.PendingApprovalForRun(ctx, run.ID); ok {
		t.Fatal("approval should no longer be pending")
	}
	if _, err := s.DecideApproval(ctx, a.ID, true); !errors.Is(err, ErrNoPendingApproval) {
		t.Fatalf("double decide should error, got %v", err)
	}

	// GetApproval still returns the decided record.
	fetched, err := s.GetApproval(ctx, a.ID)
	if err != nil || fetched.Status != ApprovalApproved {
		t.Fatalf("GetApproval after decide: %+v err=%v", fetched, err)
	}
}

func TestMemStoreApprovalReject(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	sess, _ := s.CreateSession(ctx, "u1", "t")
	run, _ := s.CreateRun(ctx, sess.ID, 1)
	a, _ := s.CreateApproval(ctx, Approval{RunID: run.ID, SessionID: sess.ID, ToolCallID: "tc", ToolName: "n"})

	dec, err := s.DecideApproval(ctx, a.ID, false)
	if err != nil {
		t.Fatalf("DecideApproval: %v", err)
	}
	if dec.Status != ApprovalRejected {
		t.Fatalf("expected rejected, got %v", dec.Status)
	}
}

func TestMemStoreApprovalUnknown(t *testing.T) {
	s := NewMemStore()
	if _, err := s.GetApproval(context.Background(), "nope"); !errors.Is(err, ErrNoPendingApproval) {
		t.Fatalf("unknown approval: %v", err)
	}
	if _, err := s.DecideApproval(context.Background(), "nope", true); !errors.Is(err, ErrNoPendingApproval) {
		t.Fatalf("decide unknown: %v", err)
	}
	if _, ok, _ := s.PendingApprovalForRun(context.Background(), "no-run"); ok {
		t.Fatal("no pending approval expected for unknown run")
	}
}

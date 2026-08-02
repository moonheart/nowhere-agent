package session

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// TestMemStoreApprovalLifecycle covers create → pending lookup → decide, plus
// the double-decide error.
func TestMemStoreApprovalLifecycle(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	sess, _ := s.CreateSession(ctx, "u1", "t")
	run, _ := s.CreateRun(ctx, sess.ID, 1)

	// Create a pending approval.
	a, err := s.CreateApproval(ctx, Approval{
		RunID: run.ID, SessionID: sess.ID,
		ToolCallID: "tc1", ToolName: "read_file",
		Payload: json.RawMessage(`{"path":"/x"}`),
	})
	if err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}
	if a.ID == "" || a.Status != ApprovalPending {
		t.Fatalf("new approval wrong: %+v", a)
	}
	// Pending lookup by session.
	got, ok, err := s.PendingApprovalForSession(ctx, sess.ID)
	if err != nil || !ok || got.ID != a.ID {
		t.Fatalf("PendingApprovalForSession: ok=%v got=%+v err=%v", ok, got, err)
	}

	// Approve it.
	dec, err := s.DecideApproval(ctx, a.ID, true, nil)
	if err != nil {
		t.Fatalf("DecideApproval: %v", err)
	}
	if dec.Status != ApprovalApproved || dec.DecidedAt == nil {
		t.Fatalf("decided approval wrong: %+v", dec)
	}

	// No longer pending for the session; double-decide errors.
	if _, ok, _ := s.PendingApprovalForSession(ctx, sess.ID); ok {
		t.Fatal("approval should no longer be pending")
	}
	if _, err := s.DecideApproval(ctx, a.ID, true, nil); !errors.Is(err, ErrNoPendingApproval) {
		t.Fatalf("double decide should error, got %v", err)
	}

	// GetApproval still returns the decided record.
	fetched, err := s.GetApproval(ctx, a.ID)
	if err != nil || fetched.Status != ApprovalApproved {
		t.Fatalf("GetApproval after decide: %+v err=%v", fetched, err)
	}
}

// TestMemStoreMultiPendingQueue pins the multi-approval queue: several pending
// interactions coexist per session (a gated batch parks one per call), the
// session query returns them in queue order, and the per-run query reports the
// batch's remaining pendings until the last is decided.
func TestMemStoreMultiPendingQueue(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	sess, _ := s.CreateSession(ctx, "u1", "t")
	run, _ := s.CreateRun(ctx, sess.ID, 1)

	// A gated batch of three calls in one run.
	ids := make([]string, 3)
	for i, tc := range []string{"tc1", "tc2", "tc3"} {
		a, err := s.CreateApproval(ctx, Approval{RunID: run.ID, SessionID: sess.ID, ToolCallID: tc, ToolName: "edit_file"})
		if err != nil {
			t.Fatalf("CreateApproval %d: %v", i, err)
		}
		ids[i] = a.ID
	}

	// The whole queue is pending, in creation order.
	queue, err := s.PendingApprovalsForSession(ctx, sess.ID)
	if err != nil || len(queue) != 3 {
		t.Fatalf("PendingApprovalsForSession = %v, err %v", queue, err)
	}
	for i, q := range queue {
		if q.ID != ids[i] {
			t.Errorf("queue[%d] = %s, want %s (creation order)", i, q.ID, ids[i])
		}
	}
	// Head is the earliest.
	head, ok, _ := s.PendingApprovalForSession(ctx, sess.ID)
	if !ok || head.ID != ids[0] {
		t.Errorf("head = %+v, want %s", head, ids[0])
	}

	// Decide the first two; the run still has one pending each time until the last.
	if _, err := s.DecideApproval(ctx, ids[0], true, nil); err != nil {
		t.Fatalf("decide 0: %v", err)
	}
	if p, _ := s.PendingApprovalsForRun(ctx, run.ID); len(p) != 2 {
		t.Fatalf("after 1 decide, run pending = %d, want 2", len(p))
	}
	if _, err := s.DecideApproval(ctx, ids[1], false, nil); err != nil {
		t.Fatalf("decide 1: %v", err)
	}
	if p, _ := s.PendingApprovalsForRun(ctx, run.ID); len(p) != 1 || p[0].ID != ids[2] {
		t.Fatalf("after 2 decides, run pending = %+v, want [%s]", p, ids[2])
	}
	// The full batch (any status) reads back in order for folding.
	batch, _ := s.ApprovalsForRun(ctx, run.ID)
	if len(batch) != 3 {
		t.Fatalf("ApprovalsForRun = %d, want 3", len(batch))
	}
	if batch[0].Status != ApprovalApproved || batch[1].Status != ApprovalRejected || batch[2].Status != ApprovalPending {
		t.Errorf("batch statuses = %v %v %v", batch[0].Status, batch[1].Status, batch[2].Status)
	}
	// Last decision empties the run's pending queue.
	if _, err := s.DecideApproval(ctx, ids[2], true, nil); err != nil {
		t.Fatalf("decide 2: %v", err)
	}
	if p, _ := s.PendingApprovalsForRun(ctx, run.ID); len(p) != 0 {
		t.Fatalf("batch complete, run pending = %d, want 0", len(p))
	}
}

func TestMemStoreApprovalReject(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	sess, _ := s.CreateSession(ctx, "u1", "t")
	run, _ := s.CreateRun(ctx, sess.ID, 1)
	a, _ := s.CreateApproval(ctx, Approval{RunID: run.ID, SessionID: sess.ID, ToolCallID: "tc", ToolName: "n"})

	dec, err := s.DecideApproval(ctx, a.ID, false, nil)
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
	if _, err := s.DecideApproval(context.Background(), "nope", true, nil); !errors.Is(err, ErrNoPendingApproval) {
		t.Fatalf("decide unknown: %v", err)
	}
	if _, ok, _ := s.PendingApprovalForSession(context.Background(), "no-session"); ok {
		t.Fatal("no pending approval expected for unknown session")
	}
}

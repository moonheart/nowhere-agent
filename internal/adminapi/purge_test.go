package adminapi

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/session"
	"nowhere-agent/internal/toolruntime"
)

// purgePNG encodes a small solid-color PNG, decodable by the image store.
func purgePNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, color.RGBA{R: 10, G: 20, B: 30, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// ---- platform purge (P2-8 no-data-hard-delete) ----

// TestAdminSessionPurgeDeletesHard pins DELETE /api/admin/sessions/{id}: the
// row is gone for real (runs/messages cascade) and non-admins are refused.
func TestAdminSessionPurgeDeletesHard(t *testing.T) {
	e := newEnv(t)
	admin := e.user(identity.PlatformRoleAdmin)
	owner := e.user(identity.PlatformRoleUser)
	sess := e.sessionFor(owner)

	// A non-admin is refused and the session survives.
	if rec := e.as(owner, "DELETE", "/api/admin/sessions/"+sess.ID, nil); rec.Code != http.StatusForbidden {
		t.Errorf("non-admin purge = %d, want 403", rec.Code)
	}
	if _, err := e.sessions.GetSession(context.Background(), sess.ID); err != nil {
		t.Fatalf("session must survive the refused purge: %v", err)
	}

	// The admin's purge is 204 and the row — with its cascade children — is gone.
	if rec := e.as(admin, "DELETE", "/api/admin/sessions/"+sess.ID, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("admin purge = %d (%s), want 204", rec.Code, rec.Body.String())
	}
	if _, err := e.sessions.GetSession(context.Background(), sess.ID); err == nil {
		t.Error("session survived the purge")
	}
	var n int
	if err := e.db.QueryRow(`SELECT count(*) FROM runs WHERE session_id = $1`, sess.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d runs survived the session purge, want 0 (cascade)", n)
	}
	var m int
	if err := e.db.QueryRow(`SELECT count(*) FROM messages WHERE session_id = $1`, sess.ID).Scan(&m); err != nil {
		t.Fatal(err)
	}
	if m != 0 {
		t.Errorf("%d messages survived the session purge, want 0 (cascade)", m)
	}
}

func TestAdminSessionPurgeMissingIsNotFound(t *testing.T) {
	e := newEnv(t)
	admin := e.user(identity.PlatformRoleAdmin)
	if rec := e.as(admin, "DELETE", "/api/admin/sessions/00000000-0000-0000-0000-000000000000", nil); rec.Code != http.StatusNotFound {
		t.Errorf("purge of a missing session = %d, want 404", rec.Code)
	}
}

// TestAdminSessionPurgeRemovesImages verifies the purge cleans the session's
// workspace image dir while leaving other sessions' images untouched.
func TestAdminSessionPurgeRemovesImages(t *testing.T) {
	e := newEnv(t)
	admin := e.user(identity.PlatformRoleAdmin)
	owner := e.user(identity.PlatformRoleUser)
	target := e.sessionFor(owner)
	other := e.sessionFor(owner)

	img := purgePNG(t)
	if _, err := e.images.Save(target.ID, "a.png", img); err != nil {
		t.Fatalf("save target image: %v", err)
	}
	if _, err := e.images.Save(other.ID, "b.png", img); err != nil {
		t.Fatalf("save other image: %v", err)
	}
	targetDir := filepath.Join(e.images.Root(), target.ID)
	otherDir := filepath.Join(e.images.Root(), other.ID)
	if _, err := os.Stat(targetDir); err != nil {
		t.Fatalf("target image dir missing before purge: %v", err)
	}

	if rec := e.as(admin, "DELETE", "/api/admin/sessions/"+target.ID, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("purge = %d (%s), want 204", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(targetDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("target image dir must be removed, stat err = %v", err)
	}
	if _, err := os.Stat(otherDir); err != nil {
		t.Errorf("other session's image dir must survive the purge: %v", err)
	}
}

// TestAdminSessionPurgeCancelsActiveRun pins the active-run contract: purging
// a session with an in-flight run must interrupt the worker FIRST — a hard
// delete under a live worker would fail its next DB write (FK cascade) with a
// bogus error while the LLM stream keeps spending tokens. The purge returns
// 204, the row is gone, and the worker unwinds (it must not linger against a
// deleted run).
func TestAdminSessionPurgeCancelsActiveRun(t *testing.T) {
	e := newEnv(t)
	admin := e.user(identity.PlatformRoleAdmin)
	owner := e.user(identity.PlatformRoleUser)
	sess := e.sessionFor(owner)
	// sessionFor leaves a queued run row; settle it so the single-active-run
	// lock does not reject the new submit.
	if _, err := e.db.Exec(`UPDATE runs SET status = 'done' WHERE session_id = $1 AND status <> 'done'`, sess.ID); err != nil {
		t.Fatalf("settle queued run: %v", err)
	}

	// Submit a run whose provider blocks until the test ends, so a worker is
	// actively executing for the session.
	release := make(chan struct{})
	defer close(release)
	msg := provider.TextMessage(provider.RoleUser, "hold")
	if _, err := e.registry.Submit(context.Background(), sess.ID, session.RunWork{
		Loop:        agent.New(gateProvider{release: release}, toolruntime.NewRegistry(), agent.Config{Model: "m", MaxTokens: 100}),
		UserMessage: &msg,
	}); err != nil {
		t.Fatalf("submit blocking run: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !e.registry.ActiveWorker(sess.ID) {
		if time.Now().After(deadline) {
			t.Fatal("run worker never became active")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if rec := e.as(admin, "DELETE", "/api/admin/sessions/"+sess.ID, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("admin purge = %d (%s), want 204", rec.Code, rec.Body.String())
	}
	if _, err := e.sessions.GetSession(context.Background(), sess.ID); err == nil {
		t.Error("session survived the purge")
	}

	// The handler waits for the worker to unwind BEFORE deleting (bounded
	// CancelAndWait), so by the time the purge returns there is no live worker
	// left to write into the cascade.
	if e.registry.ActiveWorker(sess.ID) {
		t.Error("purged session's run worker is still active after the purge returned")
	}
}

// TestAdminDeleteUserCancelsActiveRuns pins the active-run contract for user
// deletion: deleting an account with an in-flight run must interrupt the
// worker BEFORE the cascade removes its rows, or the worker fails its next DB
// write (FK cascade) while the LLM stream keeps spending. Both the admin
// delete and the self-service delete must unwind the worker.
func TestAdminDeleteUserCancelsActiveRuns(t *testing.T) {
	for _, path := range []string{"/api/admin/users/", "/api/me"} {
		t.Run(path, func(t *testing.T) {
			e := newEnv(t)
			admin := e.user(identity.PlatformRoleAdmin)
			owner := e.user(identity.PlatformRoleUser)
			sess := e.sessionFor(owner)
			// sessionFor leaves a queued run row; settle it so the single-active-run
			// lock does not reject the new submit.
			if _, err := e.db.Exec(`UPDATE runs SET status = 'done' WHERE session_id = $1 AND status <> 'done'`, sess.ID); err != nil {
				t.Fatalf("settle queued run: %v", err)
			}

			// Submit a run whose provider blocks until the test ends, so a worker
			// is actively executing for the session.
			release := make(chan struct{})
			defer close(release)
			msg := provider.TextMessage(provider.RoleUser, "hold")
			if _, err := e.registry.Submit(context.Background(), sess.ID, session.RunWork{
				Loop:        agent.New(gateProvider{release: release}, toolruntime.NewRegistry(), agent.Config{Model: "m", MaxTokens: 100}),
				UserMessage: &msg,
			}); err != nil {
				t.Fatalf("submit blocking run: %v", err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for !e.registry.ActiveWorker(sess.ID) {
				if time.Now().After(deadline) {
					t.Fatal("run worker never became active")
				}
				time.Sleep(10 * time.Millisecond)
			}

			actor := admin
			target := path + sess.UserID
			if path == "/api/me" {
				actor = owner
				target = path
			}
			rec := e.as(actor, "DELETE", target, nil)
			if rec.Code != http.StatusNoContent {
				t.Fatalf("delete = %d (%s), want 204", rec.Code, rec.Body.String())
			}

			// The handler waits for the worker to unwind BEFORE deleting
			// (bounded CancelAndWait), so no live worker survives the delete.
			if e.registry.ActiveWorker(sess.ID) {
				t.Error("deleted user's run worker is still active after the delete returned")
			}
		})
	}
}

// gateProvider blocks each run in Stream until released or cancelled, keeping
// the run active (a live worker) — the state a purge must cancel before the
// hard delete.
type gateProvider struct{ release <-chan struct{} }

func (gateProvider) Name() string { return "gate" }

func (p gateProvider) Stream(ctx context.Context, _ provider.Request) (<-chan provider.Event, error) {
	ch := make(chan provider.Event, 4)
	ch <- provider.Event{Type: provider.EventMessageStart}
	ch <- provider.Event{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockText}}
	go func() {
		defer close(ch)
		select {
		case <-ctx.Done():
			return
		case <-p.release:
		}
		ch <- provider.Event{Type: provider.EventBlockDelta, Index: 0, Delta: "ok"}
		ch <- provider.Event{Type: provider.EventBlockStop, Index: 0}
		ch <- provider.Event{Type: provider.EventMessageStop, StopReason: provider.StopEndTurn}
	}()
	return ch, nil
}

// TestAdminDeleteUserRemovesImages verifies user deletion cleans the user's
// session image dirs and upload scope.
func TestAdminDeleteUserRemovesImages(t *testing.T) {
	e := newEnv(t)
	admin := e.user(identity.PlatformRoleAdmin)
	victim := e.user(identity.PlatformRoleUser)
	sess := e.sessionFor(victim)

	img := purgePNG(t)
	if _, err := e.images.Save(sess.ID, "a.png", img); err != nil {
		t.Fatalf("save session image: %v", err)
	}
	if _, _, err := e.images.SaveUserUpload(victim.ID, "u.png", img); err != nil {
		t.Fatalf("save user upload: %v", err)
	}
	sessDir := filepath.Join(e.images.Root(), sess.ID)
	upDir := filepath.Join(e.images.Root(), "__uploads__", victim.ID)

	if rec := e.as(admin, "DELETE", "/api/admin/users/"+victim.ID, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete user = %d (%s), want 204", rec.Code, rec.Body.String())
	}
	// The account row is gone.
	if _, err := e.store.UserByID(context.Background(), victim.ID); err == nil {
		t.Error("user row survived the purge")
	}
	// Its images are gone too.
	if _, err := os.Stat(sessDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("victim's session image dir must be removed, stat err = %v", err)
	}
	if _, err := os.Stat(upDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("victim's upload scope must be removed, stat err = %v", err)
	}
}

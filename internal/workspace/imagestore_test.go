package workspace

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// makePNG encodes a small solid-color PNG in memory.
func makePNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, color.RGBA{R: 200, G: 100, B: 50, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func TestImageStoreSaveNormalizesToWebP(t *testing.T) {
	root := t.TempDir()
	s := NewImageStore(root)

	rel, err := s.Save("sess1", "photo.png", makePNG(t))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !strings.HasSuffix(rel, ".webp") {
		t.Errorf("rel path = %q, want .webp", rel)
	}

	// File exists under <root>/sess1 and is WebP (RIFF....WEBP).
	data, err := os.ReadFile(filepath.Join(root, "sess1", rel))
	if err != nil {
		t.Fatalf("read saved: %v", err)
	}
	if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		t.Errorf("saved file is not WebP: %q...", data[:min(12, len(data))])
	}
	// Open reads it back.
	rc, err := s.Open("sess1", rel)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, data) {
		t.Errorf("Open returned different bytes")
	}
}

func TestImageStoreSaveRejectsUndecodable(t *testing.T) {
	s := NewImageStore(t.TempDir())
	if _, err := s.Save("sess1", "bad.png", []byte("not an image")); err == nil {
		t.Error("expected decode failure for non-image bytes")
	}
}

func TestImageStorePathConfinement(t *testing.T) {
	root := t.TempDir()
	s := NewImageStore(root)
	if _, err := s.Save("sess1", "ok.png", makePNG(t)); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Save with a directory in the name must be stripped to a base name.
	rel, err := s.Save("sess1", "nested/photo.png", makePNG(t))
	if err != nil {
		t.Fatalf("Save nested name: %v", err)
	}
	if strings.Contains(rel, "/") || strings.Contains(rel, `\`) {
		t.Errorf("rel path contains dir separator: %q", rel)
	}

	// Open must reject escapes and absolute paths.
	for _, bad := range []string{"../other/x.webp", "..\\x.webp", "/etc/passwd", `C:\windows\system32`} {
		if _, err := s.Open("sess1", bad); err == nil {
			t.Errorf("Open(%q) should be rejected", bad)
		}
	}
}

func TestImageStoreSessionIsolation(t *testing.T) {
	root := t.TempDir()
	s := NewImageStore(root)
	rel, err := s.Save("sessA", "a.png", makePNG(t))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	// Another session cannot read sessA's file by relative name (its own dir
	// does not contain it).
	if _, err := s.Open("sessB", rel); err == nil {
		t.Error("sessB should not open sessA's image")
	}
}

// ---- user-level uploads (change user-image-uploads) ----

func TestUserUploadSaveAndReadRoundTrip(t *testing.T) {
	root := t.TempDir()
	s := NewImageStore(root)

	path, size, err := s.SaveUserUpload("user1", "photo.png", makePNG(t))
	if err != nil {
		t.Fatalf("SaveUserUpload: %v", err)
	}
	if !strings.HasPrefix(path, "uploads/") || !strings.HasSuffix(path, ".webp") {
		t.Errorf("path = %q, want uploads/<id>.webp", path)
	}
	if size <= 0 {
		t.Errorf("size = %d, want > 0", size)
	}

	rc, err := s.OpenUserUpload("user1", path)
	if err != nil {
		t.Fatalf("OpenUserUpload: %v", err)
	}
	data, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(data) != int(size) || len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		t.Errorf("round-tripped blob is not the stored WebP (%d bytes)", len(data))
	}

	// Blob lives under <root>/__uploads__/<user>/<id>.webp.
	id := strings.TrimSuffix(strings.TrimPrefix(path, "uploads/"), ".webp")
	if _, err := os.Stat(filepath.Join(root, "__uploads__", "user1", id+".webp")); err != nil {
		t.Errorf("blob file missing: %v", err)
	}
}

func TestUserUploadRejectsUnsupported(t *testing.T) {
	s := NewImageStore(t.TempDir())
	if _, _, err := s.SaveUserUpload("user1", "x.png", []byte("not an image")); !errors.Is(err, ErrUnsupportedImage) {
		t.Errorf("err = %v, want ErrUnsupportedImage", err)
	}
}

func TestUserUploadCrossUserIsolation(t *testing.T) {
	s := NewImageStore(t.TempDir())
	path, _, err := s.SaveUserUpload("userA", "a.png", makePNG(t))
	if err != nil {
		t.Fatalf("SaveUserUpload: %v", err)
	}
	// userB cannot read userA's upload even with the exact reference path.
	if _, err := s.OpenUserUpload("userB", path); err == nil {
		t.Error("userB should not open userA's upload")
	}
	// userB's delete is confined to userB's own dir — userA's blob must survive.
	_ = s.DeleteUserUpload("userB", path)
	if rc, err := s.OpenUserUpload("userA", path); err != nil {
		t.Error("userB's delete removed userA's blob")
	} else {
		rc.Close()
	}
	if err := s.DeleteUserUpload("userA", path); err != nil {
		t.Errorf("owner delete: %v", err)
	}
}

func TestUserUploadPathEscapeRejected(t *testing.T) {
	s := NewImageStore(t.TempDir())
	for _, p := range []string{"uploads/../../etc/passwd.webp", "uploads/..%2f.webp", "uploads/..webp", "uploads/.webp", "uploads/a/b.webp", "plain.webp"} {
		if _, err := s.OpenUserUpload("user1", p); err == nil {
			t.Errorf("OpenUserUpload(%q) should be rejected", p)
		}
	}
}

// The materialization resolver dispatches by path form: uploads/ references
// resolve from the user scope, session-relative references from the session dir.
func TestResolverForDispatchesPrefix(t *testing.T) {
	root := t.TempDir()
	s := NewImageStore(root)

	// A session-scoped image and a user-level upload for the same user.
	sessRel, err := s.Save("sess1", "s.png", makePNG(t))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	upPath, _, err := s.SaveUserUpload("user1", "u.png", makePNG(t))
	if err != nil {
		t.Fatalf("SaveUserUpload: %v", err)
	}

	res := s.ResolverFor("sess1", "user1")

	// Session path resolves.
	if _, err := res.ResolveImage(context.Background(), sessRel); err != nil {
		t.Errorf("session path resolve: %v", err)
	}
	// User-level path resolves.
	if _, err := res.ResolveImage(context.Background(), upPath); err != nil {
		t.Errorf("uploads path resolve: %v", err)
	}
	// A different user's resolver cannot resolve the uploads path.
	resOther := s.ResolverFor("sess1", "user2")
	if _, err := resOther.ResolveImage(context.Background(), upPath); err == nil {
		t.Error("another user's resolver resolved the upload")
	}
}

// ---- retention sweep (P2-8: no-data hard-delete, workspace image cleanup) ----

func TestSweepEndedSessionImagesRemovesOnlyListedOldSessions(t *testing.T) {
	root := t.TempDir()
	s := NewImageStore(root)

	// Two sessions with images, plus a decoy file at the root.
	for _, id := range []string{"old-sess", "fresh-sess"} {
		if _, err := s.Save(id, "photo.png", makePNG(t)); err != nil {
			t.Fatalf("Save(%s): %v", id, err)
		}
	}
	decoy := filepath.Join(root, "decoy.txt")
	if err := os.WriteFile(decoy, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A user upload must survive too (it lives under __uploads__).
	upPath, _, err := s.SaveUserUpload("user1", "u.png", makePNG(t))
	if err != nil {
		t.Fatalf("SaveUserUpload: %v", err)
	}

	var calls int
	cutoff := time.Now()
	listEnded := func(ctx context.Context, before time.Time, afterID string, limit int) ([]string, error) {
		calls++
		if calls > 1 {
			return nil, nil
		}
		if !before.Equal(cutoff) || limit != 10 {
			t.Errorf("lister args = (%v, %d), want (%v, 10)", before, limit, cutoff)
		}
		return []string{"old-sess"}, nil
	}

	removed, err := SweepEndedSessionImages(context.Background(), nil, s, listEnded, cutoff, 10)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}

	// old-sess is gone; fresh-sess, the decoy, and the user upload survive.
	if _, err := s.Open("old-sess", "photo.webp"); err == nil {
		t.Error("old session's image should be gone")
	}
	if rc, err := s.Open("fresh-sess", "photo.webp"); err != nil {
		t.Errorf("fresh session's image must survive: %v", err)
	} else {
		rc.Close()
	}
	if _, err := os.Stat(decoy); err != nil {
		t.Errorf("decoy file must survive: %v", err)
	}
	if rc, err := s.OpenUserUpload("user1", upPath); err != nil {
		t.Errorf("user upload must survive: %v", err)
	} else {
		rc.Close()
	}
}

func TestDeleteSessionImagesKeepsNonImageSiblingFiles(t *testing.T) {
	root := t.TempDir()
	s := NewImageStore(root)
	if _, err := s.Save("sess1", "photo.png", makePNG(t)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// A sibling non-image file mimics a shared sandbox workspace: it must stay.
	sibling := filepath.Join(root, "sess1", "notes.txt")
	if err := os.WriteFile(sibling, []byte("sandbox file"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteSessionImages("sess1"); err != nil {
		t.Fatalf("DeleteSessionImages: %v", err)
	}
	if _, err := s.Open("sess1", "photo.webp"); err == nil {
		t.Error("session image should be gone")
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Errorf("non-image sibling must survive: %v", err)
	}
}

func TestDeleteSessionImagesRejectsTraversal(t *testing.T) {
	s := NewImageStore(t.TempDir())
	for _, id := range []string{"..", "../evil", "a/b", `..\evil`, "", "."} {
		if err := s.DeleteSessionImages(id); err == nil {
			t.Errorf("DeleteSessionImages(%q) should be rejected", id)
		}
	}
}

func TestDeleteSessionImagesMissingDirIsNoop(t *testing.T) {
	s := NewImageStore(t.TempDir())
	if err := s.DeleteSessionImages("no-such-session"); err != nil {
		t.Errorf("missing dir should be a no-op, got %v", err)
	}
}

func TestSweepStopsWhenListExhausted(t *testing.T) {
	s := NewImageStore(t.TempDir())
	var calls int
	listEnded := func(context.Context, time.Time, string, int) ([]string, error) {
		calls++
		return nil, nil // empty first call: nothing to do
	}
	removed, err := SweepEndedSessionImages(context.Background(), nil, s, listEnded, time.Now(), 100)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 0 || calls != 1 {
		t.Errorf("removed=%d calls=%d, want 0/1 (one scan, then stop)", removed, calls)
	}
}

// TestSweepTerminatesWhenListerRepeatsPage pins the sweep's keyset guard: the
// real PG lister pages by (ended_at, id), but deleting image dirs does NOT
// move sessions rows — a lister that ignores the cursor and returns the same
// full page again would loop forever when the candidate count exceeds the
// page size. The sweep must detect the stalled page and terminate.
func TestSweepTerminatesWhenListerRepeatsPage(t *testing.T) {
	root := t.TempDir()
	s := NewImageStore(root)
	for _, id := range []string{"sess-1", "sess-2", "sess-3"} {
		if _, err := s.Save(id, "photo.png", makePNG(t)); err != nil {
			t.Fatalf("Save(%s): %v", id, err)
		}
	}

	// Page size 2 < 3 expired candidates; the stub ignores the cursor and
	// keeps returning the same first page (pre-fix this looped forever).
	var calls int
	listEnded := func(ctx context.Context, before time.Time, afterID string, limit int) ([]string, error) {
		calls++
		return []string{"sess-1", "sess-2"}, nil
	}

	removed, err := SweepEndedSessionImages(context.Background(), nil, s, listEnded, time.Now(), 2)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2 (one page deleted, then the stalled page aborts)", removed)
	}
	if calls != 2 {
		t.Errorf("lister calls = %d, want 2 (first page + repeated page detection)", calls)
	}
	// The first page's dirs are gone; the unlisted session's dir survives.
	if _, err := s.Open("sess-1", "photo.webp"); err == nil {
		t.Error("sess-1's image should be gone after the first page")
	}
	if rc, err := s.Open("sess-3", "photo.webp"); err != nil {
		t.Errorf("unlisted sess-3's image must survive: %v", err)
	} else {
		rc.Close()
	}
}

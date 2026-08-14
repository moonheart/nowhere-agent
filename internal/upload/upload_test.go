package upload

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"nowhere-agent/internal/workspace"
)

func pgTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("UPLOAD_PG_TEST_DSN")
	if dsn == "" {
		dsn = os.Getenv("DB_DSN")
	}
	if dsn == "" {
		dsn = "postgres://postgres:postgres@localhost:5432/nowhere?sslmode=disable"
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Skipf("open db: %v", err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		db.Close()
		t.Skipf("no postgres reachable: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func randHex() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// makePNG encodes a small solid-color PNG in memory.
func makePNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, color.RGBA{R: 30, G: 60, B: 90, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// svc wires a service over PG + a temp blob dir.
func svc(t *testing.T) (*Service, *sql.DB, *workspace.ImageStore) {
	t.Helper()
	db := pgTestDB(t)
	blobs := workspace.NewImageStore(t.TempDir())
	s := NewService(NewPGStore(db), blobs, nil)
	return s, db, blobs
}

// svcQuota wires a service with a fixed per-user quota.
func svcQuota(t *testing.T, q Quota) (*Service, *sql.DB) {
	t.Helper()
	db := pgTestDB(t)
	blobs := workspace.NewImageStore(t.TempDir())
	s := NewService(NewPGStore(db), blobs, func() Quota { return q })
	return s, db
}

// createUser inserts a throwaway account and cleans it up (cascading to its
// uploads, sessions, runs, and messages).
func createUser(t *testing.T, db *sql.DB) string {
	t.Helper()
	var id string
	if err := db.QueryRow(`INSERT INTO users (email, password_hash) VALUES ($1,'x') RETURNING id::text`,
		"up-"+randHex()+"@test.dev").Scan(&id); err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM users WHERE id = $1`, id) })
	return id
}

// addMessage inserts a message whose content references the upload path, so the
// reference-protection scan has something to find.
func addMessage(t *testing.T, db *sql.DB, userID, uploadID string) {
	t.Helper()
	var sessID string
	if err := db.QueryRow(`INSERT INTO sessions (user_id) VALUES ($1) RETURNING id::text`, userID).Scan(&sessID); err != nil {
		t.Fatalf("create session: %v", err)
	}
	var runID string
	if err := db.QueryRow(`INSERT INTO runs (session_id, seq) VALUES ($1, 1) RETURNING id::text`, sessID).Scan(&runID); err != nil {
		t.Fatalf("create run: %v", err)
	}
	content := `[{"type":"image","imagePath":"uploads/` + uploadID + `.webp"}]`
	if _, err := db.Exec(`INSERT INTO messages (session_id, run_id, seq, role, content) VALUES ($1,$2,1,'user',$3::jsonb)`,
		sessID, runID, content); err != nil {
		t.Fatalf("create message: %v", err)
	}
}

func TestUploadRoundTrip(t *testing.T) {
	s, db, _ := svc(t)
	userID := createUser(t, db)
	ctx := context.Background()

	u, err := s.Upload(ctx, userID, "photo.png", makePNG(t))
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if u.ID == "" || u.UserID != userID || u.Filename != "photo.png" || u.MediaType != "image/webp" || u.Size <= 0 {
		t.Errorf("upload = %+v", u)
	}

	list, err := s.List(ctx, userID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].ID != u.ID {
		t.Errorf("list = %+v, want the uploaded record", list)
	}
}

func TestListScopedToUser(t *testing.T) {
	s, db, _ := svc(t)
	a, b := createUser(t, db), createUser(t, db)
	ctx := context.Background()

	if _, err := s.Upload(ctx, a, "a.png", makePNG(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Upload(ctx, b, "b.png", makePNG(t)); err != nil {
		t.Fatal(err)
	}
	listA, _ := s.List(ctx, a)
	listB, _ := s.List(ctx, b)
	if len(listA) != 1 || listA[0].Filename != "a.png" {
		t.Errorf("A list = %+v", listA)
	}
	if len(listB) != 1 || listB[0].Filename != "b.png" {
		t.Errorf("B list = %+v", listB)
	}
}

func TestDeleteOwnUnreferenced(t *testing.T) {
	s, db, blobs := svc(t)
	userID := createUser(t, db)
	ctx := context.Background()

	u, err := s.Upload(ctx, userID, "a.png", makePNG(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, userID, u.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	list, _ := s.List(ctx, userID)
	if len(list) != 0 {
		t.Errorf("list after delete = %+v", list)
	}
	// Blob is gone from the user's scope too.
	path := "uploads/" + u.ID + ".webp"
	if rc, err := blobs.OpenUserUpload(userID, path); err == nil {
		rc.Close()
		t.Error("blob still exists after delete")
	}
}

func TestDeleteUnownedIsNotFound(t *testing.T) {
	s, db, _ := svc(t)
	a, b := createUser(t, db), createUser(t, db)
	ctx := context.Background()

	u, err := s.Upload(ctx, a, "a.png", makePNG(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, b, u.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-user delete err = %v, want ErrNotFound", err)
	}
}

func TestDeleteReferencedIsConflict(t *testing.T) {
	s, db, _ := svc(t)
	userID := createUser(t, db)
	ctx := context.Background()

	u, err := s.Upload(ctx, userID, "a.png", makePNG(t))
	if err != nil {
		t.Fatal(err)
	}
	addMessage(t, db, userID, u.ID)

	if err := s.Delete(ctx, userID, u.ID); !errors.Is(err, ErrReferenced) {
		t.Errorf("referenced delete err = %v, want ErrReferenced", err)
	}
	// Record and blob survive.
	if _, err := s.store.Get(ctx, u.ID); err != nil {
		t.Errorf("record lost after rejected delete: %v", err)
	}
	list, _ := s.List(ctx, userID)
	if len(list) != 1 {
		t.Errorf("list after rejected delete = %+v", list)
	}
}

// TestDeleteReferenceCheckScopedToOwner: the reference scan must only look at
// the upload owner's own messages. Another user's content can never resolve an
// "uploads/<id>.webp" path under this user's scope (the read route resolves
// under the author's scope), so a cross-user LIKE hit must neither block the
// delete nor leak into the result.
func TestDeleteReferenceCheckScopedToOwner(t *testing.T) {
	s, db, _ := svc(t)
	a, b := createUser(t, db), createUser(t, db)
	ctx := context.Background()

	u, err := s.Upload(ctx, a, "a.png", makePNG(t))
	if err != nil {
		t.Fatal(err)
	}
	// B's session holds a message that embeds A's exact upload reference.
	// It must not count against A.
	addMessage(t, db, b, u.ID)

	if err := s.Delete(ctx, a, u.ID); err != nil {
		t.Fatalf("delete blocked by another user's message: %v (want nil)", err)
	}
	// A's own message embedding the same reference must block.
	u2, err := s.Upload(ctx, a, "a2.png", makePNG(t))
	if err != nil {
		t.Fatal(err)
	}
	addMessage(t, db, a, u2.ID)
	if err := s.Delete(ctx, a, u2.ID); !errors.Is(err, ErrReferenced) {
		t.Errorf("delete with own reference err = %v, want ErrReferenced", err)
	}
}

func TestDeleteMissingIsNotFound(t *testing.T) {
	s, db, _ := svc(t)
	userID := createUser(t, db)
	if err := s.Delete(context.Background(), userID, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing delete err = %v, want ErrNotFound", err)
	}
}

// TestDeleteBlobFailureSurfacesError: a deletion whose record is removed but
// whose blob cannot be deleted must return ErrBlobRemovalFailed — never a
// silent success — and leave the orphan visible for the operator.
func TestDeleteBlobFailureSurfacesError(t *testing.T) {
	s, db, blobs := svc(t)
	userID := createUser(t, db)
	ctx := context.Background()

	u, err := s.Upload(ctx, userID, "a.png", makePNG(t))
	if err != nil {
		t.Fatal(err)
	}
	// Replace the blob file with a non-empty DIRECTORY of the same name, so
	// the blob store's os.Remove fails deterministically (a non-empty dir
	// cannot be removed on any platform).
	blobPath := filepath.Join(blobs.Root(), "__uploads__", userID, u.ID+".webp")
	if err := os.Remove(blobPath); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(blobPath, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	err = s.Delete(ctx, userID, u.ID)
	if !errors.Is(err, ErrBlobRemovalFailed) {
		t.Fatalf("Delete err = %v, want ErrBlobRemovalFailed", err)
	}
	// The record is deleted either way; the orphan blob dir remains.
	list, _ := s.List(ctx, userID)
	if len(list) != 0 {
		t.Errorf("record should be deleted despite the blob failure, list = %+v", list)
	}
	if _, err := os.Stat(blobPath); err != nil {
		t.Errorf("orphan blob should remain after the failed removal: %v", err)
	}
	// A retry of the delete answers ErrNotFound — the failure is terminal.
	if err := s.Delete(ctx, userID, u.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("retry delete err = %v, want ErrNotFound", err)
	}
}

func TestQuotaRejectsAtFileCap(t *testing.T) {
	s, db := svcQuota(t, Quota{MaxFiles: 1, MaxBytes: 1 << 20})
	userID := createUser(t, db)
	ctx := context.Background()

	if _, err := s.Upload(ctx, userID, "a.png", makePNG(t)); err != nil {
		t.Fatalf("first upload under the cap: %v", err)
	}
	if _, err := s.Upload(ctx, userID, "b.png", makePNG(t)); !errors.Is(err, ErrQuotaExceeded) {
		t.Errorf("second upload err = %v, want ErrQuotaExceeded", err)
	}
	// The rejected upload must not have created a record or a blob.
	list, _ := s.List(ctx, userID)
	if len(list) != 1 {
		t.Errorf("list after rejected upload = %+v, want only the first record", list)
	}
}

func TestQuotaRejectsAtByteCap(t *testing.T) {
	// Cap below any plausible WebP encoding of an 8x8 PNG: the raw payload
	// alone (a few hundred bytes) crosses the cap, so the upload is refused
	// before any blob write.
	s, db := svcQuota(t, Quota{MaxFiles: 100, MaxBytes: 10})
	userID := createUser(t, db)
	ctx := context.Background()

	if _, err := s.Upload(ctx, userID, "a.png", makePNG(t)); !errors.Is(err, ErrQuotaExceeded) {
		t.Errorf("upload beyond byte cap err = %v, want ErrQuotaExceeded", err)
	}
	list, _ := s.List(ctx, userID)
	if len(list) != 0 {
		t.Errorf("list after rejected upload = %+v, want empty", list)
	}
}

func TestQuotaAllowsUpToCaps(t *testing.T) {
	// Byte cap large enough for two encodings but not three would be brittle
	// (encoded sizes vary); instead verify count + byte caps admit everything
	// clearly inside them.
	s, db := svcQuota(t, Quota{MaxFiles: 2, MaxBytes: 1 << 20})
	userID := createUser(t, db)
	ctx := context.Background()

	for _, name := range []string{"a.png", "b.png"} {
		if _, err := s.Upload(ctx, userID, name, makePNG(t)); err != nil {
			t.Fatalf("upload %s under caps: %v", name, err)
		}
	}
	list, _ := s.List(ctx, userID)
	if len(list) != 2 {
		t.Errorf("list = %+v, want both uploads", list)
	}
}

func TestQuotaIsPerUser(t *testing.T) {
	s, db := svcQuota(t, Quota{MaxFiles: 1, MaxBytes: 1 << 20})
	a, b := createUser(t, db), createUser(t, db)
	ctx := context.Background()

	if _, err := s.Upload(ctx, a, "a.png", makePNG(t)); err != nil {
		t.Fatalf("A first upload: %v", err)
	}
	// A is now at the cap; B must still be able to use their own budget.
	if _, err := s.Upload(ctx, b, "b.png", makePNG(t)); err != nil {
		t.Fatalf("B's own quota is independent: %v", err)
	}
	if _, err := s.Upload(ctx, a, "a2.png", makePNG(t)); !errors.Is(err, ErrQuotaExceeded) {
		t.Errorf("A at the cap err = %v, want ErrQuotaExceeded", err)
	}
	if _, err := s.Upload(ctx, b, "b2.png", makePNG(t)); !errors.Is(err, ErrQuotaExceeded) {
		t.Errorf("B at its own cap err = %v, want ErrQuotaExceeded", err)
	}
	listA, _ := s.List(ctx, a)
	listB, _ := s.List(ctx, b)
	if len(listA) != 1 || len(listB) != 1 {
		t.Errorf("each user must hold exactly one upload, A=%d B=%d", len(listA), len(listB))
	}
}

func TestQuotaZeroCapsAreUnlimited(t *testing.T) {
	s, db := svcQuota(t, Quota{})
	userID := createUser(t, db)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := s.Upload(ctx, userID, "a.png", makePNG(t)); err != nil {
			t.Fatalf("upload %d with zero caps: %v", i, err)
		}
	}
	list, _ := s.List(ctx, userID)
	if len(list) != 3 {
		t.Errorf("list = %+v, want all three uploads", list)
	}
}

// failCreateStore wraps a Store and fails every Create, simulating the
// metadata-insert failure that leaves a written blob unreferenced.
type failCreateStore struct {
	Store
}

func (f failCreateStore) Create(context.Context, Upload) (Upload, error) {
	return Upload{}, errors.New("simulated record insert failure")
}

// TestUploadCompensatesOrphanBlob: when the record insert fails after the blob
// landed, the blob must be removed again — a failed upload must not leak an
// orphan blob that quota (record-based) can never reclaim.
func TestUploadCompensatesOrphanBlob(t *testing.T) {
	db := pgTestDB(t)
	blobs := workspace.NewImageStore(t.TempDir())
	s := NewService(failCreateStore{NewPGStore(db)}, blobs, nil)
	userID := createUser(t, db)

	if _, err := s.Upload(context.Background(), userID, "photo.png", makePNG(t)); err == nil {
		t.Fatal("upload should fail when the record insert fails")
	}
	// The blob must have been compensated away: nothing exists under the
	// user's upload scope (<root>/__uploads__/<userID>).
	userDir := filepath.Join(blobs.Root(), "__uploads__", userID)
	entries, err := os.ReadDir(userDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read upload scope: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".webp") {
			t.Errorf("blob %s left behind after failed upload (orphan not compensated)", e.Name())
		}
	}
}

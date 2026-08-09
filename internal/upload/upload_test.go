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
	s := NewService(NewPGStore(db), blobs)
	return s, db, blobs
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

func TestDeleteMissingIsNotFound(t *testing.T) {
	s, db, _ := svc(t)
	userID := createUser(t, db)
	if err := s.Delete(context.Background(), userID, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing delete err = %v, want ErrNotFound", err)
	}
}


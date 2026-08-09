package adminapi

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"nowhere-agent/internal/httpx"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/upload"
	"nowhere-agent/internal/usage"
)

// memUploader is an in-memory upload.Uploader for the self-service routes.
type memUploader struct {
	mu         sync.Mutex
	uploads    []upload.Upload
	referenced map[string]bool
}

func (m *memUploader) Upload(_ context.Context, userID, name string, raw []byte) (upload.Upload, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := "up-" + name
	u := upload.Upload{
		ID: id, UserID: userID, Filename: name, Size: int64(len(raw)),
		MediaType: "image/webp", CreatedAt: time.Now().UTC(),
	}
	m.uploads = append(m.uploads, u)
	return u, nil
}

func (m *memUploader) List(_ context.Context, userID string) ([]upload.Upload, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []upload.Upload
	for _, u := range m.uploads {
		if u.UserID == userID {
			out = append(out, u)
		}
	}
	return out, nil
}

func (m *memUploader) Delete(_ context.Context, userID, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, u := range m.uploads {
		if u.ID != id {
			continue
		}
		if u.UserID != userID {
			return upload.ErrNotFound
		}
		if m.referenced != nil && m.referenced[id] {
			return upload.ErrReferenced
		}
		m.uploads = append(m.uploads[:i], m.uploads[i+1:]...)
		return nil
	}
	return upload.ErrNotFound
}

// uploadsEnv wires the console with a fake uploader for the /api/me/uploads tests.
func uploadsEnv(t *testing.T) (*env, *memUploader) {
	t.Helper()
	e := newEnv(t)
	up := &memUploader{}
	h := NewHandler(e.svc, usage.NewStore(e.db), e.mem).WithUploads(up)
	e.mux = http.NewServeMux()
	authed := httpx.NewRouter(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(identity.NewContextWithUser(r.Context(), e.actor)))
		})
	})
	h.RegisterAuthed(authed)
	authed.Mount(e.mux, "/api/")
	return e, up
}

func TestMeUploadsScopedToCaller(t *testing.T) {
	e, up := uploadsEnv(t)
	a := e.user(identity.PlatformRoleUser)
	b := e.user(identity.PlatformRoleUser)
	// Seed via the uploader (chat owns the write path).
	if _, err := up.Upload(context.Background(), a.ID, "a.png", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := up.Upload(context.Background(), b.ID, "b.png", []byte("x")); err != nil {
		t.Fatal(err)
	}

	rec := e.as(a, "GET", "/api/me/uploads", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list as owner = %d (%s)", rec.Code, rec.Body.String())
	}
	if got, want := countJSON(rec.Body.String(), `"filename":"a.png"`), 1; got != want {
		t.Errorf("A sees %d of their own upload, want %d (body=%s)", got, want, rec.Body.String())
	}
	if got := countJSON(rec.Body.String(), `"filename":"b.png"`); got != 0 {
		t.Errorf("A sees B's upload %d times, want 0", got)
	}
}

func TestDeleteOwnUpload(t *testing.T) {
	e, up := uploadsEnv(t)
	u := e.user(identity.PlatformRoleUser)
	rec, _ := up.Upload(context.Background(), u.ID, "a.png", []byte("x"))

	if r := e.as(u, "DELETE", "/api/me/uploads/"+rec.ID, nil); r.Code != http.StatusNoContent {
		t.Fatalf("delete = %d (%s)", r.Code, r.Body.String())
	}
	if r := e.as(u, "GET", "/api/me/uploads", nil); countJSON(r.Body.String(), `"filename":"a.png"`) != 0 {
		t.Errorf("upload survived delete: %s", r.Body.String())
	}
}

func TestDeleteReferencedUploadIs409(t *testing.T) {
	e, up := uploadsEnv(t)
	u := e.user(identity.PlatformRoleUser)
	rec, _ := up.Upload(context.Background(), u.ID, "a.png", []byte("x"))
	up.referenced = map[string]bool{rec.ID: true}

	if r := e.as(u, "DELETE", "/api/me/uploads/"+rec.ID, nil); r.Code != http.StatusConflict {
		t.Errorf("referenced delete = %d, want 409", r.Code)
	}
}

func TestDeleteOtherUsersUploadIs404(t *testing.T) {
	e, up := uploadsEnv(t)
	a := e.user(identity.PlatformRoleUser)
	b := e.user(identity.PlatformRoleUser)
	rec, _ := up.Upload(context.Background(), a.ID, "a.png", []byte("x"))

	if r := e.as(b, "DELETE", "/api/me/uploads/"+rec.ID, nil); r.Code != http.StatusNotFound {
		t.Errorf("cross-user delete = %d, want 404", r.Code)
	}
}

// countJSON counts occurrences of a substring in a JSON body.
func countJSON(body, needle string) int {
	return strings.Count(body, needle)
}

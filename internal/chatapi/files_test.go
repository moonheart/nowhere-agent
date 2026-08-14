package chatapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/session"
	"nowhere-agent/internal/upload"
	"nowhere-agent/internal/workspace"
)

func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			img.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// hugeDimPNG builds a minimal PNG whose header claims 200000×200000 pixels
// (40 Gpx): the bytes are tiny on disk — no pixel data follows the IHDR — but
// a full decode would allocate ~160 GB. The pixel-cap check must reject it
// from the header alone, before any decode.
func hugeDimPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	chunk := func(typ string, data []byte) {
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(data)))
		buf.Write(l[:])
		buf.WriteString(typ)
		buf.Write(data)
		crc := crc32.NewIEEE()
		crc.Write([]byte(typ))
		crc.Write(data)
		var c [4]byte
		binary.BigEndian.PutUint32(c[:], crc.Sum32())
		buf.Write(c[:])
	}
	buf.Write([]byte("\x89PNG\r\n\x1a\n"))
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], 200000)
	binary.BigEndian.PutUint32(ihdr[4:8], 200000)
	ihdr[8] = 8 // bit depth
	ihdr[9] = 2 // color type RGB
	chunk("IHDR", ihdr)
	return buf.Bytes()
}

func setupFileHandler(t *testing.T) (*Handler, *session.Runtime, *workspace.ImageStore, http.Handler) {
	t.Helper()
	rt := session.NewRuntime(session.NewMemStore())
	is := workspace.NewImageStore(t.TempDir())
	h := NewHandler(newTestLoop, "sys").WithRuntime(rt).WithImageStore(is)
	mux := http.NewServeMux()
	h.Register(mux)
	return h, rt, is, mux
}

func fileReq(t *testing.T, sessID, path, userID string) *http.Request {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/chat/sessions/"+sessID+"/files/"+path, nil)
	if userID != "" {
		req = req.WithContext(identity.NewContextWithUser(req.Context(), identity.User{ID: userID}))
	}
	return req
}

func TestServeFileOwnerGetsImage(t *testing.T) {
	_, rt, is, mux := setupFileHandler(t)
	owner := identity.User{ID: "owner1"}
	sess, err := rt.CreateSession(context.Background(), owner.ID, "t")
	if err != nil {
		t.Fatal(err)
	}
	rel, err := is.Save(sess.ID, "pic.png", testPNG(t))
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, fileReq(t, sess.ID, rel, owner.ID))
	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/webp" {
		t.Errorf("content-type = %q want image/webp", ct)
	}
	body, _ := io.ReadAll(rec.Body)
	if len(body) < 12 || string(body[0:4]) != "RIFF" || string(body[8:12]) != "WEBP" {
		t.Errorf("body not WebP")
	}
}

func TestServeFileNonOwnerForbidden(t *testing.T) {
	_, rt, is, mux := setupFileHandler(t)
	sess, _ := rt.CreateSession(context.Background(), "owner1", "t")
	rel, _ := is.Save(sess.ID, "pic.png", testPNG(t))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, fileReq(t, sess.ID, rel, "intruder"))
	if rec.Code != http.StatusForbidden {
		t.Errorf("intruder status = %d want 403", rec.Code)
	}
}

func TestServeFileMissingAndEscapeNotFound(t *testing.T) {
	_, rt, _, mux := setupFileHandler(t)
	owner := identity.User{ID: "owner1"}
	sess, _ := rt.CreateSession(context.Background(), owner.ID, "t")

	// Missing file and an encoded escape that reaches the handler → 404.
	for _, p := range []string{"nope.webp", "..%2F..%2Fsecret"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, fileReq(t, sess.ID, p, owner.ID))
		if rec.Code != http.StatusNotFound {
			t.Errorf("path %q status = %d want 404", p, rec.Code)
		}
	}

	// A literal ".." path is rejected by ServeMux itself (307 redirect to the
	// cleaned path) — it never reaches the workspace, so it is not served.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, fileReq(t, sess.ID, "../x.webp", owner.ID))
	if rec.Code == http.StatusOK {
		t.Error("literal ../ path must not be served")
	}
}

func TestServeFileUnavailableWithoutImageStore(t *testing.T) {
	rt := session.NewRuntime(session.NewMemStore())
	h := NewHandler(newTestLoop, "sys").WithRuntime(rt) // no WithImageStore
	mux := http.NewServeMux()
	h.Register(mux)
	sess, _ := rt.CreateSession(context.Background(), "owner1", "t")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, fileReq(t, sess.ID, "x.webp", "owner1"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d want 503", rec.Code)
	}
}

func uploadReq(t *testing.T, sessID string, body []byte, userID string) *http.Request {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/chat/sessions/"+sessID+"/images", bytes.NewReader(body))
	if userID != "" {
		req = req.WithContext(identity.NewContextWithUser(req.Context(), identity.User{ID: userID}))
	}
	return req
}

func TestServeImageUploadOwnerStoresAndReturnsPath(t *testing.T) {
	_, rt, is, mux := setupFileHandler(t)
	owner := identity.User{ID: "owner1"}
	sess, err := rt.CreateSession(context.Background(), owner.ID, "t")
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, uploadReq(t, sess.ID, testPNG(t), owner.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Path == "" || resp.Path[len(resp.Path)-5:] != ".webp" {
		t.Errorf("path = %q, want a .webp workspace-relative path", resp.Path)
	}
	// The stored file is readable back through the ImageStore.
	rc, err := is.Open(sess.ID, resp.Path)
	if err != nil {
		t.Fatalf("stored image unreadable: %v", err)
	}
	rc.Close()
}

func TestServeImageUploadNonOwnerForbidden(t *testing.T) {
	_, rt, _, mux := setupFileHandler(t)
	sess, _ := rt.CreateSession(context.Background(), "owner1", "t")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, uploadReq(t, sess.ID, testPNG(t), "intruder"))
	if rec.Code != http.StatusForbidden {
		t.Errorf("intruder status = %d want 403", rec.Code)
	}
}

func TestServeImageUploadRejectsGarbage(t *testing.T) {
	_, rt, _, mux := setupFileHandler(t)
	sess, _ := rt.CreateSession(context.Background(), "owner1", "t")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, uploadReq(t, sess.ID, []byte("not an image at all"), "owner1"))
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("garbage status = %d want 415", rec.Code)
	}
}

func TestServeImageUploadRejectsOversize(t *testing.T) {
	_, rt, _, mux := setupFileHandler(t)
	sess, _ := rt.CreateSession(context.Background(), "owner1", "t")

	// A synthetic image larger than the cap: pad a valid PNG with trailing junk
	// is rejected at decode; instead build a big payload directly.
	big := make([]byte, maxImageUploadBytes+1)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, uploadReq(t, sess.ID, big, "owner1"))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize status = %d want 413", rec.Code)
	}
}

// TestServeImageUploadRejectsHugePixelCount: a tiny payload whose header
// claims a giant pixel grid (decompression bomb) must be rejected from the
// header alone — a full decode would OOM the request goroutine.
func TestServeImageUploadRejectsHugePixelCount(t *testing.T) {
	_, rt, _, mux := setupFileHandler(t)
	sess, _ := rt.CreateSession(context.Background(), "owner1", "t")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, uploadReq(t, sess.ID, hugeDimPNG(t), "owner1"))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("huge-dimension status = %d want 413", rec.Code)
	}
}

func setupQuotaHandler(t *testing.T, q upload.Quota) (*Handler, *session.Runtime, http.Handler) {
	t.Helper()
	rt := session.NewRuntime(session.NewMemStore())
	is := workspace.NewImageStore(t.TempDir())
	h := NewHandler(newTestLoop, "sys").
		WithRuntime(rt).
		WithImageStore(is).
		WithImageQuota(func() upload.Quota { return q })
	mux := http.NewServeMux()
	h.Register(mux)
	return h, rt, mux
}

func TestServeImageUploadQuotaCapsSessionFileCount(t *testing.T) {
	_, rt, mux := setupQuotaHandler(t, upload.Quota{MaxFiles: 1})
	sess, _ := rt.CreateSession(context.Background(), "owner1", "t")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, uploadReq(t, sess.ID, testPNG(t), "owner1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("first upload status = %d body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, uploadReq(t, sess.ID, testPNG(t), "owner1"))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("second upload status = %d want 413", rec.Code)
	}
	// A second session has its own independent budget.
	sess2, _ := rt.CreateSession(context.Background(), "owner1", "t")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, uploadReq(t, sess2.ID, testPNG(t), "owner1"))
	if rec.Code != http.StatusOK {
		t.Errorf("upload to a fresh session status = %d want 200", rec.Code)
	}
}

func TestServeImageUploadQuotaCapsSessionBytes(t *testing.T) {
	// The byte check adds the incoming raw payload to the session's stored
	// (WebP-encoded) bytes. Set the cap one byte above the raw payload: the
	// first upload passes (0 + raw), and the second must trip because the
	// stored WebP is at least the 12-byte RIFF header.
	raw := testPNG(t)
	_, rt, mux := setupQuotaHandler(t, upload.Quota{MaxBytes: int64(len(raw) + 1)})
	sess, _ := rt.CreateSession(context.Background(), "owner1", "t")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, uploadReq(t, sess.ID, raw, "owner1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("first upload status = %d body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, uploadReq(t, sess.ID, raw, "owner1"))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("second upload status = %d want 413", rec.Code)
	}
}

func TestServeImageUploadQuotaExemptWhenUnlimited(t *testing.T) {
	_, rt, mux := setupQuotaHandler(t, upload.Quota{MaxFiles: 0, MaxBytes: 0})
	sess, _ := rt.CreateSession(context.Background(), "owner1", "t")

	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, uploadReq(t, sess.ID, testPNG(t), "owner1"))
		if rec.Code != http.StatusOK {
			t.Fatalf("upload %d status = %d want 200", i, rec.Code)
		}
	}
}

// TestServeImageUploadUniqueNamesPerUpload: consecutive uploads to the SAME
// session must land under distinct uuid names — the fixed "upload.webp" name
// used to overwrite the previous file, silently re-pointing old message
// references at the newest image.
func TestServeImageUploadUniqueNamesPerUpload(t *testing.T) {
	_, rt, is, mux := setupFileHandler(t)
	sess, _ := rt.CreateSession(context.Background(), "owner1", "t")

	var paths []string
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, uploadReq(t, sess.ID, testPNG(t), "owner1"))
		if rec.Code != http.StatusOK {
			t.Fatalf("upload %d status = %d body=%s", i, rec.Code, rec.Body.String())
		}
		var resp struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode %d: %v", i, err)
		}
		paths = append(paths, resp.Path)
	}
	if paths[0] == paths[1] {
		t.Fatalf("both uploads returned the same path %q — old reference would be re-pointed", paths[0])
	}
	// Both files must still exist independently (no overwrite).
	for _, p := range paths {
		rc, err := is.Open(sess.ID, p)
		if err != nil {
			t.Errorf("stored image %q unreadable: %v", p, err)
			continue
		}
		rc.Close()
	}
}

// ---- user-level uploads (change user-image-uploads) ----

// fakeUploader implements upload.Uploader over the workspace ImageStore, so the
// handler tests need no database.
type fakeUploader struct {
	store *workspace.ImageStore
}

func (f *fakeUploader) Upload(_ context.Context, userID, name string, raw []byte) (upload.Upload, error) {
	path, size, err := f.store.SaveUserUpload(userID, name, raw)
	if err != nil {
		return upload.Upload{}, err
	}
	id := strings.TrimSuffix(strings.TrimPrefix(path, "uploads/"), ".webp")
	return upload.Upload{ID: id, UserID: userID, Filename: name, Size: size, MediaType: "image/webp", CreatedAt: time.Now()}, nil
}

func (f *fakeUploader) List(context.Context, string) ([]upload.Upload, error) { return nil, nil }
func (f *fakeUploader) Open(_ context.Context, userID, id string) (io.ReadCloser, error) {
	return f.store.OpenUserUpload(userID, "uploads/"+id+".webp")
}
func (f *fakeUploader) Delete(context.Context, string, string) error { return nil }

func setupUserUploadHandler(t *testing.T) (http.Handler, *workspace.ImageStore) {
	t.Helper()
	is := workspace.NewImageStore(t.TempDir())
	h := NewHandler(newTestLoop, "sys").
		WithImageStore(is).
		WithUploads(&fakeUploader{store: is})
	mux := http.NewServeMux()
	h.Register(mux)
	return mux, is
}

func userUploadReq(t *testing.T, userID string, body []byte) *http.Request {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/chat/uploads", bytes.NewReader(body))
	req.Header.Set("Content-Type", "image/png")
	if userID != "" {
		req = req.WithContext(identity.NewContextWithUser(req.Context(), identity.User{ID: userID}))
	}
	return req
}

func TestServeUserImageUploadReturnsUploadPath(t *testing.T) {
	mux, _ := setupUserUploadHandler(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, userUploadReq(t, "owner1", testPNG(t)))
	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasPrefix(resp.Path, "uploads/") || !strings.HasSuffix(resp.Path, ".webp") {
		t.Errorf("path = %q, want uploads/<id>.webp", resp.Path)
	}
}

func TestServeUserFileOwnerOnly(t *testing.T) {
	mux, _ := setupUserUploadHandler(t)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, userUploadReq(t, "owner1", testPNG(t)))
	var resp struct {
		Path string `json:"path"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	id := strings.TrimSuffix(strings.TrimPrefix(resp.Path, "uploads/"), ".webp")

	// Owner reads it back as WebP. The read route takes "<id>.webp" (the
	// reference minus the "uploads/" prefix), matching imageFileUrl.
	req := httptest.NewRequest("GET", "/api/chat/uploads/"+id+".webp", nil)
	req = req.WithContext(identity.NewContextWithUser(req.Context(), identity.User{ID: "owner1"}))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/webp" {
		t.Fatalf("owner read = %d (%s)", rec.Code, rec.Header().Get("Content-Type"))
	}

	// Another user 404s (blob layout confines reads to the caller's scope).
	req = httptest.NewRequest("GET", "/api/chat/uploads/"+id+".webp", nil)
	req = req.WithContext(identity.NewContextWithUser(req.Context(), identity.User{ID: "other"}))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("non-owner read = %d, want 404", rec.Code)
	}
}

func TestServeUserImageUploadRejectsUnsupported(t *testing.T) {
	mux, _ := setupUserUploadHandler(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, userUploadReq(t, "owner1", []byte("not an image")))
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415", rec.Code)
	}
}

func TestServeUserImageUploadRequiresAuth(t *testing.T) {
	mux, _ := setupUserUploadHandler(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, userUploadReq(t, "", testPNG(t)))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// requestImages carries a user-level "uploads/…" path through unchanged, so a
// first-message image part resolves from the user scope at send time.
func TestRequestImagesCarriesUploadsPath(t *testing.T) {
	blocks := requestImages(dataStreamRequest{Images: []incomingImagePart{{Path: "uploads/abc.webp"}}})
	if len(blocks) != 1 || blocks[0].Type != provider.BlockImage || blocks[0].ImagePath != "uploads/abc.webp" {
		t.Errorf("blocks = %+v, want the uploads path preserved", blocks)
	}
}

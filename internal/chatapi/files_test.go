package chatapi

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/session"
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

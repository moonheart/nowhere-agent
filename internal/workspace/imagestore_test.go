package workspace

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

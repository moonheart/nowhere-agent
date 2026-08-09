package builtin

import (
	"context"
	"strings"
	"testing"
	"time"

	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/toolruntime"
)

// fakeVisionAdapter streams back a fixed text response; it records the last
// request so tests can assert what was sent (question forwarded, image present).
type fakeVisionAdapter struct {
	name     string
	lastReq  provider.Request
	response string
	err      error
}

func (f *fakeVisionAdapter) Name() string { return f.name }

func (f *fakeVisionAdapter) Stream(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
	f.lastReq = req
	if f.err != nil {
		return nil, f.err
	}
	ch := make(chan provider.Event, 8)
	ch <- provider.Event{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockText}}
	ch <- provider.Event{Type: provider.EventBlockDelta, Index: 0, Delta: f.response}
	ch <- provider.Event{Type: provider.EventBlockStop, Index: 0}
	close(ch)
	return ch, nil
}

type memResolver struct {
	data map[string][]byte
	err  error
}

func (r memResolver) ResolveImage(_ context.Context, path string) ([]byte, error) {
	if r.err != nil {
		return nil, r.err
	}
	if b, ok := r.data[path]; ok {
		return b, nil
	}
	return nil, &pathError{path: path}
}

type pathError struct{ path string }

func (e *pathError) Error() string { return "no such file: " + e.path }

func newViewImageTest(t *testing.T) (toolruntime.Tool, *fakeVisionAdapter) {
	t.Helper()
	fake := &fakeVisionAdapter{name: "fake-vision", response: "A red circle on a white background."}
	tool := NewViewImage(fake, "vision-pro", memResolver{data: map[string][]byte{"img/photo.webp": {0x1, 0x2, 0x3}}})
	return tool, fake
}

func TestViewImageReturnsDescription(t *testing.T) {
	tool, fake := newViewImageTest(t)
	res, err := tool.Call(context.Background(), map[string]any{"path": "img/photo.webp"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %q", res.Content)
	}
	if !strings.Contains(res.Content, "red circle") {
		t.Errorf("result = %q, want the vision model's description", res.Content)
	}
	// The image bytes were materialized into the request.
	img := findImageBlock(fake.lastReq)
	if img == nil {
		t.Fatal("request has no image block")
	}
	if img.ImageData == "" {
		t.Error("image block lacks materialized base64 data")
	}
	if fake.lastReq.Model != "vision-pro" {
		t.Errorf("request model = %q, want vision-pro", fake.lastReq.Model)
	}
}

func TestViewImageForwardsQuestion(t *testing.T) {
	tool, fake := newViewImageTest(t)
	_, err := tool.Call(context.Background(), map[string]any{
		"path":     "img/photo.webp",
		"question": "What color is the shape?",
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(fake.lastReq.Messages[0].Content[0].Text, "What color") {
		t.Errorf("question not forwarded; first text block = %q", fake.lastReq.Messages[0].Content[0].Text)
	}
}

func TestViewImageMissingPathFails(t *testing.T) {
	tool, _ := newViewImageTest(t)
	res, err := tool.Call(context.Background(), map[string]any{"path": "img/missing.webp"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !res.IsError {
		t.Errorf("missing image should be an error result, got %q", res.Content)
	}
}

func TestViewImageMissingPathArg(t *testing.T) {
	tool, _ := newViewImageTest(t)
	res, err := tool.Call(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !res.IsError {
		t.Errorf("missing path arg should be an error result")
	}
}

func TestViewImageVisionStreamError(t *testing.T) {
	fake := &fakeVisionAdapter{name: "fake-vision", response: "", err: context.DeadlineExceeded}
	tool := NewViewImage(fake, "vision-pro", memResolver{data: map[string][]byte{"a.webp": {1}}})
	res, err := tool.Call(context.Background(), map[string]any{"path": "a.webp"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !res.IsError {
		t.Errorf("vision stream failure should be an error result")
	}
}

func TestViewImageRiskReadOnlyAndTimeout(t *testing.T) {
	tool, _ := newViewImageTest(t)
	if r := tool.Risk(); r != toolruntime.RiskReadOnly {
		t.Errorf("risk = %q, want read_only", r)
	}
	if tool.Timeout() <= 0 {
		t.Errorf("timeout = %v, want a generous bound", tool.Timeout())
	}
	if time.Duration(0) == tool.Timeout() {
		t.Error("view_image must not have a zero (unbounded) timeout")
	}
}

func findImageBlock(req provider.Request) *provider.Block {
	for mi := range req.Messages {
		for bi := range req.Messages[mi].Content {
			b := &req.Messages[mi].Content[bi]
			if b.Type == provider.BlockImage {
				return b
			}
		}
	}
	return nil
}

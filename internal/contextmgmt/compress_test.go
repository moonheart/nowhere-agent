package contextmgmt

import (
	"context"
	"strings"
	"testing"

	"nowhere-agent/internal/provider"
)

// stubCompressor returns a fixed summary without any LLM.
type stubCompressor struct{ got []provider.Message }

func (s *stubCompressor) Summarize(_ context.Context, dropped []provider.Message) (string, error) {
	s.got = dropped
	return "SUMMARY", nil
}

func bigHistory(n, size int) []provider.Message {
	msgs := make([]provider.Message, n)
	for i := range msgs {
		msgs[i] = provider.TextMessage(provider.RoleUser, strings.Repeat("x", size))
	}
	return msgs
}

func TestShouldCompressUnderThreshold(t *testing.T) {
	p := Policy{MaxTokens: 10000, Threshold: 0.8, KeepRecent: 2}
	h := bigHistory(2, 10) // tiny
	if ShouldCompress(h, p) {
		t.Error("should not compress tiny history")
	}
}

func TestShouldCompressOverThreshold(t *testing.T) {
	p := Policy{MaxTokens: 10, Threshold: 0.8, KeepRecent: 1}
	h := bigHistory(5, 100) // large
	if !ShouldCompress(h, p) {
		t.Error("should compress large history")
	}
}

func TestCompressNoOpUnderThreshold(t *testing.T) {
	p := Policy{MaxTokens: 100000, Threshold: 0.8, KeepRecent: 2}
	h := bigHistory(3, 10)
	c := &stubCompressor{}
	out, err := Compress(context.Background(), h, p, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(h) {
		t.Errorf("expected unchanged history, got %d msgs", len(out))
	}
	if c.got != nil {
		t.Error("compressor should not be called under threshold")
	}
}

func TestCompressKeepsRecentAndSummarizes(t *testing.T) {
	// Budget fits the summary plus both kept rounds, so the post-compression
	// budget recheck does not trigger further dropping.
	p := Policy{MaxTokens: 100, Threshold: 0.8, KeepRecent: 2}
	h := bigHistory(6, 100)
	c := &stubCompressor{}
	out, err := Compress(context.Background(), h, p, c)
	if err != nil {
		t.Fatal(err)
	}
	// 1 summary + 2 recent
	if len(out) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(out))
	}
	if !strings.Contains(out[0].Content[0].Text, "SUMMARY") {
		t.Errorf("first message should be summary, got %q", out[0].Content[0].Text)
	}
	// recent two preserved verbatim (the big x-strings)
	if out[1].Content[0].Text != h[4].Content[0].Text {
		t.Errorf("recent message not preserved")
	}
	// compressor saw the dropped 4 messages
	if len(c.got) != 4 {
		t.Errorf("compressor got %d dropped, want 4", len(c.got))
	}
}

func TestCompressKeepRecentExceedsHistory(t *testing.T) {
	p := Policy{MaxTokens: 10, Threshold: 0.8, KeepRecent: 100}
	h := bigHistory(3, 100)
	c := &stubCompressor{}
	out, err := Compress(context.Background(), h, p, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(h) {
		t.Errorf("expected unchanged when keepRecent >= len, got %d", len(out))
	}
}

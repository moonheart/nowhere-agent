package contextmgmt

import (
	"context"
	"strings"
	"testing"

	"nowhere-agent/internal/provider"
)

// countingCompressor returns a fixed summary and records every input.
type countingCompressor struct {
	calls int
	got   [][]provider.Message
}

func (s *countingCompressor) Summarize(_ context.Context, dropped []provider.Message) (string, error) {
	s.calls++
	s.got = append(s.got, dropped)
	return "SUMMARY", nil
}

func TestEstimateTokensCountsToolInputValues(t *testing.T) {
	big := strings.Repeat("v", 10000)
	msgs := []provider.Message{{Role: provider.RoleAssistant, Content: []provider.Block{
		{Type: provider.BlockToolUse, ToolUseID: "t1", ToolName: "write_file", ToolInput: map[string]any{"path": "a.go", "content": big}},
	}}}
	est := estimateTokens(msgs)
	if est < 2000 { // 10k chars ≈ 2500 tokens; the old code counted ~8 bytes
		t.Errorf("estimateTokens = %d, tool input values must be counted (want >= 2000)", est)
	}
}

func TestEstimateTokensCountsImages(t *testing.T) {
	ref := []provider.Message{{Role: provider.RoleUser, Content: []provider.Block{
		{Type: provider.BlockImage, ImagePath: "img/p.png", MediaType: "image/png"},
	}}}
	if est := estimateTokens(ref); est < 900 { // imageRefBytes/4 = 1000
		t.Errorf("path-only image estimate = %d, want >= 900", est)
	}
	mat := []provider.Message{{Role: provider.RoleUser, Content: []provider.Block{
		{Type: provider.BlockImage, ImagePath: "img/p.png", ImageData: strings.Repeat("b", 8000)},
	}}}
	if est := estimateTokens(mat); est < 1900 { // 8000/4 = 2000
		t.Errorf("materialized image estimate = %d, want >= 1900", est)
	}
}

func TestCompressWithCacheReusesSummary(t *testing.T) {
	p := Policy{MaxTokens: 200, Threshold: 0.8, KeepRecent: 2}
	c := &countingCompressor{}
	cache := &CompressionCache{}

	h6 := bigHistory(6, 150) // est 225 > 160 → compress
	out1, err := CompressWithCache(context.Background(), h6, p, c, cache)
	if err != nil {
		t.Fatal(err)
	}
	if c.calls != 1 {
		t.Fatalf("calls = %d, want 1", c.calls)
	}
	if !IsSummary(out1[0]) {
		t.Fatal("first message should be the summary")
	}

	// Extend the view (a run appends produced messages). The cached summary
	// plus the longer tail still fits the budget → reuse, no new LLM call.
	h8 := append(append([]provider.Message{}, h6...), bigHistory(2, 150)...)
	out2, err := CompressWithCache(context.Background(), h8, p, c, cache)
	if err != nil {
		t.Fatal(err)
	}
	if c.calls != 1 {
		t.Errorf("calls = %d, want 1 (summary should be reused across iterations)", c.calls)
	}
	if len(out2) != 5 { // summary + 4 verbatim messages
		t.Errorf("reused view = %d msgs, want 5 (summary + growing tail)", len(out2))
	}
	if !IsSummary(out2[0]) || out2[0].Content[0].Text != out1[0].Content[0].Text {
		t.Error("reused view must carry the byte-identical summary prefix")
	}
}

func TestCompressWithCacheExtendsIncrementally(t *testing.T) {
	p := Policy{MaxTokens: 100, Threshold: 0.8, KeepRecent: 2}
	c := &countingCompressor{}
	cache := &CompressionCache{}

	h6 := bigHistory(6, 150)
	if _, err := CompressWithCache(context.Background(), h6, p, c, cache); err != nil {
		t.Fatal(err)
	}
	if c.calls != 1 {
		t.Fatalf("calls = %d, want 1", c.calls)
	}

	// Tail grew past the full budget: re-summarize, but incrementally — the
	// summarizer sees the previous summary plus only the newly dropped rounds,
	// never the whole dropped prefix again.
	h8 := append(append([]provider.Message{}, h6...), bigHistory(2, 150)...)
	out, err := CompressWithCache(context.Background(), h8, p, c, cache)
	if err != nil {
		t.Fatal(err)
	}
	if c.calls != 2 {
		t.Fatalf("calls = %d, want 2 (incremental extension)", c.calls)
	}
	got := c.got[1]
	if len(got) != 3 { // previous summary + 2 newly dropped messages
		t.Fatalf("incremental input = %d msgs, want 3 (summary + new rounds only)", len(got))
	}
	if !IsSummary(got[0]) {
		t.Error("incremental input must lead with the previous summary")
	}
	if !IsSummary(out[0]) {
		t.Error("output must still lead with the summary")
	}
}

func TestCompressPostCheckDropsOversizedKeptRounds(t *testing.T) {
	// KeepRecent rounds are themselves over budget: the summary alone cannot
	// fix that, so the post-check hard-drops oldest rounds — but stops at
	// summary + newest round rather than strip the only record of the past
	// (a residual oversize there is the overflow fallback's job).
	p := Policy{MaxTokens: 20, Threshold: 0.8, KeepRecent: 2}
	c := &countingCompressor{}
	h := bigHistory(6, 150) // est 225 > 16; each msg 150 bytes → 37 tokens
	out, err := CompressWithCache(context.Background(), h, p, c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Errorf("view = %d msgs, want 2 (summary + newest round)", len(out))
	}
	if !IsSummary(out[0]) {
		t.Error("hard-drop must preserve the summary")
	}
	if out[1].Content[0].Text != h[5].Content[0].Text {
		t.Error("the newest round must be kept")
	}
}

func TestCompressWithCacheNilCacheBehavesLikeCompress(t *testing.T) {
	p := Policy{MaxTokens: 10, Threshold: 0.8, KeepRecent: 2}
	c := &countingCompressor{}
	h := bigHistory(6, 100)
	out, err := CompressWithCache(context.Background(), h, p, c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.calls != 1 || len(out) == 0 {
		t.Errorf("nil cache should still compress: calls=%d out=%d", c.calls, len(out))
	}
}

func TestStripAnalysisAnchored(t *testing.T) {
	cases := []struct{ in, want string }{
		{"<analysis>scratch</analysis><summary>real</summary>", "real"},
		{"<summary>the gist</summary>", "the gist"},
		{"plain summary", "plain summary"},
		// Literal tag strings inside the body are not wrapper tags.
		{"body with </analysis> literal", "body with </analysis> literal"},
		{"<analysis>a</analysis>body </analysis> tail", "body </analysis> tail"},
		{"<summary>has </summary> inside</summary>", "has </summary> inside"},
	}
	for _, tc := range cases {
		if got := stripAnalysis(tc.in); got != tc.want {
			t.Errorf("stripAnalysis(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

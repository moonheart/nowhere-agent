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
	// never the whole dropped prefix again. (The first pass's post-check
	// hard-dropped one kept round and advanced the cache past it, so that
	// round is neither resurrected nor re-summarized.)
	h8 := append(append([]provider.Message{}, h6...), bigHistory(2, 150)...)
	out, err := CompressWithCache(context.Background(), h8, p, c, cache)
	if err != nil {
		t.Fatal(err)
	}
	if c.calls != 2 {
		t.Fatalf("calls = %d, want 2 (incremental extension)", c.calls)
	}
	got := c.got[1]
	if len(got) != 2 { // previous summary + 1 newly dropped message
		t.Fatalf("incremental input = %d msgs, want 2 (summary + new rounds only)", len(got))
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
	// fix that, so the post-check hard-drops oldest rounds — down to the
	// summary alone. Handing an over-budget view on would make the overflow
	// fallback take the summary (the only record of the past), forcing a full
	// re-summarize next iteration that overflows again.
	p := Policy{MaxTokens: 20, Threshold: 0.8, KeepRecent: 2}
	c := &countingCompressor{}
	h := bigHistory(6, 150) // est 225 > 16; each msg 150 bytes → 37 tokens
	out, err := CompressWithCache(context.Background(), h, p, c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Errorf("view = %d msgs, want 1 (summary alone)", len(out))
	}
	if !IsSummary(out[0]) {
		t.Error("hard-drop must preserve the summary")
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

// TestCompressWithCacheAdvancesPastHardDroppedRounds pins the cache/hard-drop
// interaction: when the post-check hard-drops a kept round, the cache must
// advance past it, or the next iteration's hysteresis rebuild (summary +
// everything appended since, from durable history) would resurrect the
// dropped round verbatim.
func TestCompressWithCacheAdvancesPastHardDroppedRounds(t *testing.T) {
	p := Policy{MaxTokens: 100, Threshold: 0.8, KeepRecent: 2}
	c := &countingCompressor{}
	cache := &CompressionCache{}

	// est 240 > 80 → compress. summary+kept = ~90 > 80 → post-check drops one
	// kept round; summary+newest = ~50 fits.
	h6 := bigHistory(6, 160)
	out1, err := CompressWithCache(context.Background(), h6, p, c, cache)
	if err != nil {
		t.Fatal(err)
	}
	if len(out1) != 2 {
		t.Fatalf("first view = %d msgs, want 2 (summary + newest round after hard-drop)", len(out1))
	}

	// The view grows by one small message. Hysteresis reuse must NOT bring the
	// hard-dropped round back: the candidate is summary + the 2 messages after
	// the advanced coverage, not summary + 3.
	h7 := append(append([]provider.Message{}, h6...), bigHistory(1, 40)...)
	out2, err := CompressWithCache(context.Background(), h7, p, c, cache)
	if err != nil {
		t.Fatal(err)
	}
	if c.calls != 1 {
		t.Fatalf("calls = %d, want 1 (hysteresis reuse, no re-summarize)", c.calls)
	}
	if len(out2) != 3 {
		t.Fatalf("reused view = %d msgs, want 3 (hard-dropped round must stay dropped)", len(out2))
	}
	if out2[1].Content[0].Text != h7[5].Content[0].Text {
		t.Error("first kept message must be the round that survived the hard-drop")
	}
}

// TestCompressWithCacheReusesSummaryWhenPrefixUnchanged: the cache covers the
// whole split region and only the kept tail grew past the budget. The summary
// must be reused without a summarizer call — re-summarizing the byte-identical
// prefix would cost O(total) for the same text; the post-check drops from the
// tail instead.
func TestCompressWithCacheReusesSummaryWhenPrefixUnchanged(t *testing.T) {
	p := Policy{MaxTokens: 100, Threshold: 0.8, KeepRecent: 2}
	c := &countingCompressor{}
	h := bigHistory(6, 400) // est 600 > 80 → compress; splitIdx = 4
	cache := &CompressionCache{
		Covered:      4,
		CoveredBytes: contentBytes(h[:4]),
		Summary:      "CACHED",
	}
	out, err := CompressWithCache(context.Background(), h, p, c, cache)
	if err != nil {
		t.Fatal(err)
	}
	if c.calls != 0 {
		t.Errorf("summarizer called %d times for an unchanged prefix, want 0", c.calls)
	}
	if !IsSummary(out[0]) || !strings.Contains(out[0].Content[0].Text, "CACHED") {
		t.Error("view must lead with the reused cached summary")
	}
	// The kept tail (2 msgs of 400 bytes ≈ 200 tokens) busts the budget alone,
	// so the post-check hard-drops down to the summary.
	if len(out) != 1 {
		t.Errorf("view = %d msgs, want 1 (summary alone after tail hard-drop)", len(out))
	}
}

func TestCompressionCacheAdvance(t *testing.T) {
	h := bigHistory(6, 100)
	cache := &CompressionCache{Covered: 2, CoveredBytes: contentBytes(h[:2]), Summary: "S"}
	cache.Advance(h[2:4])
	if cache.Covered != 4 {
		t.Errorf("Covered = %d, want 4", cache.Covered)
	}
	if cache.CoveredBytes != contentBytes(h[:4]) {
		t.Error("CoveredBytes must stay aligned with history[:Covered] after Advance")
	}
	cache.Invalidate()
	if cache.Covered != 0 || cache.CoveredBytes != 0 || cache.Summary != "" {
		t.Error("Invalidate must reset the cache")
	}
}

// TestEstimateOverheadCountsSystemAndTools: the fixed request envelope —
// system prompt and tool schemas — must be estimable so the compression
// budget can subtract it; without it the trigger ignores everything outside
// the message view.
func TestEstimateOverheadCountsSystemAndTools(t *testing.T) {
	if got := EstimateOverhead("", nil); got != 0 {
		t.Errorf("empty overhead = %d, want 0", got)
	}
	sys := strings.Repeat("s", 4000)
	tools := []provider.ToolDefinition{{
		Name:        "write_file",
		Description: strings.Repeat("d", 400),
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}},
	}}
	got := EstimateOverhead(sys, tools)
	if got < 1100 { // 4000+400+schema bytes ≈ 4500+ chars ≈ 1100+ tokens
		t.Errorf("overhead = %d, want >= 1100 (system + tool schema must count)", got)
	}
}

func TestEstimateTokensCountsCJKRunes(t *testing.T) {
	msgs := []provider.Message{provider.TextMessage(provider.RoleUser, strings.Repeat("汉", 100))}
	est := estimateTokens(msgs)
	if est < 90 { // 100 CJK runes ≈ 100 tokens; flat bytes/4 would give 75
		t.Errorf("CJK estimate = %d, want ≈100 (bytes/4 under-reads CJK)", est)
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

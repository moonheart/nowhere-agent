package observability

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// scrape gathers the registry's text exposition into one string.
func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

func TestRecordRunIncrementsOutcome(t *testing.T) {
	m := NewMetrics()
	m.RecordRun("done")
	m.RecordRun("done")
	m.RecordRun("failed")

	out := scrape(t, m)
	if got := strings.Count(out, `nowhere_runs_total{outcome="done"} 2`); got != 1 {
		t.Errorf("done count not present in %q", out)
	}
	if !strings.Contains(out, `nowhere_runs_total{outcome="failed"} 1`) {
		t.Errorf("failed count missing in %q", out)
	}
}

func TestRecordTokensLabelsAndSkips(t *testing.T) {
	m := NewMetrics()
	// Non-positive values must not create series.
	m.RecordTokens("deepseek", "deepseek-chat", "input", 0)
	m.RecordTokens("deepseek", "deepseek-chat", "output", -5)
	// Positive values land with the exact provider/model/direction triple.
	m.RecordTokens("deepseek", "deepseek-chat", "input", 1200)
	m.RecordTokens("deepseek", "deepseek-chat", "output", 340)
	m.RecordTokens("qwen", "qwen3-max", "cache_read", 9000)

	out := scrape(t, m)
	for _, want := range []string{
		`nowhere_llm_tokens_total{direction="input",model="deepseek-chat",provider="deepseek"} 1200`,
		`nowhere_llm_tokens_total{direction="output",model="deepseek-chat",provider="deepseek"} 340`,
		`nowhere_llm_tokens_total{direction="cache_read",model="qwen3-max",provider="qwen"} 9000`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing series %q in %q", want, out)
		}
	}
	// The skipped zero/negative calls must not have materialized series.
	for _, absent := range []string{
		`direction="input",model="deepseek-chat",provider="deepseek"} 0`,
		`direction="output",model="deepseek-chat",provider="deepseek"} 0`,
	} {
		if strings.Contains(out, absent) {
			t.Errorf("unexpected zero series %q in %q", absent, out)
		}
	}
}

func TestMetricsIndependentRegistries(t *testing.T) {
	// Two Metrics instances never collide on duplicate registration (each owns
	// its registry) — the property that lets tests construct freely.
	m1, m2 := NewMetrics(), NewMetrics()
	m1.RecordRun("done")
	m2.RecordRun("done")
	if !strings.Contains(scrape(t, m1), `nowhere_runs_total{outcome="done"} 1`) {
		t.Fatal("m1 lost its counter")
	}
	if !strings.Contains(scrape(t, m2), `nowhere_runs_total{outcome="done"} 1`) {
		t.Fatal("m2 lost its counter")
	}
}

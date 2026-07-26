package chatapi

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/provider"
)

// TestSSEEmitterFinishReportsUsage pins L2: a KindUsage event stashes real token
// counts that finish() reports, replacing the hardcoded zeros. httptest's
// recorder is both an http.ResponseWriter and an http.Flusher.
func TestSSEEmitterFinishReportsUsage(t *testing.T) {
	rec := httptest.NewRecorder()
	e := &sseEmitter{w: rec, flusher: rec, msgID: "m", textID: "t", thinkID: "r"}

	// Direct-path shape: the loop emits a provider.Usage value.
	if err := e.Emit(context.Background(), agent.KindUsage, provider.Usage{InputTokens: 11, OutputTokens: 7}); err != nil {
		t.Fatal(err)
	}
	e.finish()

	body := rec.Body.String()
	if !strings.Contains(body, `"inputTokens":11`) || !strings.Contains(body, `"outputTokens":7`) {
		t.Errorf("finish frame missing real usage:\n%s", body)
	}
}

// TestSSEEmitterFinishDefaultsToZero verifies a run that reported no usage still
// emits a well-formed finish frame (the pre-L2 behaviour, now the fallback).
func TestSSEEmitterFinishDefaultsToZero(t *testing.T) {
	rec := httptest.NewRecorder()
	e := &sseEmitter{w: rec, flusher: rec, msgID: "m", textID: "t", thinkID: "r"}
	e.finish()
	if !strings.Contains(rec.Body.String(), `"inputTokens":0`) {
		t.Errorf("finish frame missing zero-usage fallback:\n%s", rec.Body.String())
	}
}

// TestSSEEmitterEmitsDataUsageFrame pins that a KindUsage event with cache
// counts writes a data-usage frame carrying the full breakdown, so the client
// can render token/cache detail the finish frame doesn't carry.
func TestSSEEmitterEmitsDataUsageFrame(t *testing.T) {
	rec := httptest.NewRecorder()
	e := &sseEmitter{w: rec, flusher: rec, msgID: "m", textID: "t", thinkID: "r"}

	if err := e.Emit(context.Background(), agent.KindUsage, provider.Usage{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 80}); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	for _, want := range []string{`"type":"data-usage"`, `"inputTokens":100`, `"cacheReadTokens":80`, `"cacheWriteTokens":0`} {
		if !strings.Contains(body, want) {
			t.Errorf("data-usage frame missing %s:\n%s", want, body)
		}
	}
}

// TestUsageTokensBrokerMapPath verifies usage decoded from storage (snake_case
// JSON object with float64 numbers — the broker/replay path) is extracted, and
// that a payload without token keys reports ok=false so it never clobbers a
// prior value with zeros.
func TestUsageTokensBrokerMapPath(t *testing.T) {
	u, ok := usageTokens(map[string]any{"input_tokens": float64(30), "output_tokens": float64(9), "cache_read_tokens": float64(25)})
	if !ok || u.InputTokens != 30 || u.OutputTokens != 9 || u.CacheReadTokens != 25 {
		t.Errorf("usageTokens map path = (%+v,%v), want ({30 9 25},true)", u, ok)
	}
	if _, ok := usageTokens(map[string]any{"nope": float64(1)}); ok {
		t.Error("a payload without token keys must return ok=false")
	}
	if _, ok := usageTokens(provider.Usage{InputTokens: 1}); !ok {
		t.Error("a provider.Usage value must be recognised")
	}
}

package chatapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/session"
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

// TestEmitStreamEventUsageFromBroker pins the live-delivery path that was broken:
// a usage frame carried by the StreamBroker (JSON-encoded like every content
// event) flows through emitStreamEvent into the emitter, stashing real counts for
// finish() and writing the data-usage frame. Before KindUsage classified as a
// content kind it never reached the broker, so live streams showed usage:0 and
// only a history reload surfaced the real counts.
func TestEmitStreamEventUsageFromBroker(t *testing.T) {
	rec := httptest.NewRecorder()
	e := &sseEmitter{w: rec, flusher: rec, msgID: "m", textID: "t", thinkID: "r"}

	// Encode the payload exactly as the broker carries it — the provider.Usage
	// marshalled to its snake_case JSON form, the same bytes the registry publishes
	// for a content event — then hand it to emitStreamEvent as the broker would.
	payload, err := json.Marshal(provider.Usage{InputTokens: 14910, OutputTokens: 283, CacheReadTokens: 13200})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/", nil)
	emitStreamEvent(req, e, session.StreamEvent{RunID: "r", Kind: string(agent.KindUsage), Payload: payload})

	e.finish()
	body := rec.Body.String()
	if !strings.Contains(body, `"inputTokens":14910`) || !strings.Contains(body, `"outputTokens":283`) {
		t.Errorf("finish frame missing broker-delivered usage:\n%s", body)
	}
	if !strings.Contains(body, `"type":"data-usage"`) || !strings.Contains(body, `"cacheReadTokens":13200`) {
		t.Errorf("data-usage frame missing broker-delivered usage:\n%s", body)
	}
}

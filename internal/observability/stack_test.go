package observability

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// rejectAll is a limiter-shaped middleware that throttles every request.
func rejectAll(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
	})
}

// passAll is a limiter-shaped middleware that lets every request through.
func passAll(next http.Handler) http.Handler { return next }

// TestStandardStackOrder pins the HTTP stack's order invariants behaviorally
// (the analogue of the agent loop's middleware order test), so a re-nesting at
// the assembly point breaks this test, not production:
//
//   - RequestID outermost: even a throttled request carries an id, so floods
//     stay traceable.
//   - AccessLog outside the limiter: a rejected request still leaves one log
//     line (429), carrying the request id.
//   - limiter before Metrics: rejected requests never reach the registry, so a
//     flood cannot churn series.
//   - Metrics outside Recovery: the 500 a recovered panic answers is counted.
func TestStandardStackOrder(t *testing.T) {
	t.Run("rejected request is traced and logged but not metered", func(t *testing.T) {
		var buf bytes.Buffer
		log := slog.New(slog.NewTextHandler(&buf, nil))
		m := NewMetrics()
		h := StandardStack(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("handler reached through a rejecting limiter")
		}), log, m, rejectAll)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/chat", nil)
		req.Pattern = "GET /api/chat"
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("code = %d, want 429", rec.Code)
		}
		// RequestID outermost: throttled requests still get a correlation id.
		if rec.Header().Get(RequestIDHeader) == "" {
			t.Error("rejected request lost its X-Request-Id — RequestID must be outermost")
		}
		// AccessLog outside the limiter: one 429 line, carrying the request id.
		out := buf.String()
		if !strings.Contains(out, "status=429") {
			t.Errorf("access log missing the rejected request:\n%s", out)
		}
		if !strings.Contains(out, "request_id=") {
			t.Errorf("access log line missing request_id — AccessLog must sit inside RequestID:\n%s", out)
		}
		// Limiter before Metrics: nothing was counted for the rejected flood.
		body := scrape(t, m)
		if strings.Contains(body, `nowhere_http_requests_total{method="GET",route="GET /api/chat"`) {
			t.Errorf("rejected request churned a metric series — limiter must sit before Metrics:\n%s", body)
		}
	})

	t.Run("recovered panic is metered as 500", func(t *testing.T) {
		m := NewMetrics()
		h := StandardStack(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			panic("boom")
		}), slog.New(slog.NewTextHandler(io.Discard, nil)), m, passAll)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/chat", nil)
		req.Pattern = "GET /api/chat"
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("code = %d, want 500", rec.Code)
		}
		body := scrape(t, m)
		if !strings.Contains(body, `nowhere_http_requests_total{method="GET",route="GET /api/chat",status="500"} 1`) {
			t.Errorf("panic 500 not counted — Recovery must sit inside Metrics:\n%s", body)
		}
	})

	t.Run("healthy request flows through every layer", func(t *testing.T) {
		var buf bytes.Buffer
		log := slog.New(slog.NewTextHandler(&buf, nil))
		m := NewMetrics()
		h := StandardStack(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if FromContext(r.Context()) == "" {
				t.Error("handler missing request id from context")
			}
			w.WriteHeader(http.StatusNoContent)
		}), log, m, passAll)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/chat", nil)
		req.Pattern = "GET /api/chat"
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusNoContent {
			t.Fatalf("code = %d, want 204", rec.Code)
		}
		if !strings.Contains(buf.String(), "status=204") {
			t.Errorf("access log missing the completed request:\n%s", buf.String())
		}
		if !strings.Contains(scrape(t, m), `status="204"`) {
			t.Error("healthy request not metered")
		}
		// SecurityHeaders: every response carries the baseline headers.
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
		}
		if got := rec.Header().Get("Referrer-Policy"); got == "" {
			t.Error("Referrer-Policy missing")
		}
		if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
			t.Errorf("X-Frame-Options = %q, want DENY", got)
		}
	})

	t.Run("security headers reach rejected responses", func(t *testing.T) {
		h := StandardStack(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("handler reached through a rejecting limiter")
		}), slog.New(slog.NewTextHandler(io.Discard, nil)), NewMetrics(), rejectAll)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/chat", nil)
		req.Pattern = "GET /api/chat"
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("code = %d, want 429", rec.Code)
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("rejected response X-Content-Type-Options = %q, want nosniff — SecurityHeaders must sit outside the limiter", got)
		}
	})
}

package observability

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRequestIDGeneratesAndEchoes(t *testing.T) {
	var gotID string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotID = FromContext(r.Context())
	})
	h := RequestID(slog.New(slog.NewTextHandler(io.Discard, nil)))(next)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	resp := rec.Result()
	id := resp.Header.Get(RequestIDHeader)
	if id == "" {
		t.Fatal("no X-Request-Id on response")
	}
	if len(id) != 32 {
		t.Errorf("generated id = %q, want 32 hex chars", id)
	}
	if gotID != id {
		t.Errorf("context id %q != response id %q", gotID, id)
	}
}

func TestRequestIDAdoptsIncoming(t *testing.T) {
	var gotID string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotID = FromContext(r.Context())
	})
	h := RequestID(slog.New(slog.NewTextHandler(io.Discard, nil)))(next)

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set(RequestIDHeader, "trace-from-proxy-123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if gotID != "trace-from-proxy-123" {
		t.Errorf("did not adopt incoming id, got %q", gotID)
	}
	if rec.Result().Header.Get(RequestIDHeader) != "trace-from-proxy-123" {
		t.Error("did not echo adopted id")
	}
}

func TestRequestIDRejectsMalformedIncoming(t *testing.T) {
	var gotID string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotID = FromContext(r.Context())
	})
	h := RequestID(slog.New(slog.NewTextHandler(io.Discard, nil)))(next)

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set(RequestIDHeader, "bad\nid\rwith\x00controls")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if strings.ContainsAny(gotID, "\n\r\x00") {
		t.Errorf("adopted a control-character id: %q", gotID)
	}
	if len(gotID) != 32 {
		t.Errorf("should have regenerated a clean id, got %q", gotID)
	}
}

func TestLoggerFromContextCarriesID(t *testing.T) {
	var reqLog *slog.Logger
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqLog = LoggerFromContext(r.Context())
	})
	h := RequestID(slog.New(slog.NewTextHandler(io.Discard, nil)))(next)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	if reqLog == nil {
		t.Fatal("no request logger injected")
	}
}

func TestMetricsMiddlewareCountsByRoute(t *testing.T) {
	m := NewMetrics()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	// Simulate ServeMux setting r.Pattern.
	h := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/users/42", nil)
	req.Pattern = "GET /api/users/{id}"
	h.ServeHTTP(httptest.NewRecorder(), req)

	body := scrape(t, m)
	if !strings.Contains(body, `nowhere_http_requests_total{method="GET",route="GET /api/users/{id}",status="418"} 1`) {
		t.Errorf("request counter missing/bad in exposition:\n%s", body)
	}
	if !strings.Contains(body, "nowhere_http_request_duration_seconds_count") {
		t.Error("latency histogram missing")
	}
}

func TestMetricsDefaultStatus200(t *testing.T) {
	m := NewMetrics()
	h := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hi"))
	}))
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Pattern = "GET /healthz"
	h.ServeHTTP(httptest.NewRecorder(), req)

	body := scrape(t, m)
	if !strings.Contains(body, `status="200"`) {
		t.Errorf("implicit 200 not recorded:\n%s", body)
	}
}

func TestMetricsUnmatchedRouteCollapsed(t *testing.T) {
	m := NewMetrics()
	h := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	// No Pattern set (e.g. a 404 from the mux itself).
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/nope", nil))
	body := scrape(t, m)
	if !strings.Contains(body, `route="unmatched"`) {
		t.Errorf("unmatched route not collapsed:\n%s", body)
	}
}

// TestMetricsPreservesFlusher pins the SSE regression: the status recorder must
// satisfy http.Flusher, because streaming endpoints assert it directly (a type
// assertion does not traverse Unwrap) and 500 "streaming unsupported" AFTER the
// run already started server-side.
func TestMetricsPreservesFlusher(t *testing.T) {
	var flushed bool
	h := NewMetrics().Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Error("wrapped writer lost http.Flusher")
			return
		}
		f.Flush()
		flushed = true
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	if !flushed {
		t.Error("flush through the recorder never reached the underlying writer")
	}
}

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	b, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestHealthzAllUp(t *testing.T) {
	h := NewHealthz(time.Second)
	h.Add("db", func(ctx context.Context) error { return nil })
	h.Add("redis", func(ctx context.Context) error { return nil })
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("all-up health = %d, want 200", rec.Code)
	}
}

func TestHealthzOneDown(t *testing.T) {
	h := NewHealthz(time.Second)
	h.Add("db", func(ctx context.Context) error { return errors.New("conn refused") })
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("down health = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "db") {
		t.Errorf("failing dependency not named: %q", rec.Body.String())
	}
}

func TestHealthzProbeTimeout(t *testing.T) {
	h := NewHealthz(50 * time.Millisecond)
	h.Add("slow", func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	start := time.Now()
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("timed-out probe should be unhealthy, got %d", rec.Code)
	}
	if time.Since(start) > time.Second {
		t.Error("hanging probe stalled the health check past the timeout")
	}
}

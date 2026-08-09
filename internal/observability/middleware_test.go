package observability

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChainOrderOutermostFirst(t *testing.T) {
	var order []string
	mw := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		order = append(order, "handler")
	}), mw("a"), mw("b"), mw("c"))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	got := strings.Join(order, ",")
	if got != "a,b,c,handler" {
		t.Errorf("chain order = %s, want a,b,c,handler", got)
	}
}

func TestWithLoggerRoundTrip(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := WithLogger(context.Background(), log)
	if LoggerFromContext(ctx) != log {
		t.Error("WithLogger value not retrievable via LoggerFromContext")
	}
	if LoggerFromContext(context.Background()) != nil {
		t.Error("empty context should yield nil logger")
	}
}

func TestRecoveryWrites500OnPanic(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	h := RequestID(log)(Recovery(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("panic response = %d, want 500", rec.Code)
	}
	out := buf.String()
	if !strings.Contains(out, "http handler panicked") || !strings.Contains(out, "boom") {
		t.Errorf("panic not logged with value: %s", out)
	}
	if !strings.Contains(out, "request_id=") {
		t.Errorf("panic log lost request correlation: %s", out)
	}
}

// TestRecoveryAfterStatusCommitted pins the SSE case: the stream already
// started, so no 500 can be written — Recovery logs and severs instead. The
// test composes metrics outside recovery exactly as production wiring does,
// because Written() is reported by the metrics status recorder.
func TestRecoveryAfterStatusCommitted(t *testing.T) {
	h := NewMetrics().Middleware(Recovery(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: partial\n\n"))
		panic("mid-stream boom")
	})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("committed status rewritten to %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "internal server error") {
		t.Error("error body appended to an already-streaming response")
	}
}

func TestRecoveryCounts500InMetrics(t *testing.T) {
	m := NewMetrics()
	h := m.Middleware(Recovery(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Pattern = "GET /x"
	h.ServeHTTP(httptest.NewRecorder(), req)
	body := scrape(t, m)
	if !strings.Contains(body, `status="500"`) {
		t.Errorf("recovered panic not counted as 500:\n%s", body)
	}
}

func TestAccessLogEmitsCompletionFields(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	h := RequestID(log)(AccessLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("hello"))
	})))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Pattern = "GET /x"
	h.ServeHTTP(httptest.NewRecorder(), req)
	out := buf.String()
	for _, want := range []string{"http request", "status=201", "bytes=5", "latency_ms=", "ttfb_ms=", `route="GET /x"`} {
		if !strings.Contains(out, want) {
			t.Errorf("access log missing %q:\n%s", want, out)
		}
	}
}

// TestAccessLogClientDisconnect499 simulates an SSE client going away: the
// request context is cancelled before the handler returns, so the exchange is
// logged as 499, not the 200 the stream started with.
func TestAccessLogClientDisconnect499(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	h := RequestID(log)(AccessLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: frame\n\n"))
	})))
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/stream", nil).WithContext(ctx)
	req.Pattern = "GET /stream"
	cancel() // client gone before the handler returns
	h.ServeHTTP(httptest.NewRecorder(), req)
	out := buf.String()
	if !strings.Contains(out, "status=499") {
		t.Errorf("disconnect not logged as 499:\n%s", out)
	}
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("499 should log at warn level:\n%s", out)
	}
}

func TestMetricsClientDisconnect499(t *testing.T) {
	m := NewMetrics()
	h := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/stream", nil).WithContext(ctx)
	req.Pattern = "GET /stream"
	cancel()
	h.ServeHTTP(httptest.NewRecorder(), req)
	body := scrape(t, m)
	if !strings.Contains(body, `status="499"`) {
		t.Errorf("disconnected stream counted as success:\n%s", body)
	}
}

func TestAccessLogPreservesFlusher(t *testing.T) {
	h := AccessLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Error("access recorder lost http.Flusher")
			return
		}
		f.Flush()
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
}

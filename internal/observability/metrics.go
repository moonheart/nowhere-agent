// Package observability provides the platform's operational surface:
// Prometheus metrics, request-id correlation, and a real health probe.
//
// Three pieces, all small and composable:
//
//   - Metrics wraps an http.Handler and records per-route request counts and
//     latency histograms into a Prometheus Registry, served via Handler.
//   - RequestID assigns (or adopts) a correlation id per request, sets the
//     X-Request-Id response header, and injects it into a request-scoped slog
//     logger so every log line in the handler chain carries it.
//   - Healthz reports liveness only when every dependency probe succeeds, so an
//     orchestrator can tell "process up but database dead" apart from "healthy".
//
// Metrics cardinality is deliberately bounded: the route label comes from the
// ServeMux pattern (r.Pattern), not the raw URL path, so /api/users/{id}
// collapses to one series no matter how many ids exist. This is what keeps the
// /metrics payload small and scrapes cheap.
package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// RequestIDHeader is the header clients may send to correlate a request and
// that every response echoes back. Standard reverse-proxy convention.
const RequestIDHeader = "X-Request-Id"

type ctxKey string

const requestIDKey ctxKey = "request-id"
const loggerKey ctxKey = "logger"

// Metrics records HTTP request metrics and exposes the registry they live in.
type Metrics struct {
	reg       *prometheus.Registry
	requests  *prometheus.CounterVec
	latency   *prometheus.HistogramVec
	inflight  prometheus.Gauge
	runsTotal *prometheus.CounterVec
	tokens    *prometheus.CounterVec
}

// NewMetrics builds a Metrics with its own registry (not the global default),
// so tests and multiple constructions never collide on duplicate registration.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	f := promauto.With(reg)
	return &Metrics{
		reg: reg,
		requests: f.NewCounterVec(prometheus.CounterOpts{
			Name: "nowhere_http_requests_total",
			Help: "HTTP requests handled, by route, method, and status code.",
		}, []string{"route", "method", "status"}),
		latency: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "nowhere_http_request_duration_seconds",
			Help:    "HTTP request latency, by route and method.",
			Buckets: prometheus.DefBuckets,
		}, []string{"route", "method"}),
		inflight: f.NewGauge(prometheus.GaugeOpts{
			Name: "nowhere_http_inflight_requests",
			Help: "HTTP requests currently being served.",
		}),
		runsTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name: "nowhere_runs_total",
			Help: "Agent runs by terminal outcome.",
		}, []string{"outcome"}),
		tokens: f.NewCounterVec(prometheus.CounterOpts{
			Name: "nowhere_llm_tokens_total",
			Help: "LLM tokens consumed, by provider, model, and direction.",
		}, []string{"provider", "model", "direction"}),
	}
}

// Handler serves the registry in Prometheus text exposition format. Mount it at
// /metrics; Prometheus (or VictoriaMetrics, common in 中国企业自建监控) scrapes it.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// Middleware instruments the wrapped handler. It is the outermost wrapper so it
// sees the final status code and full latency. The route label is r.Pattern —
// the ServeMux pattern — which is only populated on Go 1.22+, matching the rest
// of the server.
func (m *Metrics) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.inflight.Inc()
		defer m.inflight.Dec()
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		status := itoa(rec.status)
		m.requests.WithLabelValues(route, r.Method, status).Inc()
		m.latency.WithLabelValues(route, r.Method).Observe(time.Since(start).Seconds())
	})
}

// statusRecorder captures the status code that the handler writes; the default
// is 200 (an implicit WriteHeader(200) on first Write).
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the underlying writer (for SSE
// flushing), which the middleware would otherwise hide.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// RequestID adopts the incoming X-Request-Id if present and well-formed, else
// generates one. It echoes the id on the response and injects both the id and a
// request-scoped logger (carrying request_id, method, path) into the context, so
// downstream code logs with correlation for free.
func RequestID(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(RequestIDHeader)
			if !validID(id) {
				id = newID()
			}
			w.Header().Set(RequestIDHeader, id)
			reqLog := log.With("request_id", id, "method", r.Method, "path", r.URL.Path)
			ctx := context.WithValue(r.Context(), requestIDKey, id)
			ctx = context.WithValue(ctx, loggerKey, reqLog)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// FromContext returns the request id on the context, or "" if none (e.g. a
// background worker context that never passed through the middleware).
func FromContext(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

// LoggerFromContext returns the request-scoped logger injected by RequestID, or
// nil if the context did not come through the middleware. Callers fall back to
// the package-level slog default so they can log unconditionally.
func LoggerFromContext(ctx context.Context) *slog.Logger {
	if v, ok := ctx.Value(loggerKey).(*slog.Logger); ok {
		return v
	}
	return nil
}

// validID accepts a client-supplied id only if it is short and printable, so a
// caller cannot inject control characters into our logs via the header.
func validID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}

// newID returns a random 128-bit hex id. crypto/rand, not math/rand: ids must
// not be guessable, and the cost is negligible at one per request.
func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// itoa renders a small int without importing strconv for one call site.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

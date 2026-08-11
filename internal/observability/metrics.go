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

	"nowhere-agent/internal/reqctx"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// RequestIDHeader is the header clients may send to correlate a request and
// that every response echoes back. Standard reverse-proxy convention.
const RequestIDHeader = "X-Request-Id"

// Metrics records HTTP request metrics and exposes the registry they live in.
type Metrics struct {
	reg       *prometheus.Registry
	requests  *prometheus.CounterVec
	latency   *prometheus.HistogramVec
	ttfb      *prometheus.HistogramVec
	inflight  prometheus.Gauge
	runsTotal *prometheus.CounterVec
	tokens    *prometheus.CounterVec
	webhooks  *prometheus.CounterVec
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
		ttfb: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "nowhere_http_ttfb_seconds",
			Help:    "HTTP time-to-first-byte, by route and method (high value for SSE streams).",
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
		webhooks: f.NewCounterVec(prometheus.CounterOpts{
			Name: "nowhere_webhook_deliveries_total",
			Help: "Outbound run-completion deliveries by outcome (delivered, failed, dead_lettered).",
		}, []string{"outcome"}),
	}
}

// Handler serves the registry in Prometheus text exposition format. Mount it at
// /metrics; Prometheus (or VictoriaMetrics, common in 中国企业自建监控) scrapes it.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// Register adds a collector to the registry (e.g. a GaugeFunc over a
// subsystem's internal counters), so components outside this package can
// expose their own series without reaching for the global registry.
func (m *Metrics) Register(c prometheus.Collector) error {
	return m.reg.Register(c)
}

// RecordRun increments the run counter for a terminal outcome. The server
// wires it to the shared RunRegistry's run-done hook, so every run — chat,
// scheduled, inbound-triggered — is counted exactly once at settlement.
func (m *Metrics) RecordRun(outcome string) {
	m.runsTotal.WithLabelValues(outcome).Inc()
}

// RecordTokens increments the LLM token counter for a provider/model/direction
// triple. The server wires it to the loop's per-run usage observer (the same
// aggregate the runs row records), so token spend is visible in Prometheus
// the moment a run finishes.
func (m *Metrics) RecordTokens(provider, model, direction string, n int) {
	if n <= 0 {
		return
	}
	m.tokens.WithLabelValues(provider, model, direction).Add(float64(n))
}

// RecordWebhookDelivery counts an outbound run-completion delivery outcome:
// "delivered" | "failed" | "dead_lettered" (or "rejected" for permanent 4xx).
// The server wires it to the outbox hook and sweeper, so SRE dashboards can
// monitor the integration link's success rate.
func (m *Metrics) RecordWebhookDelivery(outcome string) {
	m.webhooks.WithLabelValues(outcome).Inc()
}
// Middleware instruments the wrapped handler. In the StandardStack it sits
// INSIDE the rate limiter — rejected floods never reach it, so a throttle
// cannot churn metric series — and outside Recovery, so the 500 a recovered
// panic produces is counted like any other status. The route label is
// r.Pattern — the ServeMux pattern — which is only populated on Go 1.22+,
// matching the rest of the server. A client that disconnected before the
// handler returned (the normal end of an SSE stream) is counted as 499, not
// 200, so dashboards do not read dropped streams as successful responses. Uses
// the shared statusWriter so every recorder in the stack implements the same
// Write/Flush/Unwrap contract.
func (m *Metrics) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.inflight.Inc()
		defer m.inflight.Dec()
		start := time.Now()
		rec := newStatusWriter(w, start)
		next.ServeHTTP(rec, r)

		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		status := rec.status
		if clientGone(r) {
			status = StatusClientClosed
		}
		m.requests.WithLabelValues(route, r.Method, itoa(status)).Inc()
		m.latency.WithLabelValues(route, r.Method).Observe(time.Since(start).Seconds())
		// TTFB is only meaningful once a status was committed (headers flushed);
		// a request aborted before the handler wrote anything contributes nothing.
		if rec.wrote {
			m.ttfb.WithLabelValues(route, r.Method).Observe(rec.ttfb.Seconds())
		}
	})
}

// RequestID adopts the incoming X-Request-Id if present and well-formed, else
// generates one. It echoes the id on the response and injects both the id and a
// request-scoped logger (carrying request_id, method, path) into the context, so
// downstream code logs with correlation for free. Both live in reqctx, the one
// typed home for request-scoped values.
func RequestID(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(RequestIDHeader)
			if !validID(id) {
				id = newID()
			}
			w.Header().Set(RequestIDHeader, id)
			reqLog := log.With("request_id", id, "method", r.Method, "path", r.URL.Path)
			ctx := reqctx.WithRequestID(r.Context(), id)
			ctx = reqctx.WithLogger(ctx, reqLog)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// FromContext returns the request id on the context, or "" if none (e.g. a
// background worker context that never passed through the middleware).
func FromContext(ctx context.Context) string {
	return reqctx.RequestID(ctx)
}

// LoggerFromContext returns the request-scoped logger injected by RequestID, or
// nil if the context did not come through the middleware. Callers fall back to
// the package-level slog default so they can log unconditionally.
func LoggerFromContext(ctx context.Context) *slog.Logger {
	return reqctx.Logger(ctx)
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

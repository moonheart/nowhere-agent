package observability

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// Probe checks one dependency's liveness. It should be cheap (a Ping, not a
// query) and respect the deadline on the context it is given.
type Probe func(ctx context.Context) error

// Healthz reports process liveness as the AND of its dependency probes. The
// built-in Go /healthz answered "ok" unconditionally, which tells an
// orchestrator the process is alive but nothing about whether it can actually
// serve — a server whose Postgres is down looks healthy until the first real
// request fails. This handler probes every registered dependency and only
// answers 200 when all are reachable, so a load balancer or k8s probe can route
// around a backend that is up but useless.
//
// Probes run concurrently, each bounded by Timeout, so one hanging dependency
// cannot stall the health check past the deadline.
type Healthz struct {
	mu      sync.RWMutex
	probes  map[string]Probe
	Timeout time.Duration
}

// NewHealthz builds a Healthz with a per-probe timeout. A non-positive timeout
// defaults to 2 seconds, a sane upper bound for a local Ping.
func NewHealthz(timeout time.Duration) *Healthz {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return &Healthz{probes: map[string]Probe{}, Timeout: timeout}
}

// Add registers a named dependency probe. Register at startup before serving.
func (h *Healthz) Add(name string, p Probe) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.probes[name] = p
}

// Handler returns the health endpoint. 200 "ok" when every probe passes; 503
// with the failing dependency names when any fail.
func (h *Healthz) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.RLock()
		probes := make(map[string]Probe, len(h.probes))
		for k, v := range h.probes {
			probes[k] = v
		}
		h.mu.RUnlock()

		var (
			wg     sync.WaitGroup
			fmu    sync.Mutex
			failed []string
		)
		for name, p := range probes {
			wg.Add(1)
			go func(name string, p Probe) {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(r.Context(), h.Timeout)
				defer cancel()
				if err := p(ctx); err != nil {
					fmu.Lock()
					failed = append(failed, name)
					fmu.Unlock()
				}
			}(name, p)
		}
		wg.Wait()

		if len(failed) > 0 {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			for _, f := range failed {
				_, _ = w.Write([]byte("unhealthy: " + f + "\n"))
			}
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

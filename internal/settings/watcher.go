package settings

import (
	"context"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"
)

// Watcher runs registered sync callbacks on a fixed interval until ctx is
// cancelled. Each callback re-reads the runtime snapshot and applies it to its
// component (MCP manager, schedulers, webhook notifier, rate limiter, ...), so
// an admin-console change converges within one interval without a restart.
//
// Callbacks run sequentially on one goroutine; each must be quick (an
// in-memory apply — DB or network work belongs in a component's own worker).
type Watcher struct {
	mu  sync.RWMutex
	fns []func()
}

// NewWatcher builds an empty Watcher.
func NewWatcher() *Watcher {
	return &Watcher{}
}

// Add registers callbacks run once per tick. Safe to call before or after
// StartSync; the loop snapshots the current list each tick.
func (w *Watcher) Add(fns ...func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, fn := range fns {
		if fn != nil {
			w.fns = append(w.fns, fn)
		}
	}
}

// StartSync runs every registered callback on the interval until ctx is
// cancelled. interval <= 0 falls back to 5s. Returns immediately; the
// goroutine stops on ctx.Done.
func (w *Watcher) StartSync(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	t := time.NewTicker(interval)
	go func() {
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				// Recover per tick, not per goroutine: a panicking callback
				// must not kill the watcher loop, or settings convergence
				// would stop permanently until restart.
				func() {
					defer func() {
						if p := recover(); p != nil {
							slog.Error("settings sync callback panicked", "panic", p, "stack", string(debug.Stack()))
						}
					}()
					w.mu.RLock()
					fns := append([]func(){}, w.fns...)
					w.mu.RUnlock()
					for _, fn := range fns {
						fn()
					}
				}()
			}
		}
	}()
}

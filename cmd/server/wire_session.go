package main

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"nowhere-agent/internal/session"
)

// wire_session.go — the durable session runtime: PG store, runtime, the shared
// run registry, the pending-interaction sweep, stranded-run reconciliation, the
// live content broker (mem/Redis) with its drop metrics, and the message store.
// Extracted verbatim from run() (see deps.go).

func (d *serverDeps) wireSessionRuntime(ctx context.Context) error {
	cfg, log := d.cfg, d.log

	// Durable session runtime over Postgres: chat requests persist as runs,
	// and the run log doubles as the episodes for dreaming.
	d.sessionStore = session.NewPGStore(d.pool)
	d.sessionRuntime = session.NewRuntime(d.sessionStore)
	// Shared run-execution registry: the chat handler's runs, scheduled-task
	// firings, and the admin session purge (which must interrupt an in-flight
	// run before hard-deleting its session) all operate on the one worker
	// table. Created here, outside the provider branch, so the admin console
	// can reach the same workers the chat endpoint owns.
	d.runRegistry = session.NewRunRegistry(d.sessionRuntime)

	// Pending-interaction reaper: a client that never answers a suspended
	// batch's interaction must not lock the session's pending gate forever (new
	// submissions would 409). An hourly pass ages out pendings older than a day
	// (created_at based, so a verdict raced by the sweep still wins via the
	// status='pending' predicate). Best-effort like the credential sweep: a
	// failed pass is logged and retried next hour.
	hourlySweep(ctx, log, "pending-interaction", func() error {
		cutoff := time.Now().UTC().Add(-24 * time.Hour)
		removed, err := d.sessionStore.SweepExpiredInteractions(ctx, cutoff)
		if err != nil {
			return err
		}
		if removed > 0 {
			log.Info("pending-interaction sweep expired rows", "count", removed)
		}
		return nil
	})

	// Reconcile runs stranded non-terminal by a previous process (their in-memory
	// workers died with it): mark them failed at startup so they don't read as
	// active forever and hang clients that attach to them.
	if n, err := d.sessionRuntime.RecoverStrandedRuns(ctx); err != nil {
		log.Warn("startup run reconciliation failed", "err", err)
	} else if n > 0 {
		log.Info("reconciled stranded runs at startup", "count", n)
	}

	// Live content broker (redis-stream-live): in-memory for single instance,
	// Redis Streams for multi-instance. Selected via STREAM_BROKER; a redis
	// broker that is unreachable at boot fails fast (a multi-instance deploy
	// with a dead broker is a misconfiguration worth surfacing).
	if cfg.Stream.Broker == "redis" {
		if err := session.PingRedis(ctx, cfg.Stream.RedisAddr); err != nil {
			return fmt.Errorf("stream broker redis at %s: %w", cfg.Stream.RedisAddr, err)
		}
		// Redis Streams carry live content; Redis Pub/Sub carries lifecycle events
		// so running/done/cancelled fan out across instances too (the in-memory bus
		// only reaches clients on this instance). Durability stays in Postgres.
		broker := session.NewRedisBroker(cfg.Stream.RedisAddr, 0, 0)
		eventBus := session.NewRedisEventBus(cfg.Stream.RedisAddr)
		d.sessionRuntime = d.sessionRuntime.WithBroker(broker).WithBus(eventBus)
		d.health.Add("redis", func(ctx context.Context) error {
			return session.PingRedis(ctx, cfg.Stream.RedisAddr)
		})
		log.Info("live delivery: redis streams (content) + redis pub/sub (lifecycle)", "addr", cfg.Stream.RedisAddr)
	} else {
		log.Info("live delivery: in-memory (single instance)")
	}

	// Live-delivery health: surface slow-consumer drops from the fan-out layers.
	// A rising count means attached clients are falling behind live delivery and
	// healing via Read catch-up / Replay — previously silent.
	if ds, ok := d.sessionRuntime.Bus().(session.DropStats); ok {
		_ = d.metrics.Register(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "nowhere_session_bus_dropped_total",
			Help: "Lifecycle events dropped for slow subscribers (they heal via replay).",
		}, func() float64 { return float64(ds.DroppedTotal()) }))
	}
	if ds, ok := d.sessionRuntime.Broker().(session.DropStats); ok {
		_ = d.metrics.Register(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "nowhere_session_broker_dropped_total",
			Help: "Live content frames dropped for slow subscribers (they heal via catch-up read).",
		}, func() float64 { return float64(ds.DroppedTotal()) }))
	}

	// Full-block conversation record (persist-raw-messages): messages are
	// persisted in original form and cross-run history is rebuilt from it.
	d.messageStore = session.NewPGMessageStore(d.pool)
	return nil
}

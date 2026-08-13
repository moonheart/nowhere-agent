package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"
)

// outboxBackoffs is the retry schedule for pending deliveries: a failed
// attempt is retried after 1m → 5m → 15m → 1h → 4h → 12h → 24h, then the row
// is dead-lettered (failed, final).
var outboxBackoffs = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, 4 * time.Hour, 12 * time.Hour, 24 * time.Hour}

// Sweeper retries pending outbox deliveries with backoff, one pass per
// scheduler tick, and purges expired rows. Safe to share across instances:
// claims are atomic and carry a lease, so concurrent sweepers never
// double-send.
type Sweeper struct {
	store    *DeliveryStore
	notifier *Notifier
	record   func(outcome string) // metrics hook; nil disables
	log      *slog.Logger
}

// NewSweeper builds a Sweeper over the delivery store and notifier. record
// receives each delivery outcome ("delivered" | "failed" | "dead_lettered")
// for metrics; nil disables recording.
func NewSweeper(store *DeliveryStore, notifier *Notifier, record func(outcome string), log *slog.Logger) *Sweeper {
	if log == nil {
		log = slog.Default()
	}
	return &Sweeper{store: store, notifier: notifier, record: record, log: log}
}

// Sweep runs one outbox pass: purge expired rows, then claim and deliver due
// rows until the queue is empty. Unreadable payloads and permanent consumer
// rejections (4xx) are dead-lettered immediately; transient failures are
// retried on the backoff schedule.
func (s *Sweeper) Sweep(ctx context.Context) error {
	if _, err := s.store.PurgeExpired(ctx, time.Now().UTC()); err != nil {
		s.log.Warn("webhook outbox purge failed", "err", err)
	}
	for {
		d, err := s.store.ClaimNext(ctx, time.Now().UTC())
		if errors.Is(err, ErrNoPending) {
			return nil
		}
		if err != nil {
			return err
		}
		var payload RunCompletedPayload
		if err := json.Unmarshal(d.Payload, &payload); err != nil {
			// Unreadable payload: dead-letter immediately.
			_ = s.store.MarkFailed(ctx, d.ID, time.Now().Add(-time.Minute), "unreadable payload: "+err.Error())
			continue
		}
		if err := s.notifier.Deliver(ctx, d.TargetURL, payload); err != nil {
			// A 4xx is a PERMANENT consumer rejection — dead-letter right
			// away rather than hammering the consumer for days. Past the
			// backoff list → dead-letter (failed, final). The MarkFailed
			// CASE reads the timestamp, so a past time flips the status; an
			// in-list failure stays pending for the next sweep.
			if IsRejected(err) || d.Attempts > len(outboxBackoffs) {
				_ = s.store.MarkFailed(ctx, d.ID, time.Now().Add(-time.Minute), err.Error())
				s.recordOutcome("dead_lettered")
				s.log.Warn("webhook outbox delivery dead-lettered", "delivery", d.ID, "attempts", d.Attempts, "err", err)
				continue
			}
			next := time.Now().UTC().Add(outboxBackoffs[d.Attempts-1])
			_ = s.store.MarkFailed(ctx, d.ID, next, err.Error())
			s.recordOutcome("failed")
			s.log.Warn("webhook outbox retry failed", "delivery", d.ID, "attempt", d.Attempts, "next", next, "err", err)
			continue
		}
		s.recordOutcome("delivered")
		if err := s.store.MarkDelivered(ctx, d.ID, time.Now().UTC()); err != nil {
			s.log.Warn("webhook outbox mark delivered failed", "delivery", d.ID, "err", err)
		}
	}
}

func (s *Sweeper) recordOutcome(outcome string) {
	if s.record != nil {
		s.record(outcome)
	}
}

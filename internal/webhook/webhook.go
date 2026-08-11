// Package webhook delivers outbound run-completion notifications to external
// systems (enterprise integration): when a run reaches a terminal state, the
// platform POSTs a JSON payload to the task's webhook URL (or the global
// WEBHOOK_URL). This is the notification half of the "agent finishes work →
// enterprise system learns about it" loop that scheduled tasks otherwise
// cannot close — a fire-and-forget run would leave its result only in a
// session row nobody polls.
//
// Delivery is deliberately best-effort and out-of-band: it runs on its own
// goroutine with a bounded timeout and a few retries, and a failure only logs.
// A notification must never block or fail the run that produced it.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// RunCompletedPayload is the JSON body POSTed to a webhook URL when a run
// reaches a terminal state. It carries identity and a text summary — enough
// for an enterprise system (IM bot, ITSM, workflow engine) to route the
// notification — but never tool internals, PII, or secret material.
type RunCompletedPayload struct {
	// Event discriminates payload kinds; always "run.completed" today, the
	// field exists so the contract can grow without breaking consumers.
	Event     string `json:"event"`
	RunID     string `json:"run_id"`
	SessionID string `json:"session_id"`
	UserID    string `json:"user_id,omitempty"`
	// TaskID links the notification to the scheduled task that produced the
	// run; empty for an interactive (chat) run.
	TaskID string `json:"task_id,omitempty"`
	// Status is the terminal run status: done | failed | cancelled.
	Status string `json:"status"`
	TeamID string `json:"team_id,omitempty"`
	Model  string `json:"model,omitempty"`
	// EndedAt is the instant the run reached its terminal state (UTC).
	EndedAt time.Time `json:"ended_at"`
	// Summary is the last assistant text of the run, truncated; empty when the
	// run produced no assistant text (a failed fire, say).
	Summary string `json:"summary,omitempty"`
}

// Notifier delivers payloads to webhook URLs with a bounded timeout and a few
// retries. It is safe for concurrent use; one instance is shared by every run.
type Notifier struct {
	client  *http.Client
	timeout time.Duration
	retries int
	ssrf    *Guard // nil = no SSRF screening (tests/legacy)
	// signingSecret, when set, HMAC-SHA256-signs every payload body; the
	// consumer verifies authenticity via the X-Nowhere-Signature header.
	signingSecret []byte
	log           *slog.Logger
}

// Options tunes delivery. Zero values pick safe defaults.
type Options struct {
	// Timeout bounds one HTTP attempt; 0 defaults to 10s.
	Timeout time.Duration
	// Retries is the number of attempts after the first; 0 defaults to 3.
	Retries int
	// SSRF, when set, screens every delivery target (and every redirect hop)
	// against private/loopback network ranges before any connection is made.
	// Sites that legitimately notify internal systems add allowlisted CIDRs/
	// hosts via NewGuard.
	SSRF *Guard
	// SigningSecret, when set, signs every payload with HMAC-SHA256
	// (X-Nowhere-Signature: sha256=<hex> over the raw body). The consumer
	// shares the secret and verifies, so a notification cannot be forged by
	// anyone who can reach the webhook URL.
	SigningSecret string
	// Logger receives delivery outcomes; nil uses slog.Default().
	Logger *slog.Logger
}

// New builds a Notifier with the given options.
func New(opts Options) *Notifier {
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Second
	}
	if opts.Retries < 0 {
		opts.Retries = 0
	}
	if opts.Retries == 0 {
		opts.Retries = 3
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	n := &Notifier{
		client:        &http.Client{Timeout: opts.Timeout},
		timeout:       opts.Timeout,
		retries:       opts.Retries,
		ssrf:          opts.SSRF,
		signingSecret: []byte(opts.SigningSecret),
		log:           opts.Logger,
	}
	if opts.SSRF != nil {
		// Re-validate every redirect hop with the same guard, so a public URL
		// cannot smuggle the request into a private network via a 302.
		n.client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return opts.SSRF.CheckRedirect(req, via)
		}
	}
	return n
}

// Deliver posts payload to url. An empty url is a no-op (no notifications
// configured). Delivery runs synchronously on the caller's goroutine, bounded
// by the notifier's timeout; run-completion hooks call it in their own
// goroutine. Transient failures (5xx, network errors) retry with exponential
// backoff; a 2xx/4xx response is final (a rejected notification will not be
// hammered — the operator fixes the consumer, not the sender).
func (n *Notifier) Deliver(ctx context.Context, url string, payload RunCompletedPayload) error {
	if url == "" {
		return nil
	}
	// SSRF screen before anything else: a blocked target is refused without a
	// single connection attempt. Resolution happens per attempt (DNS can
	// change between runs), so a rebinding host cannot slip through.
	if n.ssrf != nil {
		if err := n.ssrf.CheckURL(ctx, url); err != nil {
			n.log.Warn("webhook delivery blocked by SSRF guard", "url", url, "run", payload.RunID, "err", err)
			return err
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	backoff := 500 * time.Millisecond
	var lastErr error
	for attempt := 0; attempt <= n.retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "nowhere-agent-webhook/1")
		req.Header.Set("X-Nowhere-Event", payload.Event)
		req.Header.Set("X-Nowhere-Run-Id", payload.RunID)
		if len(n.signingSecret) > 0 {
			// Sign the raw body: the consumer shares the secret and verifies
			// sha256=<hex HMAC-SHA256(body)> in constant time, so a
			// notification cannot be forged by anyone who can reach the URL.
			m := hmac.New(sha256.New, n.signingSecret)
			m.Write(body)
			req.Header.Set("X-Nowhere-Signature", "sha256="+hex.EncodeToString(m.Sum(nil)))
		}
		resp, err := n.client.Do(req)
		if err != nil {
			lastErr = err
			n.log.Warn("webhook delivery attempt failed", "url", url, "run", payload.RunID, "attempt", attempt, "err", err)
			continue // network/transport error: retryable
		}
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			n.log.Debug("webhook delivered", "url", url, "run", payload.RunID, "status", resp.StatusCode)
			return nil
		}
		lastErr = errUnexpectedStatus(resp.StatusCode)
		if resp.StatusCode < 500 {
			// A 4xx means the consumer rejected the payload — retrying is
			// noise, not recovery. Report and stop.
			n.log.Warn("webhook delivery rejected", "url", url, "run", payload.RunID, "status", resp.StatusCode)
			return lastErr
		}
		n.log.Warn("webhook delivery attempt failed", "url", url, "run", payload.RunID, "attempt", attempt, "status", resp.StatusCode)
	}
	return lastErr
}

func errUnexpectedStatus(code int) error {
	return &statusError{code: code}
}

type statusError struct{ code int }

func (e *statusError) Error() string { return "webhook: unexpected status " + itoa(e.code) }

// itoa is a tiny int formatter keeping the package import-light.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

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
	"errors"
	"log/slog"
	"net/http"
	"sync"
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
// The delivery policy (timeout, retries, signing secret, SSRF guard) is
// mutable via SetPolicy/SetGuard so the admin console can retune it without a
// restart; readers snapshot the current values under a RWMutex per delivery.
type Notifier struct {
	mu      sync.RWMutex
	client  *http.Client
	timeout time.Duration
	retries int
	ssrf    *Guard // nil = no SSRF screening (tests/legacy)
	// signingSecret, when set, HMAC-SHA256-signs every payload body; the
	// consumer verifies authenticity via the X-Nowhere-Signature header.
	signingSecret []byte
	// imHosts is the set of domestic IM-bot hosts whose payloads are
	// reformatted (DingTalk/WeCom/Feishu); tests swap it to route deliveries
	// at a local server.
	imHosts map[string]bool
	log     *slog.Logger
}

// Options tunes delivery. Zero values pick safe defaults.
type Options struct {
	// Timeout is the WHOLE delivery budget: one context covers every attempt
	// and the backoff waits between them, so a slow consumer cannot stretch
	// one notification across N attempts × timeout. 0 defaults to 10s.
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
		client:        httpClientFor(opts.SSRF, opts.Timeout),
		timeout:       opts.Timeout,
		retries:       opts.Retries,
		ssrf:          opts.SSRF,
		signingSecret: []byte(opts.SigningSecret),
		imHosts:       imBotHosts,
		log:           opts.Logger,
	}
	return n
}

// httpClientFor builds the shared HTTP client. The client's own Timeout stays
// zero — per-delivery timeouts come from a context bound to the CURRENT
// policy (SetPolicy retunes live, so a stale client timeout would win).
func httpClientFor(g *Guard, _ time.Duration) *http.Client {
	c := &http.Client{}
	if g != nil {
		// Dial through the guard: every connection is pinned to the addresses
		// the guard vetted for the current delivery (Guard.DialContext), and
		// every redirect hop is re-validated and re-pinned (CheckRedirect), so
		// neither a rebinding host nor a 302 can route a delivery onto a
		// private address. Keep-alives are off so a connection opened for one
		// delivery's vetted address set is never reused by another.
		c.Transport = &http.Transport{
			DialContext:       g.DialContext,
			Proxy:             http.ProxyFromEnvironment,
			DisableKeepAlives: true,
		}
		c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return g.CheckRedirect(req, via)
		}
	}
	return c
}

// SetPolicy retunes the delivery policy live (admin console): timeout bounds
// the whole delivery (one context across all attempts and backoff waits),
// retries bounds the attempts after the first, and signingSecret (may be
// empty to stop signing) signs every payload. Zero timeout/retries fall back
// to the 10s/3 defaults. The next delivery uses the new policy.
func (n *Notifier) SetPolicy(timeout time.Duration, retries int, signingSecret string) {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if retries < 0 {
		retries = 0
	}
	if retries == 0 {
		retries = 3
	}
	n.mu.Lock()
	n.timeout = timeout
	n.retries = retries
	n.signingSecret = []byte(signingSecret)
	n.mu.Unlock()
}

// SetGuard swaps the SSRF screen live. A nil guard disables screening (only
// meaningful for tests/legacy); a guard's CheckRedirect binding is rebuilt
// with the client, so redirect screening follows the new guard.
func (n *Notifier) SetGuard(g *Guard) {
	n.mu.Lock()
	n.ssrf = g
	n.client = httpClientFor(g, n.timeout)
	n.mu.Unlock()
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
	// Snapshot the policy once: a console retune mid-delivery applies to the
	// next delivery, keeping this one internally consistent.
	n.mu.RLock()
	timeout, retries, ssrf := n.timeout, n.retries, n.ssrf
	signingSecret, client := n.signingSecret, n.client
	n.mu.RUnlock()
	deliverCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// SSRF screen before anything else: a blocked target is refused without a
	// single connection attempt. Every attempt re-resolves and pins the vetted
	// addresses into the request (pinRequest + Guard.DialContext), so a host
	// rebinding between the check and the dial cannot slip through; an
	// allowlisted hostname is the operator's explicit trust, dialed freely.
	if ssrf != nil {
		if err := ssrf.CheckURL(deliverCtx, url); err != nil {
			n.log.Warn("webhook delivery blocked by SSRF guard", "url", url, "run", payload.RunID, "err", err)
			return err
		}
	}
	// Domestic IM bots (DingTalk/WeCom/Feishu) take their own payload schema
	// and need no retry amplification (a 4xx from the bot API is permanent).
	if n.isIMBotURL(url) {
		return n.deliverIM(deliverCtx, url, payload, ssrf)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	backoff := 500 * time.Millisecond
	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			select {
			case <-deliverCtx.Done():
				return deliverCtx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		}
		req, err := http.NewRequestWithContext(deliverCtx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "nowhere-agent-webhook/1")
		req.Header.Set("X-Nowhere-Event", payload.Event)
		req.Header.Set("X-Nowhere-Run-Id", payload.RunID)
		if len(signingSecret) > 0 {
			// Sign the raw body: the consumer shares the secret and verifies
			// sha256=<hex HMAC-SHA256(body)> in constant time, so a
			// notification cannot be forged by anyone who can reach the URL.
			m := hmac.New(sha256.New, signingSecret)
			m.Write(body)
			req.Header.Set("X-Nowhere-Signature", "sha256="+hex.EncodeToString(m.Sum(nil)))
		}
		if ssrf != nil {
			// Validate and pin this attempt's addresses into the request
			// (Guard.DialContext dials exactly the vetted addresses): the
			// resolution is per attempt, and the connection can never be
			// re-resolved to an unvetted address.
			if err := ssrf.pinRequest(req, url); err != nil {
				n.log.Warn("webhook delivery blocked by SSRF guard", "url", url, "run", payload.RunID, "err", err)
				return err
			}
		}
		resp, err := client.Do(req)
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

// IsRejected reports whether err is a permanent consumer rejection (a 4xx
// from the target — the consumer answered, it refuses this payload). Callers
// (the outbox sweeper) dead-letter these immediately instead of retrying a
// rejection for days.
func IsRejected(err error) bool {
	var se *statusError
	return errors.As(err, &se) && se.code >= 400 && se.code < 500
}

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

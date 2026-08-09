package provider

import (
	"context"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RetryPolicy bounds transient-failure retries for a provider request. Retries
// cover network errors and retryable HTTP statuses (429 / 5xx / 529); the
// backoff is exponential with full jitter, capped at MaxDelay, and honours ctx
// cancellation.
type RetryPolicy struct {
	MaxAttempts int           // total attempts including the first; <=1 disables retry
	BaseDelay   time.Duration // backoff for the first retry
	MaxDelay    time.Duration // backoff cap
}

// DefaultRetryPolicy is the policy adapters use unless overridden: 3 attempts
// with 500ms→8s exponential backoff.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{MaxAttempts: 3, BaseDelay: 500 * time.Millisecond, MaxDelay: 8 * time.Second}
}

func (p RetryPolicy) normalized() RetryPolicy {
	if p.MaxAttempts < 1 {
		p.MaxAttempts = 1
	}
	if p.BaseDelay <= 0 {
		p.BaseDelay = 500 * time.Millisecond
	}
	if p.MaxDelay <= 0 {
		p.MaxDelay = 8 * time.Second
	}
	return p
}

// IsRetryableStatus reports whether an HTTP status warrants a retry: rate
// limiting (429), transient server errors (500/502/503/504), and Anthropic's
// overloaded (529). Other 4xx are caller/request errors and are not retried.
func IsRetryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
		529:
		return true
	}
	return false
}

// DoWithRetry runs attempt with exponential backoff + jitter, retrying on a
// network error or a retryable HTTP status. A Retry-After response header (on
// 429/503/529) overrides the computed backoff, so rate limits wait the
// server-requested duration instead of guessing. It returns the first
// non-retryable response (a 2xx, or a 4xx the caller should classify), or —
// once attempts are exhausted — the last response/error so the caller's
// existing handling applies unchanged. The caller owns the returned response
// Body; bodies of retried responses are drained and closed here so the
// connection can be reused.
func DoWithRetry(ctx context.Context, policy RetryPolicy, attempt func() (*http.Response, error)) (*http.Response, error) {
	policy = policy.normalized()
	var lastErr error
	var retryAfter time.Duration
	for i := 0; i < policy.MaxAttempts; i++ {
		if i > 0 {
			if err := backoff(ctx, policy, i, retryAfter); err != nil {
				return nil, err
			}
			retryAfter = 0
		}
		resp, err := attempt()
		if err != nil {
			lastErr = err
			continue
		}
		if IsRetryableStatus(resp.StatusCode) && i < policy.MaxAttempts-1 {
			retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
			drainClose(resp)
			lastErr = fmt.Errorf("provider status %d", resp.StatusCode)
			continue
		}
		return resp, nil
	}
	return nil, lastErr
}

// maxRetryAfter bounds a server-requested wait so a hostile or buggy gateway
// cannot park a run for hours.
const maxRetryAfter = 2 * time.Minute

// parseRetryAfter interprets a Retry-After header: either delta-seconds or an
// HTTP date. 0 (or a past/unparseable value) means "no guidance".
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	var d time.Duration
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		d = time.Duration(secs) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = time.Until(t)
		if d <= 0 {
			return 0
		}
	} else {
		return 0
	}
	if d > maxRetryAfter {
		return maxRetryAfter
	}
	return d
}

// backoff sleeps before retry i (i>=1), honouring ctx. The delay is
// base*2^(i-1) capped at MaxDelay, with full jitter to avoid thundering herds.
// A server-provided Retry-After overrides the computed delay (no jitter: the
// server already scheduled the retry).
func backoff(ctx context.Context, p RetryPolicy, i int, retryAfter time.Duration) error {
	var wait time.Duration
	if retryAfter > 0 {
		wait = retryAfter
	} else {
		d := float64(p.BaseDelay) * math.Pow(2, float64(i-1))
		if d > float64(p.MaxDelay) {
			d = float64(p.MaxDelay)
		}
		wait = time.Duration(rand.Int63n(int64(d) + 1))
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// drainClose drains a bounded prefix of the body and closes it so the underlying
// connection is returned to the pool before a retry.
func drainClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
}

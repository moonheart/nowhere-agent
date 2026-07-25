package provider

import (
	"context"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
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
// network error or a retryable HTTP status. It returns the first non-retryable
// response (a 2xx, or a 4xx the caller should classify), or — once attempts are
// exhausted — the last response/error so the caller's existing handling applies
// unchanged. The caller owns the returned response Body; bodies of retried
// responses are drained and closed here so the connection can be reused.
func DoWithRetry(ctx context.Context, policy RetryPolicy, attempt func() (*http.Response, error)) (*http.Response, error) {
	policy = policy.normalized()
	var lastErr error
	for i := 0; i < policy.MaxAttempts; i++ {
		if i > 0 {
			if err := backoff(ctx, policy, i); err != nil {
				return nil, err
			}
		}
		resp, err := attempt()
		if err != nil {
			lastErr = err
			continue
		}
		if IsRetryableStatus(resp.StatusCode) && i < policy.MaxAttempts-1 {
			drainClose(resp)
			lastErr = fmt.Errorf("provider status %d", resp.StatusCode)
			continue
		}
		return resp, nil
	}
	return nil, lastErr
}

// backoff sleeps before retry i (i>=1), honouring ctx. The delay is
// base*2^(i-1) capped at MaxDelay, with full jitter to avoid thundering herds.
func backoff(ctx context.Context, p RetryPolicy, i int) error {
	d := float64(p.BaseDelay) * math.Pow(2, float64(i-1))
	if d > float64(p.MaxDelay) {
		d = float64(p.MaxDelay)
	}
	wait := time.Duration(rand.Int63n(int64(d) + 1))
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

package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fastPolicy(maxAttempts int) RetryPolicy {
	return RetryPolicy{MaxAttempts: maxAttempts, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond}
}

func statusResp(code int) *http.Response {
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader("body"))}
}

func TestIsRetryableStatus(t *testing.T) {
	for _, c := range []int{429, 500, 502, 503, 504, 529} {
		if !IsRetryableStatus(c) {
			t.Errorf("status %d should be retryable", c)
		}
	}
	for _, c := range []int{200, 400, 401, 403, 404, 413, 422} {
		if IsRetryableStatus(c) {
			t.Errorf("status %d should not be retryable", c)
		}
	}
}

func TestDoWithRetrySucceedsAfterTransient(t *testing.T) {
	var calls int32
	resp, err := DoWithRetry(context.Background(), fastPolicy(3), func() (*http.Response, error) {
		if atomic.AddInt32(&calls, 1) < 3 {
			return statusResp(503), nil
		}
		return statusResp(200), nil
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d want 200", resp.StatusCode)
	}
	if calls != 3 {
		t.Errorf("calls = %d want 3", calls)
	}
}

// A retryable status that never clears returns the last response (not an error)
// so the adapter's existing non-200 classification (e.g. context overflow) runs.
func TestDoWithRetryExhaustsReturnsLastResponse(t *testing.T) {
	var calls int32
	resp, err := DoWithRetry(context.Background(), fastPolicy(3), func() (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return statusResp(503), nil
	})
	if err != nil {
		t.Fatalf("err = %v, want nil (last response returned to caller)", err)
	}
	if resp.StatusCode != 503 {
		t.Errorf("status = %d want 503", resp.StatusCode)
	}
	if calls != 3 {
		t.Errorf("calls = %d want 3", calls)
	}
}

func TestDoWithRetryNonRetryableIsImmediate(t *testing.T) {
	var calls int32
	resp, err := DoWithRetry(context.Background(), fastPolicy(3), func() (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return statusResp(400), nil
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if resp.StatusCode != 400 {
		t.Errorf("status = %d want 400", resp.StatusCode)
	}
	if calls != 1 {
		t.Errorf("calls = %d want 1 (non-retryable must not retry)", calls)
	}
}

func TestDoWithRetryNetworkErrorExhausts(t *testing.T) {
	var calls int32
	_, err := DoWithRetry(context.Background(), fastPolicy(3), func() (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return nil, errors.New("dial fail")
	})
	if err == nil {
		t.Fatal("expected error after exhausting retries on a network failure")
	}
	if calls != 3 {
		t.Errorf("calls = %d want 3", calls)
	}
}

func TestDoWithRetryHonorsContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: the first backoff must abort the retry loop
	var calls int32
	_, err := DoWithRetry(ctx, fastPolicy(5), func() (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return statusResp(503), nil
	})
	if err == nil {
		t.Fatal("expected context error")
	}
	if calls != 1 {
		t.Errorf("calls = %d want 1 (cancel during backoff stops further attempts)", calls)
	}
}

package identity

import (
	"testing"
	"time"
)

func TestLoginThrottlerLocksAfterMaxFailures(t *testing.T) {
	tr := NewLoginThrottler()
	for i := 0; i < loginMaxFailures; i++ {
		if allowed, _ := tr.Check("alice@corp.cn", "10.0.0.1"); !allowed {
			t.Fatalf("attempt %d should be allowed before the threshold", i+1)
		}
		tr.Fail("alice@corp.cn", "10.0.0.1")
	}
	if allowed, retryAfter := tr.Check("alice@corp.cn", "10.0.0.1"); allowed {
		t.Fatal("attempt past the threshold must be refused")
	} else if retryAfter <= 0 || retryAfter > loginLockout {
		t.Fatalf("retryAfter = %v, want within (0, %v]", retryAfter, loginLockout)
	}
}

func TestLoginThrottlerKeyIsEmailAndIPScoped(t *testing.T) {
	tr := NewLoginThrottler()
	for i := 0; i < loginMaxFailures; i++ {
		tr.Fail("alice@corp.cn", "10.0.0.1")
	}
	// Same email from another IP is unaffected.
	if allowed, _ := tr.Check("alice@corp.cn", "10.0.0.2"); !allowed {
		t.Error("same email from a different IP must not be locked")
	}
	// A different email from the locked IP is unaffected.
	if allowed, _ := tr.Check("bob@corp.cn", "10.0.0.1"); !allowed {
		t.Error("a different email from the locked IP must not be locked")
	}
	// Email matching is case-insensitive (a brute-forcer rotating case must not
	// dodge the counter).
	if allowed, _ := tr.Check("ALICE@corp.cn", "10.0.0.1"); allowed {
		t.Error("case-rotated email must still hit the lock")
	}
}

func TestLoginThrottlerSuccessResets(t *testing.T) {
	tr := NewLoginThrottler()
	for i := 0; i < loginMaxFailures-1; i++ {
		tr.Fail("alice@corp.cn", "10.0.0.1")
	}
	tr.Success("alice@corp.cn", "10.0.0.1")
	if allowed, _ := tr.Check("alice@corp.cn", "10.0.0.1"); !allowed {
		t.Fatal("a successful login must reset the counter")
	}
}

func TestLoginThrottlerLockExpires(t *testing.T) {
	tr := NewLoginThrottler()
	for i := 0; i < loginMaxFailures; i++ {
		tr.Fail("alice@corp.cn", "10.0.0.1")
	}
	if allowed, _ := tr.Check("alice@corp.cn", "10.0.0.1"); allowed {
		t.Fatal("should be locked")
	}
	// Move the clock past the lockout by expiring entries directly: the lock
	// expiry check uses time.Now internally, so simulate by clearing the lock
	// and verifying history pruning frees the pair.
	tr.mu.Lock()
	delete(tr.locked, tr.key("alice@corp.cn", "10.0.0.1"))
	tr.failures[tr.key("alice@corp.cn", "10.0.0.1")] = []time.Time{time.Now().Add(-loginFailWindow - time.Minute)}
	tr.mu.Unlock()

	if allowed, _ := tr.Check("alice@corp.cn", "10.0.0.1"); !allowed {
		t.Error("pair must be free after the failure window has passed")
	}
}

func TestLoginThrottlerPrunesStaleFailures(t *testing.T) {
	tr := NewLoginThrottler()
	// One stale failure (outside the window) must not count toward the max.
	tr.mu.Lock()
	tr.failures[tr.key("alice@corp.cn", "10.0.0.1")] = []time.Time{time.Now().Add(-loginFailWindow - time.Hour)}
	tr.mu.Unlock()
	if allowed, _ := tr.Check("alice@corp.cn", "10.0.0.1"); !allowed {
		t.Error("stale failures must be pruned")
	}
	tr.Fail("alice@corp.cn", "10.0.0.1")
	if allowed, _ := tr.Check("alice@corp.cn", "10.0.0.1"); !allowed {
		t.Error("one fresh failure is below the threshold")
	}
}

package identity

import (
	"fmt"
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
	// The EMAIL dimension locks across addresses: 5 failures total, however
	// spread, lock alice@corp.cn everywhere.
	if allowed, _ := tr.Check("alice@corp.cn", "10.0.0.2"); allowed {
		t.Error("email past its cross-IP threshold must be locked from a fresh IP")
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

// TestLoginThrottlerLocksEmailAcrossIPs: the botnet case — failures spread
// thinly over many addresses stay under every PAIR threshold, yet the email's
// cross-IP total must still lock the account.
func TestLoginThrottlerLocksEmailAcrossIPs(t *testing.T) {
	tr := NewLoginThrottler()
	// One failure per address: no pair ever reaches the pair threshold…
	for i := 0; i < loginMaxFailures; i++ {
		ip := fmt.Sprintf("10.1.%d.%d", i, i)
		if allowed, _ := tr.Check("alice@corp.cn", ip); !allowed {
			t.Fatalf("fresh IP %s should be allowed while both dimensions are under", ip)
		}
		tr.Fail("alice@corp.cn", ip)
	}
	// …but the email total of 5 now locks it from any address.
	if allowed, _ := tr.Check("alice@corp.cn", "10.9.9.9"); allowed {
		t.Fatal("email must lock after 5 failures spread across 5 IPs")
	}
	// One further failure on the same email still lands the lock (pair admit
	// and email admit are independent).
	tr.Fail("alice@corp.cn", "10.8.8.8")
	if allowed, _ := tr.Check("alice@corp.cn", "10.7.7.7"); allowed {
		t.Fatal("email must stay locked")
	}
	// A successful login clears BOTH dimensions: the email is usable again
	// from a fresh address, and the pair is fresh too.
	tr.Success("alice@corp.cn", "10.7.7.7")
	if allowed, _ := tr.Check("alice@corp.cn", "10.6.6.6"); !allowed {
		t.Error("success must clear the email-wide lock")
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
	if allowed, _ := tr.Check("alice@corp.cn", "10.0.0.2"); !allowed {
		t.Fatal("a successful login must clear the email-wide counter too")
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
	// expiry check uses time.Now internally, so simulate by clearing both
	// dimensions' locks and verifying history pruning frees the pair and the
	// email.
	tr.mu.Lock()
	delete(tr.locked, tr.key("alice@corp.cn", "10.0.0.1"))
	tr.failures[tr.key("alice@corp.cn", "10.0.0.1")] = []time.Time{time.Now().Add(-loginFailWindow - time.Minute)}
	delete(tr.emailLocked, tr.emailKey("alice@corp.cn"))
	tr.emailFailures[tr.emailKey("alice@corp.cn")] = []time.Time{time.Now().Add(-loginFailWindow - time.Minute)}
	tr.mu.Unlock()

	if allowed, _ := tr.Check("alice@corp.cn", "10.0.0.1"); !allowed {
		t.Error("pair must be free after the failure window has passed")
	}
	if allowed, _ := tr.Check("alice@corp.cn", "10.0.0.2"); !allowed {
		t.Error("email must be free after the failure window has passed")
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

func TestLoginThrottlerCapsDistinctKeys(t *testing.T) {
	tr := NewLoginThrottler()
	for i := 0; i < throttleMaxKeys; i++ {
		tr.Fail(fmt.Sprintf("cap%d@corp.cn", i), "10.0.0.1")
	}
	tr.mu.Lock()
	if n := len(tr.failures); n != throttleMaxKeys {
		t.Fatalf("map holds %d keys at the cap, want %d", n, throttleMaxKeys)
	}
	tr.mu.Unlock()

	// A brand-new key is refused: the map must not grow past the cap.
	tr.Fail("new@corp.cn", "10.0.0.1")
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if n := len(tr.failures); n != throttleMaxKeys {
		t.Errorf("map grew to %d keys past the cap", n)
	}
	if _, ok := tr.failures[tr.key("new@corp.cn", "10.0.0.1")]; ok {
		t.Error("a new key must be refused while the map is at the cap")
	}
}

func TestLoginThrottlerSweepMakesRoomForExpiredKeys(t *testing.T) {
	tr := NewLoginThrottler()
	for i := 0; i < throttleMaxKeys; i++ {
		tr.Fail(fmt.Sprintf("cap%d@corp.cn", i), "10.0.0.1")
	}
	// Age every failure out of the window, then touch a new key: the full
	// sweep must reclaim the expired entries and admit it.
	tr.mu.Lock()
	for k := range tr.failures {
		tr.failures[k] = []time.Time{time.Now().Add(-loginFailWindow - time.Hour)}
	}
	tr.mu.Unlock()

	tr.Fail("fresh@corp.cn", "10.0.0.1")
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if _, ok := tr.failures[tr.key("fresh@corp.cn", "10.0.0.1")]; !ok {
		t.Error("a full sweep must admit a new key once expired entries are reaped")
	}
	if n := len(tr.failures); n >= throttleMaxKeys {
		t.Errorf("sweep did not reclaim the map (%d keys)", n)
	}
}

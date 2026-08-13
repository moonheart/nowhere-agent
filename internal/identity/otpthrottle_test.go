package identity

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestOTPThrottlerVerifyLockout(t *testing.T) {
	th := NewOTPThrottler()
	base := time.Now()
	th.now = func() time.Time { return base }

	for i := 0; i < otpMaxVerifyFailures; i++ {
		th.FailVerify("13800138000", "1.2.3.4")
	}
	allowed, _ := th.CheckVerify("13800138000", "1.2.3.4")
	if allowed {
		t.Fatal("pair allowed after the failure cap")
	}
	// A different IP for the same phone is not affected by the pair lock.
	allowed, _ = th.CheckVerify("13800138000", "5.6.7.8")
	if !allowed {
		t.Fatal("different IP must not share the pair lock")
	}
	// Success resets the pair.
	th.SuccessVerify("13800138000", "1.2.3.4")
	allowed, _ = th.CheckVerify("13800138000", "1.2.3.4")
	if !allowed {
		t.Fatal("pair still locked after Success")
	}
}

func TestOTPThrottlerVerifyWindowDecays(t *testing.T) {
	th := NewOTPThrottler()
	base := time.Now()
	th.now = func() time.Time { return base }

	for i := 0; i < otpMaxVerifyFailures-1; i++ {
		th.FailVerify("13800138000", "1.2.3.4")
	}
	// Age the failures past the window: the count resets.
	th.now = func() time.Time { return base.Add(otpVerifyWindow + time.Second) }
	allowed, _ := th.CheckVerify("13800138000", "1.2.3.4")
	if !allowed {
		t.Fatal("stale failures must not lock the pair")
	}
}

func TestOTPThrottlerSendQuotas(t *testing.T) {
	th := NewOTPThrottler()
	base := time.Now()
	th.now = func() time.Time { return base }

	// Per-phone quota: 10 sends to one number are fine, the 11th is refused.
	for i := 0; i < otpDailyPerPhone; i++ {
		if err := th.AllowSend("13800138000", "1.2.3.4"); err != nil {
			t.Fatalf("send %d refused: %v", i, err)
		}
		th.RecordSend("13800138000", "1.2.3.4")
	}
	if err := th.AllowSend("13800138000", "1.2.3.4"); !errors.Is(err, ErrOTPSendQuota) {
		t.Fatalf("11th send to one phone: %v, want ErrOTPSendQuota", err)
	}
	// Per-IP quota: one IP sweeping many numbers hits the IP cap last (each
	// sweep uses a fresh number so the per-phone cap is not the limiter).
	th2 := NewOTPThrottler()
	th2.now = func() time.Time { return base }
	for i := 0; i < otpDailyPerIP; i++ {
		num := "1390000000" + string(rune('0'+i%10))
		if err := th2.AllowSend(num, "9.9.9.9"); err != nil {
			t.Fatalf("ip send %d refused: %v", i, err)
		}
		th2.RecordSend(num, "9.9.9.9")
	}
	if err := th2.AllowSend("13700000000", "9.9.9.9"); !errors.Is(err, ErrOTPSendQuota) {
		t.Fatalf("ip quota exhausted but another number allowed: %v", err)
	}
	// A different phone from a different IP is fine.
	if err := th2.AllowSend("13700000000", "8.8.8.8"); err != nil {
		t.Fatalf("unrelated pair refused: %v", err)
	}
	// Quotas roll over with the day.
	th2.now = func() time.Time { return base.Add(otpDay + time.Second) }
	if err := th2.AllowSend("13900000000", "9.9.9.9"); err != nil {
		t.Fatalf("quota did not roll over: %v", err)
	}
}

func TestOTPThrottlerCapsDistinctKeys(t *testing.T) {
	th := NewOTPThrottler()
	base := time.Now()
	th.now = func() time.Time { return base }

	for i := 0; i < throttleMaxKeys; i++ {
		th.FailVerify(fmt.Sprintf("1390000%04d", i), "1.2.3.4")
	}
	th.mu.Lock()
	if n := len(th.verify); n != throttleMaxKeys {
		t.Fatalf("verify map holds %d keys at the cap, want %d", n, throttleMaxKeys)
	}
	th.mu.Unlock()

	// A brand-new pair is refused: the map must not grow past the cap.
	th.FailVerify("13800000000", "1.2.3.4")
	th.mu.Lock()
	defer th.mu.Unlock()
	if n := len(th.verify); n != throttleMaxKeys {
		t.Errorf("verify map grew to %d keys past the cap", n)
	}
	if _, ok := th.verify[th.key("13800000000", "1.2.3.4")]; ok {
		t.Error("a new key must be refused while the map is at the cap")
	}
}

func TestOTPThrottlerSweepMakesRoomForExpiredKeys(t *testing.T) {
	th := NewOTPThrottler()
	base := time.Now()
	th.now = func() time.Time { return base }

	for i := 0; i < throttleMaxKeys; i++ {
		th.FailVerify(fmt.Sprintf("1390000%04d", i), "1.2.3.4")
	}
	// Age every failure out of the window, then touch a new pair: the full
	// sweep must reclaim the expired entries and admit it.
	th.mu.Lock()
	for k := range th.verify {
		th.verify[k] = []time.Time{base.Add(-otpVerifyWindow - time.Hour)}
	}
	th.mu.Unlock()

	th.FailVerify("13800000000", "1.2.3.4")
	th.mu.Lock()
	defer th.mu.Unlock()
	if _, ok := th.verify[th.key("13800000000", "1.2.3.4")]; !ok {
		t.Error("a full sweep must admit a new key once expired entries are reaped")
	}
	if n := len(th.verify); n >= throttleMaxKeys {
		t.Errorf("sweep did not reclaim the map (%d keys)", n)
	}
}

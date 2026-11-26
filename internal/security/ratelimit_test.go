package security

import (
	"fmt"
	"testing"
	"time"
)

func TestAuthRateLimiter_ProgressiveLockout(t *testing.T) {
	now := time.Date(2026, 11, 23, 19, 0, 0, 0, time.UTC)
	rl := NewAuthRateLimiterWithClock(
		func() time.Time { return now },
		10*time.Minute,
		3,
		30*time.Second,
		2*time.Minute,
	)

	// First 2 failures do not lock.
	for i := 0; i < 2; i++ {
		if locked, _ := rl.RecordFailure("198.51.100.5", "dan"); locked {
			t.Fatalf("unexpected lock on failure %d", i+1)
		}
		if ok, _ := rl.Check("198.51.100.5", "dan"); !ok {
			t.Fatalf("expected allowed before threshold")
		}
	}

	// 3rd failure locks for 30s.
	locked, retry := rl.RecordFailure("198.51.100.5", "dan")
	if !locked || retry != 30*time.Second {
		t.Fatalf("expected 30s lock, got locked=%v retry=%v", locked, retry)
	}
	if ok, wait := rl.Check("198.51.100.5", "dan"); ok || wait != 30*time.Second {
		t.Fatalf("expected blocked for 30s, got ok=%v wait=%v", ok, wait)
	}

	// Unrelated IP remains allowed.
	if ok, _ := rl.Check("203.0.113.9", "dan"); !ok {
		t.Fatal("unrelated IP should not be locked")
	}

	// Advance past first lock, fail 3 more times -> escalates to 60s.
	now = now.Add(31 * time.Second)
	for i := 0; i < 2; i++ {
		rl.RecordFailure("198.51.100.5", "dan")
	}
	locked, retry = rl.RecordFailure("198.51.100.5", "dan")
	if !locked || retry != 60*time.Second {
		t.Fatalf("expected escalated 60s lock, got locked=%v retry=%v", locked, retry)
	}
}

func TestAuthRateLimiter_GlobalAccountThrottle(t *testing.T) {
	now := time.Date(2026, 11, 23, 19, 0, 0, 0, time.UTC)
	rl := NewAuthRateLimiterWithClock(
		func() time.Time { return now },
		10*time.Minute,
		5,
		30*time.Second,
		15*time.Minute,
	)

	// 19 distributed failures from distinct IPs should not yet lock the global account bucket.
	for i := 0; i < 19; i++ {
		ip := fmt.Sprintf("198.51.100.%d", i+1)
		if locked, _ := rl.RecordFailure(ip, "victim"); locked {
			t.Fatalf("unexpected global lock on failure %d", i+1)
		}
	}
	if ok, _ := rl.Check("203.0.113.200", "victim"); !ok {
		t.Fatal("expected victim allowed before 20th global failure")
	}

	// 20th failure from yet another IP triggers global account lockout.
	locked, retry := rl.RecordFailure("198.51.100.20", "victim")
	if !locked || retry != 30*time.Second {
		t.Fatalf("expected 30s global lock, got locked=%v retry=%v", locked, retry)
	}
	if ok, _ := rl.Check("203.0.113.201", "victim"); ok {
		t.Fatal("expected fresh IP to be blocked for globally locked account")
	}
	// Other accounts remain unaffected from a fresh IP.
	if ok, _ := rl.Check("203.0.113.201", "other"); !ok {
		t.Fatal("expected unrelated account to remain allowed")
	}
}

func TestAuthRateLimiter_MaxBucketsCapAndCleanupThrottle(t *testing.T) {
	now := time.Date(2026, 11, 23, 19, 0, 0, 0, time.UTC)
	rl := NewAuthRateLimiterWithClock(
		func() time.Time { return now },
		10*time.Minute,
		5,
		30*time.Second,
		15*time.Minute,
	)

	// Insert more than maxBuckets distinct IPs; map size must stay bounded at maxBuckets.
	for i := 0; i < maxBuckets+50; i++ {
		now = now.Add(time.Millisecond)
		rl.RecordFailure(fmt.Sprintf("10.%d.%d.%d", (i>>16)&0xff, (i>>8)&0xff, i&0xff), "")
	}
	if len(rl.buckets) > maxBuckets {
		t.Fatalf("buckets len = %d, exceeds maxBuckets %d", len(rl.buckets), maxBuckets)
	}

	// Cleanup throttle: within 30s of lastCleanup, maybeCleanupLocked must not scan/reset lastCleanup.
	prevCleanup := rl.lastCleanup
	now = prevCleanup.Add(10 * time.Second)
	rl.Check("203.0.113.1", "")
	if !rl.lastCleanup.Equal(prevCleanup) {
		t.Fatalf("cleanup ran before 30s throttle elapsed")
	}
}

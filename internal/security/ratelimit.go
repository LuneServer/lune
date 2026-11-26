package security

import (
	"math"
	"sync"
	"time"
)

const (
	maxBuckets               = 16384
	cleanupMinInterval       = 30 * time.Second
	globalAccountMaxFailures = 20
)

// AuthRateLimiter provides in-process adaptive brute-force protection and progressive lockouts.
type AuthRateLimiter struct {
	mu          sync.Mutex
	now         func() time.Time
	window      time.Duration
	maxFailures int
	baseLockout time.Duration
	maxLockout  time.Duration
	buckets     map[string]*failureBucket
	lastCleanup time.Time
}

type failureBucket struct {
	failures    int
	firstFailed time.Time
	lockedUntil time.Time
	lockCount   int
}

// NewAuthRateLimiter returns a limiter configured with conservative defaults:
// 5 failed attempts within 10m triggers a progressive lockout (30s -> 1m -> 2m ... up to 15m).
func NewAuthRateLimiter() *AuthRateLimiter {
	return NewAuthRateLimiterWithClock(time.Now, 10*time.Minute, 5, 30*time.Second, 15*time.Minute)
}

func NewAuthRateLimiterWithClock(
	now func() time.Time,
	window time.Duration,
	maxFailures int,
	baseLockout time.Duration,
	maxLockout time.Duration,
) *AuthRateLimiter {
	return &AuthRateLimiter{
		now:         now,
		window:      window,
		maxFailures: maxFailures,
		baseLockout: baseLockout,
		maxLockout:  maxLockout,
		buckets:     make(map[string]*failureBucket),
		lastCleanup: now(),
	}
}

// Check verifies whether an auth attempt from ip (and optional account) is currently permitted.
func (l *AuthRateLimiter) Check(ip, username string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.maybeCleanupLocked(now)

	var maxWait time.Duration
	for _, key := range bucketKeys(ip, username) {
		b, ok := l.buckets[key]
		if !ok {
			continue
		}
		if now.Before(b.lockedUntil) {
			if wait := b.lockedUntil.Sub(now); wait > maxWait {
				maxWait = wait
			}
		}
	}
	if maxWait > 0 {
		return false, maxWait
	}
	return true, 0
}

// RecordFailure increments failure counters and applies progressive lockout if threshold is exceeded.
func (l *AuthRateLimiter) RecordFailure(ip, username string) (locked bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.maybeCleanupLocked(now)

	for _, spec := range l.bucketSpecs(ip, username) {
		b, ok := l.buckets[spec.key]
		if !ok {
			if len(l.buckets) >= maxBuckets {
				if now.Sub(l.lastCleanup) >= cleanupMinInterval {
					l.cleanupLocked(now)
				}
				if len(l.buckets) >= maxBuckets {
					l.evictOldestLocked()
				}
			}
			b = &failureBucket{firstFailed: now}
			l.buckets[spec.key] = b
		} else if now.Sub(b.firstFailed) > l.window && !now.Before(b.lockedUntil) {
			b = &failureBucket{firstFailed: now}
			l.buckets[spec.key] = b
		}
		b.failures++
		if b.failures >= spec.maxFailures {
			b.lockCount++
			exp := math.Pow(2, float64(b.lockCount-1))
			dur := time.Duration(float64(l.baseLockout) * exp)
			if dur > l.maxLockout {
				dur = l.maxLockout
			}
			b.lockedUntil = now.Add(dur)
			b.failures = 0
			b.firstFailed = now
			locked = true
			if dur > retryAfter {
				retryAfter = dur
			}
		}
	}
	return locked, retryAfter
}

// RecordSuccess clears account-specific failures while retaining broad IP-level burst tracking.
func (l *AuthRateLimiter) RecordSuccess(ip, username string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if username != "" {
		delete(l.buckets, "acct:"+ip+":"+username)
	}
}

type bucketSpec struct {
	key         string
	maxFailures int
}

func (l *AuthRateLimiter) bucketSpecs(ip, username string) []bucketSpec {
	if username == "" {
		return []bucketSpec{{key: "ip:" + ip, maxFailures: l.maxFailures}}
	}
	globalMax := globalAccountMaxFailures
	if l.maxFailures*4 > globalMax {
		globalMax = l.maxFailures * 4
	}
	return []bucketSpec{
		{key: "ip:" + ip, maxFailures: l.maxFailures},
		{key: "acct:" + ip + ":" + username, maxFailures: l.maxFailures},
		{key: "globalacct:" + username, maxFailures: globalMax},
	}
}

func bucketKeys(ip, username string) []string {
	if username == "" {
		return []string{"ip:" + ip}
	}
	return []string{"ip:" + ip, "acct:" + ip + ":" + username, "globalacct:" + username}
}

func (l *AuthRateLimiter) maybeCleanupLocked(now time.Time) {
	if now.Sub(l.lastCleanup) < cleanupMinInterval {
		return
	}
	if now.Sub(l.lastCleanup) < 5*time.Minute && len(l.buckets) < 4096 {
		return
	}
	l.cleanupLocked(now)
}

func (l *AuthRateLimiter) cleanupLocked(now time.Time) {
	for k, b := range l.buckets {
		if now.Sub(b.firstFailed) > l.window*2 && now.After(b.lockedUntil) {
			delete(l.buckets, k)
		}
	}
	l.lastCleanup = now
}

func (l *AuthRateLimiter) evictOldestLocked() {
	var oldestKey string
	var oldestTime time.Time
	first := true
	for k, b := range l.buckets {
		if first || b.firstFailed.Before(oldestTime) {
			oldestKey = k
			oldestTime = b.firstFailed
			first = false
		}
	}
	if !first {
		delete(l.buckets, oldestKey)
	}
}

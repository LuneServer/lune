package security

import (
	"fmt"
	"hash/fnv"
	"log/slog"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	guardShards       = 64
	guardMaxPerShard  = 256 // 64 * 256 = 16,384 tracked IPs max
	strikeWindow      = 10 * time.Minute
	strikeBanLimit    = 15
	baseAutoBanTTL    = 15 * time.Minute
	maxAutoBanTTL     = 24 * time.Hour
	publicRatePerSec  = 15.0
	publicBurst       = 30.0
	authedRatePerSec  = 60.0
	authedBurst       = 150.0
)

// IPBan describes an active automatic or manual IP ban.
type IPBan struct {
	IP        string     `json:"ip"`
	Reason    string     `json:"reason"`
	Strikes   int        `json:"strikes"`
	BannedAt  time.Time  `json:"banned_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Permanent bool       `json:"permanent"`
}

type ipEntry struct {
	tokens     float64
	lastRefill time.Time
	strikes    int
	firstStrike time.Time
	banCount   int
	bannedAt   time.Time
	bannedTill time.Time
	permanent  bool
	banReason  string
	lastSeen   time.Time
}

type guardShard struct {
	mu      sync.Mutex
	entries map[string]*ipEntry
}

// EdgeGuard provides low-overhead sharded per-IP rate limiting, automatic temporary
// IP banning for abusive scanners/forged tokens, and manual CIDR allowlist/blocklist enforcement.
type EdgeGuard struct {
	shards [guardShards]guardShard
	now    func() time.Time

	listMu    sync.RWMutex
	allowlist []netip.Prefix
	blocklist []netip.Prefix

	onBanMu sync.RWMutex
	onBan   func(ip, reason string, dur time.Duration, permanent bool)
	onUnban func(ip string)
}

// NewEdgeGuard constructs a new EdgeGuard with default thresholds.
func NewEdgeGuard() *EdgeGuard {
	return NewEdgeGuardWithClock(time.Now)
}

// NewEdgeGuardWithClock constructs an EdgeGuard using a custom clock (useful for tests).
func NewEdgeGuardWithClock(now func() time.Time) *EdgeGuard {
	g := &EdgeGuard{now: now}
	for i := range g.shards {
		g.shards[i].entries = make(map[string]*ipEntry)
	}
	return g
}

// SetOnBan registers a callback invoked whenever an IP is automatically or manually banned.
func (g *EdgeGuard) SetOnBan(fn func(ip, reason string, dur time.Duration, permanent bool)) {
	g.onBanMu.Lock()
	g.onBan = fn
	g.onBanMu.Unlock()
}

// SetOnUnban registers a callback invoked whenever an IP ban is manually removed.
func (g *EdgeGuard) SetOnUnban(fn func(ip string)) {
	g.onBanMu.Lock()
	g.onUnban = fn
	g.onBanMu.Unlock()
}

// LoadBans hydrates previously persisted bans into memory at startup without triggering onBan callbacks.
func (g *EdgeGuard) LoadBans(bans []IPBan) {
	now := g.now()
	for _, b := range bans {
		if !b.Permanent && (b.ExpiresAt == nil || !now.Before(*b.ExpiresAt)) {
			continue
		}
		addr, err := netip.ParseAddr(strings.TrimSpace(b.IP))
		if err != nil {
			continue
		}
		ipStr := addr.Unmap().String()
		sh := g.shardFor(ipStr)
		sh.mu.Lock()
		e := &ipEntry{
			tokens:     publicBurst,
			lastRefill: now,
			lastSeen:   now,
			bannedAt:   b.BannedAt,
			banReason:  b.Reason,
			permanent:  b.Permanent,
		}
		if !b.Permanent && b.ExpiresAt != nil {
			e.bannedTill = *b.ExpiresAt
		}
		sh.entries[ipStr] = e
		sh.mu.Unlock()
	}
}

// UpdateLists atomically replaces the manual IP allowlist and blocklist CIDRs.
func (g *EdgeGuard) UpdateLists(allowlist, blocklist []netip.Prefix) {
	var allowCopy, blockCopy []netip.Prefix
	if len(allowlist) > 0 {
		allowCopy = make([]netip.Prefix, len(allowlist))
		copy(allowCopy, allowlist)
	}
	if len(blocklist) > 0 {
		blockCopy = make([]netip.Prefix, len(blocklist))
		copy(blockCopy, blocklist)
	}
	g.listMu.Lock()
	g.allowlist = allowCopy
	g.blocklist = blockCopy
	g.listMu.Unlock()
}

// IsAllowlisted reports whether addr matches the configured manual IP allowlist.
func (g *EdgeGuard) IsAllowlisted(addr netip.Addr) bool {
	if !addr.IsValid() {
		return false
	}
	addr = addr.Unmap()
	g.listMu.RLock()
	defer g.listMu.RUnlock()
	for _, p := range g.allowlist {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// IsBlocklisted reports whether addr matches the configured manual IP blocklist.
func (g *EdgeGuard) IsBlocklisted(addr netip.Addr) bool {
	if !addr.IsValid() {
		return false
	}
	addr = addr.Unmap()
	g.listMu.RLock()
	defer g.listMu.RUnlock()
	for _, p := range g.blocklist {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// CheckRequest evaluates allowlist, blocklist, active bans, and token-bucket rate limits.
// Returns (allowed, statusCode, retryAfter).
func (g *EdgeGuard) CheckRequest(addr netip.Addr, authenticated, bypassRateLimit bool) (bool, int, time.Duration) {
	if !addr.IsValid() {
		return true, 0, 0
	}
	addr = addr.Unmap()
	if g.IsAllowlisted(addr) {
		return true, 0, 0
	}
	if g.IsBlocklisted(addr) {
		return false, http.StatusForbidden, 0
	}

	ip := addr.String()
	sh := g.shardFor(ip)
	now := g.now()

	sh.mu.Lock()
	defer sh.mu.Unlock()

	e, ok := sh.entries[ip]
	if ok {
		if e.permanent {
			return false, http.StatusForbidden, 0
		}
		if now.Before(e.bannedTill) {
			return false, http.StatusTooManyRequests, e.bannedTill.Sub(now)
		}
	}

	if bypassRateLimit {
		if ok {
			e.lastSeen = now
		}
		return true, 0, 0
	}

	rate := publicRatePerSec
	burst := publicBurst
	if authenticated {
		rate = authedRatePerSec
		burst = authedBurst
	}

	if !ok {
		if len(sh.entries) >= guardMaxPerShard {
			evictOldestEntryLocked(sh.entries, now)
		}
		e = &ipEntry{
			tokens:     burst - 1,
			lastRefill: now,
			lastSeen:   now,
		}
		sh.entries[ip] = e
		return true, 0, 0
	}

	elapsed := now.Sub(e.lastRefill).Seconds()
	if elapsed > 0 {
		e.tokens += elapsed * rate
		if e.tokens > burst {
			e.tokens = burst
		}
		e.lastRefill = now
	}
	e.lastSeen = now

	if e.tokens < 1.0 {
		waitSec := (1.0 - e.tokens) / rate
		retryAfter := time.Duration(waitSec * float64(time.Second))
		if retryAfter < time.Second {
			retryAfter = time.Second
		}
		return false, http.StatusTooManyRequests, retryAfter
	}

	e.tokens -= 1.0
	return true, 0, 0
}

// RecordAbuse adds weighted abuse strikes for ip and automatically bans the IP if it crosses the threshold.
func (g *EdgeGuard) RecordAbuse(ip, reason string, weight int) bool {
	if ip == "" || weight <= 0 {
		return false
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	if g.IsAllowlisted(addr) || addr.IsLoopback() {
		return false
	}

	ipStr := addr.String()
	sh := g.shardFor(ipStr)
	now := g.now()

	var banned bool
	var banDur time.Duration

	sh.mu.Lock()
	e, ok := sh.entries[ipStr]
	if !ok {
		if len(sh.entries) >= guardMaxPerShard {
			evictOldestEntryLocked(sh.entries, now)
		}
		e = &ipEntry{
			tokens:      publicBurst,
			lastRefill:  now,
			firstStrike: now,
			lastSeen:    now,
		}
		sh.entries[ipStr] = e
	}
	e.lastSeen = now

	if e.permanent || now.Before(e.bannedTill) {
		sh.mu.Unlock()
		return false
	}

	if e.firstStrike.IsZero() || now.Sub(e.firstStrike) > strikeWindow {
		e.strikes = 0
		e.firstStrike = now
	}
	e.strikes += weight

	if e.strikes >= strikeBanLimit {
		e.banCount++
		banDur = baseAutoBanTTL << min(e.banCount-1, 6)
		if banDur > maxAutoBanTTL {
			banDur = maxAutoBanTTL
		}
		e.bannedAt = now
		e.bannedTill = now.Add(banDur)
		e.banReason = reason
		e.strikes = 0
		e.firstStrike = now
		banned = true
	}
	sh.mu.Unlock()

	if banned {
		g.notifyBan(ipStr, reason, banDur, false)
	}
	return banned
}

// BanIP manually bans an IP address for dur (or permanently if dur <= 0).
func (g *EdgeGuard) BanIP(ip, reason string, dur time.Duration) error {
	addr, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return fmt.Errorf("invalid IP address %q: %w", ip, err)
	}
	addr = addr.Unmap()
	ipStr := addr.String()
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "manual admin ban"
	}

	sh := g.shardFor(ipStr)
	now := g.now()
	permanent := dur <= 0

	sh.mu.Lock()
	e, ok := sh.entries[ipStr]
	if !ok {
		if len(sh.entries) >= guardMaxPerShard {
			evictOldestEntryLocked(sh.entries, now)
		}
		e = &ipEntry{
			tokens:     publicBurst,
			lastRefill: now,
		}
		sh.entries[ipStr] = e
	}
	e.lastSeen = now
	e.bannedAt = now
	e.banReason = reason
	e.permanent = permanent
	if permanent {
		e.bannedTill = time.Time{}
	} else {
		e.bannedTill = now.Add(dur)
	}
	sh.mu.Unlock()

	g.notifyBan(ipStr, reason, dur, permanent)
	return nil
}

// UnbanIP clears any active automatic or manual ban for ip.
func (g *EdgeGuard) UnbanIP(ip string) bool {
	addr, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return false
	}
	ipStr := addr.Unmap().String()
	sh := g.shardFor(ipStr)
	sh.mu.Lock()
	var wasBanned bool
	if e, ok := sh.entries[ipStr]; ok {
		wasBanned = e.permanent || g.now().Before(e.bannedTill)
		e.permanent = false
		e.bannedTill = time.Time{}
		e.banReason = ""
		e.strikes = 0
		e.banCount = 0
	}
	sh.mu.Unlock()

	g.onBanMu.RLock()
	fn := g.onUnban
	g.onBanMu.RUnlock()
	if fn != nil {
		fn(ipStr)
	}
	return wasBanned
}

// ListBans returns all currently active IP bans sorted by most recent first.
func (g *EdgeGuard) ListBans() []IPBan {
	now := g.now()
	out := make([]IPBan, 0)
	for i := range g.shards {
		sh := &g.shards[i]
		sh.mu.Lock()
		for ip, e := range sh.entries {
			if e.permanent || now.Before(e.bannedTill) {
				ban := IPBan{
					IP:        ip,
					Reason:    e.banReason,
					Strikes:   e.strikes,
					BannedAt:  e.bannedAt.UTC(),
					Permanent: e.permanent,
				}
				if !e.permanent {
					exp := e.bannedTill.UTC()
					ban.ExpiresAt = &exp
				}
				out = append(out, ban)
			}
		}
		sh.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].BannedAt.After(out[j].BannedAt)
	})
	return out
}

func (g *EdgeGuard) notifyBan(ip, reason string, dur time.Duration, permanent bool) {
	g.onBanMu.RLock()
	fn := g.onBan
	g.onBanMu.RUnlock()
	if fn != nil {
		fn(ip, reason, dur, permanent)
	}
}

func (g *EdgeGuard) shardFor(ip string) *guardShard {
	h := fnv.New32a()
	_, _ = h.Write([]byte(ip))
	return &g.shards[h.Sum32()%guardShards]
}

func evictOldestEntryLocked(entries map[string]*ipEntry, now time.Time) {
	var oldestKey string
	var oldestTime time.Time
	first := true
	for k, e := range entries {
		// Prefer never evicting active bans when non-banned entries exist.
		if e.permanent || now.Before(e.bannedTill) {
			continue
		}
		if first || e.lastSeen.Before(oldestTime) {
			oldestKey = k
			oldestTime = e.lastSeen
			first = false
		}
	}
	if !first {
		delete(entries, oldestKey)
	}
}

// IsPrivateOrLocalIP reports whether addr is loopback, RFC1918 private, IPv6 ULA,
// link-local, or Tailscale CGNAT (100.64.0.0/10).
func IsPrivateOrLocalIP(addr netip.Addr) bool {
	if !addr.IsValid() {
		return false
	}
	addr = addr.Unmap()
	if addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() {
		return true
	}
	tailscaleCGNAT := netip.MustParsePrefix("100.64.0.0/10")
	return tailscaleCGNAT.Contains(addr)
}

type statusCaptureWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusCaptureWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusCaptureWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusCaptureWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// EdgeGuardMiddleware enforces allowlist/blocklist, per-IP rate limits, and
// automatic strike recording for forged stream tokens and API scanning.
func EdgeGuardMiddleware(guard *EdgeGuard, resolver *ClientIPResolver, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if guard == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			addr := resolver.Resolve(r)
			path := r.URL.Path
			isStream := strings.HasPrefix(path, "/stream/")
			isHashedAsset := strings.HasPrefix(path, "/assets/")
			hasCreds := strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !hasCreds {
				if c, err := r.Cookie("lune_session"); err == nil && c.Value != "" {
					hasCreds = true
				}
			}

			allowed, status, retryAfter := guard.CheckRequest(addr, hasCreds, isStream || isHashedAsset)
			if !allowed {
				if retryAfter > 0 {
					secs := int(retryAfter.Seconds())
					if secs < 1 {
						secs = 1
					}
					w.Header().Set("Retry-After", strconv.Itoa(secs))
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				if status == http.StatusForbidden {
					_, _ = w.Write([]byte(`{"status":403,"title":"Forbidden","detail":"access denied"}`))
				} else {
					_, _ = w.Write([]byte(`{"status":429,"title":"Too Many Requests","detail":"rate limit exceeded"}`))
				}
				return
			}

			sw := &statusCaptureWriter{ResponseWriter: w}
			next.ServeHTTP(sw, r)

			code := sw.status
			if code == 0 {
				code = http.StatusOK
			}

			// Accrue abuse strikes for forged stream tokens or unauthenticated API scanning.
			if addr.IsValid() {
				ipStr := addr.Unmap().String()
				if isStream && (code == http.StatusUnauthorized || code == http.StatusForbidden) {
					if guard.RecordAbuse(ipStr, "invalid stream token", 3) && logger != nil {
						logger.Warn("security.ip_banned", "ip", ipStr, "reason", "invalid stream token")
					}
				} else if !hasCreds && strings.HasPrefix(path, "/api/") && (code == http.StatusNotFound || code == http.StatusUnauthorized) {
					// Ignore normal initial session check (/api/v1/auth/me) which returns 401 before login.
					if path != "/api/v1/auth/me" {
						if guard.RecordAbuse(ipStr, "unauthenticated api probing", 1) && logger != nil {
							logger.Warn("security.ip_banned", "ip", ipStr, "reason", "unauthenticated api probing")
						}
					}
				}
			}
		})
	}
}

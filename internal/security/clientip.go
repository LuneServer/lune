package security

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
)

// ClientIPResolver extracts the authentic client IP address.
// By default (no trusted proxies), X-Forwarded-For is ignored to prevent spoofing.
// ClientIPResolver is safe for concurrent use and supports live trusted-CIDR updates.
type ClientIPResolver struct {
	mu      sync.RWMutex
	trusted []netip.Prefix
}

// NewClientIPResolver creates a resolver with optional explicitly trusted proxy CIDRs.
func NewClientIPResolver(trusted []netip.Prefix) *ClientIPResolver {
	r := &ClientIPResolver{}
	r.UpdateTrusted(trusted)
	return r
}

// UpdateTrusted replaces the resolver's trusted proxy CIDRs safely at runtime.
func (r *ClientIPResolver) UpdateTrusted(trusted []netip.Prefix) {
	var cp []netip.Prefix
	if len(trusted) > 0 {
		cp = make([]netip.Prefix, len(trusted))
		copy(cp, trusted)
	}
	r.mu.Lock()
	r.trusted = cp
	r.mu.Unlock()
}

// Resolve returns the client's IP address for the request.
func (r *ClientIPResolver) Resolve(req *http.Request) netip.Addr {
	peer := parseRemoteAddr(req.RemoteAddr)
	if !peer.IsValid() {
		return peer
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	if len(r.trusted) == 0 || !r.isTrustedLocked(peer) {
		return peer
	}

	xff := req.Header.Get("X-Forwarded-For")
	if xff == "" {
		return peer
	}

	// Walk right-to-left and stop at the first hop not in our trusted CIDR list.
	parts := strings.Split(xff, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		cand, err := netip.ParseAddr(strings.TrimSpace(parts[i]))
		if err != nil {
			return peer
		}
		cand = cand.Unmap()
		if !r.isTrustedLocked(cand) {
			return cand
		}
	}
	return peer
}

// IsHTTPS reports whether the client connected over TLS directly or via a trusted
// reverse proxy / Tailscale serve terminating TLS (X-Forwarded-Proto: https).
func (r *ClientIPResolver) IsHTTPS(req *http.Request) bool {
	if req.TLS != nil {
		return true
	}
	peer := parseRemoteAddr(req.RemoteAddr)
	if !peer.IsValid() {
		return false
	}

	r.mu.RLock()
	trusted := len(r.trusted) > 0 && r.isTrustedLocked(peer)
	r.mu.RUnlock()
	if !trusted {
		return false
	}

	rawProto := req.Header.Get("X-Forwarded-Proto")
	if rawProto == "" {
		return false
	}
	parts := strings.Split(rawProto, ",")
	proto := strings.TrimSpace(parts[len(parts)-1])
	return strings.EqualFold(proto, "https")
}

func (r *ClientIPResolver) isTrustedLocked(addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, pfx := range r.trusted {
		if pfx.Contains(addr) {
			return true
		}
	}
	return false
}

func parseRemoteAddr(remote string) netip.Addr {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(host))
	if err != nil {
		return netip.Addr{}
	}
	return addr.Unmap()
}

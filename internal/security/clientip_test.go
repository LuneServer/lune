package security

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestClientIPResolver_IgnoresSpoofedHeaderByDefault(t *testing.T) {
	r := NewClientIPResolver(nil)
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "198.51.100.10:54321"
	req.Header.Set("X-Forwarded-For", "203.0.113.9")

	got := r.Resolve(req)
	if got != netip.MustParseAddr("198.51.100.10") {
		t.Fatalf("expected direct peer IP 198.51.100.10, got %v", got)
	}
}

func TestClientIPResolver_TrustsExplicitProxyCIDR(t *testing.T) {
	r := NewClientIPResolver([]netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")})
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.5:443"
	req.Header.Set("X-Forwarded-For", "1.1.1.1, 203.0.113.42, 10.0.0.2")

	got := r.Resolve(req)
	if got != netip.MustParseAddr("203.0.113.42") {
		t.Fatalf("expected rightmost untrusted IP 203.0.113.42, got %v", got)
	}
}

func TestClientIPResolver_IsHTTPSUsesRightmostXForwardedProto(t *testing.T) {
	r := NewClientIPResolver([]netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")})

	// Attacker prepends "https" before proxy appends "http" -> rightmost is "http" (not HTTPS).
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.5:443"
	req.Header.Set("X-Forwarded-Proto", "https, http")
	if r.IsHTTPS(req) {
		t.Fatal("expected IsHTTPS=false when rightmost X-Forwarded-Proto is http")
	}

	// Client sends "http", proxy appends "https" -> rightmost is "https".
	req2, _ := http.NewRequest(http.MethodGet, "/", nil)
	req2.RemoteAddr = "10.0.0.5:443"
	req2.Header.Set("X-Forwarded-Proto", "http, https")
	if !r.IsHTTPS(req2) {
		t.Fatal("expected IsHTTPS=true when rightmost X-Forwarded-Proto is https")
	}
}

func TestHeadersMiddleware_HSTSBehindProxyAndSecurityHeaders(t *testing.T) {
	r := NewClientIPResolver([]netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")})
	h := HeadersMiddleware(r)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Over trusted HTTPS proxy -> emits HSTS, CSP, Permissions-Policy.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.5:443"
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Strict-Transport-Security"); got != "max-age=31536000; includeSubDomains" {
		t.Fatalf("HSTS = %q, want max-age=31536000; includeSubDomains", got)
	}
	if got := rec.Header().Get("Permissions-Policy"); got != "camera=(), microphone=(), geolocation=(), payment=()" {
		t.Fatalf("Permissions-Policy = %q", got)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "object-src 'none'") {
		t.Fatalf("unexpected CSP: %q", csp)
	}

	// Untrusted peer spoofing X-Forwarded-Proto -> no HSTS.
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.RemoteAddr = "198.51.100.5:12345"
	req2.Header.Set("X-Forwarded-Proto", "https")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if got := rec2.Header().Get("Strict-Transport-Security"); got != "" {
		t.Fatalf("expected no HSTS for untrusted peer, got %q", got)
	}
}

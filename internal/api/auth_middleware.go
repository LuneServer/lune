package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/LuneServer/lune/internal/auth"
	"github.com/LuneServer/lune/internal/config"
	"github.com/LuneServer/lune/internal/security"
	"github.com/LuneServer/lune/internal/store"
)

const sessionCookieName = "lune_session"

type contextKey string

const (
	ctxKeyClientIP  contextKey = "client_ip"
	ctxKeyUserAgent contextKey = "user_agent"
	ctxKeyIsHTTPS   contextKey = "is_https"
	ctxKeyUser      contextKey = "auth_user"
	ctxKeySession   contextKey = "auth_session"
)

// requestMetaMiddleware resolves authentic client IP, TLS/proxy scheme, and User-Agent into request context.
func requestMetaMiddleware(resolver *security.ClientIPResolver) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := ""
			if addr := resolver.Resolve(r); addr.IsValid() {
				ip = addr.String()
			}
			ctx := context.WithValue(r.Context(), ctxKeyClientIP, ip)
			ctx = context.WithValue(ctx, ctxKeyUserAgent, r.UserAgent())
			ctx = context.WithValue(ctx, ctxKeyIsHTTPS, resolver.IsHTTPS(r))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// optionalAuthMiddleware resolves either Bearer token or HttpOnly session cookie when present.
func optionalAuthMiddleware(authSvc *auth.Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rawToken := extractToken(r)
			if rawToken != "" {
				user, sess, err := authSvc.Authenticate(r.Context(), rawToken, clientIPFromContext(r.Context()))
				if err == nil {
					ctx := context.WithValue(r.Context(), ctxKeyUser, user)
					ctx = context.WithValue(ctx, ctxKeySession, sess)
					r = r.WithContext(ctx)
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// totpEnrollmentGateMiddleware blocks API/stream access with 403 for authenticated users
// whose account has totp_required=true until they finish enrolling TOTP 2FA.
func totpEnrollmentGateMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if user, _, ok := currentPrincipal(r.Context()); ok && user.TOTPRequired && !user.TOTPEnabled {
				p := r.URL.Path
				allowed := p == "/healthz" ||
					p == "/api/v1/system/info" ||
					strings.HasPrefix(p, "/api/v1/auth/") ||
					strings.HasPrefix(p, "/api/v1/users/me/totp/") ||
					(p == "/api/v1/users/me" && r.Method == http.MethodGet) ||
					(!strings.HasPrefix(p, "/api/") && !strings.HasPrefix(p, "/stream/"))
				if !allowed {
					writeJSONError(w, http.StatusForbidden, "two-factor authentication setup is required before accessing this server")
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// adminLANOnlyMiddleware restricts /api/v1/admin/* endpoints to loopback, LAN/RFC1918,
// Tailscale CGNAT, or explicitly allowlisted IPs when AdminLANOnly is enabled.
func adminLANOnlyMiddleware(runtimeNet *runtimeNetwork, resolver *security.ClientIPResolver, guard *security.EdgeGuard) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if runtimeNet != nil && runtimeNet.AdminLANOnly() && strings.HasPrefix(r.URL.Path, "/api/v1/admin/") {
				addr := resolver.Resolve(r)
				if !security.IsPrivateOrLocalIP(addr) && (guard == nil || !guard.IsAllowlisted(addr)) {
					writeJSONError(w, http.StatusForbidden, "admin console is restricted to local network or allowlisted IPs")
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// csrfMiddleware enforces Fetch Metadata and Origin checks on state-changing requests
// authenticated via the lune_session cookie (without an Authorization: Bearer header).
func csrfMiddleware(runtimeNet *runtimeNetwork) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			default:
				next.ServeHTTP(w, r)
				return
			}

			// Bearer-authenticated API requests are not vulnerable to cookie CSRF.
			if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") && strings.TrimSpace(strings.TrimPrefix(h, "Bearer ")) != "" {
				next.ServeHTTP(w, r)
				return
			}

			// Only enforce when authenticating via the session cookie.
			if c, err := r.Cookie(sessionCookieName); err != nil || c.Value == "" {
				next.ServeHTTP(w, r)
				return
			}

			if sfs := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))); sfs == "cross-site" || sfs == "same-site" {
				writeJSONError(w, http.StatusForbidden, "cross-origin state-changing request rejected")
				return
			}

			if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
				u, err := url.Parse(origin)
				if err != nil || u.Host == "" {
					writeJSONError(w, http.StatusForbidden, "invalid origin header")
					return
				}
				allowed := strings.EqualFold(u.Host, r.Host)
				if !allowed && runtimeNet != nil {
					if pub := strings.TrimSpace(runtimeNet.PublicURL()); pub != "" {
						if pubURL, err := url.Parse(pub); err == nil && pubURL.Host != "" && strings.EqualFold(u.Host, pubURL.Host) {
							allowed = true
						}
					}
				}
				if !allowed {
					writeJSONError(w, http.StatusForbidden, "cross-origin state-changing request rejected")
					return
				}
			}

			next.ServeHTTP(w, r)
		})
	}
}

func extractToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		if tok := strings.TrimSpace(strings.TrimPrefix(h, "Bearer ")); tok != "" {
			return tok
		}
	}
	if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
		return c.Value
	}
	return ""
}

func clientIPFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyClientIP).(string)
	return v
}

func userAgentFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyUserAgent).(string)
	return v
}

func useSecureCookie(ctx context.Context, runtimeNet *runtimeNetwork) bool {
	if runtimeNet != nil {
		if runtimeNet.EffectiveMode() == config.EdgeModeACME {
			return true
		}
		if strings.HasPrefix(strings.ToLower(runtimeNet.PublicURL()), "https://") {
			return true
		}
	}
	v, _ := ctx.Value(ctxKeyIsHTTPS).(bool)
	return v
}

func currentPrincipal(ctx context.Context) (store.User, store.Session, bool) {
	u, okU := ctx.Value(ctxKeyUser).(store.User)
	s, okS := ctx.Value(ctxKeySession).(store.Session)
	return u, s, okU && okS
}

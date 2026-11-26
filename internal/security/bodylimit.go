package security

import (
	"net/http"
	"strings"
)

const (
	DefaultMaxBodyBytes int64 = 1 << 20 // 1 MiB
	AvatarMaxBodyBytes  int64 = 4 << 20 // 4 MiB (avatar handler enforces 2 MiB image cap)
)

// BodyLimitMiddleware caps incoming request bodies at the HTTP layer to prevent
// memory exhaustion from oversized payloads.
func BodyLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.Body != http.NoBody {
			limit := DefaultMaxBodyBytes
			if strings.HasSuffix(r.URL.Path, "/avatar") {
				limit = AvatarMaxBodyBytes
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		next.ServeHTTP(w, r)
	})
}

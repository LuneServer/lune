package web

import (
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// NewSPAHandler serves a built static web bundle (e.g. lune-web/dist) with
// clean SPA history fallback to index.html and aggressive caching for hashed assets.
func NewSPAHandler(distDir string) http.Handler {
	indexPath := filepath.Join(distDir, "index.html")
	fileServer := http.FileServer(http.Dir(distDir))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}

		// Never hijack API, streaming, or OpenAPI endpoints.
		p := r.URL.Path
		if strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/stream/") ||
			p == "/healthz" || p == "/docs" || strings.HasPrefix(p, "/openapi.") {
			http.NotFound(w, r)
			return
		}

		cleaned := path.Clean("/" + p)
		if cleaned == "/" {
			serveIndex(w, r, indexPath)
			return
		}
		rel := strings.TrimPrefix(cleaned, "/")
		for _, seg := range strings.Split(rel, "/") {
			if strings.HasPrefix(seg, ".") {
				http.NotFound(w, r)
				return
			}
		}

		full := filepath.Join(distDir, filepath.FromSlash(rel))
		if info, err := os.Stat(full); err == nil && !info.IsDir() {
			if strings.HasPrefix(cleaned, "/assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "public, max-age=300")
			}
			fileServer.ServeHTTP(w, r)
			return
		}

		serveIndex(w, r, indexPath)
	})
}

func serveIndex(w http.ResponseWriter, r *http.Request, indexPath string) {
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFile(w, r, indexPath)
}

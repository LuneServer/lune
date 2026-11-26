package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestSPAHandler(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>lune-web</html>"), 0o644)
	_ = os.MkdirAll(filepath.Join(dir, "assets"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "assets", "app.js"), []byte("console.log('lune')"), 0o644)

	h := NewSPAHandler(dir)

	// Static asset served with immutable cache header.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "console.log('lune')" {
		t.Fatalf("static asset: status=%d body=%q", rec.Code, rec.Body.String())
	}

	// Client-side route falls back to index.html.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/console/libraries", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "<html>lune-web</html>" {
		t.Fatalf("spa fallback: status=%d body=%q", rec.Code, rec.Body.String())
	}

	// Unknown /api/* route stays 404 (never hijacked by SPA).
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/does-not-exist", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("api 404: status=%d, want 404", rec.Code)
	}

	// Dotfiles are rejected with 404 even if present on disk.
	_ = os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=1"), 0o644)
	_ = os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("[core]"), 0o644)
	for _, p := range []string{"/.env", "/.git/config", "/assets/../.env"} {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("dotfile %q: status=%d, want 404", p, rec.Code)
		}
	}
}

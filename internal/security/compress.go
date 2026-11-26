package security

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"sync"
)

var gzipWriterPool = sync.Pool{
	New: func() any {
		w, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
		return w
	},
}

// CompressMiddleware applies fast gzip compression to JSON, JS, CSS, HTML, and SVG responses
// while explicitly bypassing media streams, Range requests, and binary artwork so streaming
// stays zero-overhead.
func CompressMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead ||
			r.Header.Get("Range") != "" ||
			r.Header.Get("Upgrade") != "" ||
			!strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}

		p := r.URL.Path
		if strings.HasPrefix(p, "/stream/") ||
			strings.HasPrefix(p, "/api/v1/artwork/") ||
			strings.Contains(p, "/artwork/") ||
			strings.HasSuffix(p, "/avatar") {
			next.ServeHTTP(w, r)
			return
		}

		w.Header().Add("Vary", "Accept-Encoding")
		gw := &lazyGzipResponseWriter{ResponseWriter: w}
		defer gw.Close()
		next.ServeHTTP(gw, r)
	})
}

type lazyGzipResponseWriter struct {
	http.ResponseWriter
	gz          *gzip.Writer
	wroteHeader bool
	compressing bool
}

func isCompressibleContentType(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(ct))
	if idx := strings.IndexByte(ct, ';'); idx >= 0 {
		ct = strings.TrimSpace(ct[:idx])
	}
	switch {
	case strings.HasPrefix(ct, "text/"),
		ct == "application/json",
		ct == "application/problem+json",
		ct == "application/javascript",
		ct == "application/x-javascript",
		ct == "application/xml",
		ct == "image/svg+xml":
		return true
	default:
		return false
	}
}

func (w *lazyGzipResponseWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true

	// Never compress empty/no-body responses or already-encoded responses.
	if code == http.StatusNoContent || code == http.StatusNotModified || w.Header().Get("Content-Encoding") != "" {
		w.ResponseWriter.WriteHeader(code)
		return
	}

	if isCompressibleContentType(w.Header().Get("Content-Type")) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Del("Content-Length")
		w.compressing = true
		gz := gzipWriterPool.Get().(*gzip.Writer)
		gz.Reset(w.ResponseWriter)
		w.gz = gz
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *lazyGzipResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", http.DetectContentType(p))
		}
		w.WriteHeader(http.StatusOK)
	}
	if w.compressing && w.gz != nil {
		return w.gz.Write(p)
	}
	return w.ResponseWriter.Write(p)
}

func (w *lazyGzipResponseWriter) Flush() {
	if w.compressing && w.gz != nil {
		_ = w.gz.Flush()
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *lazyGzipResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *lazyGzipResponseWriter) Close() {
	if w.gz != nil {
		_ = w.gz.Close()
		w.gz.Reset(io.Discard)
		gzipWriterPool.Put(w.gz)
		w.gz = nil
	}
}

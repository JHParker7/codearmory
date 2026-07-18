package main

import (
	"net/http"
	"os"
	"path/filepath"
)

// newSPAHandler serves the built React SPA from dir: real files (hashed bundles,
// assets) are served directly, and any unmatched path falls back to index.html
// so client-side routing works. Only the fallback is rate-limited per client IP;
// static assets are not. filepath.Clean neutralizes any '../' traversal before
// the path is joined onto dir.
func newSPAHandler(dir string, limiter *ipRateLimiter) http.Handler {
	index := filepath.Join(dir, "index.html")
	fileServer := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		full := filepath.Join(dir, filepath.Clean("/"+r.URL.Path))
		if info, err := os.Stat(full); err == nil && !info.IsDir() {
			fileServer.ServeHTTP(w, r)
			return
		}
		if !limiter.allow(clientIP(r)) {
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		http.ServeFile(w, r, index)
	})
}

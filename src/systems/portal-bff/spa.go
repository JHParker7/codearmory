package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
			// Vite emits content-hashed asset URLs (/assets/index-<hash>.js), so a given
			// URL's bytes never change — cache them hard. Everything else (notably
			// index.html, whose URL is stable but whose contents change every deploy to
			// point at new asset hashes) must revalidate, or the browser keeps loading a
			// stale bundle after a deploy — the "my fix didn't take until I cleared the
			// cache" trap.
			if strings.HasPrefix(r.URL.Path, "/assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
			fileServer.ServeHTTP(w, r)
			return
		}
		if !limiter.allow(clientIP(r)) {
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		// The SPA-routing fallback returns index.html for every client-side path; it must
		// never be cached, so a deploy's new asset hashes are picked up on next load.
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, index)
	})
}

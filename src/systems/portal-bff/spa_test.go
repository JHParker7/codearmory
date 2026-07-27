package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// The SPA index.html must never be cached (so a deploy's new asset hashes are picked
// up), while content-hashed /assets/* may be cached immutably. Regression guard for
// the "stale bundle after deploy" trap.
func TestSPAHandler_CacheHeaders(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<!doctype html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	assets := filepath.Join(dir, "assets")
	if err := os.MkdirAll(assets, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(assets, "index-abc123.js"), []byte("console.log(1)"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newSPAHandler(dir, newIPRateLimiter(1000, 1000))

	cases := []struct {
		path, wantCC string
	}{
		{"/assets/index-abc123.js", "public, max-age=31536000, immutable"}, // hashed → immutable
		{"/index.html", "no-cache"},                                        // real index → revalidate
		{"/app/codearmory_git_factory", "no-cache"},                        // SPA fallback → revalidate
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))
		if got := rec.Header().Get("Cache-Control"); got != c.wantCC {
			t.Errorf("%s: Cache-Control = %q, want %q", c.path, got, c.wantCC)
		}
	}
}

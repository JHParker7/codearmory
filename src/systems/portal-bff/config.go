// Package main is the portal BFF — a small Go server that backs the React SPA.
// It serves the built SPA, proxies every /api/* request through to conductor
// (verbatim, except the normalized /state/* workspace routes and the /:svc/ui/*
// mini-portal streaming), and emits OpenTelemetry traces/metrics/logs. The SPA
// only ever talks to this server, never to conductor directly. Ported from the
// original Node/Express BFF to drop the npm runtime from the production image.
package main

import (
	"os"
	"strconv"
	"time"
)

var (
	// conductorURL is the API gateway every request is proxied to. Overridable in
	// tests. Defaults to the local compose endpoint.
	conductorURL = envOrDefault("CONDUCTOR_URL", "http://localhost:8080")
	listenPort   = envOrDefault("PORT", "3001")
	// publicDir holds the built SPA assets (index.html + hashed bundles). Absent
	// in dev, where Vite serves the SPA on its own port and proxies /api here.
	publicDir = envOrDefault("PUBLIC_DIR", "./public")
	// trustProxy mirrors Express's `trust proxy`: when not "false"/"0" the client
	// IP is read from X-Forwarded-For so rate-limit buckets are per real client.
	trustProxy    = envOrDefault("TRUST_PROXY", "1")
	stateCacheTTL = parseTTL()
)

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// parseTTL reads STATE_CACHE_TTL_MS. A missing, non-numeric, or non-positive
// value falls back to 2s (matching the Node BFF) rather than yielding a zero or
// negative TTL that would make cached entries either immortal or instantly stale.
func parseTTL() time.Duration {
	raw := os.Getenv("STATE_CACHE_TTL_MS")
	if raw == "" {
		return 2 * time.Second
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 2 * time.Second
	}
	return time.Duration(n) * time.Millisecond
}

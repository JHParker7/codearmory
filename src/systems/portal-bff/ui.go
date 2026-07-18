package main

import (
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
)

// A service name is a simple slug; the strict match also blocks any path-traversal
// attempt from smuggling through the service segment.
var svcRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// isServiceUIPath reports whether an /api-stripped path targets a service's
// embedded mini-portal, i.e. /{svc}/ui or /{svc}/ui/...
func isServiceUIPath(apiPath string) bool {
	trimmed := strings.TrimPrefix(apiPath, "/")
	parts := strings.SplitN(trimmed, "/", 3)
	return len(parts) >= 2 && parts[1] == "ui" && svcRe.MatchString(parts[0])
}

// handleServiceUI serves a registered service's embedded mini-portal. Unlike the
// JSON /api passthrough (which forces application/json and re-serializes the
// body), a mini-portal serves HTML/JS/CSS/font/image assets, so this streams the
// upstream response verbatim: the upstream Content-Type and the raw, binary-safe
// body. The bearer Authorization header is forwarded exactly as for /api, so the
// shell loads the iframe same-origin and never hands a token to a cross-origin
// frame.
func handleServiceUI(w http.ResponseWriter, r *http.Request) {
	// apiPath is the conductor path with the /api mount prefix stripped, e.g.
	// /blueprints/ui/asset.js — its first segment is the service name.
	apiPath := strings.TrimPrefix(r.URL.Path, "/api")
	svc := strings.SplitN(strings.TrimPrefix(apiPath, "/"), "/", 2)[0]
	if !svcRe.MatchString(svc) {
		writeJSONError(w, http.StatusBadRequest, "invalid service name")
		return
	}

	target := conductorURL + apiPath
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, _ := http.NewRequestWithContext(r.Context(), r.Method, target, nil)
	if auth := r.Header.Get("Authorization"); auth != "" {
		req.Header.Set("Authorization", auth)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		slog.ErrorContext(r.Context(), "upstream ui fetch failed", "error", err, "url", target)
		writeJSONError(w, http.StatusBadGateway, "upstream unavailable")
		return
	}
	defer resp.Body.Close()

	// Pass the upstream content type through verbatim (HTML/JS/CSS/font/image) —
	// unlike the JSON proxy — and stream the body as raw bytes to stay binary-safe.
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "" {
		w.Header().Set("Cache-Control", cc)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

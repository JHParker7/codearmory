package main

import (
	"log/slog"
	"net/http/httputil"
	"net/url"
	"strings"
)

// Conductor as the single web entry point.
//
// When PORTAL_URL is set, conductor reverse-proxies every request that does NOT resolve
// to a registered API route to the portal BFF. That makes one origin (one port /
// port-forward) serve both the JSON API and the web UI: the SPA loads from "/", its
// hashed assets from "/assets/...", and its API calls hit "/api/..." on the same host.
// Disabled (a non-API path 404s as before) when PORTAL_URL is empty.
var portalProxy *httputil.ReverseProxy

func initPortalProxy() {
	raw := envOrDefault("PORTAL_URL", "")
	if raw == "" {
		return
	}
	u, err := url.Parse(raw)
	if err != nil {
		slog.Error("invalid PORTAL_URL; portal proxying disabled", "url", raw, "error", err)
		return
	}
	portalProxy = httputil.NewSingleHostReverseProxy(u)
	slog.Info("portal proxying enabled — non-API paths served from the portal", "target", raw)
}

// stripAPIPrefix lets the SPA address the API under "/api" on the same origin it is
// served from: "/api/gatekeeper/login" is routed exactly as "/gatekeeper/login". This
// mirrors the portal BFF, which mounts the API at "/api" and strips it before
// forwarding to conductor — so the SPA behaves identically whether it talks to the BFF
// or straight to conductor. A bare "/api" becomes "/"; a non-/api path is untouched.
func stripAPIPrefix(path string) string {
	switch {
	case path == "/api":
		return "/"
	case strings.HasPrefix(path, "/api/"):
		return path[len("/api"):]
	default:
		return path
	}
}

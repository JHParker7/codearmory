package main

import (
	"embed"
	"net/http"
)

// The embedded mini-portal. A service that advertises a ui_path is rendered by the
// portal shell in an iframe at /api/<service><ui_path>/ with no change to the portal
// repo, so this is how a builder-deployed service gets a UI.
//
// It is one self-contained HTML file on purpose: the frame is loaded through the BFF,
// and a single document with inline CSS/JS has no relative asset URLs to resolve and
// no build step to keep in sync with the Go binary.
//
//go:embed ui/index.html
var uiFS embed.FS

// handleUI serves the mini-portal. The page itself carries no credential — it calls
// the API with relative fetches, and the BFF attaches the caller's bearer token.
func handleUI(w http.ResponseWriter, r *http.Request) {
	page, err := uiFS.ReadFile("ui/index.html")
	if err != nil {
		http.Error(w, "ui unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The shell re-loads the frame on navigation; no-cache keeps a redeployed UI from
	// being served stale out of the browser cache.
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(page)
}

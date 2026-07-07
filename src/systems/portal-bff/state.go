package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
)

// encodeWsPath rejects path-traversal and percent-encodes each segment of an
// untrusted workspace path, preserving the user/workspace slash structure. This
// stops a '../' segment from normalizing the request off the /blueprints/state/
// prefix (and mis-keying the cache). Returns ("", false) when the path is malformed.
func encodeWsPath(wsPath string) (string, bool) {
	segments := strings.Split(wsPath, "/")
	for _, s := range segments {
		if s == "" || s == "." || s == ".." {
			return "", false
		}
	}
	encoded := make([]string, len(segments))
	for i, s := range segments {
		encoded[i] = url.PathEscape(s)
	}
	return strings.Join(encoded, "/"), true
}

// handleState dispatches /state/* — the one place the SPA does not get a verbatim
// conductor passthrough. GET normalizes conductor's 200/204/423 branching into a
// single WorkspaceView (cached per token); DELETE and other write methods proxy
// through and invalidate the cache.
func handleState(cache *stateCache) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// The /api/state/ mount prefix is stripped here, leaving the raw workspace path.
		wsPath := strings.TrimPrefix(r.URL.Path, "/api/state/")
		safePath, ok := encodeWsPath(wsPath)
		if !ok {
			writeJSONError(w, http.StatusBadRequest, "invalid workspace path")
			return
		}
		switch r.Method {
		case http.MethodGet:
			handleStateGet(w, r, cache, safePath)
		case http.MethodDelete:
			handleStateDelete(w, r, cache, safePath)
		default:
			// POST/PUT/PATCH/LOCK/UNLOCK (terraform apply/lock): proxy and invalidate
			// so a subsequent GET is write-coherent rather than serving stale state.
			query := ""
			if r.URL.RawQuery != "" {
				query = "?" + r.URL.RawQuery
			}
			if err := proxyToUpstream(w, r, "/blueprints/state/"+safePath+query); err != nil {
				writeJSONError(w, http.StatusBadGateway, "upstream unavailable")
				return
			}
			cache.invalidate(safePath)
		}
	}
}

func handleStateGet(w http.ResponseWriter, r *http.Request, cache *stateCache, safePath string) {
	auth := r.Header.Get("Authorization")
	if v, ok := cache.get(safePath, auth); ok {
		recordCacheHit(r.Context())
		writeJSON(w, http.StatusOK, v)
		return
	}
	recordCacheMiss(r.Context())

	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, conductorURL+"/blueprints/state/"+safePath, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		slog.ErrorContext(r.Context(), "upstream state fetch failed", "error", err, "wsPath", safePath)
		writeJSONError(w, http.StatusBadGateway, "upstream unavailable")
		return
	}
	defer resp.Body.Close()

	var view workspaceView
	switch {
	case resp.StatusCode == http.StatusNoContent:
		view = fromEmpty()
	case resp.StatusCode != http.StatusLocked && (resp.StatusCode < 200 || resp.StatusCode >= 300):
		// A real upstream error (not 423): mirror the status and body verbatim.
		body, _ := io.ReadAll(resp.Body)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
		return
	default:
		// 200 or 423: the body should be JSON, but guard against an empty or
		// non-JSON payload so a malformed upstream response doesn't collapse into a
		// generic 502 and discard the real status (especially the 423 lock info).
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == http.StatusLocked {
			var lock lockInfo
			if len(body) > 0 {
				_ = json.Unmarshal(body, &lock) // best-effort: 423 lock info is optional
			}
			view = fromLocked(lock)
		} else {
			var st terraformState
			if len(body) > 0 {
				if err := json.Unmarshal(body, &st); err != nil {
					writeJSONError(w, http.StatusBadGateway, "invalid upstream response")
					return
				}
			}
			view = fromState(st)
		}
	}

	cache.set(safePath, auth, view)
	writeJSON(w, http.StatusOK, view)
}

func handleStateDelete(w http.ResponseWriter, r *http.Request, cache *stateCache, safePath string) {
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodDelete, conductorURL+"/blueprints/state/"+safePath, nil)
	if auth := r.Header.Get("Authorization"); auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		slog.ErrorContext(r.Context(), "upstream state delete failed", "error", err, "wsPath", safePath)
		writeJSONError(w, http.StatusBadGateway, "upstream unavailable")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		cache.invalidate(safePath)
	}
	w.WriteHeader(resp.StatusCode)
}

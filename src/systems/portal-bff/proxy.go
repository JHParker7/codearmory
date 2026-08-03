package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
)

// forwardCredentials copies the caller's credentials from an incoming request onto
// the upstream one.
//
// BOTH shapes matter. A bearer is what a token-holding client (CLI, script) sends.
// The Cookie is what the SPA sends: the portal keeps its session in an HttpOnly
// `armory_session` cookie, which JavaScript cannot read and therefore cannot turn
// into an Authorization header. Forwarding only the bearer silently breaks every
// browser request — the call reaches conductor carrying no credential at all and
// comes back 401, which reads as "logged out" the moment the page reloads.
func forwardCredentials(dst, src *http.Request) {
	if auth := src.Header.Get("Authorization"); auth != "" {
		dst.Header.Set("Authorization", auth)
	}
	if c := src.Header.Get("Cookie"); c != "" {
		dst.Header.Set("Cookie", c)
	}
}

// copySetCookie mirrors upstream Set-Cookie headers onto the client response. Must be
// called before WriteHeader.
//
// Login and logout carry their whole effect in this header: gatekeeper sets
// `armory_session` on login and expires it on logout. Dropping it means the browser
// never receives a session — login returns 200 with a token in the body and the very
// next request is unauthenticated — and logout leaves the cookie in place.
func copySetCookie(w http.ResponseWriter, resp *http.Response) {
	for _, c := range resp.Header.Values("Set-Cookie") {
		w.Header().Add("Set-Cookie", c)
	}
}

// credentialKey returns a stable per-caller key for cache partitioning.
//
// It must not be the Authorization header alone. A cookie-authenticated caller sends
// no such header, so every one of them would collapse onto the same empty key and
// share a single cache entry — one user's cached workspace state served to another.
// The session cookie is used when there is no bearer, so each caller keeps its own
// partition either way. Hashed so raw credentials are not retained as map keys.
func credentialKey(r *http.Request) string {
	raw := r.Header.Get("Authorization")
	if raw == "" {
		if c, err := r.Cookie("armory_session"); err == nil {
			raw = "cookie:" + c.Value
		}
	}
	if raw == "" {
		return "" // genuinely anonymous
	}
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// proxyToUpstream forwards an incoming SPA request to conductor and mirrors the
// response back. The bearer Authorization header (when present) is passed
// through; the request body is forwarded for any method that can carry one
// (including DELETE, which some routes require a body on). The upstream status
// and body are mirrored onto w. Returns an error on a transport failure so the
// caller can emit a 502.
//
// path is the conductor path (already prefixed/built by the caller), appended to
// conductorURL. The request context propagates, so a client disconnect cancels
// the upstream call.
func proxyToUpstream(w http.ResponseWriter, r *http.Request, path string) error {
	var body io.Reader
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		body = r.Body
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, conductorURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	forwardCredentials(req, r)

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	// Mirror the upstream status; only set the JSON content type when there is a
	// body, matching the Node BFF (an empty response ends with no content type).
	// Before WriteHeader: login/logout deliver their entire effect this way.
	copySetCookie(w, resp)
	if len(data) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(data)
	} else {
		w.WriteHeader(resp.StatusCode)
	}
	return nil
}

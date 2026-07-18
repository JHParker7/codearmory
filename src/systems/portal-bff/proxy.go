package main

import (
	"io"
	"net/http"
)

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
	if auth := r.Header.Get("Authorization"); auth != "" {
		req.Header.Set("Authorization", auth)
	}

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
	if len(data) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(data)
	} else {
		w.WriteHeader(resp.StatusCode)
	}
	return nil
}

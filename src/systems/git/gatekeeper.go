package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// checkGatekeeper authorises the incoming request by forwarding its Bearer token
// to gatekeeper's /check_permissions endpoint and returns the resolved user_id.
// Like forge and the former gitea_integration, the git broker authenticates with
// gatekeeper directly (forward_auth: true) so credentials are never issued on the
// basis of a conductor-injected X-User-ID header alone.
func checkGatekeeper(ctx context.Context, w http.ResponseWriter, r *http.Request, action, resource string) (string, bool) {
	token, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !found || token == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return "", false
	}

	body, _ := json.Marshal(map[string]string{
		"service":  "git",
		"resource": resource,
		"action":   action,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gatekeeperURL+"/check_permissions", bytes.NewReader(body))
	if err != nil {
		slog.ErrorContext(ctx, "git: failed to build gatekeeper request", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return "", false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := httpClient.Do(req)
	if err != nil {
		slog.ErrorContext(ctx, "git: gatekeeper check_permissions failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return "", false
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return "", false
	}
	if resp.StatusCode == http.StatusForbidden {
		http.Error(w, "forbidden", http.StatusForbidden)
		return "", false
	}
	if resp.StatusCode >= 500 {
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return "", false
	}
	if resp.StatusCode != http.StatusOK {
		http.Error(w, "forbidden", http.StatusForbidden)
		return "", false
	}

	var result struct {
		UserID string `json:"user_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || result.UserID == "" {
		slog.ErrorContext(ctx, "git: decode gatekeeper response", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return "", false
	}
	return result.UserID, true
}

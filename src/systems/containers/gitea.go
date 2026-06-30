package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

type cachedGiteaCreds struct {
	creds   registryCreds
	expires time.Time
}

var giteaCredsCache sync.Map

// resolveGiteaUserCreds fetches a per-user Gitea registry token from the
// gitea_integration service using the caller's own Bearer token for auth.
// Results are cached per user ID for 5 minutes to avoid a round-trip on
// every OCI blob chunk. Returns an error when GITEA_INTEGRATION_URL is unset,
// the user has no linked Gitea account, or the call fails — callers fall back
// to org or global credentials silently.
func resolveGiteaUserCreds(ctx context.Context, userID, bearerToken string) (registryCreds, error) {
	if giteaIntegrationURL == "" {
		return registryCreds{}, fmt.Errorf("gitea integration not configured")
	}

	if v, ok := giteaCredsCache.Load(userID); ok {
		if c := v.(cachedGiteaCreds); time.Now().Before(c.expires) {
			return c.creds, nil
		}
		giteaCredsCache.Delete(userID)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		giteaIntegrationURL+"/internal/registry-token", http.NoBody)
	if err != nil {
		return registryCreds{}, fmt.Errorf("build gitea token request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+bearerToken)

	resp, err := httpClient.Do(req)
	if err != nil {
		return registryCreds{}, fmt.Errorf("gitea token request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return registryCreds{}, fmt.Errorf("no linked gitea account")
	}
	if resp.StatusCode != http.StatusOK {
		return registryCreds{}, fmt.Errorf("gitea token request returned %d", resp.StatusCode)
	}

	var result struct {
		Username string `json:"username"`
		Token    string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return registryCreds{}, fmt.Errorf("decode gitea token response: %w", err)
	}
	if result.Username == "" || result.Token == "" {
		return registryCreds{}, fmt.Errorf("gitea token response missing fields")
	}

	creds := registryCreds{username: result.Username, password: result.Token}
	giteaCredsCache.Store(userID, cachedGiteaCreds{creds: creds, expires: time.Now().Add(5 * time.Minute)})
	return creds, nil
}

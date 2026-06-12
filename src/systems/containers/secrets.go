package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ctxCredsKey is the context key for per-request registry credentials.
// Using a private struct type (not a string) prevents collisions with any
// other package storing values in the same context.
type ctxCredsKey struct{}

type registryCreds struct {
	username string
	password string
}

type cachedCreds struct {
	creds   registryCreds
	expires time.Time
}

var orgCredsCache sync.Map

// resolveOrgCreds fetches the per-org registry credentials from Gatekeeper secrets.
// The secret value must be "username:token". Results are cached for 5 minutes to
// avoid a Gatekeeper round-trip on every OCI blob chunk.
// Returns an error (and falls back to global credentials) when:
//   - REGISTRY_ORG_SECRET_NAME is not set
//   - orgID is empty (user not in an org)
//   - the secret does not exist for this org
func resolveOrgCreds(ctx context.Context, orgID string) (registryCreds, error) {
	if orgSecretName == "" || orgID == "" {
		return registryCreds{}, errors.New("per-org credentials not configured")
	}

	if v, ok := orgCredsCache.Load(orgID); ok {
		if c := v.(cachedCreds); time.Now().Before(c.expires) {
			return c.creds, nil
		}
		orgCredsCache.Delete(orgID)
	}

	body, _ := json.Marshal(map[string]string{"org_id": orgID, "name": orgSecretName})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gatekeeperURL+"/internal/secrets/lookup", bytes.NewReader(body))
	if err != nil {
		return registryCreds{}, fmt.Errorf("build lookup request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Key", "containers:"+getServiceKey())

	resp, err := httpClient.Do(req)
	if err != nil {
		return registryCreds{}, fmt.Errorf("lookup request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return registryCreds{}, errors.New("org registry secret not found")
	}
	if resp.StatusCode != http.StatusOK {
		return registryCreds{}, fmt.Errorf("lookup returned %d", resp.StatusCode)
	}

	var result struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return registryCreds{}, fmt.Errorf("decode lookup response: %w", err)
	}

	username, token, ok := strings.Cut(result.Value, ":")
	if !ok || username == "" || token == "" {
		return registryCreds{}, errors.New("org registry secret must be in username:token format")
	}

	creds := registryCreds{username: username, password: token}
	orgCredsCache.Store(orgID, cachedCreds{creds: creds, expires: time.Now().Add(5 * time.Minute)})
	return creds, nil
}

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// lookupScopedSecret resolves a secret value from gatekeeper under an ownership scope: the
// org when one is bound, otherwise the owner's personal secrets. Same call forge and
// containers make; events must be listed in gatekeeper's SECRETS_LOOKUP_ALLOWED_CALLERS for
// it to be answered.
func lookupScopedSecret(ctx context.Context, orgID, userID, name string) (string, error) {
	if orgID == "" && userID == "" {
		return "", fmt.Errorf("no org or user bound for secret lookup")
	}
	if eventsServiceKey == nil {
		return "", fmt.Errorf("gatekeeper service key not initialised")
	}
	body, _ := json.Marshal(map[string]string{"org_id": orgID, "user_id": userID, "name": name})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gatekeeperURL+"/internal/secrets/lookup", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Key", serviceName+":"+eventsServiceKey())

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("secret lookup request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("secret %q not found", name)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("secret lookup returned %d", resp.StatusCode)
	}
	var result struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode secret lookup response: %w", err)
	}
	return result.Value, nil
}

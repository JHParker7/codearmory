package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// git-factory is the platform's own git host, so git_connector can authorize a clone
// against it the way no third-party host allows: by asking gatekeeper for a token that
// IS the run's owner, instead of holding a durable credential for the host.
//
// The exchange is service-account to service-account. git_connector authenticates to
// gatekeeper with its rotating service key (registry.StartKeyRotation in main.go) and
// calls POST /internal/run-tokens, which gatekeeper allows because git_connector is on
// its canMintScopedRoles list. The returned session token carries exactly the
// permissions the user already holds — gatekeeper mints it from the user's own record,
// so a clone can never reach a repo its owner could not.
//
// Nothing here is provisioned by hand. That is the point: the old path required an
// operator to mint a durable git-factory access token and paste it into a backend as a
// basic-auth password, which is a long-lived shared secret sitting in the cluster. This
// path has no secret to place, so builder can establish the link on its own.

// gitFactoryTokenTTL is how long a minted clone token is advertised as valid. It is a
// floor for the caller's cache/expiry bookkeeping, not the authority — gatekeeper owns
// the real session lifetime and will reject the token when it lapses regardless of
// what we report here.
const gitFactoryTokenTTL = 15 * time.Minute

// gatekeeperServiceKey returns git_connector's current gatekeeper service key. It is
// set in main() from the rotation the SDK drives, and is nil until then (in tests, and
// briefly at startup), in which case minting fails closed rather than panicking.
var gatekeeperServiceKey func() string

// mintGitFactoryToken asks gatekeeper for a short-lived session token minted for
// userID, to be used as the basic-auth password on a git-factory clone.
func mintGitFactoryToken(ctx context.Context, userID string) (string, time.Time, error) {
	if gatekeeperServiceKey == nil {
		return "", time.Time{}, fmt.Errorf("git_factory: gatekeeper service key not initialised")
	}
	key := gatekeeperServiceKey()
	if key == "" {
		return "", time.Time{}, fmt.Errorf("git_factory: no gatekeeper service key available")
	}

	body, _ := json.Marshal(map[string]string{"user_id": userID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		gatekeeperURL+"/internal/run-tokens", bytes.NewReader(body))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Key", "git_connector:"+key)

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("git_factory: mint token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", time.Time{}, fmt.Errorf("git_factory: gatekeeper returned %d: %s",
			resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var result struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", time.Time{}, err
	}
	if result.Token == "" {
		return "", time.Time{}, fmt.Errorf("git_factory: gatekeeper returned an empty token")
	}
	return result.Token, time.Now().UTC().Add(gitFactoryTokenTTL), nil
}

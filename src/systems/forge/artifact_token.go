package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
)

// Minting a scoped token for a sandbox.
//
// An artifact step needs a bearer for the artifact store, and every way of supplying
// one from outside is worse than minting one here:
//   - the caller's own bearer would hand the sandbox whatever the caller can do
//     (for an admin, everything);
//   - a stored long-lived secret is a standing credential sitting in the secret store
//     waiting to leak, and any pipeline could reference it.
//
// So forge does what workflows already does for a run: provision a MINIMAL role and
// mint a short-lived token against it. The token can do exactly one thing — read and
// write that user's own artifacts — so handing it to a sandbox grants no authority
// the step did not already need. Nothing is persisted and nothing is user-configured.

// artifactRoleCache memoises the per-user artifacts role, since provisioning is a
// round-trip and the role is identical for every execution of the same user.
var (
	artifactRoleMu    sync.Mutex
	artifactRoleCache = map[string]string{} // userID -> roleID
)

// artifactPermissions is the whole authority an artifact token carries: artifact
// read/write, and nothing else. Deliberately NOT the quota endpoints — a sandbox must
// never be able to raise its own cap.
//
// Resources are UNPREFIXED, matching how workflows provisions a run role
// ("forge/executions", not "{username}/forge/executions"): the {username} form is a
// default_grant template gatekeeper expands at grant time, not something a role
// carries. The token is already bound to one user, and the store scopes every artifact
// by the caller's id, so this cannot reach another user's data.
func artifactPermissions() []map[string]string {
	var out []map[string]string
	for _, act := range []string{"listArtifact", "getArtifact", "createArtifact", "deleteArtifact"} {
		out = append(out,
			map[string]string{"service": "artifacts", "action": act, "resource": "artifacts/artifacts"},
			map[string]string{"service": "artifacts", "action": act, "resource": "artifacts/artifacts/*"},
		)
	}
	return out
}

// provisionArtifactRole asks gatekeeper for a role granting only this user's own
// artifact access, returning its id.
func provisionArtifactRole(ctx context.Context, userID, orgID string) (string, error) {
	artifactRoleMu.Lock()
	cached, ok := artifactRoleCache[userID]
	artifactRoleMu.Unlock()
	if ok {
		return cached, nil
	}

	key := ""
	if forgeServiceKey != nil {
		key = forgeServiceKey()
	}
	if key == "" {
		return "", fmt.Errorf("gatekeeper service key unavailable")
	}
	payload, _ := json.Marshal(map[string]any{
		// A stable id per user so re-provisioning updates one role rather than
		// accreting a new one on every execution.
		"workflow_id": "forge-artifacts-" + userID,
		"user_id":     userID,
		"org_id":      orgID,
		"permissions": artifactPermissions(),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gatekeeperURL+"/internal/workflow-roles", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Key", "forge:"+key)
	resp, err := forgeHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return "", fmt.Errorf("gatekeeper returned %d: %s", resp.StatusCode, string(raw))
	}
	var result struct {
		RoleID string `json:"role_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	artifactRoleMu.Lock()
	artifactRoleCache[userID] = result.RoleID
	artifactRoleMu.Unlock()
	return result.RoleID, nil
}

// mintArtifactToken returns a short-lived bearer scoped to the user's own artifacts.
func mintArtifactToken(ctx context.Context, userID, orgID string) (string, error) {
	roleID, err := provisionArtifactRole(ctx, userID, orgID)
	if err != nil {
		return "", fmt.Errorf("provision artifact role: %w", err)
	}
	key := ""
	if forgeServiceKey != nil {
		key = forgeServiceKey()
	}
	if key == "" {
		return "", fmt.Errorf("gatekeeper service key unavailable")
	}
	body, _ := json.Marshal(map[string]any{"user_id": userID, "role_id": roleID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gatekeeperURL+"/internal/run-tokens", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Key", "forge:"+key)
	resp, err := forgeHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return "", fmt.Errorf("gatekeeper returned %d: %s", resp.StatusCode, string(raw))
	}
	var result struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.Token, nil
}

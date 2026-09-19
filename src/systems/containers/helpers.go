package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"

	"github.com/google/uuid"
)

// newID returns a fresh random UUID string for primary keys.
func newID() string { return uuid.NewString() }

// orgNameCache maps orgID → CodeArmory org name. Org names are stable, so entries
// live for the process lifetime.
var orgNameCache sync.Map

// resolveOrgName returns the CodeArmory org name for orgID via gatekeeper's
// GET /orgs/{id}, authenticated with the caller's forwarded Bearer token. Returns
// "" on any error so callers degrade to personal-namespace access only.
func resolveOrgName(ctx context.Context, r *http.Request, orgID string) string {
	if orgID == "" {
		return ""
	}
	if cached, ok := orgNameCache.Load(orgID); ok {
		return cached.(string)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gatekeeperURL+"/orgs/"+orgID, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", r.Header.Get("Authorization"))
	resp, err := httpClient.Do(req)
	if err != nil {
		slog.DebugContext(ctx, "resolveOrgName: gatekeeper request failed", "org_id", orgID, "error", err)
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var org struct {
		OrgName string `json:"org_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&org); err != nil {
		return ""
	}
	if org.OrgName != "" {
		orgNameCache.Store(orgID, org.OrgName)
	}
	return org.OrgName
}

// usernameCache maps userID → CodeArmory username. Usernames are stable, so entries
// live for the process lifetime.
var usernameCache sync.Map

// resolveUsername returns the CodeArmory username for userID via gatekeeper's
// GET /users/{id}, authenticated with the caller's forwarded Bearer token. Returns
// "" on any error. This is the authoritative identity provider in this platform;
// it exists so namespace-ownership works when gitea_integration (the legacy source)
// is not deployed. Returns "" on any error so callers fail closed.
func resolveUsername(ctx context.Context, r *http.Request, userID string) string {
	if userID == "" {
		return ""
	}
	if cached, ok := usernameCache.Load(userID); ok {
		return cached.(string)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gatekeeperURL+"/users/"+userID, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", r.Header.Get("Authorization"))
	resp, err := httpClient.Do(req)
	if err != nil {
		slog.DebugContext(ctx, "resolveUsername: gatekeeper request failed", "user_id", userID, "error", err)
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var u struct {
		Username string `json:"username"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return ""
	}
	if u.Username != "" {
		usernameCache.Store(userID, u.Username)
	}
	return u.Username
}

// namespaceAllowed reports whether the caller may act on a repository under the
// given registry namespace. A namespace is owned by the caller when it matches
// their personal username or their CodeArmory org name (by convention the
// Gitea org name equals the org name) — the same binding gitea_integration's
// ownerAllowed enforces.
//
// The REST manifest/tag handlers talk to the registry with the global service
// account, so RBAC is their only tenant boundary; the default grant
// (<user>/containers/repositories/*) wildcards across every namespace. Without
// this check any authenticated user could read or delete another tenant's
// private images. Fails closed when neither identity resolves.
func namespaceAllowed(ctx context.Context, r *http.Request, userID, orgID, namespace string) bool {
	if namespace == "" {
		return false
	}
	if creds, err := resolveGiteaUserCreds(ctx, userID, bearerToken(r)); err == nil && namespace == creds.username {
		return true
	}
	// Fall back to the caller's CodeArmory username from gatekeeper (the authoritative
	// identity provider) when the Gitea lookup is unavailable — gitea_integration is not
	// deployed in a git-factory-native cluster. Same ownership rule ("namespace == your
	// own username"), just sourced from the IdP that exists here; neither widens nor
	// narrows who may act on a namespace.
	if uname := resolveUsername(ctx, r, userID); uname != "" && namespace == uname {
		return true
	}
	if orgName := resolveOrgName(ctx, r, orgID); orgName != "" && namespace == orgName {
		return true
	}
	return false
}

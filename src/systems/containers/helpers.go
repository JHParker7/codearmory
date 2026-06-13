package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
)

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

// namespaceAllowed reports whether the caller may act on a repository under the
// given registry namespace. A namespace is owned by the caller when it matches
// their personal Gitea username or their CodeArmory org name (by convention the
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
	if orgName := resolveOrgName(ctx, r, orgID); orgName != "" && namespace == orgName {
		return true
	}
	return false
}

package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
)

// sudoFor returns the Gitea username linked to userID for use as the Sudo
// header value. Writes an HTTP error and returns ("", false) if the user has
// not yet linked a Gitea account.
func sudoFor(ctx context.Context, w http.ResponseWriter, userID string) (string, bool) {
	account, err := getAccount(ctx, userID)
	if err != nil {
		if isDbNotFound(err) {
			http.Error(w, "gitea account not linked — call PUT /account first", http.StatusUnprocessableEntity)
			return "", false
		}
		http.Error(w, "failed to resolve gitea account", http.StatusInternalServerError)
		return "", false
	}
	return account.GiteaUsername, true
}

// ownerAllowed returns true when the caller may act on a repo under {owner}:
//   - owner matches the caller's personal Gitea username (personal repos), or
//   - owner matches the caller's CodeArmory org name and the caller belongs to
//     that org (org repos — by convention the Gitea org name equals the
//     CodeArmory org name).
func ownerAllowed(owner, callerUsername, orgName string) bool {
	return owner == callerUsername || (orgName != "" && owner == orgName)
}

// orgNameCache is an in-memory store of orgID → org name. Org names are
// stable so we never evict — the cache lives for the process lifetime.
var orgNameCache sync.Map

// resolveOrgName returns the CodeArmory org name for orgID by calling
// gatekeeper's GET /orgs/{id} endpoint with the caller's Bearer token
// (forwarded from the original request). Returns "" on any error so callers
// degrade gracefully to personal-only access.
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
		slog.Debug("resolveOrgName: gatekeeper request failed", "org_id", orgID, "error", err)
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

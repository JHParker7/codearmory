package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
)

// Project scoping (see docs/projects/design.md). An execution may be filed into a
// gatekeeper Project; when it is, access is granted by a project role rather than only
// by ownership. These helpers talk to gatekeeper directly with the caller's own bearer
// — the same forward-the-bearer pattern checkGatekeeper uses — because the coarse
// permission check writes a 403 on denial and so cannot express an "owner OR project"
// fallback.

const projectService = "forge"

// authorizeExecution reports whether the caller may act on e: by ownership/tenancy
// (the submitter is the caller), or — when e is filed into a real gatekeeper project —
// by holding the project grant for (action) on it. This is the "owner OR project" gate
// the coarse check_permissions call cannot express.
func authorizeExecution(ctx context.Context, bearer, action string, e Execution, userID, orgID string) bool {
	if e.UserID == userID {
		return true
	}
	if e.ProjectID != "" && e.ProjectNamespace != "" && e.Project != "" {
		return checkProjectPermission(ctx, bearer, action, e.ProjectNamespace, "executions", e.Project, e.ExecutionID)
	}
	return false
}

// accessibleProject mirrors gatekeeper's GET /projects/accessible entries.
type accessibleProject struct {
	ProjectID string `json:"project_id"`
	Slug      string `json:"slug"`
	Namespace string `json:"namespace"`
	Tier      string `json:"tier"`
}

// fetchAccessibleProjects returns every project the caller can reach. Fails closed to
// an empty slice so a gatekeeper hiccup never widens access.
func fetchAccessibleProjects(ctx context.Context, bearer string) []accessibleProject {
	if bearer == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gatekeeperURL+"/projects/accessible", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", bearer)
	resp, err := forgeHTTPClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		return nil
	}
	var out []accessibleProject
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil
	}
	return out
}

// resolveProjectSlug finds the accessible project a slug refers to, preferring one the
// caller owns when a slug collides across namespaces. Returns nil when the slug matches
// no accessible project — the caller then keeps it as a free-text label (backward
// compatible: an unresolved project is the old view-filter, owner-scoped).
func resolveProjectSlug(ctx context.Context, bearer, slug string) *accessibleProject {
	if slug == "" {
		return nil
	}
	ps := fetchAccessibleProjects(ctx, bearer)
	var match *accessibleProject
	for i := range ps {
		if ps[i].Slug != slug {
			continue
		}
		if ps[i].Tier == "owner" {
			return &ps[i]
		}
		if match == nil {
			match = &ps[i]
		}
	}
	return match
}

// accessibleProjectIDs returns just the ids, for widening list queries.
func accessibleProjectIDs(ctx context.Context, bearer string) []string {
	ps := fetchAccessibleProjects(ctx, bearer)
	ids := make([]string, 0, len(ps))
	for _, p := range ps {
		ids = append(ids, p.ProjectID)
	}
	return ids
}

// checkProjectPermission asks gatekeeper whether the caller holds (action) on a
// project-scoped resource, owner-namespace-qualified so a member's grant matches
// (an unqualified resource would be scoped to the caller and never match the owner's
// project role). collection is e.g. "executions"; id may be "" for a collection-wide
// check (used when filing a new resource into a project).
func checkProjectPermission(ctx context.Context, bearer, action, ownerNS, collection, slug, id string) bool {
	if bearer == "" || ownerNS == "" || slug == "" {
		return false
	}
	resource := ownerNS + "/" + projectService + "/projects/" + slug + "/" + collection
	if id != "" {
		resource += "/" + id
	} else {
		resource += "/*"
	}
	body, _ := json.Marshal(map[string]string{"service": projectService, "action": action, "resource": resource})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gatekeeperURL+"/check_permissions", bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", bearer)
	req.Header.Set("Content-Type", "application/json")
	resp, err := forgeHTTPClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		return false
	}
	var res struct {
		Authorized bool `json:"authorized"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return false
	}
	return res.Authorized
}

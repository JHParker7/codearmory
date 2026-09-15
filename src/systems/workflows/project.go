package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// Project scoping (see docs/projects/design.md). A pipeline may be filed into a
// gatekeeper Project; when it is, access is granted by a project role rather than only
// by ownership. These helpers talk to gatekeeper directly with the caller's own bearer
// — the same forward-the-bearer pattern resolveUsername uses — because the SDK's
// CheckPermissions writes a 403 on denial and so cannot express an "owner OR project"
// fallback.

const projectService = "workflows"

// authorizeWorkflow reports whether the caller may act on wf: by ownership/tenancy
// (canAccessWorkflow), or — when wf is filed into a real gatekeeper project — by
// holding the project grant for (action) on it. This is the "owner OR project" gate
// the SDK's response-writing CheckPermissions cannot express.
func authorizeWorkflow(ctx context.Context, bearer, action string, wf Workflow, userID, orgID string) bool {
	if canAccessWorkflow(wf, userID, orgID) {
		return true
	}
	if wf.ProjectID != "" && wf.Project != "" {
		return checkProjectPermission(ctx, bearer, action, "pipelines", wf.Project, wf.WorkflowID)
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
	resp, err := httpClient.Do(req)
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
// project-scoped resource. A project is its own top-level namespace, so the resource is
// "project/<slug>/<service>/<collection>/<id>" — no owner namespace needed, and a
// project role's "project/<slug>/*" grant matches it for any member. collection is e.g.
// "pipelines"; id may be "" for a collection-wide check (filing a new resource).
func checkProjectPermission(ctx context.Context, bearer, action, collection, slug, id string) bool {
	if bearer == "" || slug == "" {
		return false
	}
	resource := "project/" + slug + "/" + projectService + "/" + collection
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
	resp, err := httpClient.Do(req)
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

// fetchProjectAncestors returns a project's ancestor chain [self, parent, …, root]
// from gatekeeper's GET /projects/{id}/ancestors. Fails closed to nil.
func fetchProjectAncestors(ctx context.Context, bearer, projectID string) []accessibleProject {
	if bearer == "" || projectID == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gatekeeperURL+"/projects/"+projectID+"/ancestors", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", bearer)
	resp, err := httpClient.Do(req)
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

// pipelineMayTargetProject is the PROJECT-ISOLATION gate: a pipeline filed into
// project pipelineSlug may run against — and tag resources in — targetSlug ONLY when
// targetSlug is pipelineSlug itself or a DESCENDANT of it (pipelineSlug is in
// targetSlug's ancestor chain). So a bootstrap parent's pipeline (agentic-dev-flow)
// may run for its child (demo), but a pipeline in an unrelated project (ops) cannot
// touch demo — even though the run executes as the owner, who could. A pipeline with
// no project (legacy, pre-enforcement) is unconstrained.
func pipelineMayTargetProject(ctx context.Context, bearer, pipelineSlug, targetSlug string) bool {
	pipelineSlug = strings.TrimSpace(pipelineSlug)
	targetSlug = strings.TrimSpace(targetSlug)
	if pipelineSlug == "" || targetSlug == "" || pipelineSlug == targetSlug {
		return true
	}
	tp := resolveProjectSlug(ctx, bearer, targetSlug)
	if tp == nil {
		return false
	}
	for _, a := range fetchProjectAncestors(ctx, bearer, tp.ProjectID) {
		if a.Slug == pipelineSlug {
			return true
		}
	}
	return false
}

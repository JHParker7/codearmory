package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"gorm.io/gorm"
)

// Project scoping (see docs/projects/design.md). An artifact may be filed into a
// gatekeeper Project; when it is, access is granted by a project role rather than only
// by ownership. These helpers talk to gatekeeper directly with the caller's own bearer
// — the same forward-the-bearer pattern forge and tickets use — because the SDK's
// CheckPermissions writes a 403 on denial and so cannot express an "owner OR project"
// fallback.

const projectService = "artifacts"

// authorizeArtifact reports whether the caller may act on a: by ownership (the caller
// is the owner), or — when a is filed into a real gatekeeper project — by holding the
// project grant for (action) on it. This is the "owner OR project" gate the SDK's
// response-writing CheckPermissions cannot express. Mirrors forge's authorizeExecution.
func authorizeArtifact(ctx context.Context, bearer, action string, a Artifact, userID string) bool {
	if a.UserID == userID {
		return true
	}
	if a.ProjectID != "" && a.Project != "" {
		return checkProjectPermission(ctx, bearer, action, "artifacts", a.Project, a.Name)
	}
	return false
}

// loadAuthorizedArtifact loads the artifact named `name` the caller may act on: their
// own, or — when they own none by that name and pass ?project=<slug> — one filed into a
// project they hold `action` on. Returns errNoSuch when neither resolves, so a non-owner
// without a project grant is indistinguishable from a missing artifact (404, not 403).
//
// This is the artifacts-shaped equivalent of forge loading an execution globally by its
// unique id and then gating with authorizeExecution. An artifact is keyed by (user,
// name) and a name is NOT globally unique, so the caller names the project the artifact
// lives in with ?project=<slug> to disambiguate which owner's row to reach.
func loadAuthorizedArtifact(ctx context.Context, bearer, userID, name, action, projectSlug string) (Artifact, error) {
	a, err := getArtifact(ctx, userID, name)
	if err == nil {
		return a, nil // the caller's own — the existing owner-scoped check
	}
	if !errors.Is(err, errNoSuch) {
		return Artifact{}, err
	}
	// Not the caller's own. The ADDITIONAL project path: resolve the named project and,
	// if the artifact is filed there and the caller holds the grant, allow it.
	if projectSlug == "" {
		return Artifact{}, errNoSuch
	}
	p := resolveProjectSlug(ctx, bearer, projectSlug)
	if p == nil {
		return Artifact{}, errNoSuch
	}
	pa, err := getArtifactInProject(ctx, p.ProjectID, name)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Artifact{}, errNoSuch
	}
	if err != nil {
		return Artifact{}, err
	}
	if !authorizeArtifact(ctx, bearer, action, pa, userID) {
		return Artifact{}, errNoSuch
	}
	return pa, nil
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
// project-scoped resource, owner-namespace-qualified so a member's grant matches
// (an unqualified resource would be scoped to the caller and never match the owner's
// project role). collection is e.g. "artifacts"; id may be "" for a collection-wide
// check (used when filing a new artifact into a project).
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
	return gatekeeperAuthorizes(ctx, bearer, action, resource)
}

// gatekeeperAuthorizes puts one (action, resource) question to gatekeeper with the
// caller's own bearer. Every failure — no credential, malformed request, transport
// error, non-200, undecodable body — answers NO, so a gatekeeper that is unreachable
// or unhappy narrows access rather than widening it.
func gatekeeperAuthorizes(ctx context.Context, bearer, action, resource string) bool {
	if bearer == "" {
		return false
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

package main

import (
	"context"
	"net/http"
)

// Project scoping (see the monorepo's docs/projects/design.md). A repo may be filed
// into a gatekeeper Project; a project role then grants access to every repo in the
// project at once, instead of a per-repo share. These helpers reuse gatekeeperCall
// (the same forward-the-bearer path collab.go uses) so the owner-OR-project decision
// can fall back after the per-repo grant check denies.

// projectResRepo is the project-scoped resource for a repo, e.g.
// "project/core/codearmory_git_factory/repos/<id>". A project is its own top-level
// namespace, so a project role's "project/core/*" grant matches for any member without
// any owner-namespace qualification. re.Project is the project slug.
func projectResRepo(re Repo) string {
	return "project/" + re.Project + "/" + serviceName + "/repos/" + re.ID
}

// repoProjectAuthorizes reports whether the caller holds (action) on the repo's project
// scope, returning the resolved user id when granted. Fails closed on any error.
func repoProjectAuthorizes(ctx context.Context, bearer, action string, re Repo) (string, bool) {
	if re.ProjectID == "" || re.Project == "" {
		return "", false
	}
	body := map[string]string{"service": serviceName, "action": action, "resource": projectResRepo(re)}
	var res struct {
		Authorized bool   `json:"authorized"`
		UserID     string `json:"user_id"`
	}
	if err := gatekeeperCall(ctx, bearer, http.MethodPost, "/check_permissions", body, &res); err != nil || !res.Authorized {
		return "", false
	}
	return res.UserID, true
}

// projectAllowsRepoAction reports whether the caller may perform (action) over a
// project's repo collection — the collection-wide check used when filing a new or
// existing repo into a project (id is "*", not a specific repo).
func projectAllowsRepoAction(ctx context.Context, bearer, action, slug string) bool {
	if slug == "" {
		return false
	}
	body := map[string]string{
		"service":  serviceName,
		"action":   action,
		"resource": "project/" + slug + "/" + serviceName + "/repos/*",
	}
	var res struct {
		Authorized bool `json:"authorized"`
	}
	if err := gatekeeperCall(ctx, bearer, http.MethodPost, "/check_permissions", body, &res); err != nil {
		return false
	}
	return res.Authorized
}

// accessibleProject mirrors gatekeeper's GET /projects/accessible entries.
type accessibleProject struct {
	ProjectID string `json:"project_id"`
	Slug      string `json:"slug"`
	Namespace string `json:"namespace"`
	Tier      string `json:"tier"`
}

func fetchAccessibleProjects(ctx context.Context, bearer string) []accessibleProject {
	if bearer == "" {
		return nil
	}
	var ps []accessibleProject
	if err := gatekeeperCall(ctx, bearer, http.MethodGet, "/projects/accessible", nil, &ps); err != nil {
		return nil
	}
	return ps
}

// accessibleProjectIDs lists the project ids the caller can reach, for widening repo
// list views to project-shared repos.
func accessibleProjectIDs(ctx context.Context, bearer string) []string {
	ps := fetchAccessibleProjects(ctx, bearer)
	ids := make([]string, 0, len(ps))
	for _, p := range ps {
		ids = append(ids, p.ProjectID)
	}
	return ids
}

// resolveProjectSlug finds the accessible project a slug refers to, preferring one the
// caller owns on a cross-namespace collision. nil ⇒ the slug is a free-text label, not
// a real project (backward compatible).
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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
)

// Project scoping for containers. Unlike forge/tickets — where the resource (an
// execution, a board) is a persisted row that can carry ProjectID directly — a
// container repository has NO row: it is `namespace/image`, proxied live from the
// upstream registry and authorized by namespaceAllowed against the caller's Gitea
// username / org name. There is nothing to stamp a project onto.
//
// So the link lives in its own small table (see project_links.go): a repository
// (namespace, optional image) is associated with a gatekeeper project, and that
// association is what widens access. These helpers are the gatekeeper side of the
// "owner OR project" gate — the same forward-the-bearer pattern namespaceAllowed
// already uses, because the SDK's CheckPermissions writes a 403 on denial and so
// cannot express a fallback.

const projectService = "containers"

// accessibleProject mirrors gatekeeper's GET /projects/accessible entries.
type accessibleProject struct {
	ProjectID string `json:"project_id"`
	Slug      string `json:"slug"`
	Namespace string `json:"namespace"`
	Tier      string `json:"tier"`
}

// fetchAccessibleProjects returns every project the caller can reach. Fails closed
// to an empty slice so a gatekeeper hiccup never widens access.
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

// resolveProjectSlug finds the accessible project a slug refers to, preferring one
// the caller owns when a slug collides across namespaces. Returns nil when the slug
// matches no accessible project — the caller is not a member and cannot file into
// (or list) it.
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

// checkProjectPermission asks gatekeeper whether the caller holds (action) on a
// project-scoped resource, owner-namespace-qualified so a member's grant matches
// (an unqualified resource would be scoped to the caller and never match the
// owner's project role). collection is "repositories"; id is the concrete repo
// "namespace/image" being accessed, or "" for a collection-wide check (used when
// filing a new link into a project).
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

// authorizeRepo reports whether the caller may perform action on the repository
// namespace/image. It is the "owner OR project" gate that sits ALONGSIDE the
// existing namespace check — never replacing it:
//
//  1. If the caller owns the namespace (their Gitea username or org name), allow —
//     exactly as today. Unlinked repositories therefore behave unchanged.
//  2. Otherwise, if the repository is linked to a gatekeeper project (an exact
//     namespace/image link, or a namespace-wide link), ask gatekeeper whether the
//     caller holds (action) on that project's repositories. A project member is
//     thus authorized for the project's container repositories without owning the
//     namespace.
//
// Fails closed: any error resolving the link or asking gatekeeper answers NO.
func authorizeRepo(ctx context.Context, r *http.Request, userID, orgID, action, namespace, image string) bool {
	if namespaceAllowed(ctx, r, userID, orgID, namespace) {
		return true
	}
	link, ok, err := lookupRepoProjectLink(ctx, namespace, image)
	if err != nil || !ok || link.Project == "" {
		return false
	}
	return checkProjectPermission(ctx, bearerHeader(r), action, "repositories", link.Project, namespace+"/"+image)
}

// bearerHeader returns the raw Authorization header, the credential the
// project-permission and accessible-project calls forward to gatekeeper.
func bearerHeader(r *http.Request) string { return r.Header.Get("Authorization") }

// filterByProject narrows a live registry catalog to the repositories linked to the
// project named by slug. It resolves the slug against the caller's accessible
// projects (so a non-member's slug matches nothing), loads that project's links, and
// keeps only the catalog entries they govern. Fails closed to an empty slice.
func filterByProject(ctx context.Context, r *http.Request, slug string, catalog []string) []string {
	p := resolveProjectSlug(ctx, bearerHeader(r), slug)
	if p == nil {
		return []string{}
	}
	links, err := linksForProject(ctx, p.ProjectID)
	if err != nil {
		return []string{}
	}
	return filterCatalogByLinks(catalog, links)
}

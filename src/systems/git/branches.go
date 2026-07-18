package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// branchView is one entry in the branch selector for a repo. Default marks the
// repo's default branch so the UI can preselect it (and forge's checkout.ref
// defaults to it when the user picks nothing).
type branchView struct {
	Name    string `json:"name"`
	Default bool   `json:"default,omitempty"`
}

// Like repo enumeration, branch enumeration fetches a single page per backend
// (per_page=100 / limit=100); repos with more branches are rare, and the user can
// always type the ref directly since forge's checkout.ref accepts any branch/tag.
//
// enumerateBranches lists the branches of repoURL by calling the owning backend's
// API with a freshly-minted credential. It is best-effort: generic backends have
// no standard branch API (returns nil so the UI falls back to a free-text ref), and
// any API error is returned to the caller to surface.
func enumerateBranches(ctx context.Context, b GitBackend, repoURL string) ([]branchView, error) {
	if b.Type == backendGeneric {
		return nil, nil // no standard branch API; users type the ref directly
	}
	cred, updated, err := mintForBackend(ctx, b, "")
	if err != nil {
		return nil, err
	}
	persistRotatedAuth(ctx, b, updated)
	if cred.Secret == "" {
		return nil, fmt.Errorf("backend %q produced no credential to enumerate with", b.Name)
	}
	switch b.Type {
	case backendGitHub:
		return listGitHubBranches(ctx, b, repoURL, cred.Secret)
	case backendGitLab:
		return listGitLabBranches(ctx, b, repoURL, cred.Secret)
	case backendForgejo:
		return listForgejoBranches(ctx, b, repoURL, cred.Secret)
	}
	return nil, nil
}

// repoPathFromURL returns the full project path from a clone URL — "owner/repo",
// or a nested "group/sub/project" on GitLab — trimmed of any ".git" suffix. Unlike
// repoNameFromURL (which keeps only the last two segments for display) this keeps
// the whole path so GitLab's nested-group projects resolve correctly.
func repoPathFromURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("invalid repo url: %w", err)
	}
	p := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
	if p == "" {
		return "", fmt.Errorf("repo url has no path")
	}
	return p, nil
}

// listGitHubBranches lists a repo's branches via the REST API, marking the repo's
// default branch (fetched separately since /branches doesn't flag it).
func listGitHubBranches(ctx context.Context, b GitBackend, repoURL, token string) ([]branchView, error) {
	path, err := repoPathFromURL(repoURL)
	if err != nil {
		return nil, err
	}
	apiBase := githubAPIBase(b.Host)
	setAuth := func(req *http.Request) {
		req.Header.Set("Authorization", "token "+token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}

	def := ""
	if req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+"/repos/"+path, nil); err == nil {
		setAuth(req)
		var repo struct {
			DefaultBranch string `json:"default_branch"`
		}
		if enumDo(req, &repo) == nil {
			def = repo.DefaultBranch
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+"/repos/"+path+"/branches?per_page=100", nil)
	if err != nil {
		return nil, err
	}
	setAuth(req)
	var branches []struct {
		Name string `json:"name"`
	}
	if err := enumDo(req, &branches); err != nil {
		return nil, err
	}
	out := make([]branchView, 0, len(branches))
	for _, br := range branches {
		out = append(out, branchView{Name: br.Name, Default: br.Name == def && def != ""})
	}
	return out, nil
}

// listGitLabBranches lists a project's branches. The branches API flags the default
// per entry, so no extra call is needed. The project id is the URL-encoded path.
func listGitLabBranches(ctx context.Context, b GitBackend, repoURL, token string) ([]branchView, error) {
	path, err := repoPathFromURL(repoURL)
	if err != nil {
		return nil, err
	}
	endpoint := strings.TrimRight(b.BaseURL, "/") + "/api/v4/projects/" + url.PathEscape(path) + "/repository/branches?per_page=100"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	var branches []struct {
		Name    string `json:"name"`
		Default bool   `json:"default"`
	}
	if err := enumDo(req, &branches); err != nil {
		return nil, err
	}
	out := make([]branchView, 0, len(branches))
	for _, br := range branches {
		out = append(out, branchView{Name: br.Name, Default: br.Default})
	}
	return out, nil
}

// listForgejoBranches lists a repo's branches via the Forgejo/Gitea API, marking the
// default branch (fetched from the repo object since /branches doesn't flag it).
func listForgejoBranches(ctx context.Context, b GitBackend, repoURL, token string) ([]branchView, error) {
	path, err := repoPathFromURL(repoURL)
	if err != nil {
		return nil, err
	}
	base := strings.TrimRight(b.BaseURL, "/") + "/api/v1/repos/" + path
	setAuth := func(req *http.Request) {
		req.Header.Set("Authorization", "token "+token)
		req.Header.Set("Accept", "application/json")
	}

	def := ""
	if req, err := http.NewRequestWithContext(ctx, http.MethodGet, base, nil); err == nil {
		setAuth(req)
		var repo struct {
			DefaultBranch string `json:"default_branch"`
		}
		if enumDo(req, &repo) == nil {
			def = repo.DefaultBranch
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/branches?limit=100&page=1", nil)
	if err != nil {
		return nil, err
	}
	setAuth(req)
	var branches []struct {
		Name string `json:"name"`
	}
	if err := enumDo(req, &branches); err != nil {
		return nil, err
	}
	out := make([]branchView, 0, len(branches))
	for _, br := range branches {
		out = append(out, branchView{Name: br.Name, Default: br.Name == def && def != ""})
	}
	return out, nil
}

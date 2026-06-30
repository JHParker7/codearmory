package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Enumeration fetches a single page per backend (per_page=100 / limit=50); users
// with more repos pin the rest manually, or type the URL directly since the forge
// git: ref accepts any URL whose host a linked backend owns.
//
// enumerateRepos lists the repos a linked backend can clone, by calling that
// backend's API with a freshly-minted credential. It is best-effort: generic
// backends can't be enumerated (no standard repo API), and any API error is
// returned to the caller to log-and-skip so one broken backend never blanks the
// whole selector.
func enumerateRepos(ctx context.Context, b GitBackend) ([]repoView, error) {
	if b.Type == backendGeneric {
		return nil, nil // no standard enumeration API; pin generic repos manually
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
		return enumerateGitHub(ctx, b, cred.Secret)
	case backendGitLab:
		return enumerateGitLab(ctx, b, cred.Secret)
	case backendForgejo:
		return enumerateForgejo(ctx, b, cred.Secret)
	}
	return nil, nil
}

// enumDo runs an enumeration GET and decodes a 2xx JSON body into out.
func enumDo(req *http.Request, out any) error {
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("repo enumeration returned %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ghRepo is the subset of a GitHub repository object we surface.
type ghRepo struct {
	FullName string `json:"full_name"`
	CloneURL string `json:"clone_url"`
}

// enumerateGitHub lists repos via the installation-repositories API (app mode) or
// the authenticated user's repos (pat mode). The installation endpoint wraps the
// list in {"repositories":[…]}; /user/repos returns a bare array.
func enumerateGitHub(ctx context.Context, b GitBackend, token string) ([]repoView, error) {
	apiBase := githubAPIBase(b.Host)
	app := b.AuthMode == modeApp
	url := apiBase + "/user/repos?per_page=100&sort=updated"
	if app {
		url = apiBase + "/installation/repositories?per_page=100"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	var repos []ghRepo
	if app {
		var wrapped struct {
			Repositories []ghRepo `json:"repositories"`
		}
		if err := enumDo(req, &wrapped); err != nil {
			return nil, err
		}
		repos = wrapped.Repositories
	} else if err := enumDo(req, &repos); err != nil {
		return nil, err
	}

	out := make([]repoView, 0, len(repos))
	for _, r := range repos {
		if r.CloneURL == "" {
			continue
		}
		out = append(out, repoFromBackend(b, r.FullName, r.CloneURL))
	}
	return out, nil
}

// enumerateGitLab lists the projects the credential is a member of. GitLab accepts
// both OAuth access tokens and PATs as Bearer tokens.
func enumerateGitLab(ctx context.Context, b GitBackend, token string) ([]repoView, error) {
	url := strings.TrimRight(b.BaseURL, "/") + "/api/v4/projects?membership=true&simple=true&per_page=100&order_by=last_activity_at"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	var projects []struct {
		PathWithNamespace string `json:"path_with_namespace"`
		HTTPURLToRepo     string `json:"http_url_to_repo"`
	}
	if err := enumDo(req, &projects); err != nil {
		return nil, err
	}
	out := make([]repoView, 0, len(projects))
	for _, p := range projects {
		if p.HTTPURLToRepo == "" {
			continue
		}
		out = append(out, repoFromBackend(b, p.PathWithNamespace, p.HTTPURLToRepo))
	}
	return out, nil
}

// enumerateForgejo lists the authenticated user's repos via the Forgejo/Gitea API.
func enumerateForgejo(ctx context.Context, b GitBackend, token string) ([]repoView, error) {
	url := strings.TrimRight(b.BaseURL, "/") + "/api/v1/user/repos?limit=50&page=1"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Accept", "application/json")

	var repos []struct {
		FullName string `json:"full_name"`
		CloneURL string `json:"clone_url"`
	}
	if err := enumDo(req, &repos); err != nil {
		return nil, err
	}
	out := make([]repoView, 0, len(repos))
	for _, r := range repos {
		if r.CloneURL == "" {
			continue
		}
		out = append(out, repoFromBackend(b, r.FullName, r.CloneURL))
	}
	return out, nil
}

// repoFromBackend builds an enumerated repoView, falling back to the URL path for
// the display name when the backend gave none.
func repoFromBackend(b GitBackend, name, cloneURL string) repoView {
	if name == "" {
		name = repoNameFromURL(cloneURL)
	}
	return repoView{
		Name:        name,
		URL:         cloneURL,
		Backend:     b.Name,
		BackendType: b.Type,
		Source:      repoSourceEnumerated,
	}
}

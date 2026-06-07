package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
)

type giteaClient struct {
	baseURL    string
	adminToken string
	http       *http.Client
}

func (c *giteaClient) newRequest(ctx context.Context, method, path, sudo string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+"/api/v1"+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "token "+c.adminToken)
	req.Header.Set("Content-Type", "application/json")
	if sudo != "" {
		req.Header.Set("Sudo", sudo)
	}
	return req, nil
}

func (c *giteaClient) do(req *http.Request, out interface{}) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return &giteaError{Status: resp.StatusCode, Body: string(b)}
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

type giteaError struct {
	Status int
	Body   string
}

func (e *giteaError) Error() string {
	return fmt.Sprintf("gitea %d: %s", e.Status, e.Body)
}

func isNotFoundErr(err error) bool {
	if e, ok := err.(*giteaError); ok {
		return e.Status == http.StatusNotFound
	}
	return false
}

// verifyUserToken calls GET /api/v1/user authenticated with the caller's own
// token (no admin Sudo) and returns the login name on the Gitea instance.
// This is used to prove that the caller controls the Gitea account they claim.
func (c *giteaClient) verifyUserToken(ctx context.Context, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v1/user", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "token "+token)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return "", &giteaError{Status: resp.StatusCode, Body: string(b)}
	}
	var u struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return "", err
	}
	return u.Login, nil
}

func (c *giteaClient) get(ctx context.Context, path, sudo string, out interface{}) error {
	req, err := c.newRequest(ctx, http.MethodGet, path, sudo, nil)
	if err != nil {
		return err
	}
	return c.do(req, out)
}

func (c *giteaClient) post(ctx context.Context, path, sudo string, payload, out interface{}) error {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := c.newRequest(ctx, http.MethodPost, path, sudo, body)
	if err != nil {
		return err
	}
	return c.do(req, out)
}

func (c *giteaClient) delete(ctx context.Context, path, sudo string) error {
	req, err := c.newRequest(ctx, http.MethodDelete, path, sudo, nil)
	if err != nil {
		return err
	}
	return c.do(req, nil)
}

func (c *giteaClient) listRepos(ctx context.Context, sudo string, page, limit int) ([]Repo, error) {
	path := fmt.Sprintf("/repos/search?limit=%d&page=%d", limit, page)
	var result struct {
		Data []Repo `json:"data"`
	}
	if err := c.get(ctx, path, sudo, &result); err != nil {
		return nil, err
	}
	if result.Data == nil {
		result.Data = []Repo{}
	}
	return result.Data, nil
}

func (c *giteaClient) getRepo(ctx context.Context, owner, name, sudo string) (*Repo, error) {
	var r Repo
	if err := c.get(ctx, "/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name), sudo, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (c *giteaClient) createRepo(ctx context.Context, sudo string, payload map[string]interface{}) (*Repo, error) {
	var r Repo
	if err := c.post(ctx, "/user/repos", sudo, payload, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (c *giteaClient) deleteRepo(ctx context.Context, owner, name, sudo string) error {
	return c.delete(ctx, "/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name), sudo)
}

func (c *giteaClient) listBranches(ctx context.Context, owner, name, sudo string) ([]Branch, error) {
	var branches []Branch
	if err := c.get(ctx, "/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name)+"/branches", sudo, &branches); err != nil {
		return nil, err
	}
	if branches == nil {
		branches = []Branch{}
	}
	return branches, nil
}

func (c *giteaClient) listTags(ctx context.Context, owner, name, sudo string) ([]Tag, error) {
	var tags []Tag
	if err := c.get(ctx, "/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name)+"/tags", sudo, &tags); err != nil {
		return nil, err
	}
	if tags == nil {
		tags = []Tag{}
	}
	return tags, nil
}

func (c *giteaClient) listReleases(ctx context.Context, owner, name, sudo string) ([]Release, error) {
	var releases []Release
	if err := c.get(ctx, "/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name)+"/releases", sudo, &releases); err != nil {
		return nil, err
	}
	if releases == nil {
		releases = []Release{}
	}
	return releases, nil
}

func (c *giteaClient) listPulls(ctx context.Context, owner, name, state, sudo string) ([]PullRequest, error) {
	path := fmt.Sprintf("/repos/%s/%s/pulls?state=%s",
		url.PathEscape(owner), url.PathEscape(name), url.QueryEscape(state))
	var prs []PullRequest
	if err := c.get(ctx, path, sudo, &prs); err != nil {
		return nil, err
	}
	if prs == nil {
		prs = []PullRequest{}
	}
	return prs, nil
}

func (c *giteaClient) getPull(ctx context.Context, owner, name, index, sudo string) (*PullRequest, error) {
	var pr PullRequest
	if err := c.get(ctx,
		"/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name)+"/pulls/"+index, sudo, &pr); err != nil {
		return nil, err
	}
	return &pr, nil
}

func (c *giteaClient) createPull(ctx context.Context, owner, name, sudo string, payload map[string]interface{}) (*PullRequest, error) {
	var pr PullRequest
	if err := c.post(ctx,
		"/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name)+"/pulls", sudo, payload, &pr); err != nil {
		return nil, err
	}
	return &pr, nil
}

func (c *giteaClient) mergePull(ctx context.Context, owner, name, index, sudo string, payload map[string]interface{}) error {
	return c.post(ctx,
		"/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name)+"/pulls/"+index+"/merge",
		sudo, payload, nil)
}

func (c *giteaClient) listCommits(ctx context.Context, owner, name, sudo string, page, limit int) ([]Commit, error) {
	path := fmt.Sprintf("/repos/%s/%s/commits?limit=%d&page=%d",
		url.PathEscape(owner), url.PathEscape(name), limit, page)
	var commits []Commit
	if err := c.get(ctx, path, sudo, &commits); err != nil {
		return nil, err
	}
	if commits == nil {
		commits = []Commit{}
	}
	return commits, nil
}

// ── Registry token management ─────────────────────────────────────────────────

type giteaUserToken struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// listUserTokens returns all personal access tokens for the given Gitea user.
// Uses the admin token with Sudo to act on behalf of the user.
func (c *giteaClient) listUserTokens(ctx context.Context, username string) ([]giteaUserToken, error) {
	var tokens []giteaUserToken
	if err := c.get(ctx, "/users/"+url.PathEscape(username)+"/tokens", username, &tokens); err != nil {
		return nil, err
	}
	if tokens == nil {
		tokens = []giteaUserToken{}
	}
	return tokens, nil
}

// createRegistryToken creates a Gitea API token scoped to package read/write
// for the given user via admin Sudo. Returns the raw token value (only available
// at creation time — Gitea does not expose it again after this call).
func (c *giteaClient) createRegistryToken(ctx context.Context, username, name string) (string, error) {
	var result struct {
		SHA1 string `json:"sha1"`
	}
	if err := c.post(ctx, "/users/"+url.PathEscape(username)+"/tokens", username, map[string]any{
		"name":   name,
		"scopes": []string{"read:package", "write:package"},
	}, &result); err != nil {
		return "", err
	}
	if result.SHA1 == "" {
		return "", fmt.Errorf("gitea returned empty token value")
	}
	return result.SHA1, nil
}

// cleanRegistryTokens deletes all tokens matching the "codearmory-reg-" prefix
// for the given user. Deletion failures are logged but do not abort the loop.
func (c *giteaClient) cleanRegistryTokens(ctx context.Context, username string) error {
	tokens, err := c.listUserTokens(ctx, username)
	if err != nil {
		return fmt.Errorf("list tokens: %w", err)
	}
	for _, t := range tokens {
		if !strings.HasPrefix(t.Name, "codearmory-reg-") {
			continue
		}
		path := fmt.Sprintf("/users/%s/tokens/%d", url.PathEscape(username), t.ID)
		if err := c.delete(ctx, path, username); err != nil {
			slog.Warn("clean registry tokens: delete failed", "username", username, "token_id", t.ID, "error", err)
		}
	}
	return nil
}

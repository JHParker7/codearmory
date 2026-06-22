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
	"sort"
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
		return newGiteaError(resp.StatusCode, b)
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

// maxGiteaErrorBody bounds the upstream response body stored on a giteaError.
// The body is embedded in Error() and logged at many call sites, so cap it to
// keep logs from dumping large or sensitive upstream content.
const maxGiteaErrorBody = 512

// newGiteaError constructs a giteaError, truncating the upstream body to
// maxGiteaErrorBody bytes so it can't bloat or leak through logs.
func newGiteaError(status int, body []byte) *giteaError {
	return &giteaError{Status: status, Body: string(body[:min(len(body), maxGiteaErrorBody)])}
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
		return "", newGiteaError(resp.StatusCode, b)
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

// Token-name prefixes used for the per-purpose tokens this service mints. Each
// prefix is both how a token is named at creation and how it is matched for
// prefix-based cleanup, so the two must stay in sync — keep them here.
const (
	registryTokenPrefix = "codearmory-reg-"
	cloneTokenPrefix    = "codearmory-clone-"
)

// cloneTokenRetain bounds how many of the newest clone tokens pruneCloneTokens
// keeps. It must comfortably exceed the number of a single user's concurrently
// in-flight forge clones so a prune never revokes a token a running job still
// needs (forge runs ~10 workers globally).
const cloneTokenRetain = 16

// createScopedToken creates a Gitea API token with the given scopes for the user
// via admin Sudo. Returns the raw token value (only available at creation time —
// Gitea does not expose it again after this call).
func (c *giteaClient) createScopedToken(ctx context.Context, username, name string, scopes []string) (string, error) {
	var result struct {
		SHA1 string `json:"sha1"`
	}
	if err := c.post(ctx, "/users/"+url.PathEscape(username)+"/tokens", username, map[string]any{
		"name":   name,
		"scopes": scopes,
	}, &result); err != nil {
		return "", err
	}
	if result.SHA1 == "" {
		return "", fmt.Errorf("gitea returned empty token value")
	}
	return result.SHA1, nil
}

// createRegistryToken creates a Gitea API token scoped to package read/write.
func (c *giteaClient) createRegistryToken(ctx context.Context, username, name string) (string, error) {
	return c.createScopedToken(ctx, username, name, []string{"read:package", "write:package"})
}

// createCloneToken creates a Gitea API token scoped to repository read for the
// given user. It is minted per forge execution so a sandboxed job can clone a
// private repo over HTTPS. The caller prunes older clone tokens afterward (see
// pruneCloneTokens) so they do not accumulate, since Gitea personal access
// tokens cannot be given an expiry.
func (c *giteaClient) createCloneToken(ctx context.Context, username, name string) (string, error) {
	return c.createScopedToken(ctx, username, name, []string{"read:repository"})
}

// cleanTokensByPrefix deletes all of the user's tokens whose name starts with
// prefix. Deletion failures are logged but do not abort the loop.
func (c *giteaClient) cleanTokensByPrefix(ctx context.Context, username, prefix string) error {
	tokens, err := c.listUserTokens(ctx, username)
	if err != nil {
		return fmt.Errorf("list tokens: %w", err)
	}
	for _, t := range tokens {
		if !strings.HasPrefix(t.Name, prefix) {
			continue
		}
		path := fmt.Sprintf("/users/%s/tokens/%d", url.PathEscape(username), t.ID)
		if err := c.delete(ctx, path, username); err != nil {
			slog.WarnContext(ctx, "clean tokens: delete failed", "username", username, "prefix", prefix, "token_id", t.ID, "error", err)
		}
	}
	return nil
}

// cleanRegistryTokens deletes all of the user's "codearmory-reg-" tokens.
func (c *giteaClient) cleanRegistryTokens(ctx context.Context, username string) error {
	return c.cleanTokensByPrefix(ctx, username, registryTokenPrefix)
}

// pruneCloneTokens deletes the user's "codearmory-clone-" tokens beyond the
// newest keep (ordered by token ID, which Gitea assigns monotonically). Callers
// mint the new token FIRST and prune afterward — rather than deleting all then
// creating — so a concurrent execution's just-minted token (a high ID) survives
// and parallel forge runs for the same user don't revoke each other's token.
func (c *giteaClient) pruneCloneTokens(ctx context.Context, username string, keep int) error {
	tokens, err := c.listUserTokens(ctx, username)
	if err != nil {
		return fmt.Errorf("list tokens: %w", err)
	}
	clone := make([]giteaUserToken, 0, len(tokens))
	for _, t := range tokens {
		if strings.HasPrefix(t.Name, cloneTokenPrefix) {
			clone = append(clone, t)
		}
	}
	sort.Slice(clone, func(i, j int) bool { return clone[i].ID > clone[j].ID })
	for i := keep; i < len(clone); i++ {
		path := fmt.Sprintf("/users/%s/tokens/%d", url.PathEscape(username), clone[i].ID)
		if err := c.delete(ctx, path, username); err != nil {
			slog.WarnContext(ctx, "prune clone tokens: delete failed", "username", username, "token_id", clone[i].ID, "error", err)
		}
	}
	return nil
}

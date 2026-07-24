package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Pull-through-mirror integration (DESIGN-read-replicas.md §7, git-connector side).
//
// On the INTERNAL clone-token path only, when the repo's backend has opted in
// (GitBackend.PreferMirror), the broker asks git-factory to keep a warm mirror of the
// repo and hands the runner git-factory's in-cluster clone URL instead of upstream's.
// git-factory does the actual cloning; the broker's job is to (1) supply an already
// authenticated upstream URL so git-factory can fetch, and (2) fetch back a runner
// credential for the mirror. Everything here is best-effort: any failure returns "" and
// the caller falls back to the upstream URL, so a mirror problem never breaks a clone.

// deriveRepoPath splits a repo URL into the (namespace, name) git-factory addresses it
// by. namespace is the first path segment (the owner/org), name the last, with any
// trailing ".git" removed — e.g. https://host/jp01/codearmory.git → ("jp01","codearmory").
func deriveRepoPath(raw string) (namespace, name string, err error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", "", fmt.Errorf("invalid url: %w", err)
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(segs) < 2 || segs[0] == "" {
		return "", "", fmt.Errorf("url path %q is not owner/repo", u.Path)
	}
	name = strings.TrimSuffix(segs[len(segs)-1], ".git")
	namespace = segs[0]
	if namespace == "" || name == "" {
		return "", "", fmt.Errorf("url path %q is not owner/repo", u.Path)
	}
	return namespace, name, nil
}

// maybeMirrorCloneURL returns a git-factory clone URL (and its expiry) for repoURL when
// the backend opts in and git-factory is configured; otherwise "" so the caller keeps
// the upstream URL. upstream.CloneURL must be the AUTHENTICATED upstream URL — git-factory
// fetches with it and never persists it.
func maybeMirrorCloneURL(ctx context.Context, owner, repoURL string, upstream credential) (string, time.Time) {
	if gitFactoryURL == "" || gitFactoryKey == "" {
		return "", time.Time{}
	}
	host, err := deriveHost(repoURL)
	if err != nil {
		return "", time.Time{}
	}
	b, err := getBackendByHost(ctx, owner, host)
	if err != nil || !b.PreferMirror {
		return "", time.Time{}
	}
	ns, name, err := deriveRepoPath(repoURL)
	if err != nil {
		slog.WarnContext(ctx, "mirror: cannot derive repo path, using upstream", "repo_url", repoURL, "error", err)
		return "", time.Time{}
	}

	// 1. Ensure git-factory holds a warm mirror, fetching from the authenticated upstream.
	if status, err := gitFactoryPost(ctx, "/internal/mirrors", map[string]any{
		"upstream_url": upstream.CloneURL,
		"namespace":    ns,
		"name":         name,
		"owner":        owner,
	}, nil); err != nil || status/100 != 2 {
		slog.WarnContext(ctx, "mirror: ensure failed, using upstream", "namespace", ns, "name", name, "status", status, "error", err)
		return "", time.Time{}
	}

	// 2. Mint a runner credential for the mirror.
	var tok struct {
		CloneURL  string    `json:"clone_url"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if status, err := gitFactoryPost(ctx, "/internal/clone-token", map[string]any{
		"namespace": ns, "name": name,
	}, &tok); err != nil || status/100 != 2 || tok.CloneURL == "" {
		slog.WarnContext(ctx, "mirror: clone-token failed, using upstream", "namespace", ns, "name", name, "status", status, "error", err)
		return "", time.Time{}
	}
	slog.InfoContext(ctx, "mirror: serving clone from git-factory", "namespace", ns, "name", name)
	return tok.CloneURL, tok.ExpiresAt
}

// gitFactoryPost POSTs body to git-factory's internal surface with the shared key and,
// when out is non-nil and the response is 2xx, decodes the JSON body into it. Returns the
// HTTP status so the caller can distinguish "reached it, said no" from "couldn't reach".
func gitFactoryPost(ctx context.Context, path string, body any, out any) (int, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gitFactoryURL+path, bytes.NewReader(raw))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Key", gitFactoryKey)
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode/100 == 2 {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

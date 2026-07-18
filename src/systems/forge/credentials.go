package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Credential reference schemes for a job's secret_refs. The map key is the
// target env var name; the value is a "<scheme>:<arg>" reference resolved at
// dispatch and injected into the runtime env — never into the persisted env.
const (
	refSchemeSecret = "secret" // secret:<gatekeeper-org-secret-name>  — e.g. a git SSH key / token for GitHub, Bitbucket, …
	refSchemeGitea  = "gitea"  // gitea:<owner>/<repo>                 — mints a short-lived Forgejo clone URL
	refSchemeGit    = "git"    // git:<https-repo-url>                 — broker mints creds for the URL's backend
	// token:artifacts — forge mints a SHORT-LIVED bearer scoped to this user's own
	// artifacts (see artifact_token.go). It exists so an artifact step needs no
	// standing credential and never carries the caller's own (possibly admin) token
	// into a sandbox: the minted token can do exactly one thing.
	refSchemeToken = "token"
	// tokenArgArtifacts is the only audience token: accepts today.
	tokenArgArtifacts = "artifacts"
)

var (
	// giteaInternalURL is the base URL of the gitea_integration service, used to
	// mint short-lived Forgejo clone URLs. Empty disables gitea: references.
	giteaInternalURL = strings.TrimRight(envOrDefault("GITEA_INTERNAL_URL", ""), "/")
	// giteaInternalKey authenticates forge → gitea_integration internal calls
	// (sent as X-Internal-Key). Empty disables gitea: references.
	giteaInternalKey = secret("GITEA_INTERNAL_KEY")
	// gitInternalURL is the base URL of the core git credential-broker service,
	// used to mint short-lived clone URLs for any linked backend (GitHub, GitLab,
	// Forgejo, generic). Empty disables git: references.
	gitInternalURL = strings.TrimRight(envOrDefault("GIT_INTERNAL_URL", ""), "/")
	// gitInternalKey authenticates forge → git broker internal calls (sent as
	// X-Internal-Key). Empty disables git: references.
	gitInternalKey = secret("GIT_INTERNAL_KEY")
	// forgeServiceKey returns the current rotated gatekeeper service key. It is
	// wired in main() from registry.StartKeyRotation's accessor so secret lookups
	// authenticate with the live key rather than the bootstrap value.
	forgeServiceKey func() string
)

// parseCredentialRef splits a credential reference into its scheme and argument
// and validates the shape. Valid forms: "secret:<name>", "gitea:<owner>/<repo>".
func parseCredentialRef(ref string) (scheme, arg string, err error) {
	scheme, arg, ok := strings.Cut(ref, ":")
	if !ok || arg == "" {
		return "", "", fmt.Errorf("reference must be \"secret:<name>\", \"git:<repo-url>\", \"gitea:<owner>/<repo>\", or \"token:artifacts\"")
	}
	switch scheme {
	case refSchemeSecret:
		return scheme, arg, nil
	case refSchemeGitea:
		owner, repo, ok := strings.Cut(arg, "/")
		if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
			return "", "", fmt.Errorf("gitea reference must be \"gitea:<owner>/<repo>\"")
		}
		return scheme, arg, nil
	case refSchemeToken:
		if arg != tokenArgArtifacts {
			return "", "", fmt.Errorf("token reference must be \"token:%s\"", tokenArgArtifacts)
		}
		return scheme, arg, nil
	case refSchemeGit:
		// strings.Cut split on the first ':', so arg is already the full clone URL
		// (e.g. "https://github.com/acme/widgets.git"). The broker resolves which
		// linked backend owns the URL's host.
		u, perr := url.Parse(arg)
		if perr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return "", "", fmt.Errorf("git reference must be \"git:<http(s)-repo-url>\"")
		}
		return scheme, arg, nil
	default:
		return "", "", fmt.Errorf("unknown reference scheme %q (want secret:, git:, or gitea:)", scheme)
	}
}

// validateSecretRefs checks each secret_ref's target env var name and reference
// at submit time. env holds the plaintext env keys the same request sets, so a
// secret_ref cannot silently collide with a plaintext value. A secret: reference
// resolves under the submitter's ownership scope — their org if they have one,
// otherwise their personal secrets — so it needs either an org or a user; a
// submitter always has a user, so secret: refs are permitted for org-less users.
func validateSecretRefs(refs, env map[string]string, orgID, userID string) error {
	for target, ref := range refs {
		if !envKeyRe.MatchString(target) {
			return fmt.Errorf("invalid secret_ref target %q: must match [A-Za-z_][A-Za-z0-9_]*", target)
		}
		if blockedEnvKeys[strings.ToUpper(target)] {
			return fmt.Errorf("secret_ref target %q is not permitted", target)
		}
		if _, clash := env[target]; clash {
			return fmt.Errorf("secret_ref target %q is also set in env; set it in only one", target)
		}
		scheme, _, err := parseCredentialRef(ref)
		if err != nil {
			return fmt.Errorf("secret_ref %q: %w", target, err)
		}
		if scheme == refSchemeSecret && orgID == "" && userID == "" {
			return fmt.Errorf("secret_ref %q uses a secret: reference, which requires an org or an authenticated user", target)
		}
	}
	return nil
}

// resolveCredentials resolves an execution's secret_refs into env var values at
// dispatch. The returned map is merged into the runtime env only; values are
// never persisted or logged. Any error fails the whole execution so a job never
// runs with a half-resolved or missing credential.
func resolveCredentials(ctx context.Context, exec Execution) (map[string]string, error) {
	if len(exec.SecretRefs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(exec.SecretRefs))
	for target, ref := range exec.SecretRefs {
		scheme, arg, err := parseCredentialRef(ref)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", target, err)
		}
		var value string
		switch scheme {
		case refSchemeSecret:
			value, err = lookupScopedSecret(ctx, exec.OrgID, exec.UserID, arg)
		case refSchemeGitea:
			value, err = mintGiteaCloneURL(ctx, exec.UserID, arg)
		case refSchemeGit:
			value, err = mintGitCloneURL(ctx, exec.UserID, arg)
		case refSchemeToken:
			value, err = mintArtifactToken(ctx, exec.UserID, exec.OrgID)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", target, err)
		}
		out[target] = value
	}
	return out, nil
}

// lookupScopedSecret resolves a single secret value from gatekeeper under the
// execution's ownership scope: its org if one is bound, otherwise the submitting
// user's personal secrets. forge must be listed in gatekeeper's
// SECRETS_LOOKUP_ALLOWED_CALLERS. Both bindings were established by the gatekeeper
// permission check at submit time and snapshotted onto the execution, so
// gatekeeper trusts the org_id/user_id forge sends.
func lookupScopedSecret(ctx context.Context, orgID, userID, name string) (string, error) {
	if orgID == "" && userID == "" {
		return "", fmt.Errorf("no org or user bound to execution for secret lookup")
	}
	if forgeServiceKey == nil {
		return "", fmt.Errorf("gatekeeper service key not initialised")
	}
	body, _ := json.Marshal(map[string]string{"org_id": orgID, "user_id": userID, "name": name})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gatekeeperURL+"/internal/secrets/lookup", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Key", "forge:"+forgeServiceKey())

	resp, err := forgeHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("secret lookup request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("secret %q not found", name)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("secret lookup returned %d", resp.StatusCode)
	}
	var result struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode secret lookup response: %w", err)
	}
	return result.Value, nil
}

// mintGitCloneURL asks the core git credential-broker to mint clone credentials
// for repoURL and returns an authenticated HTTPS clone URL. The broker resolves
// which of the user's linked backends (GitHub, GitLab, Forgejo, generic) owns the
// URL's host and mints short-lived credentials where the backend supports it.
func mintGitCloneURL(ctx context.Context, userID, repoURL string) (string, error) {
	if gitInternalURL == "" || gitInternalKey == "" {
		return "", fmt.Errorf("git credential broker not configured (set GIT_INTERNAL_URL and GIT_INTERNAL_KEY)")
	}
	body, _ := json.Marshal(map[string]string{"user_id": userID, "repo_url": repoURL})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gitInternalURL+"/internal/clone-token", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Key", gitInternalKey)

	resp, err := forgeHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("clone-token request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("no linked git backend for repo %q", repoURL)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("clone-token returned %d", resp.StatusCode)
	}
	var result struct {
		CloneURL string `json:"clone_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode clone-token response: %w", err)
	}
	if result.CloneURL == "" {
		return "", fmt.Errorf("clone-token response missing clone_url")
	}
	return result.CloneURL, nil
}

// mintGiteaCloneURL asks gitea_integration to mint a Forgejo clone token for the
// execution's user and returns an authenticated HTTPS clone URL. The token is
// revoked-on-reuse: gitea_integration deletes the user's prior clone tokens
// before issuing each new one, so they do not accumulate.
func mintGiteaCloneURL(ctx context.Context, userID, ownerRepo string) (string, error) {
	if giteaInternalURL == "" || giteaInternalKey == "" {
		return "", fmt.Errorf("gitea integration not configured (set GITEA_INTERNAL_URL and GITEA_INTERNAL_KEY)")
	}
	owner, repo, _ := strings.Cut(ownerRepo, "/")
	body, _ := json.Marshal(map[string]string{"user_id": userID, "owner": owner, "repo": repo})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, giteaInternalURL+"/internal/clone-token", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Key", giteaInternalKey)

	resp, err := forgeHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("clone-token request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("no linked gitea account for repo %q", ownerRepo)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("clone-token returned %d", resp.StatusCode)
	}
	var result struct {
		CloneURL string `json:"clone_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode clone-token response: %w", err)
	}
	if result.CloneURL == "" {
		return "", fmt.Errorf("clone-token response missing clone_url")
	}
	return result.CloneURL, nil
}

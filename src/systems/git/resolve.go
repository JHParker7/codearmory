package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// validBackendType reports whether t is a supported backend type.
func validBackendType(t string) bool {
	switch t {
	case backendGitHub, backendGitLab, backendForgejo, backendGeneric:
		return true
	}
	return false
}

// validModeForType reports whether mode is valid for the given backend type.
func validModeForType(t, mode string) bool {
	switch t {
	case backendGitHub:
		return mode == modeApp || mode == modePAT
	case backendGitLab:
		return mode == modeToken || mode == modeOAuth
	case backendForgejo:
		return mode == modeToken || mode == modeAdmin
	case backendGeneric:
		return mode == modeBasic
	}
	return false
}

// deriveHost extracts the lowercased host (no port) from a base or clone URL.
func deriveHost(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("invalid url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("url must be http(s)")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", fmt.Errorf("url has no host")
	}
	return host, nil
}

// injectCloneCreds returns repoURL with HTTPS basic-auth userinfo injected.
func injectCloneCreds(repoURL, username, secret string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(repoURL))
	if err != nil {
		return "", fmt.Errorf("invalid repo url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("repo url must be http(s)")
	}
	u.User = url.UserPassword(username, secret)
	return u.String(), nil
}

// mintForBackend resolves credentials for repoURL against backend b. When the
// backend's stored auth must be updated as a side effect (e.g. a rotated GitLab
// OAuth refresh token), it is returned as the non-nil second value for the caller
// to persist.
func mintForBackend(ctx context.Context, b GitBackend, repoURL string) (credential, *authConfig, error) {
	auth, err := openAuth(b.AuthEnc)
	if err != nil {
		return credential{}, nil, err
	}
	cred := credential{Type: "basic", Backend: b.Name, BackendType: b.Type}

	switch b.Type {
	case backendGitHub:
		switch auth.Mode {
		case modeApp:
			tok, exp, err := githubInstallationToken(ctx, githubAPIBase(b.Host), auth.AppID, auth.InstallationID, auth.PrivateKey)
			if err != nil {
				return credential{}, nil, err
			}
			cred.Username, cred.Secret, cred.ExpiresAt = "x-access-token", tok, exp
		case modePAT:
			user := auth.Username
			if user == "" {
				user = "x-access-token"
			}
			cred.Username, cred.Secret = user, auth.Token
		default:
			return credential{}, nil, fmt.Errorf("unsupported github auth mode %q", auth.Mode)
		}

	case backendGitLab:
		switch auth.Mode {
		case modeToken:
			cred.Username, cred.Secret = "oauth2", auth.Token
		case modeOAuth:
			tok, exp, newRefresh, err := gitlabRefresh(ctx, b.BaseURL, auth)
			if err != nil {
				return credential{}, nil, err
			}
			cred.Username, cred.Secret, cred.ExpiresAt = "oauth2", tok, exp
			if newRefresh != "" && newRefresh != auth.RefreshToken {
				updated := auth
				updated.RefreshToken = newRefresh
				return cred, &updated, finishCred(&cred, repoURL)
			}
		default:
			return credential{}, nil, fmt.Errorf("unsupported gitlab auth mode %q", auth.Mode)
		}

	case backendForgejo:
		switch auth.Mode {
		case modeToken:
			user := auth.Username
			if user == "" {
				user = "git"
			}
			cred.Username, cred.Secret = user, auth.Token
		case modeAdmin:
			if auth.Username == "" {
				return credential{}, nil, fmt.Errorf("forgejo admin mode requires a username")
			}
			tok, err := forgejoMintToken(ctx, b.BaseURL, auth.AdminToken, auth.Username)
			if err != nil {
				return credential{}, nil, err
			}
			cred.Username, cred.Secret = auth.Username, tok
		default:
			return credential{}, nil, fmt.Errorf("unsupported forgejo auth mode %q", auth.Mode)
		}

	case backendGeneric:
		if auth.Mode != modeBasic {
			return credential{}, nil, fmt.Errorf("unsupported generic auth mode %q", auth.Mode)
		}
		cred.Username, cred.Secret = auth.Username, auth.Password

	default:
		return credential{}, nil, fmt.Errorf("unsupported backend type %q", b.Type)
	}

	return cred, nil, finishCred(&cred, repoURL)
}

// finishCred fills CloneURL from the resolved secret, when a repo URL is given.
func finishCred(cred *credential, repoURL string) error {
	if repoURL == "" || cred.Secret == "" {
		return nil
	}
	cloneURL, err := injectCloneCreds(repoURL, cred.Username, cred.Secret)
	if err != nil {
		return err
	}
	cred.CloneURL = cloneURL
	return nil
}

// gitlabRefresh exchanges an OAuth refresh token for a short-lived access token.
// GitLab rotates refresh tokens, so the new one is returned for persistence.
func gitlabRefresh(ctx context.Context, baseURL string, auth authConfig) (token string, expiresAt time.Time, newRefresh string, err error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {auth.RefreshToken},
		"client_id":     {auth.ClientID},
		"client_secret": {auth.ClientSecret},
	}
	endpoint := strings.TrimRight(baseURL, "/") + "/oauth/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", time.Time{}, "", fmt.Errorf("gitlab oauth refresh: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", time.Time{}, "", fmt.Errorf("gitlab oauth refresh returned %d", resp.StatusCode)
	}
	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || result.AccessToken == "" {
		return "", time.Time{}, "", fmt.Errorf("gitlab oauth refresh: failed to parse response")
	}
	exp := time.Now().Add(2 * time.Hour)
	if result.ExpiresIn > 0 {
		exp = time.Now().Add(time.Duration(result.ExpiresIn) * time.Second)
	}
	return result.AccessToken, exp, result.RefreshToken, nil
}

// forgejoToken is the subset of a Forgejo access-token API object we use.
type forgejoToken struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	SHA1 string `json:"sha1"`
}

// forgejoTokenPrefix names tokens minted by this broker so prior ones can be
// revoked-on-reuse rather than accumulating.
const forgejoTokenPrefix = "codearmory-git-"

// forgejoMintToken revokes the broker's prior tokens for the user and mints a new
// scoped one via the Forgejo admin token API. The returned sha1 is usable as an
// HTTPS clone password for that user.
func forgejoMintToken(ctx context.Context, baseURL, adminToken, username string) (string, error) {
	base := strings.TrimRight(baseURL, "/") + "/api/v1/users/" + url.PathEscape(username) + "/tokens"

	// Revoke prior broker tokens.
	listReq, err := http.NewRequestWithContext(ctx, http.MethodGet, base, nil)
	if err != nil {
		return "", err
	}
	forgejoAuth(listReq, adminToken)
	listResp, err := httpClient.Do(listReq)
	if err == nil {
		func() {
			defer listResp.Body.Close()
			if listResp.StatusCode >= 200 && listResp.StatusCode < 300 {
				var existing []forgejoToken
				if json.NewDecoder(listResp.Body).Decode(&existing) == nil {
					for _, t := range existing {
						if strings.HasPrefix(t.Name, forgejoTokenPrefix) {
							delReq, _ := http.NewRequestWithContext(ctx, http.MethodDelete, fmt.Sprintf("%s/%d", base, t.ID), nil)
							forgejoAuth(delReq, adminToken)
							if dr, derr := httpClient.Do(delReq); derr == nil {
								dr.Body.Close()
							}
						}
					}
				}
			}
		}()
	}

	// Mint a fresh scoped token.
	body, _ := json.Marshal(map[string]any{
		"name":   forgejoTokenPrefix + shortID(),
		"scopes": []string{"read:repository", "write:repository"},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	forgejoAuth(req, adminToken)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("forgejo token create: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("forgejo token create returned %d", resp.StatusCode)
	}
	var created forgejoToken
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil || created.SHA1 == "" {
		return "", fmt.Errorf("forgejo token create: failed to parse response")
	}
	return created.SHA1, nil
}

func forgejoAuth(req *http.Request, adminToken string) {
	req.Header.Set("Authorization", "token "+adminToken)
	req.Header.Set("Accept", "application/json")
}

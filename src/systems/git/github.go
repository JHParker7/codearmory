package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// githubInstallationToken mints a short-lived (~1h) GitHub App installation access
// token. apiBase is the REST API root ("https://api.github.com" for github.com, or
// "https://<host>/api/v3" for GitHub Enterprise). The returned token is scoped to
// the installation and expires server-side.
func githubInstallationToken(ctx context.Context, apiBase string, appID, installationID int64, privateKeyPEM string) (string, time.Time, error) {
	key, err := parseRSAKey(privateKeyPEM)
	if err != nil {
		return "", time.Time{}, err
	}
	jwt, err := makeGitHubJWT(appID, key)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("make JWT: %w", err)
	}

	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", strings.TrimRight(apiBase, "/"), installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("installation token request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", time.Time{}, fmt.Errorf("installation token: GitHub returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var result struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &result); err != nil || result.Token == "" {
		return "", time.Time{}, fmt.Errorf("installation token: failed to parse response")
	}
	expiresAt := time.Now().Add(time.Hour)
	if t, err := time.Parse(time.RFC3339, result.ExpiresAt); err == nil {
		expiresAt = t
	}
	return result.Token, expiresAt, nil
}

// parseRSAKey decodes a PEM RSA private key, trying PKCS1 then PKCS8.
func parseRSAKey(privateKeyPEM string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(privateKeyPEM))
	if block == nil {
		return nil, fmt.Errorf("failed to decode PEM block from GitHub App private key")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse GitHub App private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("GitHub App private key is not an RSA key")
	}
	return key, nil
}

// makeGitHubJWT builds a signed RS256 JWT for GitHub App authentication.
func makeGitHubJWT(appID int64, key *rsa.PrivateKey) (string, error) {
	now := time.Now()
	headerJSON, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	payloadJSON, err := json.Marshal(map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": strconv.FormatInt(appID, 10),
	})
	if err != nil {
		return "", err
	}
	unsigned := b64url(headerJSON) + "." + b64url(payloadJSON)
	hash := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hash[:])
	if err != nil {
		return "", err
	}
	return unsigned + "." + b64url(sig), nil
}

func b64url(data []byte) string {
	return strings.TrimRight(base64.URLEncoding.EncodeToString(data), "=")
}

// githubAPIBase derives the REST API root from a backend base URL. github.com maps
// to api.github.com; any other host is treated as GitHub Enterprise (/api/v3).
func githubAPIBase(host string) string {
	if host == "github.com" || host == "www.github.com" {
		return "https://api.github.com"
	}
	return "https://" + host + "/api/v3"
}

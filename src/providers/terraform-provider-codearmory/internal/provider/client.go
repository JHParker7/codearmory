package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/path"
)

// Client is a thin HTTP client for the codearmory API. All routes are
// service-prefixed (e.g. /forge/runner-classes, /hooks/rules) because every
// request goes through the Conductor gateway.
type Client struct {
	endpoint string
	token    string
	http     *http.Client
}

func newClient(endpoint, token string) *Client {
	return &Client{
		endpoint: strings.TrimRight(endpoint, "/"),
		token:    token,
		http:     &http.Client{Timeout: 30 * time.Second},
	}
}

// fetchClientCredentialsToken exchanges an OAuth client_id/secret for a bearer
// access token via gatekeeper's token endpoint (proxied through Conductor).
func fetchClientCredentialsToken(ctx context.Context, endpoint, clientID, clientSecret string) (string, error) {
	endpoint = strings.TrimRight(endpoint, "/")
	form := url.Values{"grant_type": {"client_credentials"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		endpoint+"/gatekeeper/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(clientID, clientSecret)

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token request returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var tr struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if tr.AccessToken == "" {
		return "", fmt.Errorf("token response missing access_token")
	}
	return tr.AccessToken, nil
}

// do performs an authenticated request. body is JSON-encoded when non-nil.
// It returns the HTTP status code and raw response body; non-2xx is not an error
// here so callers can branch on status (e.g. 404 → remove from state).
func (c *Client) do(ctx context.Context, method, p string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+p, reader)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, respBody, nil
}

// apiError formats a non-2xx response into a diagnostic-friendly error.
func apiError(action string, status int, body []byte) error {
	return fmt.Errorf("%s: unexpected status %d: %s", action, status, strings.TrimSpace(string(body)))
}

// pathRoot is a tiny helper so provider.go doesn't need to import the path package.
func pathRoot(name string) path.Path { return path.Root(name) }

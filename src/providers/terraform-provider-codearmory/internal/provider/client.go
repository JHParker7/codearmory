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

	"github.com/hashicorp/terraform-plugin-framework/diag"
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

// do2xx runs a request, treats anything but wantStatus as a failure, and decodes
// the body into out (when non-nil). 404 is reported via notFound (with no
// diagnostic) so callers can branch — resources remove themselves from state,
// the data source raises a not-found error. summary prefixes diagnostics so each
// caller keeps its specific wording; action is used in the unexpected-status text.
func (c *Client) do2xx(ctx context.Context, summary, action, method, p string, body, out any, wantStatus int) (notFound bool, diags diag.Diagnostics) {
	status, respBody, err := c.do(ctx, method, p, body)
	if err != nil {
		diags.AddError(summary, err.Error())
		return false, diags
	}
	if status == http.StatusNotFound {
		return true, diags
	}
	if status != wantStatus {
		diags.AddError(summary, apiError(action, status, respBody).Error())
		return false, diags
	}
	if out != nil {
		if err := json.Unmarshal(respBody, out); err != nil {
			diags.AddError("Decode response failed", err.Error())
		}
	}
	return false, diags
}

// clientFromProviderData extracts the configured *Client from a resource or
// data-source ConfigureRequest. It returns (nil, nil) during early validation,
// before the provider has been configured.
func clientFromProviderData(data any) (*Client, diag.Diagnostics) {
	var diags diag.Diagnostics
	if data == nil {
		return nil, diags
	}
	client, ok := data.(*Client)
	if !ok {
		diags.AddError("Unexpected provider data", fmt.Sprintf("expected *Client, got %T", data))
		return nil, diags
	}
	return client, diags
}

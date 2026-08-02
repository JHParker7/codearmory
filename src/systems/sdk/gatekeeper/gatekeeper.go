// Package gatekeeper provides a permission-check client for services that
// delegate authorisation to the gatekeeper service.
package gatekeeper

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// Client calls gatekeeper on behalf of a named service: /check_permissions to
// authorise a caller, and /internal/audit-logs to contribute to the platform audit
// trail.
type Client struct {
	// URL is the gatekeeper base URL, e.g. "http://localhost:8080".
	URL string
	// Service is this service's registered name, e.g. "hooks".
	Service string
	// HTTPClient is used for outbound requests; defaults to http.DefaultClient.
	HTTPClient *http.Client
	// ServiceKey returns the current east-west service key — the accessor
	// registry.StartKeyRotation hands back. Only Audit needs it (CheckPermissions
	// authenticates with the caller's own bearer), so leaving it nil is valid and
	// simply makes Audit a no-op. It is a func, not a string, because the key
	// rotates every 25 minutes and a captured copy goes stale.
	ServiceKey func() string
}

func (c *Client) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

// Subject is the authenticated caller, as gatekeeper resolved them.
type Subject struct {
	// UserID is the caller's stable id — what records store as their owner.
	UserID string
	// Username is the caller's RBAC NAMESPACE. Gatekeeper keys resources by username,
	// not by user id (see scopeResource), so this is the segment an owner-first
	// resource needs. Empty for a client-credentials subject, which owns no namespace.
	Username string
	// OrgID is the caller's org, or "" when they belong to none.
	OrgID string
}

// Namespace returns the caller's own RBAC namespace and whether it is known.
//
// It returns two values deliberately. The tempting one-liner —
// `sub.Username + "/tickets/tickets/" + id` — silently produces "/tickets/tickets/x"
// when the username is absent, which matches no grant. That fails closed, so it is not
// a security problem, but it is an opaque 403 with no way to tell a permission the
// caller genuinely lacks from a namespace the service never had. Handle the false case
// rather than building a resource around an empty string.
func (s Subject) Namespace() (string, bool) {
	return s.Username, s.Username != ""
}

// CheckPermissions validates the caller's Bearer JWT and returns (userID, orgID, true)
// when gatekeeper authorises the (action, resource) pair for c.Service.
// On any failure it writes the appropriate HTTP error and returns ("", "", false).
//
// Prefer Check when the handler needs to build an owner-first resource: this form
// cannot report the caller's username, which is the namespace such a resource needs.
func (c *Client) CheckPermissions(ctx context.Context, w http.ResponseWriter, r *http.Request, action, resource string) (userID, orgID string, ok bool) {
	sub, ok := c.Check(ctx, w, r, action, resource)
	return sub.UserID, sub.OrgID, ok
}

// Check is CheckPermissions returning the full resolved Subject, including the caller's
// username.
//
// That extra field is what lets a service name an owner. Gatekeeper prefixes the
// CALLER's namespace to an unscoped resource, so "tickets/tickets/{id}" evaluates
// identically whoever asks and authorises every id — a per-record gate in appearance
// only. Sending an owner-first resource instead is the fix, and it needs a namespace to
// name; before this the response carried only user_id, so a service had to make a second
// round trip (/oauth/userinfo) just to learn who its own caller was.
//
// On any failure it writes the appropriate HTTP error and returns (Subject{}, false).
func (c *Client) Check(ctx context.Context, w http.ResponseWriter, r *http.Request, action, resource string) (Subject, bool) {
	token, hasBearerPrefix := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !hasBearerPrefix || token == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return Subject{}, false
	}

	body, _ := json.Marshal(map[string]string{
		"service":  c.Service,
		"resource": resource,
		"action":   action,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/check_permissions", bytes.NewReader(body))
	if err != nil {
		slog.Error("gatekeeper: failed to build check_permissions request", "service", c.Service, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return Subject{}, false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.client().Do(req)
	if err != nil {
		slog.Error("gatekeeper: check_permissions request failed", "service", c.Service, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return Subject{}, false
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return Subject{}, false
	}
	if resp.StatusCode >= 500 {
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		slog.Error("gatekeeper: service unavailable", "service", c.Service, "status", resp.StatusCode)
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return Subject{}, false
	}

	var result struct {
		Authorized bool    `json:"authorized"`
		UserID     string  `json:"user_id"`
		OrgID      *string `json:"org_id"`
		// Absent on an older gatekeeper and for a client-credentials subject, so this
		// stays the zero value rather than being required — the SDK must keep working
		// against a control plane that has not been upgraded yet.
		Username string `json:"username"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || !result.Authorized {
		http.Error(w, "forbidden", http.StatusForbidden)
		return Subject{}, false
	}
	org := ""
	if result.OrgID != nil {
		org = *result.OrgID
	}
	return Subject{UserID: result.UserID, Username: result.Username, OrgID: org}, true
}

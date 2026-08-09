package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Org mirrors gatekeeper's Org type (systems/gatekeeper/types.go). GET /orgs/{id}
// returns the full row, so every field is populated.
type Org struct {
	OrgID     string    `json:"org_id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	OrgName   string    `json:"org_name"`
	OwnerID   string    `json:"owner_id"`
	Active    bool      `json:"active"`
}

// User mirrors the SAFE public shape gatekeeper returns from GET /users/{id}
// (its internal userResponse), NOT the full User row. Gatekeeper deliberately
// omits HashedPassword, Email, and the name fields from the API, so those can
// never be populated here — do not add them expecting values.
type User struct {
	UserID    string    `json:"user_id"`
	Username  string    `json:"username"`
	OrgID     *string   `json:"org_id"`
	TeamID    *string   `json:"team_id"`
	RoleID    *string   `json:"role_id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Active    bool      `json:"active"`
}

// getOrg resolves an org id to an Org via gatekeeper's GET /orgs/{id}. bearerToken
// is the caller's JWT (forward the inbound request's token) — the endpoint runs a
// getOrg permission check against that identity.
func getOrg(ctx context.Context, bearerToken, orgID string) (Org, error) {
	var org Org
	if err := gatekeeperGet(ctx, bearerToken, "/orgs/"+orgID, &org); err != nil {
		return Org{}, fmt.Errorf("resolve org %q: %w", orgID, err)
	}
	return org, nil
}

// getUser resolves a user id to a User via gatekeeper's GET /users/{id}. bearerToken
// is the caller's JWT (forward the inbound request's token) — the endpoint runs a
// getUser permission check against that identity.
func getUser(ctx context.Context, bearerToken, userID string) (User, error) {
	var user User
	if err := gatekeeperGet(ctx, bearerToken, "/users/"+userID, &user); err != nil {
		return User{}, fmt.Errorf("resolve user %q: %w", userID, err)
	}
	return user, nil
}

// getUserByName resolves a USERNAME to a User. gatekeeper's /users listing is the only
// way round: it has no by-name lookup, and a namespace is a username rather than an id,
// so a transfer addressed to "alice" has to become a user_id somehow.
//
// The match is exact and case-sensitive, because a namespace is a path segment: matching
// "Alice" to "alice" here would hand the repo to a namespace that does not route.
func getUserByName(ctx context.Context, bearerToken, username string) (User, error) {
	var users []User
	if err := gatekeeperGet(ctx, bearerToken, "/users", &users); err != nil {
		return User{}, fmt.Errorf("list users: %w", err)
	}
	for _, u := range users {
		if u.Username == username {
			return u, nil
		}
	}
	return User{}, fmt.Errorf("no user named %q", username)
}

// gatekeeperGet issues an authenticated GET to gatekeeper and decodes the JSON body
// into out. It maps gatekeeper's HTTP status codes onto Go errors.
func gatekeeperGet(ctx context.Context, bearerToken, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gatekeeperURL+path, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+bearerToken)

	client := httpClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("gatekeeper request failed: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
		return nil
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("gatekeeper: not found (404)")
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("gatekeeper: not authorized (%d)", resp.StatusCode)
	default:
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		return fmt.Errorf("gatekeeper: unexpected status %d", resp.StatusCode)
	}
}

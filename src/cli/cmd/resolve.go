package cmd

import (
	"encoding/json"
	"fmt"
	"strings"
)

// resolveOrgID accepts an org name or UUID and returns the org_id.
func resolveOrgID(nameOrID string) (string, error) {
	return resolveID("/orgs", "org_id", nameOrID, "org_name")
}

// resolveTeamID accepts a team name or UUID and returns the team_id.
func resolveTeamID(nameOrID string) (string, error) {
	return resolveID("/teams", "team_id", nameOrID, "team_name")
}

// resolveUserID accepts a username, email, or UUID and returns the user_id.
func resolveUserID(nameOrID string) (string, error) {
	return resolveID("/users", "user_id", nameOrID, "username", "email")
}

// resolveRoleID accepts a role name or UUID and returns the role_id.
func resolveRoleID(nameOrID string) (string, error) {
	return resolveID("/roles", "role_id", nameOrID, "name")
}

// resolveID looks up nameOrID against the given list endpoint.
// If nameOrID is already a UUID it is returned as-is without a network call.
// Otherwise the list is fetched and each nameField is checked case-insensitively.
func resolveID(listPath, idField, nameOrID string, nameFields ...string) (string, error) {
	if looksLikeUUID(nameOrID) {
		return nameOrID, nil
	}
	data, err := doRequest("GET", listPath, nil)
	if err != nil {
		return "", err
	}
	var items []map[string]any
	if err := json.Unmarshal(data, &items); err != nil {
		return "", fmt.Errorf("parsing response: %w", err)
	}
	lower := strings.ToLower(nameOrID)
	for _, item := range items {
		for _, field := range nameFields {
			if val, ok := item[field].(string); ok && strings.ToLower(val) == lower {
				if id, ok := item[idField].(string); ok {
					return id, nil
				}
			}
		}
	}
	return "", fmt.Errorf("%q: not found", nameOrID)
}

// looksLikeUUID returns true for standard RFC 4122 UUIDs
// (xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx).
func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				return false
			}
		}
	}
	return true
}

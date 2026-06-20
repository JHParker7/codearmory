package cmd

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

var usersCmd = &cobra.Command{
	Use:   "users",
	Short: "Manage users",
}

func init() {
	var (
		updateEmail     string
		updateUsername  string
		updatePassword  string
		updateFirstname string
		updateLastname  string
	)

	updateCmd := &cobra.Command{
		Use:   "update <name-or-id>",
		Short: "Update a user",
		Long: `Update a user's profile fields.

  armory users update <name-or-id> --email new@example.com --username newname
  armory users update <name-or-id> --firstname Alice --lastname Smith`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if updateEmail == "" && updateUsername == "" && updateFirstname == "" && updateLastname == "" && updatePassword == "" {
				return fmt.Errorf("at least one flag is required (--email, --username, --firstname, --lastname, --password)")
			}
			id, err := resolveUserID(args[0])
			if err != nil {
				return err
			}
			payload := map[string]any{}
			if updateEmail != "" {
				payload["email"] = updateEmail
			}
			if updateUsername != "" {
				payload["username"] = updateUsername
			}
			if updatePassword != "" {
				payload["password"] = updatePassword
			}
			if updateFirstname != "" {
				payload["firstname"] = updateFirstname
			}
			if updateLastname != "" {
				payload["lastname"] = updateLastname
			}
			body, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			return apiCall("PUT", "/gatekeeper/users/"+id, body)
		},
	}
	updateCmd.Flags().StringVar(&updateEmail, "email", "", "New email address")
	updateCmd.Flags().StringVar(&updateUsername, "username", "", "New username")
	updateCmd.Flags().StringVar(&updatePassword, "password", "", "New password")
	updateCmd.Flags().StringVar(&updateFirstname, "firstname", "", "First name")
	updateCmd.Flags().StringVar(&updateLastname, "lastname", "", "Last name")

	usersCmd.AddCommand(
		&cobra.Command{
			Use:   "me",
			Short: "Get your own profile",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				id, err := subjectFromToken(bearerToken())
				if err != nil {
					return fmt.Errorf("not logged in or invalid token — run `armory auth login`")
				}
				return apiCall("GET", "/gatekeeper/users/"+id, nil)
			},
		},
		&cobra.Command{
			Use:   "list",
			Short: "List all users",
			RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("GET", "/gatekeeper/users", nil) },
		},
		&cobra.Command{
			Use:   "get <name-or-id>",
			Short: "Get a user",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				id, err := resolveUserID(args[0])
				if err != nil {
					return err
				}
				return apiCall("GET", "/gatekeeper/users/"+id, nil)
			},
		},
		updateCmd,
		&cobra.Command{
			Use:   "delete <name-or-id>",
			Short: "Delete a user",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				id, err := resolveUserID(args[0])
				if err != nil {
					return err
				}
				return apiCall("DELETE", "/gatekeeper/users/"+id, nil)
			},
		},
	)
	RegisterModule(Module{Name: "users", Admin: true, Command: usersCmd})
}

// myProfile fetches the authenticated user's own profile.
func myProfile() (map[string]any, error) {
	sub, err := subjectFromToken(bearerToken())
	if err != nil {
		return nil, fmt.Errorf("not logged in or invalid token — run `armory auth login`")
	}
	data, err := doRequest("GET", "/gatekeeper/users/"+sub, nil)
	if err != nil {
		return nil, err
	}
	var profile map[string]any
	if err := json.Unmarshal(data, &profile); err != nil {
		return nil, fmt.Errorf("parsing profile: %w", err)
	}
	return profile, nil
}

// myFieldID returns a UUID field from the current user's profile, e.g. "org_id".
// Returns a clear error if the user has no membership in that resource.
func myFieldID(field string) (string, error) {
	profile, err := myProfile()
	if err != nil {
		return "", err
	}
	resource := strings.TrimSuffix(field, "_id")
	id, ok := profile[field].(string)
	if !ok || id == "" {
		return "", fmt.Errorf("you are not a member of any %s", resource)
	}
	return id, nil
}

// subjectFromToken extracts the "sub" claim from a JWT without verifying the
// signature. The server will verify it; we just need the user ID to build the
// request path.
func subjectFromToken(token string) (string, error) {
	parts := strings.SplitN(token, ".", 3)
	if len(parts) != 3 {
		return "", fmt.Errorf("invalid token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("decoding token: %w", err)
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Sub == "" {
		return "", fmt.Errorf("no subject in token")
	}
	return claims.Sub, nil
}

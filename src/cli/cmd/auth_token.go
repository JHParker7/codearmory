package cmd

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Scoped tokens: a credential you mint for yourself that can do LESS than you can.
//
// The problem this solves is that a session token is all-or-nothing — a pipeline that
// only files tickets ends up carrying a credential that can delete repositories. A
// scoped token names exactly the permissions it needs, expires, and can be revoked on
// its own without touching your login.
//
// Consumption is free: the token IS a session token, so anything that reads
// CODEARMORY_TOKEN accepts it, and git takes it as the password in HTTP Basic — which
// is what makes a token scoped to one repo a deploy key.

var authTokenCmd = &cobra.Command{
	Use:   "token",
	Short: "Mint, list and revoke scoped tokens",
	Long: `Scoped tokens are credentials restricted to a SUBSET of the permissions you
already hold. Use one wherever a full session token is more authority than the job
needs — CI, a pipeline, a git client on a shared machine.

A token can never carry a permission you do not hold, or reach outside your own
namespace; either is refused rather than quietly dropped. The token is displayed once,
at creation, and cannot be retrieved afterwards.`,
}

// canFlags are the --can values, "service:action:resource" or "service:action" with
// the resource defaulting to the caller's whole namespace for that service.
var (
	tokenCan       []string
	tokenExpiresIn int
)

// parsePermissionFlag turns a --can value into the permission triple the API expects.
//
// All three parts are required. The resource is not defaulted, here or on the server:
// resources are per-collection ("alice/tickets/tickets"), so any guess broad enough to
// cover them — "alice/tickets/*" — is broader than the grants a user holds and would be
// refused. `armory admin permissions` lists the resource strings you can name.
func parsePermissionFlag(raw string) (map[string]any, error) {
	parts := strings.SplitN(strings.TrimSpace(raw), ":", 3)
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" || strings.TrimSpace(parts[2]) == "" {
		return nil, fmt.Errorf("--can %q: expected service:action:resource, e.g. tickets:createTicket:alice/tickets/tickets", raw)
	}
	return map[string]any{
		"service":  parts[0],
		"action":   parts[1],
		"resource": strings.TrimSpace(parts[2]),
	}, nil
}

var authTokenCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Mint a scoped token (shown once)",
	Example: `  armory auth token create ci --can tickets:createTicket:alice/tickets/tickets
  armory auth token create deploy-key --can codearmory_git_factory:readRepo:alice/codearmory_git_factory/repos/abc --expires-in 30`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(tokenCan) == 0 {
			return fmt.Errorf("a token needs at least one --can service:action:resource")
		}
		perms := make([]map[string]any, 0, len(tokenCan))
		for _, raw := range tokenCan {
			p, err := parsePermissionFlag(raw)
			if err != nil {
				return err
			}
			perms = append(perms, p)
		}

		body := map[string]any{"name": args[0], "permissions": perms}
		if tokenExpiresIn > 0 {
			body["expires_in_days"] = tokenExpiresIn
		}
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		data, err := doRequest("POST", "/gatekeeper/tokens", raw)
		if err != nil {
			return err
		}

		var created struct {
			TokenID   string    `json:"token_id"`
			Token     string    `json:"token"`
			Name      string    `json:"name"`
			ExpiresAt time.Time `json:"expires_at"`
		}
		if err := json.Unmarshal(data, &created); err != nil {
			printResponse(data)
			return nil
		}
		// Printed with the warning attached, because there is no second chance: the
		// server stores only the session's public key and cannot show it again.
		fmt.Printf("Token %q created (id %s, expires %s).\n", created.Name, created.TokenID, created.ExpiresAt.Format(time.RFC3339))
		fmt.Println("Copy it now — it is shown once and cannot be retrieved later:")
		fmt.Println()
		fmt.Println("  " + created.Token)
		fmt.Println()
		fmt.Println("Use it by exporting CODEARMORY_TOKEN, or as the password for a git clone.")
		return nil
	},
}

var authTokenListCmd = &cobra.Command{
	Use:   "list",
	Short: "List your scoped tokens",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return apiCall("GET", "/gatekeeper/tokens", nil)
	},
}

var authTokenRevokeCmd = &cobra.Command{
	Use:   "revoke <token-id>",
	Short: "Revoke a scoped token immediately",
	Long: `Revocation takes effect at once, not at expiry: the token's session is
deactivated, so the next request carrying it is rejected before any permission is
evaluated.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return apiCall("DELETE", "/gatekeeper/tokens/"+args[0], nil)
	},
}

func init() {
	authTokenCreateCmd.Flags().StringArrayVar(&tokenCan, "can", nil,
		"permission the token may use: service:action:resource (repeatable)")
	authTokenCreateCmd.Flags().IntVar(&tokenExpiresIn, "expires-in", 0,
		"lifetime in days (default: the server's 90, maximum 365)")
	authTokenCmd.AddCommand(authTokenCreateCmd, authTokenListCmd, authTokenRevokeCmd)
	authCmd.AddCommand(authTokenCmd)
}

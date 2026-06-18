package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

var secretsCmd = &cobra.Command{
	Use:   "secrets",
	Short: "Manage encrypted secrets and secret providers",
}

func init() {
	// ── armory secrets create ─────────────────────────────────────────────────

	secretsCmd.AddCommand(
		&cobra.Command{
			Use:   "create <name> <value>",
			Short: "Store a new secret",
			Long: `Create a named secret in the org's secret store.

  armory secrets create DATABASE_URL "postgres://..."
  armory secrets create API_KEY "sk-..."`,
			Args: cobra.ExactArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				body, err := json.Marshal(map[string]string{"name": args[0], "value": args[1]})
				if err != nil {
					return err
				}
				return apiCall("POST", "/gatekeeper/secrets", body)
			},
		},

		// ── armory secrets list ───────────────────────────────────────────────
		&cobra.Command{
			Use:   "list",
			Short: "List secrets (names only — values are never returned)",
			Args:  cobra.NoArgs,
			RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("GET", "/gatekeeper/secrets", nil) },
		},

		// ── armory secrets update ─────────────────────────────────────────────
		secretsUpdateCmd(),

		// ── armory secrets delete ─────────────────────────────────────────────
		&cobra.Command{
			Use:   "delete <id>",
			Short: "Delete a secret",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return apiCall("DELETE", "/gatekeeper/secrets/"+args[0], nil)
			},
		},

		// ── armory secrets provider ───────────────────────────────────────────
		secretsProviderCmd(),
	)

	RegisterModule(Module{Name: "secrets", Command: secretsCmd})
}

func secretsUpdateCmd() *cobra.Command {
	var value string
	cmd := &cobra.Command{
		Use:   "update <id>",
		Short: "Update a secret's value",
		Long: `Replace the stored value for a secret.

  armory secrets update <id> --value "new-value"`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if value == "" {
				return fmt.Errorf("--value is required")
			}
			body, err := json.Marshal(map[string]string{"value": value})
			if err != nil {
				return err
			}
			return apiCall("PUT", "/gatekeeper/secrets/"+args[0], body)
		},
	}
	cmd.Flags().StringVar(&value, "value", "", "New secret value (required)")
	return cmd
}

func secretsProviderCmd() *cobra.Command {
	providerCmd := &cobra.Command{
		Use:   "provider",
		Short: "Manage the org's external secret provider",
	}

	var providerConfig string

	setCmd := &cobra.Command{
		Use:   "set <org-id> <provider>",
		Short: "Configure an external secret provider for an org",
		Long: `Set the secret provider for an org. Provider must be one of:
  builtin, doppler, vault, aws_sm

  # Use vault with address config:
  armory secrets provider set <org-id> vault --config '{"address":"https://vault.example.com"}'

  # Switch back to built-in:
  armory secrets provider set <org-id> builtin`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			orgID, provider := args[0], args[1]
			payload := map[string]any{"provider": provider}
			if providerConfig != "" {
				var cfg map[string]any
				if err := json.Unmarshal([]byte(providerConfig), &cfg); err != nil {
					return fmt.Errorf("--config must be valid JSON: %w", err)
				}
				payload["config"] = cfg
			}
			body, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			return apiCall("PUT", "/gatekeeper/orgs/"+orgID+"/secret-provider", body)
		},
	}
	setCmd.Flags().StringVar(&providerConfig, "config", "", "Provider-specific config as JSON (e.g. vault address)")

	providerCmd.AddCommand(
		&cobra.Command{
			Use:   "get <org-id>",
			Short: "Get the current secret provider for an org",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return apiCall("GET", "/gatekeeper/orgs/"+args[0]+"/secret-provider", nil)
			},
		},
		setCmd,
		&cobra.Command{
			Use:   "delete <org-id>",
			Short: "Remove the external secret provider (resets to built-in)",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return apiCall("DELETE", "/gatekeeper/orgs/"+args[0]+"/secret-provider", nil)
			},
		},
	)

	return providerCmd
}

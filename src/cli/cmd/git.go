package cmd

import (
	"encoding/json"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
)

// The git module is a front-end for the `git` core service: a backend-agnostic
// git credential broker. An admin registers one or more backends (GitHub,
// GitLab, Forgejo, or a generic basic-auth host), each holding its own
// credentials; users then mint short-lived clone credentials for a repo URL,
// which the broker resolves to the matching backend. Secrets are never returned
// in backend reads — only credential minting and the connection test surface
// live secrets / expiries.

var gitCmd = &cobra.Command{
	Use:   "git",
	Short: "Manage git backends and mint clone credentials",
}

// gitBackendAuth assembles the auth object for a backend from the flags set on a
// create command, validating that the fields required by (type, mode) are
// present. It mirrors the server's accepted (type → mode → fields) matrix.
func gitBackendAuth(typ, mode string, flags gitAuthFlags) (map[string]any, error) {
	auth := map[string]any{}
	switch typ {
	case "github":
		switch mode {
		case "app":
			if flags.appID == 0 || flags.installationID == 0 || flags.privateKey == "" {
				return nil, fmt.Errorf("github app auth requires --app-id, --installation-id and --private-key")
			}
			auth["mode"] = "app"
			auth["app_id"] = flags.appID
			auth["installation_id"] = flags.installationID
			auth["private_key"] = flags.privateKey
		case "pat":
			if flags.token == "" {
				return nil, fmt.Errorf("github pat auth requires --token")
			}
			auth["mode"] = "pat"
			auth["token"] = flags.token
			if flags.username != "" {
				auth["username"] = flags.username
			}
		default:
			return nil, fmt.Errorf("github auth mode must be app or pat, got %q", mode)
		}
	case "gitlab":
		switch mode {
		case "token":
			if flags.token == "" {
				return nil, fmt.Errorf("gitlab token auth requires --token")
			}
			auth["mode"] = "token"
			auth["token"] = flags.token
		case "oauth":
			if flags.refreshToken == "" || flags.clientID == "" || flags.clientSecret == "" {
				return nil, fmt.Errorf("gitlab oauth auth requires --refresh-token, --client-id and --client-secret")
			}
			auth["mode"] = "oauth"
			auth["refresh_token"] = flags.refreshToken
			auth["client_id"] = flags.clientID
			auth["client_secret"] = flags.clientSecret
		default:
			return nil, fmt.Errorf("gitlab auth mode must be token or oauth, got %q", mode)
		}
	case "forgejo":
		switch mode {
		case "token":
			if flags.token == "" || flags.username == "" {
				return nil, fmt.Errorf("forgejo token auth requires --token and --username")
			}
			auth["mode"] = "token"
			auth["token"] = flags.token
			auth["username"] = flags.username
		case "admin":
			if flags.adminToken == "" || flags.username == "" {
				return nil, fmt.Errorf("forgejo admin auth requires --admin-token and --username")
			}
			auth["mode"] = "admin"
			auth["admin_token"] = flags.adminToken
			auth["username"] = flags.username
		default:
			return nil, fmt.Errorf("forgejo auth mode must be token or admin, got %q", mode)
		}
	case "generic":
		switch mode {
		case "basic":
			if flags.username == "" || flags.password == "" {
				return nil, fmt.Errorf("generic basic auth requires --username and --password")
			}
			auth["mode"] = "basic"
			auth["username"] = flags.username
			auth["password"] = flags.password
		default:
			return nil, fmt.Errorf("generic auth mode must be basic, got %q", mode)
		}
	default:
		return nil, fmt.Errorf("type must be one of github, gitlab, forgejo, generic; got %q", typ)
	}
	return auth, nil
}

// gitAuthFlags is the union of every auth field across backend types, populated
// from the create command's flags. Only the fields valid for the chosen
// (type, mode) are consulted by gitBackendAuth.
type gitAuthFlags struct {
	mode           string
	token          string
	username       string
	password       string
	appID          int
	installationID int
	privateKey     string
	refreshToken   string
	clientID       string
	clientSecret   string
	adminToken     string
}

func init() {
	// ── armory git add ─────────────────────────────────────────────────────────

	var (
		addName    string
		addType    string
		addBaseURL string
		authFlags  gitAuthFlags
	)

	addCmd := &cobra.Command{
		Use:   "add",
		Short: "Register a git backend",
		Long: `Register a git backend that holds credentials for a git host.

  # GitHub App
  armory git add --name gh --type github --auth-mode app \
    --app-id 123 --installation-id 456 --private-key "$(cat key.pem)"

  # GitHub personal access token
  armory git add --name gh --type github --auth-mode pat --token ghp_xxx

  # GitLab token
  armory git add --name gl --type gitlab --auth-mode token --token glpat-xxx

  # Forgejo token
  armory git add --name fj --type forgejo --auth-mode token --token xxx --username alice

  # Generic basic-auth host
  armory git add --name g --type generic --base-url https://git.example.com \
    --auth-mode basic --username alice --password secret`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if addName == "" {
				return fmt.Errorf("--name is required")
			}
			if addType == "" || authFlags.mode == "" {
				return fmt.Errorf("--type and --auth-mode are required")
			}
			auth, err := gitBackendAuth(addType, authFlags.mode, authFlags)
			if err != nil {
				return err
			}
			payload := map[string]any{"name": addName, "type": addType, "auth": auth}
			if addBaseURL != "" {
				payload["base_url"] = addBaseURL
			}
			body, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			return apiCall("POST", "/git/backends", body)
		},
	}
	addCmd.Flags().StringVar(&addName, "name", "", "Backend name (required)")
	addCmd.Flags().StringVar(&addType, "type", "", "Backend type: github|gitlab|forgejo|generic (required)")
	addCmd.Flags().StringVar(&addBaseURL, "base-url", "", "Base URL of the git host (required for generic/self-hosted)")
	addCmd.Flags().StringVar(&authFlags.mode, "auth-mode", "", "Auth mode for the type (required)")
	addCmd.Flags().StringVar(&authFlags.token, "token", "", "Access token (github pat / gitlab token / forgejo token)")
	addCmd.Flags().StringVar(&authFlags.username, "username", "", "Username (forgejo / generic / optional for github pat)")
	addCmd.Flags().StringVar(&authFlags.password, "password", "", "Password (generic basic auth)")
	addCmd.Flags().IntVar(&authFlags.appID, "app-id", 0, "GitHub App ID (github app auth)")
	addCmd.Flags().IntVar(&authFlags.installationID, "installation-id", 0, "GitHub App installation ID (github app auth)")
	addCmd.Flags().StringVar(&authFlags.privateKey, "private-key", "", "GitHub App private key PEM (github app auth)")
	addCmd.Flags().StringVar(&authFlags.refreshToken, "refresh-token", "", "OAuth refresh token (gitlab oauth auth)")
	addCmd.Flags().StringVar(&authFlags.clientID, "client-id", "", "OAuth client id (gitlab oauth auth)")
	addCmd.Flags().StringVar(&authFlags.clientSecret, "client-secret", "", "OAuth client secret (gitlab oauth auth)")
	addCmd.Flags().StringVar(&authFlags.adminToken, "admin-token", "", "Admin token (forgejo admin auth)")

	// ── armory git creds ───────────────────────────────────────────────────────

	var credsShow bool

	credsCmd := &cobra.Command{
		Use:   "creds <repo-url>",
		Short: "Mint short-lived clone credentials for a repo URL",
		Long: `Mint short-lived clone credentials for a repo URL. The matching backend is
resolved from the URL. The secret is redacted unless --show is passed.

  armory git creds https://github.com/owner/repo.git
  armory git creds https://github.com/owner/repo.git --show`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := json.Marshal(map[string]string{"repo_url": args[0]})
			if err != nil {
				return err
			}
			data, err := doRequest("POST", "/git/credentials", body)
			if err != nil {
				return err
			}
			var c gitCreds
			if err := json.Unmarshal(data, &c); err != nil {
				// Fall back to raw output if the shape is unexpected.
				printResponse(data)
				return nil
			}
			if !credsShow {
				c.Secret = "(redacted — pass --show to reveal)"
			}
			out, _ := json.MarshalIndent(c, "", "  ")
			fmt.Println(string(out))
			return nil
		},
	}
	credsCmd.Flags().BoolVar(&credsShow, "show", false, "Reveal the minted secret (default redacted)")

	gitCmd.AddCommand(
		&cobra.Command{
			Use:   "backends",
			Short: "List git backends",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				return apiCall("GET", "/git/backends", nil)
			},
		},
		&cobra.Command{
			Use:   "get <id>",
			Short: "Get a git backend",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return apiCall("GET", "/git/backends/"+args[0], nil)
			},
		},
		addCmd,
		&cobra.Command{
			Use:   "rm <id>",
			Short: "Delete a git backend",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return apiCall("DELETE", "/git/backends/"+args[0], nil)
			},
		},
		&cobra.Command{
			Use:   "test <id>",
			Short: "Test a git backend's connection",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return apiCall("POST", "/git/backends/"+args[0]+"/test", nil)
			},
		},
		credsCmd,
	)

	RegisterModule(Module{
		Name:    "git",
		Slot:    "git",
		Service: "git",
		Order:   78,
		Command: gitCmd,
		Screens: []HubScreen{{
			Title: "Git Backends",
			Desc:  "Register git backends and mint clone credentials",
			New:   func() tea.Model { return newGitModel() },
		}},
	})
}

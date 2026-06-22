package cmd

import (
	"encoding/json"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
)

var reposCmd = &cobra.Command{
	Use:   "repos",
	Short: "Manage Gitea/Forgejo repositories",
}

// splitOwnerName splits "owner/name" into (owner, name) or returns an error.
func splitOwnerName(arg string) (string, string, error) {
	owner, name, ok := strings.Cut(arg, "/")
	if !ok || owner == "" || name == "" {
		return "", "", fmt.Errorf("expected <owner>/<name>, got %q", arg)
	}
	return owner, name, nil
}

func init() {
	// ── account ───────────────────────────────────────────────────────────────

	accountCmd := &cobra.Command{
		Use:   "account",
		Short: "Manage your linked Gitea account",
	}

	var linkToken string

	linkCmd := &cobra.Command{
		Use:   "link",
		Short: "Link your Gitea account with an API token",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if linkToken == "" {
				return fmt.Errorf("--token is required")
			}
			body, err := json.Marshal(map[string]string{"token": linkToken})
			if err != nil {
				return err
			}
			return apiCall("PUT", "/gitea_integration/account", body)
		},
	}
	linkCmd.Flags().StringVar(&linkToken, "token", "", "Gitea personal access token (required)")

	accountCmd.AddCommand(
		&cobra.Command{
			Use:   "get",
			Short: "Get your linked Gitea account",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				return apiCall("GET", "/gitea_integration/account", nil)
			},
		},
		linkCmd,
		&cobra.Command{
			Use:   "unlink",
			Short: "Unlink your Gitea account",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				return apiCall("DELETE", "/gitea_integration/account", nil)
			},
		},
	)

	// ── repos list / create / get / delete ───────────────────────────────────

	var (
		createRepoName    string
		createRepoDesc    string
		createRepoPrivate bool
	)

	createRepoCmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new repository",
		Long: `Create a new repository in your linked Gitea account.

  armory repos create --name my-project --description "My project" --private`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if createRepoName == "" {
				return fmt.Errorf("--name is required")
			}
			payload := map[string]any{"name": createRepoName, "private": createRepoPrivate}
			if createRepoDesc != "" {
				payload["description"] = createRepoDesc
			}
			body, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			return apiCall("POST", "/gitea_integration/repos", body)
		},
	}
	createRepoCmd.Flags().StringVar(&createRepoName, "name", "", "Repository name (required)")
	createRepoCmd.Flags().StringVar(&createRepoDesc, "description", "", "Repository description")
	createRepoCmd.Flags().BoolVar(&createRepoPrivate, "private", false, "Create as private repository")

	// ── repo sub-resource commands ────────────────────────────────────────────

	branchesCmd := &cobra.Command{
		Use:   "branches <owner>/<name>",
		Short: "List branches in a repository",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			owner, name, err := splitOwnerName(args[0])
			if err != nil {
				return err
			}
			return apiCall("GET", "/gitea_integration/repos/"+owner+"/"+name+"/branches", nil)
		},
	}

	tagsCmd := &cobra.Command{
		Use:   "tags <owner>/<name>",
		Short: "List tags in a repository",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			owner, name, err := splitOwnerName(args[0])
			if err != nil {
				return err
			}
			return apiCall("GET", "/gitea_integration/repos/"+owner+"/"+name+"/tags", nil)
		},
	}

	commitsCmd := &cobra.Command{
		Use:   "commits <owner>/<name>",
		Short: "List recent commits in a repository",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			owner, name, err := splitOwnerName(args[0])
			if err != nil {
				return err
			}
			return apiCall("GET", "/gitea_integration/repos/"+owner+"/"+name+"/commits", nil)
		},
	}

	// ── pulls ─────────────────────────────────────────────────────────────────

	pullsCmd := &cobra.Command{
		Use:   "pulls",
		Short: "Manage pull requests",
	}

	var (
		prTitle string
		prBody  string
		prHead  string
		prBase  string
	)

	createPRCmd := &cobra.Command{
		Use:   "create <owner>/<name>",
		Short: "Create a pull request",
		Long: `Create a pull request in a repository.

  armory repos pulls create owner/repo --title "Fix bug" --head feature-branch --base main`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			owner, name, err := splitOwnerName(args[0])
			if err != nil {
				return err
			}
			if prTitle == "" || prHead == "" || prBase == "" {
				return fmt.Errorf("--title, --head, and --base are required")
			}
			payload := map[string]any{
				"title": prTitle,
				"head":  prHead,
				"base":  prBase,
			}
			if prBody != "" {
				payload["body"] = prBody
			}
			body, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			return apiCall("POST", "/gitea_integration/repos/"+owner+"/"+name+"/pulls", body)
		},
	}
	createPRCmd.Flags().StringVar(&prTitle, "title", "", "PR title (required)")
	createPRCmd.Flags().StringVar(&prBody, "body", "", "PR description")
	createPRCmd.Flags().StringVar(&prHead, "head", "", "Source branch (required)")
	createPRCmd.Flags().StringVar(&prBase, "base", "", "Target branch (required)")

	pullsCmd.AddCommand(
		&cobra.Command{
			Use:   "list <owner>/<name>",
			Short: "List pull requests",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				owner, name, err := splitOwnerName(args[0])
				if err != nil {
					return err
				}
				return apiCall("GET", "/gitea_integration/repos/"+owner+"/"+name+"/pulls", nil)
			},
		},
		createPRCmd,
		&cobra.Command{
			Use:   "get <owner>/<name> <index>",
			Short: "Get a pull request by index",
			Args:  cobra.ExactArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				owner, name, err := splitOwnerName(args[0])
				if err != nil {
					return err
				}
				return apiCall("GET", "/gitea_integration/repos/"+owner+"/"+name+"/pulls/"+args[1], nil)
			},
		},
		&cobra.Command{
			Use:   "merge <owner>/<name> <index>",
			Short: "Merge a pull request",
			Args:  cobra.ExactArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				owner, name, err := splitOwnerName(args[0])
				if err != nil {
					return err
				}
				return apiCall("POST", "/gitea_integration/repos/"+owner+"/"+name+"/pulls/"+args[1]+"/merge", nil)
			},
		},
	)

	reposCmd.AddCommand(
		accountCmd,
		&cobra.Command{
			Use:   "list",
			Short: "List your repositories",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				return apiCall("GET", appendProjectParam("/gitea_integration/repos"), nil)
			},
		},
		createRepoCmd,
		&cobra.Command{
			Use:   "get <owner>/<name>",
			Short: "Get a repository",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				owner, name, err := splitOwnerName(args[0])
				if err != nil {
					return err
				}
				return apiCall("GET", "/gitea_integration/repos/"+owner+"/"+name, nil)
			},
		},
		&cobra.Command{
			Use:   "delete <owner>/<name>",
			Short: "Delete a repository",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				owner, name, err := splitOwnerName(args[0])
				if err != nil {
					return err
				}
				return apiCall("DELETE", "/gitea_integration/repos/"+owner+"/"+name, nil)
			},
		},
		&cobra.Command{
			Use:   "project <owner>/<name> [project]",
			Short: "Assign a repo to a project (omit the project to clear it)",
			Args:  cobra.RangeArgs(1, 2),
			RunE: func(cmd *cobra.Command, args []string) error {
				owner, name, err := splitOwnerName(args[0])
				if err != nil {
					return err
				}
				project := ""
				if len(args) == 2 {
					project = args[1]
				}
				body, err := json.Marshal(map[string]string{"project": project})
				if err != nil {
					return err
				}
				return apiCall("PUT", "/gitea_integration/repos/"+owner+"/"+name+"/project", body)
			},
		},
		branchesCmd,
		tagsCmd,
		commitsCmd,
		pullsCmd,
	)

	// "repos" is a capability slot: a deployment backed by GitHub instead of
	// Gitea can register an alternative provider and select it via the
	// "providers" config (providers.repos = "github").
	RegisterModule(Module{
		Name:    "gitea",
		Slot:    "repos",
		Order:   75,
		Command: reposCmd,
		Screens: []HubScreen{{
			Title: "Repos",
			Desc:  "Repositories, branches, commits and pull requests",
			New:   func() tea.Model { return newReposModel() },
		}},
	})
}

package cmd

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
)

var containersCmd = &cobra.Command{
	Use:   "containers",
	Short: "Manage container image repositories",
}

var containersListCmd = &cobra.Command{Use: "list", Short: "List container resources"}
var containersGetCmd = &cobra.Command{Use: "get", Short: "Get a container resource"}
var containersDeleteCmd = &cobra.Command{Use: "delete", Short: "Delete a container resource"}

// splitImage splits "namespace/image" into its two parts.
func splitImage(arg string) (namespace, image string, err error) {
	namespace, image, ok := strings.Cut(arg, "/")
	if !ok || namespace == "" || image == "" {
		return "", "", fmt.Errorf("image must be in namespace/image format, got %q", arg)
	}
	return namespace, image, nil
}

func init() {
	// ── armory containers list repos ──────────────────────────────────────────

	containersListCmd.AddCommand(&cobra.Command{
		Use:   "repos",
		Short: "List image repositories",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("GET", "/containers/repositories", nil) },
	})

	// ── armory containers list tags ───────────────────────────────────────────

	containersListCmd.AddCommand(&cobra.Command{
		Use:   "tags <namespace/image>",
		Short: "List tags for an image",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ns, img, err := splitImage(args[0])
			if err != nil {
				return err
			}
			return apiCall("GET", "/containers/repositories/"+ns+"/"+img+"/tags", nil)
		},
	})

	// ── armory containers get manifest ────────────────────────────────────────

	containersGetCmd.AddCommand(&cobra.Command{
		Use:   "manifest <namespace/image> <reference>",
		Short: "Get an image manifest (tag or digest)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ns, img, err := splitImage(args[0])
			if err != nil {
				return err
			}
			ref := args[1]
			return apiCall("GET", "/containers/repositories/"+ns+"/"+img+"/manifests/"+ref, nil)
		},
	})

	// ── armory containers delete manifest ─────────────────────────────────────

	containersDeleteCmd.AddCommand(&cobra.Command{
		Use:   "manifest <namespace/image> <digest>",
		Short: "Delete an image manifest by digest (sha256:...)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ns, img, err := splitImage(args[0])
			if err != nil {
				return err
			}
			digest := args[1]
			if !strings.HasPrefix(digest, "sha256:") {
				return fmt.Errorf("digest must start with sha256:, got %q", digest)
			}
			return apiCall("DELETE", "/containers/repositories/"+ns+"/"+img+"/manifests/"+digest, nil)
		},
	})

	containersCmd.AddCommand(containersListCmd, containersGetCmd, containersDeleteCmd)
	// "containers" is a capability slot so a deployment using a different image
	// registry can register an alternative provider and select it via the
	// "providers" config (providers.containers = "...").
	RegisterModule(Module{
		Name:    "registry",
		Slot:    "containers",
		Service: "containers",
		Order:   80,
		Command: containersCmd,
		Screens: []HubScreen{{
			Title: "Containers",
			Desc:  "Browse image repositories, tags and manifests",
			New:   func() tea.Model { return newContainersModel() },
		}},
	})
}

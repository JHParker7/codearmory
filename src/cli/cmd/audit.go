package cmd

import (
	"net/url"

	"github.com/spf13/cobra"
)

var auditCmd = &cobra.Command{
	Use:   "audit",
	Short: "View audit logs",
}

func init() {
	var (
		auditActor    string
		auditAction   string
		auditResource string
		auditLimit    string
		auditOffset   string
	)

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List audit log entries",
		Long: `List audit log entries, newest first. Supports filtering and pagination.

  armory audit list
  armory audit list --actor <user-id>
  armory audit list --action role.create
  armory audit list --resource <role-id>
  armory audit list --limit 50 --offset 100`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			q := url.Values{}
			if auditActor != "" {
				q.Set("actor_id", auditActor)
			}
			if auditAction != "" {
				q.Set("action", auditAction)
			}
			if auditResource != "" {
				q.Set("resource_id", auditResource)
			}
			if auditLimit != "" {
				q.Set("limit", auditLimit)
			}
			if auditOffset != "" {
				q.Set("offset", auditOffset)
			}
			path := "/audit-logs"
			if len(q) > 0 {
				path += "?" + q.Encode()
			}
			return apiCall("GET", path, nil)
		},
	}
	listCmd.Flags().StringVar(&auditActor, "actor", "", "Filter by actor user ID")
	listCmd.Flags().StringVar(&auditAction, "action", "", "Filter by action (e.g. role.create, user.delete)")
	listCmd.Flags().StringVar(&auditResource, "resource", "", "Filter by resource ID")
	listCmd.Flags().StringVar(&auditLimit, "limit", "", "Max results to return")
	listCmd.Flags().StringVar(&auditOffset, "offset", "", "Pagination offset")

	auditCmd.AddCommand(listCmd)
	rootCmd.AddCommand(auditCmd)
}

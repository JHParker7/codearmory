package cmd

import (
	"net/url"

	"github.com/spf13/cobra"
)

var serviceRequestsCmd = &cobra.Command{
	Use:   "service-requests",
	Short: "Review and approve service permission requests",
}

func init() {
	var (
		sprService string
		sprStatus  string
	)

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List service permission requests",
		Long: `List service permission requests. Filter by service name or status.

  armory service-requests list
  armory service-requests list --status pending
  armory service-requests list --service forge`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			q := url.Values{}
			if sprService != "" {
				q.Set("service_name", sprService)
			}
			if sprStatus != "" {
				q.Set("status", sprStatus)
			}
			path := "/gatekeeper/service-permission-requests"
			if len(q) > 0 {
				path += "?" + q.Encode()
			}
			return apiCall("GET", path, nil)
		},
	}
	listCmd.Flags().StringVar(&sprService, "service", "", "Filter by service name")
	listCmd.Flags().StringVar(&sprStatus, "status", "", "Filter: pending, approved, declined")

	serviceRequestsCmd.AddCommand(
		listCmd,
		&cobra.Command{
			Use:   "get <id>",
			Short: "Get a service permission request",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return apiCall("GET", "/gatekeeper/service-permission-requests/"+args[0], nil)
			},
		},
		&cobra.Command{
			Use:   "approve <id>",
			Short: "Approve a service permission request",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return apiCall("POST", "/gatekeeper/service-permission-requests/"+args[0]+"/approve", nil)
			},
		},
		&cobra.Command{
			Use:   "decline <id>",
			Short: "Decline a service permission request",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return apiCall("POST", "/gatekeeper/service-permission-requests/"+args[0]+"/decline", nil)
			},
		},
	)

	rootCmd.AddCommand(serviceRequestsCmd)
}

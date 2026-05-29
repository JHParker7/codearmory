package cmd

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

var stateCmd = &cobra.Command{
	Use:   "state",
	Short: "Manage Terraform state (/state/{username}/{workspace})",
}

func readStateFile(file string) ([]byte, error) {
	switch file {
	case "", "-":
		return io.ReadAll(os.Stdin)
	default:
		return os.ReadFile(file)
	}
}

func init() {
	// ── User-scoped state ─────────────────────────────────────────────────────
	var pushFile, lockData string

	pushCmd := &cobra.Command{
		Use:   "push <username> <workspace>",
		Short: "Upload Terraform state",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := readStateFile(pushFile)
			if err != nil {
				return fmt.Errorf("reading state: %w", err)
			}
			return apiCall("POST", "/state/"+args[0]+"/"+args[1], body)
		},
	}
	pushCmd.Flags().StringVarP(&pushFile, "file", "f", "-", "state file path (default: stdin)")

	lockCmd := &cobra.Command{
		Use:   "lock <username> <workspace>",
		Short: "Lock a workspace",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := parseData(lockData)
			if err != nil {
				return err
			}
			return apiCall("LOCK", "/state/"+args[0]+"/"+args[1], body)
		},
	}
	lockCmd.Flags().StringVar(&lockData, "data", "", "lock info JSON or @file")

	var unlockData string
	unlockCmd := &cobra.Command{
		Use:   "unlock <username> <workspace>",
		Short: "Unlock a workspace",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := parseData(unlockData)
			if err != nil {
				return err
			}
			return apiCall("UNLOCK", "/state/"+args[0]+"/"+args[1], body)
		},
	}
	unlockCmd.Flags().StringVar(&unlockData, "data", "", "lock info JSON or @file (must include matching lock ID)")

	stateCmd.AddCommand(
		&cobra.Command{
			Use:   "get <username> <workspace>",
			Short: "Download Terraform state",
			Args:  cobra.ExactArgs(2),
			RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("GET", "/state/"+args[0]+"/"+args[1], nil) },
		},
		pushCmd,
		&cobra.Command{
			Use:   "delete <username> <workspace>",
			Short: "Delete Terraform state",
			Args:  cobra.ExactArgs(2),
			RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("DELETE", "/state/"+args[0]+"/"+args[1], nil) },
		},
		lockCmd,
		unlockCmd,
	)

	rootCmd.AddCommand(stateCmd)
}

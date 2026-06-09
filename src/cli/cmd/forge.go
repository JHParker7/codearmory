package cmd

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

var forgeCmd = &cobra.Command{
	Use:   "forge",
	Short: "Sandboxed code execution and runner class management",
}

func init() {
	// ── armory forge exec ─────────────────────────────────────────────────────

	execCmd := &cobra.Command{
		Use:   "exec",
		Short: "Manage forge executions",
	}

	var (
		execImage       string
		execTimeout     int
		execEnvs        []string
		execRunnerClass string
	)

	runCmd := &cobra.Command{
		Use:   "run -- <cmd> [args...]",
		Short: "Submit a sandboxed execution",
		Long: `Submit a command to run inside a sandboxed container.

  armory forge exec run --image ubuntu:22.04 -- bash -c "echo hello"
  armory forge exec run --image python:3.12 --timeout 120 --env FOO=bar -- python script.py
  armory forge exec run --image golang:1.23 --runner-class large -- go test ./...`,
		Args: func(cmd *cobra.Command, args []string) error {
			if execImage == "" {
				return fmt.Errorf("--image is required")
			}
			if len(args) == 0 {
				return fmt.Errorf("a command is required after --")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			env := map[string]string{}
			for _, e := range execEnvs {
				k, v, ok := strings.Cut(e, "=")
				if !ok {
					return fmt.Errorf("invalid --env %q: must be KEY=VALUE", e)
				}
				env[k] = v
			}
			payload := map[string]any{
				"image":   execImage,
				"command": args,
			}
			if len(env) > 0 {
				payload["env"] = env
			}
			if execTimeout > 0 {
				payload["timeout"] = execTimeout
			}
			if execRunnerClass != "" {
				payload["runner_class"] = execRunnerClass
			}
			body, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			return apiCall("POST", "/forge/executions", body)
		},
	}
	runCmd.Flags().StringVar(&execImage, "image", "", "Container image to run (required)")
	runCmd.Flags().IntVar(&execTimeout, "timeout", 0, "Timeout in seconds (default: server default)")
	runCmd.Flags().StringArrayVar(&execEnvs, "env", nil, "Environment variable KEY=VALUE (repeatable)")
	runCmd.Flags().StringVar(&execRunnerClass, "runner-class", "", "Runner class name (default: standard)")

	var execListStatus string

	listExecCmd := &cobra.Command{
		Use:   "list",
		Short: "List your executions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path := "/forge/executions"
			if execListStatus != "" {
				path += "?status=" + url.QueryEscape(execListStatus)
			}
			return apiCall("GET", path, nil)
		},
	}
	listExecCmd.Flags().StringVar(&execListStatus, "status", "", "Filter: pending, running, completed, failed, timed_out, cancelled")

	execCmd.AddCommand(
		runCmd,
		listExecCmd,
		&cobra.Command{
			Use:   "get <id>",
			Short: "Get an execution",
			Args:  cobra.ExactArgs(1),
			RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("GET", "/forge/executions/"+args[0], nil) },
		},
		&cobra.Command{
			Use:   "cancel <id>",
			Short: "Cancel a running or pending execution",
			Args:  cobra.ExactArgs(1),
			RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("DELETE", "/forge/executions/"+args[0], nil) },
		},
	)

	// ── armory forge runner-classes ───────────────────────────────────────────

	rcCmd := &cobra.Command{
		Use:   "runner-classes",
		Short: "Manage forge runner class definitions",
	}

	var (
		rcName          string
		rcMemoryMB      int64
		rcCPUMillicores int64
		rcPidsLimit     int64
		rcTmpfsMB       int64
		rcEnabled       bool
	)

	createRCCmd := &cobra.Command{
		Use:   "create",
		Short: "Create a runner class",
		Long: `Create a runner class that controls resource limits for forge executions.

  armory forge runner-classes create --name large --memory-mb 2048 --cpu-millicores 2000 \
    --pids-limit 128 --tmpfs-mb 512 --enabled`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if rcName == "" {
				return fmt.Errorf("--name is required")
			}
			payload := map[string]any{
				"name":           rcName,
				"memory_mb":      rcMemoryMB,
				"cpu_millicores": rcCPUMillicores,
				"pids_limit":     rcPidsLimit,
				"tmpfs_mb":       rcTmpfsMB,
				"enabled":        rcEnabled,
			}
			body, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			return apiCall("POST", "/forge/runner-classes", body)
		},
	}
	createRCCmd.Flags().StringVar(&rcName, "name", "", "Runner class name (required)")
	createRCCmd.Flags().Int64Var(&rcMemoryMB, "memory-mb", 512, "Memory limit in MiB (min 64)")
	createRCCmd.Flags().Int64Var(&rcCPUMillicores, "cpu-millicores", 500, "CPU limit in millicores (min 100)")
	createRCCmd.Flags().Int64Var(&rcPidsLimit, "pids-limit", 64, "Max process count (min 8)")
	createRCCmd.Flags().Int64Var(&rcTmpfsMB, "tmpfs-mb", 128, "tmpfs size in MiB (min 16)")
	createRCCmd.Flags().BoolVar(&rcEnabled, "enabled", true, "Enable the class on creation")

	var (
		updateRCMemoryMB      int64
		updateRCCPUMillicores int64
		updateRCPidsLimit     int64
		updateRCTmpfsMB       int64
		updateRCEnabled       bool
	)

	updateRCCmd := &cobra.Command{
		Use:   "update <name>",
		Short: "Update a runner class",
		Long: `Update resource limits for a runner class. All fields are replaced.

  armory forge runner-classes update large --memory-mb 4096 --cpu-millicores 4000 \
    --pids-limit 256 --tmpfs-mb 1024 --enabled`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			payload := map[string]any{
				"memory_mb":      updateRCMemoryMB,
				"cpu_millicores": updateRCCPUMillicores,
				"pids_limit":     updateRCPidsLimit,
				"tmpfs_mb":       updateRCTmpfsMB,
				"enabled":        updateRCEnabled,
			}
			body, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			return apiCall("PUT", "/forge/runner-classes/"+args[0], body)
		},
	}
	updateRCCmd.Flags().Int64Var(&updateRCMemoryMB, "memory-mb", 512, "Memory limit in MiB")
	updateRCCmd.Flags().Int64Var(&updateRCCPUMillicores, "cpu-millicores", 500, "CPU limit in millicores")
	updateRCCmd.Flags().Int64Var(&updateRCPidsLimit, "pids-limit", 64, "Max process count")
	updateRCCmd.Flags().Int64Var(&updateRCTmpfsMB, "tmpfs-mb", 128, "tmpfs size in MiB")
	updateRCCmd.Flags().BoolVar(&updateRCEnabled, "enabled", true, "Enable or disable the class")

	rcCmd.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "List all runner classes",
			Args:  cobra.NoArgs,
			RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("GET", "/forge/runner-classes", nil) },
		},
		createRCCmd,
		&cobra.Command{
			Use:   "get <name>",
			Short: "Get a runner class",
			Args:  cobra.ExactArgs(1),
			RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("GET", "/forge/runner-classes/"+args[0], nil) },
		},
		updateRCCmd,
		&cobra.Command{
			Use:   "delete <name>",
			Short: "Delete a runner class",
			Args:  cobra.ExactArgs(1),
			RunE:  func(cmd *cobra.Command, args []string) error { return apiCall("DELETE", "/forge/runner-classes/"+args[0], nil) },
		},
	)

	forgeCmd.AddCommand(execCmd, rcCmd)
	rootCmd.AddCommand(forgeCmd)
}

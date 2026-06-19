package cmd

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

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
		execWait        bool
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
			return submitForgeExecution(cmd, payload, execTimeout, execWait)
		},
	}
	runCmd.Flags().StringVar(&execImage, "image", "", "Container image to run (required)")
	runCmd.Flags().IntVar(&execTimeout, "timeout", 0, "Timeout in seconds (default: server default)")
	runCmd.Flags().StringArrayVar(&execEnvs, "env", nil, "Environment variable KEY=VALUE (repeatable)")
	runCmd.Flags().StringVar(&execRunnerClass, "runner-class", "", "Runner class name (default: standard)")
	runCmd.Flags().BoolVar(&execWait, "wait", false, "Wait for the execution to finish, print its output, and exit non-zero if it failed")

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

	var execRerunWait bool

	rerunCmd := &cobra.Command{
		Use:   "rerun <id>",
		Short: "Resubmit an execution with the same image, command, env, and runner class",
		Long: `Resubmit a previous execution as a brand-new run, copying its image,
command, environment, timeout, and runner class. The original execution is left
untouched, and resubmission goes through the normal submit path so the current
image allowlist and runner-class rules are re-enforced.

  armory forge exec rerun 3f2a1b…          # queue a fresh copy
  armory forge exec rerun 3f2a1b… --wait   # wait for it to finish and print its output`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return rerunForgeExecution(cmd, args[0], execRerunWait)
		},
	}
	rerunCmd.Flags().BoolVar(&execRerunWait, "wait", false, "Wait for the execution to finish, print its output, and exit non-zero if it failed")

	execCmd.AddCommand(
		runCmd,
		listExecCmd,
		rerunCmd,
		&cobra.Command{
			Use:   "get <id>",
			Short: "Get an execution, including its stdout and stderr",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				data, err := doRequest("GET", "/forge/executions/"+args[0], nil)
				if err != nil {
					return err
				}
				printExecution(data)
				return nil
			},
		},
		&cobra.Command{
			Use:   "cancel <id>",
			Short: "Cancel a running or pending execution",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return apiCall("DELETE", "/forge/executions/"+args[0], nil)
			},
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
			RunE: func(cmd *cobra.Command, args []string) error {
				return apiCall("GET", "/forge/runner-classes/"+args[0], nil)
			},
		},
		updateRCCmd,
		&cobra.Command{
			Use:   "delete <name>",
			Short: "Delete a runner class",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return apiCall("DELETE", "/forge/runner-classes/"+args[0], nil)
			},
		},
	)

	forgeCmd.AddCommand(execCmd, rcCmd)
	// The forge module (command + home-screen) is registered in forge_tui.go,
	// alongside the screen it contributes.
}

// submitForgeExecution POSTs a forge execution payload, then either prints the
// started-job hint or — when wait is true — blocks until the job finishes,
// prints its output, and returns a non-nil error if it failed. timeout bounds
// the --wait poll (0 = server default). It is shared by `exec run` and
// `exec rerun`, which differ only in how they assemble the payload.
func submitForgeExecution(cmd *cobra.Command, payload map[string]any, timeout int, wait bool) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	data, err := doRequest("POST", "/forge/executions", body)
	if err != nil {
		return err
	}
	var resp struct {
		ExecutionID string `json:"execution_id"`
	}
	if err := json.Unmarshal(data, &resp); err != nil || resp.ExecutionID == "" {
		printResponse(data) // show whatever came back
		// A 2xx without an execution_id means the job wasn't created (or the
		// response is unrecognised); don't report success or silently skip --wait.
		cmd.SilenceUsage = true
		return fmt.Errorf("submission did not return an execution_id")
	}
	if wait {
		fmt.Fprintf(os.Stderr, "forge job: %s started\n", resp.ExecutionID)
		if err := waitForExecution(resp.ExecutionID, timeout); err != nil {
			cmd.SilenceUsage = true // the job ran but failed — not a CLI usage error
			return err
		}
		return nil
	}
	fmt.Printf("forge job: %s started\n", resp.ExecutionID)
	fmt.Printf("track output with: armory forge exec get %s\n", resp.ExecutionID)
	return nil
}

// rerunForgeExecution fetches an existing execution and resubmits it as a new
// run with the same image, command, env, timeout, and runner class. The
// original record is never modified.
func rerunForgeExecution(cmd *cobra.Command, id string, wait bool) error {
	data, err := doRequest("GET", "/forge/executions/"+id, nil)
	if err != nil {
		return err
	}
	var src struct {
		Image       string            `json:"image"`
		Command     []string          `json:"command"`
		Env         map[string]string `json:"env"`
		Timeout     int64             `json:"timeout"`
		RunnerClass string            `json:"runner_class"`
	}
	if err := json.Unmarshal(data, &src); err != nil {
		return fmt.Errorf("unexpected execution response: %s", data)
	}
	if src.Image == "" || len(src.Command) == 0 {
		cmd.SilenceUsage = true
		return fmt.Errorf("execution %s cannot be rerun: it has no recorded image or command", id)
	}
	payload := forgeRerunPayload(src.Image, src.Command, src.Env, src.Timeout, src.RunnerClass)
	return submitForgeExecution(cmd, payload, int(src.Timeout), wait)
}

// forgeRerunPayload assembles the POST body that resubmits an execution with the
// same parameters, omitting empty optional fields so forge applies its own
// defaults. Shared by the `exec rerun` command and the TUI rerun action.
func forgeRerunPayload(image string, command []string, env map[string]string, timeout int64, runnerClass string) map[string]any {
	payload := map[string]any{"image": image, "command": command}
	if len(env) > 0 {
		payload["env"] = env
	}
	if timeout > 0 {
		payload["timeout"] = timeout
	}
	if runnerClass != "" {
		payload["runner_class"] = runnerClass
	}
	return payload
}

// waitForExecution polls a forge execution until it reaches a terminal state,
// then prints its captured output. serverTimeout is the execution's --timeout
// (0 = server default) and bounds how long we poll. It returns a non-nil error
// when the job failed, timed out, or was cancelled so the CLI exits non-zero.
func waitForExecution(id string, serverTimeout int) error {
	const pollInterval = 2 * time.Second
	// pollGrace covers image-pull, worker pickup, and the server's own timeout-
	// detection margin (k8s allows ~60s over the job deadline before it records
	// timed_out), so the CLI doesn't give up while a job is still legitimately
	// finishing or being marked timed_out.
	const pollGrace = 2 * time.Minute
	maxWait := time.Hour
	if serverTimeout > 0 {
		// Clamp before converting to a Duration: serverTimeout is an unbounded
		// --timeout value and time.Duration(serverTimeout)*time.Second would
		// overflow int64 nanoseconds for absurd inputs, yielding a deadline in
		// the past and an immediate give-up. The server caps executions far
		// below this anyway.
		capped := min(serverTimeout, 24*3600)
		maxWait = time.Duration(capped)*time.Second + pollGrace
	}
	deadline := time.Now().Add(maxWait)
	fmt.Fprintln(os.Stderr, "waiting for completion…")

	for {
		data, err := doRequest("GET", "/forge/executions/"+id, nil)
		if err != nil {
			return err
		}
		var ex struct {
			Status   string  `json:"status"`
			Image    string  `json:"image"`
			Stdout   *string `json:"stdout"`
			Stderr   *string `json:"stderr"`
			ExitCode *int    `json:"exit_code"`
		}
		if err := json.Unmarshal(data, &ex); err != nil {
			return fmt.Errorf("unexpected execution response: %s", data)
		}

		switch ex.Status {
		case "completed":
			printStream(os.Stdout, ex.Stdout)
			printStream(os.Stderr, ex.Stderr)
			return nil
		case "failed", "timed_out":
			// On failure the captured output is diagnostic, not a result, so route
			// it all to stderr. (The k8s runtime merges stdout+stderr into the
			// stdout field, so printing that to os.Stdout would hide the real error
			// from `2>` redirection.)
			printStream(os.Stderr, ex.Stdout)
			printStream(os.Stderr, ex.Stderr)

			ctxStr := ""
			if ex.Image != "" {
				ctxStr = " (image " + ex.Image + ")"
			}
			if ex.Status == "timed_out" {
				if serverTimeout > 0 {
					return fmt.Errorf("forge job %s%s timed out after %ds — raise --timeout or check the command", id, ctxStr, serverTimeout)
				}
				return fmt.Errorf("forge job %s%s timed out", id, ctxStr)
			}

			exit := "failed"
			if ex.ExitCode != nil {
				exit = fmt.Sprintf("exited with code %d", *ex.ExitCode)
			}
			if nonEmpty(ex.Stdout) || nonEmpty(ex.Stderr) {
				return fmt.Errorf("forge job %s%s %s — see output above", id, ctxStr, exit)
			}
			return fmt.Errorf("forge job %s%s %s but captured no output — run `armory forge exec get %s -v` for details", id, ctxStr, exit, id)
		case "cancelled":
			return fmt.Errorf("forge job %s was cancelled", id)
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("gave up waiting for forge job %s after %s (last status %q); it may still be running — check `armory forge exec get %s`", id, maxWait, ex.Status, id)
		}
		time.Sleep(pollInterval)
	}
}

// printStream writes a captured output stream to w with a guaranteed trailing newline.
func printStream(w *os.File, s *string) {
	if s == nil || *s == "" {
		return
	}
	fmt.Fprint(w, *s)
	if !strings.HasSuffix(*s, "\n") {
		fmt.Fprintln(w)
	}
}

// nonEmpty reports whether s points to a non-blank string.
func nonEmpty(s *string) bool { return s != nil && strings.TrimSpace(*s) != "" }

// printExecution renders an execution's metadata record followed by its captured
// stdout and stderr — which the generic record view omits, making failures hard
// to debug. Both streams are written to stdout under clear labels.
func printExecution(data []byte) {
	printResponse(data) // metadata record (stdout/stderr intentionally omitted there)

	var ex struct {
		Stdout *string `json:"stdout"`
		Stderr *string `json:"stderr"`
		Status string  `json:"status"`
	}
	if json.Unmarshal(data, &ex) != nil {
		return
	}

	if nonEmpty(ex.Stdout) {
		fmt.Println("\nstdout:")
		printStream(os.Stdout, ex.Stdout)
	}
	if nonEmpty(ex.Stderr) {
		fmt.Println("\nstderr:")
		printStream(os.Stdout, ex.Stderr)
	}
	if !nonEmpty(ex.Stdout) && !nonEmpty(ex.Stderr) {
		switch ex.Status {
		case "pending", "running":
			fmt.Printf("\n(no output yet — execution is %s)\n", ex.Status)
		default:
			fmt.Println("\n(no output was captured for this execution)")
		}
	}
}

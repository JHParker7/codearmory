package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// pollInterval is how often each idle worker polls the pending queue for a
// claimable execution. Kept short so a freshly submitted execution is picked up
// almost immediately instead of waiting up to a second to be noticed — the
// dominant slice of a fast run's time-to-first-log. The claim is a single
// `FOR UPDATE SKIP LOCKED` row lock (see claimPendingExecution), so N workers
// polling in parallel is cheap and contention-free. Override with
// FORGE_POLL_INTERVAL_MS.
var pollInterval = time.Duration(envIntOrDefault("FORGE_POLL_INTERVAL_MS", 250)) * time.Millisecond

// WorkerPool runs executions pulled from the pending queue in PostgreSQL.
type WorkerPool struct {
	registry *runtimeRegistry
	cancels  sync.Map // executionID -> runningExec
}

// runningExec tracks an in-flight execution so Cancel can both unblock Run (via
// the context) and ask the runtime to tear down its work explicitly (stop+destroy
// a VM, delete a job). rt is the runtime resolved for this execution's backend.
type runningExec struct {
	cancel context.CancelFunc
	rt     Runtime
}

func newWorkerPool(registry *runtimeRegistry) *WorkerPool {
	return &WorkerPool{registry: registry}
}

// Start launches n worker goroutines. Call with a context that lives for the
// duration of the process; cancel it to drain the pool on shutdown.
func (p *WorkerPool) Start(ctx context.Context, n int) {
	for range n {
		go p.loop(ctx)
	}
}

// Cancel terminates a running execution. It cancels the execution's context
// (unblocking Run) and then asks the resolved runtime to tear down its work
// explicitly — a no-op for docker, a job delete for kubernetes. Returns false if
// the execution is not currently tracked (already finished or not yet started).
func (p *WorkerPool) Cancel(executionID string) bool {
	v, ok := p.cancels.Load(executionID)
	if !ok {
		return false
	}
	re := v.(runningExec)
	re.cancel()
	if re.rt != nil {
		if err := re.rt.Cancel(context.Background(), executionID); err != nil {
			slog.Warn("worker: runtime cancel failed", "execution_id", executionID, "error", err)
		}
	}
	return true
}

func (p *WorkerPool) loop(ctx context.Context) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.tryOne(ctx)
		}
	}
}

func (p *WorkerPool) tryOne(ctx context.Context) {
	exec, ok := claimPendingExecution(ctx)
	if !ok {
		return
	}
	p.run(ctx, exec)
}

// systemExitDiagnostic returns a human-readable reason and true when a non-zero
// exit code signals that the container runtime could not run the command, rather
// than the command running and failing on its own. forge execs the command
// directly with no shell wrapper, so the Docker/OCI-reserved codes 125–128 always
// mean the platform failed to start or locate the command — a non-user error that
// otherwise lands as an empty "failed" record with no clue why.
func systemExitDiagnostic(code int, exec Execution) (string, bool) {
	cmd := ""
	if len(exec.Command) > 0 {
		cmd = exec.Command[0]
	}
	switch code {
	case 125:
		return fmt.Sprintf("container runtime error (exit 125): the daemon could not run image %q", exec.Image), true
	case 126:
		return fmt.Sprintf("command %q is not executable (exit 126): check its permissions in image %q", cmd, exec.Image), true
	case 127:
		return fmt.Sprintf("command %q not found in image %q (exit 127)", cmd, exec.Image), true
	case 128:
		return fmt.Sprintf("container process could not be started (exit 128): %q may not be an executable in image %q — shell built-ins such as \"cd\" are not valid commands", cmd, exec.Image), true
	default:
		return "", false
	}
}

// classifyResult maps a runtime outcome to the stored execution status. It
// surfaces a non-user failure into stderr so the user sees a reason instead of an
// empty "failed" record, and returns that failure as the third value so the
// worker can log and trace it as an error. Two kinds of non-user failure are
// recognised: a runtime error returned by Run (image pull, container create, …),
// and a Docker/OCI-reserved exit code (125–128) meaning the command could not be
// run at all. Cancellation and timeout are recognised from the error; any other
// non-zero exit code is a normal command failure (sysErr nil).
func classifyResult(exec Execution, result RunResult, runErr error) (status string, out RunResult, sysErr error) {
	switch {
	case runErr == nil:
		if result.ExitCode == nil || *result.ExitCode == 0 {
			return StatusCompleted, result, nil
		}
		if diag, ok := systemExitDiagnostic(*result.ExitCode, exec); ok {
			if result.Stderr == "" {
				result.Stderr = "forge: " + diag
			}
			return StatusFailed, result, fmt.Errorf("execution %s: %s", exec.ExecutionID, diag)
		}
		return StatusFailed, result, nil
	case errors.Is(runErr, context.Canceled):
		result.ExitCode = nil // cancelled before the command produced an exit code
		return StatusCancelled, result, nil
	case errors.Is(runErr, context.DeadlineExceeded):
		result.ExitCode = nil // killed at the deadline; no real exit code
		return StatusTimedOut, result, nil
	default:
		result.ExitCode = nil // runtime failure (image pull, create, …) — no exit code
		if result.Stderr == "" {
			result.Stderr = "forge: " + runErr.Error()
		}
		return StatusFailed, result, runErr
	}
}

func (p *WorkerPool) run(ctx context.Context, exec Execution) {
	// Each execution is its own trace root: the worker poll loop has no inbound
	// request span, so without this a runtime failure would have nowhere to be
	// recorded. The span carries the status/exit code and an error status when the
	// platform — not the user's command — is what failed.
	ctx, span := otel.Tracer("forge").Start(ctx, "execution.run", trace.WithAttributes(
		attribute.String("execution.id", exec.ExecutionID),
		attribute.String("execution.image", exec.Image),
		attribute.String("execution.runner_class", exec.RunnerClass),
		attribute.String("execution.backend", exec.Backend),
	))
	defer span.End()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	slog.InfoContext(ctx, "worker: starting execution", "execution_id", exec.ExecutionID, "image", exec.Image, "backend", exec.Backend)

	// Resolve the execution's backend. A missing/disabled/misconfigured backend
	// fails only this execution (classifyResult's runtime-error path) rather than
	// crashing the worker — see R5 in the design.
	var result RunResult
	var runErr error
	// Prepend an actions/checkout-style `git clone … && cd …` prologue so the
	// command runs inside a checked-out repo. Applied before wrapOutputEnv so the
	// clone runs first and the output-env trailer stays at the very end. When the
	// execution's working dir is a shared workspace volume (workdir: true, as
	// forge/git-clone sets), the checkout clones into the volume root so downstream
	// steps that mount the same volume see the working tree at its root.
	if exec.Checkout != nil {
		exec.Command = applyCheckout(exec.Command, exec.Checkout, exec.SecretRefs, checkoutIntoWorkdirRoot(exec.Volumes))
	}
	// Capture requested output env vars: wrap the command so it emits them after a
	// unique marker, then split them back out of stdout once the run finishes.
	marker := ""
	if len(exec.OutputEnv) > 0 {
		marker = "__forge_output_" + uuid.NewString() + "__"
		exec.Command = wrapOutputEnv(exec.Command, exec.OutputEnv, marker)
	}
	rt, gerr := p.registry.Get(ctx, exec.Backend)
	if gerr != nil {
		runErr = fmt.Errorf("runtime backend %q: %w", exec.Backend, gerr)
	} else {
		// Register the cancel handler before any potentially-blocking work.
		// Credential resolution makes network calls (gatekeeper, gitea), so a
		// DELETE arriving during that window must be able to interrupt runCtx
		// rather than silently no-op because the execution isn't tracked yet.
		p.cancels.Store(exec.ExecutionID, runningExec{cancel: cancel, rt: rt})
		defer p.cancels.Delete(exec.ExecutionID)

		// Resolve credential references (secret_refs) into a runtime-only env copy.
		// Resolved values are injected into the sandbox but never persisted or
		// logged. A resolution failure fails the execution rather than running the
		// command without the credentials it asked for.
		creds, cerr := resolveCredentials(runCtx, exec)
		if cerr != nil {
			runErr = fmt.Errorf("resolve credentials: %w", cerr)
		} else {
			if len(creds) > 0 {
				merged := make(map[string]string, len(exec.Env)+len(creds))
				maps.Copy(merged, exec.Env)
				maps.Copy(merged, creds)
				exec.Env = merged // local copy only; Complete() never writes env back
			}
			result, runErr = rt.Run(runCtx, exec)
			if marker != "" && runErr == nil {
				result.Stdout, result.Outputs = parseOutputEnv(result.Stdout, exec.OutputEnv, marker)
			}
		}
	}

	status, result, sysErr := classifyResult(exec, result, runErr)

	meterComplete.Add(ctx, 1, metric.WithAttributes(attribute.String("status", status)))
	exitCode := -1 // -1 = no exit code (cancelled/timed out/runtime failure)
	if result.ExitCode != nil {
		exitCode = *result.ExitCode
	}
	span.SetAttributes(attribute.String("execution.status", status), attribute.Int("execution.exit_code", exitCode))

	if sysErr != nil {
		// A non-user failure: forge could not run the command (image pull, container
		// create/start, command-not-found, OCI runtime error, …). Log and trace it as
		// an error so it is debuggable instead of buried in a generic "failed" record.
		slog.ErrorContext(ctx, "worker: execution failed to run", "execution_id", exec.ExecutionID, "status", status, "exit_code", exitCode, "error", sysErr)
		span.RecordError(sysErr)
		span.SetStatus(codes.Error, sysErr.Error())
	} else {
		slog.InfoContext(ctx, "worker: execution done", "execution_id", exec.ExecutionID, "status", status, "exit_code", exitCode)
	}

	if err := exec.Complete(ctx, status, result); err != nil {
		slog.ErrorContext(ctx, "worker: update execution result", "execution_id", exec.ExecutionID, "error", err)
	}
}

// wrapOutputEnv appends a trailer to a `[<shell> -c <script>]` command so that,
// after the user's script runs in the SAME shell (so exported/computed vars are
// visible), it prints each requested var as `NAME=<base64(value)>` after a unique
// marker line. The value is base64-encoded so it round-trips intact regardless of
// its content — newlines (e.g. a `find ... -printf '%f\n'` list), spaces, '=', or
// binary — where the old raw `NAME=value` form was line-based and truncated any
// value containing a newline to its first line. The worker splits and decodes these
// back out of stdout (parseOutputEnv). A non-`-c` command is returned unchanged —
// capture needs a shell trailer. Names are validated POSIX identifiers, so they are
// safe to inject. base64 is present on every runner image (coreutils / busybox).
func wrapOutputEnv(cmd, names []string, marker string) []string {
	if len(cmd) < 3 || cmd[1] != "-c" || len(names) == 0 {
		return cmd
	}
	var b strings.Builder
	b.WriteString(cmd[2])
	b.WriteString("\nprintf '\\n%s\\n' '" + marker + "'\n")
	// One printf per name, expanding "$NAME" in the same shell so both exported and
	// plain shell vars the script set are captured (an unset var base64-encodes to an
	// empty string). The value is piped through base64 (newlines stripped) so it stays
	// on a single NAME= line. Names are validated POSIX identifiers, so $NAME can't inject.
	for _, n := range names {
		b.WriteString("printf '%s=%s\\n' '" + n + "' \"$(printf '%s' \"$" + n + "\" | base64 | tr -d '\\n')\"\n")
	}
	out := append([]string(nil), cmd...)
	out[2] = b.String()
	return out
}

// parseOutputEnv splits captured stdout at the marker emitted by wrapOutputEnv:
// everything before is the real stdout; the `NAME=<base64(value)>` lines after it
// become the captured map (restricted to the requested names), with each value
// base64-decoded so multi-line/arbitrary content round-trips. A missing marker (the
// script failed before the trailer ran) yields the stdout unchanged and no captures;
// a value that fails to decode is skipped rather than surfaced raw.
func parseOutputEnv(stdout string, names []string, marker string) (string, map[string]string) {
	before, after, found := strings.Cut(stdout, "\n"+marker+"\n")
	if !found {
		return stdout, nil
	}
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	out := map[string]string{}
	for _, line := range strings.Split(after, "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok || !want[k] {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v))
		if err != nil {
			continue
		}
		out[k] = string(decoded)
	}
	if len(out) == 0 {
		out = nil
	}
	return before, out
}

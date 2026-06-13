package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestClassifyResult(t *testing.T) {
	t.Run("completed on zero exit, output preserved", func(t *testing.T) {
		status, res, sysErr := classifyResult(Execution{}, RunResult{ExitCode: ptr(0), Stdout: "hi"}, nil)
		if status != StatusCompleted {
			t.Fatalf("status = %q, want %q", status, StatusCompleted)
		}
		if res.Stdout != "hi" {
			t.Errorf("stdout = %q, want preserved", res.Stdout)
		}
		if res.ExitCode == nil || *res.ExitCode != 0 {
			t.Errorf("exit code = %v, want 0 preserved", res.ExitCode)
		}
		if sysErr != nil {
			t.Errorf("sysErr = %v, want nil for a successful run", sysErr)
		}
	})

	t.Run("failed on non-zero exit, stderr not overwritten", func(t *testing.T) {
		status, res, sysErr := classifyResult(Execution{}, RunResult{ExitCode: ptr(1), Stderr: "boom"}, nil)
		if status != StatusFailed {
			t.Fatalf("status = %q, want %q", status, StatusFailed)
		}
		if res.Stderr != "boom" {
			t.Errorf("stderr = %q, want unchanged 'boom'", res.Stderr)
		}
		if res.ExitCode == nil || *res.ExitCode != 1 {
			t.Errorf("exit code = %v, want 1 preserved", res.ExitCode)
		}
		if sysErr != nil {
			t.Errorf("sysErr = %v, want nil — exit 1 is a normal user command failure", sysErr)
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		if status, _, _ := classifyResult(Execution{}, RunResult{}, context.Canceled); status != StatusCancelled {
			t.Fatalf("status = %q, want %q", status, StatusCancelled)
		}
	})

	t.Run("timed out", func(t *testing.T) {
		if status, _, _ := classifyResult(Execution{}, RunResult{}, context.DeadlineExceeded); status != StatusTimedOut {
			t.Fatalf("status = %q, want %q", status, StatusTimedOut)
		}
	})

	t.Run("wrapped cancel still recognised", func(t *testing.T) {
		if status, _, _ := classifyResult(Execution{}, RunResult{}, fmt.Errorf("ctx: %w", context.Canceled)); status != StatusCancelled {
			t.Fatalf("status = %q, want %q for wrapped error", status, StatusCancelled)
		}
	})

	t.Run("runtime error surfaces reason into stderr and sysErr", func(t *testing.T) {
		status, res, sysErr := classifyResult(Execution{}, RunResult{}, errors.New("image pull failed"))
		if status != StatusFailed {
			t.Fatalf("status = %q, want %q", status, StatusFailed)
		}
		if !strings.Contains(res.Stderr, "image pull failed") {
			t.Errorf("stderr = %q, want it to surface the runtime error", res.Stderr)
		}
		if res.ExitCode != nil {
			t.Errorf("exit code = %v, want nil (no exit code for a runtime failure)", *res.ExitCode)
		}
		if sysErr == nil {
			t.Error("sysErr = nil, want the runtime error so it is logged and traced")
		}
	})

	t.Run("runtime error keeps already-captured stderr", func(t *testing.T) {
		status, res, sysErr := classifyResult(Execution{}, RunResult{Stderr: "real output"}, errors.New("late error"))
		if status != StatusFailed {
			t.Fatalf("status = %q, want %q", status, StatusFailed)
		}
		if res.Stderr != "real output" {
			t.Errorf("stderr = %q, want the captured output kept", res.Stderr)
		}
		if sysErr == nil {
			t.Error("sysErr = nil, want the runtime error reported")
		}
	})

	// Docker/OCI-reserved codes (125–128) mean forge could not run the command at
	// all. They must be flagged as system errors (non-nil sysErr) and get a
	// diagnostic stderr so an otherwise output-less failure is debuggable.
	t.Run("system exit codes flagged with diagnostic", func(t *testing.T) {
		exec := Execution{ExecutionID: "abc", Image: "alpine:3.19", Command: []string{"cd"}}
		for _, code := range []int{125, 126, 127, 128} {
			status, res, sysErr := classifyResult(exec, RunResult{ExitCode: ptr(code)}, nil)
			if status != StatusFailed {
				t.Errorf("exit %d: status = %q, want %q", code, status, StatusFailed)
			}
			if sysErr == nil {
				t.Errorf("exit %d: sysErr = nil, want a non-user error to log and trace", code)
			}
			if res.Stderr == "" {
				t.Errorf("exit %d: stderr empty, want a synthesized diagnostic", code)
			}
			if res.ExitCode == nil || *res.ExitCode != code {
				t.Errorf("exit %d: exit code = %v, want it preserved", code, res.ExitCode)
			}
		}
	})

	t.Run("exit 128 names the command and the shell-builtin hint", func(t *testing.T) {
		exec := Execution{ExecutionID: "abc", Image: "alpine:3.19", Command: []string{"cd"}}
		_, res, _ := classifyResult(exec, RunResult{ExitCode: ptr(128)}, nil)
		if !strings.Contains(res.Stderr, "cd") || !strings.Contains(res.Stderr, "alpine:3.19") {
			t.Errorf("stderr = %q, want it to name the command and image", res.Stderr)
		}
	})

	t.Run("system exit code does not overwrite captured output", func(t *testing.T) {
		exec := Execution{Image: "alpine:3.19", Command: []string{"cd"}}
		_, res, _ := classifyResult(exec, RunResult{ExitCode: ptr(127), Stderr: "real stderr"}, nil)
		if res.Stderr != "real stderr" {
			t.Errorf("stderr = %q, want the captured output kept over the diagnostic", res.Stderr)
		}
	})
}

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
		status, res := classifyResult(RunResult{ExitCode: ptr(0), Stdout: "hi"}, nil)
		if status != StatusCompleted {
			t.Fatalf("status = %q, want %q", status, StatusCompleted)
		}
		if res.Stdout != "hi" {
			t.Errorf("stdout = %q, want preserved", res.Stdout)
		}
		if res.ExitCode == nil || *res.ExitCode != 0 {
			t.Errorf("exit code = %v, want 0 preserved", res.ExitCode)
		}
	})

	t.Run("failed on non-zero exit, stderr not overwritten", func(t *testing.T) {
		status, res := classifyResult(RunResult{ExitCode: ptr(1), Stderr: "boom"}, nil)
		if status != StatusFailed {
			t.Fatalf("status = %q, want %q", status, StatusFailed)
		}
		if res.Stderr != "boom" {
			t.Errorf("stderr = %q, want unchanged 'boom'", res.Stderr)
		}
		if res.ExitCode == nil || *res.ExitCode != 1 {
			t.Errorf("exit code = %v, want 1 preserved", res.ExitCode)
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		if status, _ := classifyResult(RunResult{}, context.Canceled); status != StatusCancelled {
			t.Fatalf("status = %q, want %q", status, StatusCancelled)
		}
	})

	t.Run("timed out", func(t *testing.T) {
		if status, _ := classifyResult(RunResult{}, context.DeadlineExceeded); status != StatusTimedOut {
			t.Fatalf("status = %q, want %q", status, StatusTimedOut)
		}
	})

	t.Run("wrapped cancel still recognised", func(t *testing.T) {
		if status, _ := classifyResult(RunResult{}, fmt.Errorf("ctx: %w", context.Canceled)); status != StatusCancelled {
			t.Fatalf("status = %q, want %q for wrapped error", status, StatusCancelled)
		}
	})

	t.Run("runtime error surfaces reason into stderr", func(t *testing.T) {
		status, res := classifyResult(RunResult{}, errors.New("image pull failed"))
		if status != StatusFailed {
			t.Fatalf("status = %q, want %q", status, StatusFailed)
		}
		if !strings.Contains(res.Stderr, "image pull failed") {
			t.Errorf("stderr = %q, want it to surface the runtime error", res.Stderr)
		}
		if res.ExitCode != nil {
			t.Errorf("exit code = %v, want nil (no exit code for a runtime failure)", *res.ExitCode)
		}
	})

	t.Run("runtime error keeps already-captured stderr", func(t *testing.T) {
		status, res := classifyResult(RunResult{Stderr: "real output"}, errors.New("late error"))
		if status != StatusFailed {
			t.Fatalf("status = %q, want %q", status, StatusFailed)
		}
		if res.Stderr != "real output" {
			t.Errorf("stderr = %q, want the captured output kept", res.Stderr)
		}
	})
}

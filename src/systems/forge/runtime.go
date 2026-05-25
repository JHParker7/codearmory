package main

import (
	"context"
	"fmt"
	"os"
)

// Runtime is the interface for running sandboxed container commands.
// Select via RUNTIME env var: "kubernetes" (default) or "docker".
type Runtime interface {
	// Run executes the command described by exec. It blocks until the command
	// completes, the context is cancelled, or the execution times out.
	// Returning a non-nil error does not imply a specific exit code — the worker
	// inspects ctx.Err() to distinguish cancellation from failure.
	Run(ctx context.Context, exec Execution) (RunResult, error)

	// Cancel terminates a running execution identified by executionID.
	// Called when the user issues DELETE /executions/{id} while the job is running.
	Cancel(ctx context.Context, executionID string) error
}

func newRuntime() (Runtime, error) {
	rt := os.Getenv("RUNTIME")
	if rt == "" {
		rt = "docker"
	}
	switch rt {
	case "kubernetes":
		r, err := newKubernetesRuntime()
		if err != nil {
			return nil, fmt.Errorf("kubernetes runtime: %w", err)
		}
		return r, nil
	case "docker":
		r, err := newDockerRuntime()
		if err != nil {
			return nil, fmt.Errorf("docker runtime: %w", err)
		}
		return r, nil
	default:
		return nil, fmt.Errorf("unknown RUNTIME %q: expected kubernetes or docker", rt)
	}
}

func ptr[T any](v T) *T { return &v }

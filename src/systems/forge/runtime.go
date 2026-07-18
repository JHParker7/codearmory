package main

import (
	"context"
	"os"
)

// Runtime is the interface for running sandboxed container commands. Concrete
// runtimes are built lazily by the runtimeRegistry from a RuntimeBackend; see
// registry.go.
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

// defaultRuntimeType returns the Type seeded onto the "default" runtime backend
// from the legacy RUNTIME env var, preserving the single-runtime deployment
// contract: RUNTIME unset means docker. An unrecognised value is passed through
// and fails when the registry tries to build it, mirroring the old behaviour.
func defaultRuntimeType() string {
	if rt := os.Getenv("RUNTIME"); rt != "" {
		return rt
	}
	return "docker"
}

// proxyEnvPairs is the set of egress-proxy environment variables forge injects
// into a sandboxed job (upper- and lower-case forms, plus NO_PROXY for loopback).
// Used by the docker runtime; the kubernetes runtime injects the same set separately.
func proxyEnvPairs(proxy string) [][2]string {
	return [][2]string{
		{"HTTP_PROXY", proxy},
		{"HTTPS_PROXY", proxy},
		{"NO_PROXY", "localhost,127.0.0.1"},
		{"http_proxy", proxy},
		{"https_proxy", proxy},
		{"no_proxy", "localhost,127.0.0.1"},
	}
}

func ptr[T any](v T) *T { return &v }

// bytesPerMiB is the divisor for converting a byte count to whole MiB, used by
// the docker and kubernetes runtimes when recording peak memory usage so the
// conversion lives in one place.
const bytesPerMiB = 1024 * 1024

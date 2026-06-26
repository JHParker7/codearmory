package main

import (
	"errors"
	"testing"
)

// failureOutput must surface a failed step's captured output (forge stdout/stderr,
// an HTTP body, ...) alongside its error summary so the run view shows the real
// failure detail, not just "<action> failed (exit code: N)".
func TestFailureOutput(t *testing.T) {
	err := errors.New("forge/run failed (exit code: 1)")
	cases := []struct {
		name   string
		output string
		want   string
	}{
		{"output and error", "compiling…\n./main.go:3: undefined: foo\n", "compiling…\n./main.go:3: undefined: foo\n\nforge/run failed (exit code: 1)"},
		{"empty output falls back to error", "", "forge/run failed (exit code: 1)"},
		{"whitespace-only output falls back to error", "\n\n", "forge/run failed (exit code: 1)"},
		{"trailing newlines collapsed before footer", "boom", "boom\n\nforge/run failed (exit code: 1)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := failureOutput(tc.output, err); got != tc.want {
				t.Errorf("failureOutput(%q) = %q, want %q", tc.output, got, tc.want)
			}
		})
	}
}

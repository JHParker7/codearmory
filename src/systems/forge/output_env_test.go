package main

import (
	"strings"
	"testing"
)

// wrapOutputEnv must append the emit trailer to a `-c` script (same shell, so the
// script's exported vars are visible) and leave the script body intact.
func TestWrapOutputEnv_AppendsTrailerToShellScript(t *testing.T) {
	cmd := wrapOutputEnv([]string{"sh", "-c", "export FOO=bar"}, []string{"FOO", "BAZ"}, "MARKER123")
	if len(cmd) != 3 || cmd[0] != "sh" || cmd[1] != "-c" {
		t.Fatalf("command shape changed: %v", cmd)
	}
	if !strings.HasPrefix(cmd[2], "export FOO=bar\n") {
		t.Errorf("user script not preserved at the front: %q", cmd[2])
	}
	if !strings.Contains(cmd[2], "MARKER123") || !strings.Contains(cmd[2], `'FOO' "$FOO"`) || !strings.Contains(cmd[2], `'BAZ' "$BAZ"`) {
		t.Errorf("trailer missing marker/vars: %q", cmd[2])
	}
}

func TestWrapOutputEnv_NonShellOrEmptyUnchanged(t *testing.T) {
	orig := []string{"python", "script.py"}
	if got := wrapOutputEnv(orig, []string{"FOO"}, "M"); len(got) != 2 || got[0] != "python" {
		t.Errorf("non -c command should be unchanged, got %v", got)
	}
	if got := wrapOutputEnv([]string{"sh", "-c", "echo hi"}, nil, "M"); got[2] != "echo hi" {
		t.Errorf("empty names should not wrap, got %q", got[2])
	}
}

// parseOutputEnv splits the real stdout from the marker-delimited NAME=value lines
// and keeps only the requested names.
func TestParseOutputEnv_SplitsStdoutAndCaptures(t *testing.T) {
	stdout := "build log line 1\nbuild log line 2\nMARKER\nFOO=bar\nVERSION=1.2.3\nIGNORED=x\n"
	real, out := parseOutputEnv(stdout, []string{"FOO", "VERSION"}, "MARKER")
	if real != "build log line 1\nbuild log line 2" {
		t.Errorf("real stdout = %q", real)
	}
	if out["FOO"] != "bar" || out["VERSION"] != "1.2.3" {
		t.Errorf("captured = %v", out)
	}
	if _, ok := out["IGNORED"]; ok {
		t.Errorf("captured an unrequested var: %v", out)
	}
}

func TestParseOutputEnv_NoMarkerLeavesStdout(t *testing.T) {
	// The emit never ran (e.g. the script failed under set -e before the trailer).
	real, out := parseOutputEnv("partial output\n", []string{"FOO"}, "MARKER")
	if real != "partial output\n" || out != nil {
		t.Errorf("expected unchanged stdout and no captures, got %q / %v", real, out)
	}
}

func TestValidateOutputEnv(t *testing.T) {
	if err := validateOutputEnv([]string{"FOO", "BAR_1", "_x"}); err != nil {
		t.Errorf("valid names rejected: %v", err)
	}
	if err := validateOutputEnv([]string{"bad-name"}); err == nil {
		t.Error("expected rejection of a non-POSIX name")
	}
	big := make([]string, maxOutputEnv+1)
	for i := range big {
		big[i] = "V"
	}
	if err := validateOutputEnv(big); err == nil {
		t.Error("expected rejection when over the cap")
	}
}

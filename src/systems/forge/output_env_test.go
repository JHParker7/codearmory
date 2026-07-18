package main

import (
	"encoding/base64"
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
	if !strings.Contains(cmd[2], "MARKER123") || !strings.Contains(cmd[2], "base64") ||
		!strings.Contains(cmd[2], `'FOO'`) || !strings.Contains(cmd[2], `"$FOO"`) ||
		!strings.Contains(cmd[2], `'BAZ'`) || !strings.Contains(cmd[2], `"$BAZ"`) {
		t.Errorf("trailer missing marker/vars/base64: %q", cmd[2])
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

// parseOutputEnv splits the real stdout from the marker-delimited NAME=<base64> lines,
// base64-decodes each value, and keeps only the requested names.
func TestParseOutputEnv_SplitsStdoutAndCaptures(t *testing.T) {
	enc := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	stdout := "build log line 1\nbuild log line 2\nMARKER\n" +
		"FOO=" + enc("bar") + "\nVERSION=" + enc("1.2.3") + "\nIGNORED=" + enc("x") + "\n"
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

// The regression this fixes: a value with embedded newlines (e.g. a
// `find ... -printf '%f\n'` directory list) must be captured whole, not truncated to
// its first line the way the old raw NAME=value line protocol did.
func TestParseOutputEnv_MultilineValueRoundTrips(t *testing.T) {
	list := "outpost-gateway\nbuilder\ngit"
	stdout := "some log\nMARKER\nDIRS=" + base64.StdEncoding.EncodeToString([]byte(list)) + "\n"
	real, out := parseOutputEnv(stdout, []string{"DIRS"}, "MARKER")
	if real != "some log" {
		t.Errorf("real stdout = %q", real)
	}
	if out["DIRS"] != list {
		t.Errorf("DIRS = %q, want the full multi-line list %q", out["DIRS"], list)
	}
}

// A value that isn't valid base64 (e.g. base64 missing on the runner image, so the
// emit produced an empty/garbled field) is skipped rather than surfaced raw.
func TestParseOutputEnv_SkipsUndecodableValue(t *testing.T) {
	stdout := "log\nMARKER\nFOO=not!valid!base64!\n"
	_, out := parseOutputEnv(stdout, []string{"FOO"}, "MARKER")
	if _, ok := out["FOO"]; ok {
		t.Errorf("expected undecodable value to be skipped, got %v", out)
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

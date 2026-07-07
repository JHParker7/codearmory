package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestValidateResolve covers the shape checks: nil is valid, the regex is required and
// must compile, the mode is dir/file, and a scannable volume must resolve.
func TestValidateResolve(t *testing.T) {
	wd := []VolumeMount{{Name: "base", MountPath: "/workspace", Workdir: true}}
	cases := []struct {
		name    string
		spec    *ResolveSpec
		mounts  []VolumeMount
		wantErr string
	}{
		{name: "nil is valid", spec: nil},
		{name: "regex required", spec: &ResolveSpec{}, mounts: wd, wantErr: "regex is required"},
		{name: "bad regex", spec: &ResolveSpec{Regex: "("}, mounts: wd, wantErr: "regex"},
		{name: "bad mode", spec: &ResolveSpec{Regex: ".*", Mode: "socket"}, mounts: wd, wantErr: "mode"},
		{name: "negative depth", spec: &ResolveSpec{Regex: ".*", MaxDepth: -1}, mounts: wd, wantErr: "max_depth"},
		{name: "bad output name", spec: &ResolveSpec{Regex: ".*", Output: "1bad"}, mounts: wd, wantErr: "output"},
		{name: "named volume missing", spec: &ResolveSpec{Regex: ".*", Volume: "ghost"}, mounts: wd, wantErr: "not attached"},
		{name: "no scannable volume", spec: &ResolveSpec{Regex: ".*"}, mounts: []VolumeMount{{Name: "a", MountPath: "/a"}, {Name: "b", MountPath: "/b"}}, wantErr: "requires a volume"},
		{name: "valid workdir default", spec: &ResolveSpec{Regex: "svc/.*"}, mounts: wd},
		{name: "valid single mount", spec: &ResolveSpec{Regex: ".*"}, mounts: []VolumeMount{{Name: "only", MountPath: "/only"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateResolve(tc.spec, tc.mounts)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateResolve = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validateResolve = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestResolveCommand_Functional executes the synthesised scan against a real directory
// tree and captures the output variable the same way the worker's output_env does, so
// it proves the actual find | grep behaviour: regex filter, dir-vs-file mode, relative
// paths, sorted, and an empty result on no match.
func TestResolveCommand_Functional(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"svc/a/Dockerfile", "svc/b/Dockerfile", "lib/c/main.go", "README.md"} {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mounts := []VolumeMount{{Name: "base", MountPath: root, Workdir: true}}

	run := func(r *ResolveSpec) []string {
		t.Helper()
		cmd := resolveCommand(r, mounts)
		// Emulate output_env capture: append `printf "%s" "$paths"` and read stdout.
		script := cmd[2] + "\nprintf '%s' \"$" + resolveOutputVar(r) + "\"\n"
		out, err := exec.Command(cmd[0], "-c", script).Output() //nolint:gosec // test-controlled
		if err != nil {
			t.Fatalf("resolve script failed: %v", err)
		}
		s := strings.TrimSpace(string(out))
		if s == "" {
			return nil
		}
		return strings.Split(s, "\n")
	}

	t.Run("dirs matching a subtree regex", func(t *testing.T) {
		got := run(&ResolveSpec{Regex: "^svc/[^/]+$", Mode: "dir"})
		want := []string{"svc/a", "svc/b"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("files by extension", func(t *testing.T) {
		got := run(&ResolveSpec{Regex: `Dockerfile$`, Mode: "file"})
		want := []string{"svc/a/Dockerfile", "svc/b/Dockerfile"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("no match yields empty", func(t *testing.T) {
		if got := run(&ResolveSpec{Regex: "nothing-matches-this", Mode: "dir"}); got != nil {
			t.Errorf("got %v, want empty", got)
		}
	})
}

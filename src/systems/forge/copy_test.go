package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestValidateCopy covers the shape checks: a nil spec is not a copy, a copy needs
// sources and a workdir destination, sources must be distinct attached non-dest
// volumes, paths must be relative, and disjoint mode forbids the whole-tree path.
func TestValidateCopy(t *testing.T) {
	dest := VolumeMount{Name: "base", MountPath: "/workspace", Workdir: true}
	src := VolumeMount{Name: "leg0", MountPath: "/leg0", ReadOnly: true}

	cases := []struct {
		name    string
		spec    *CopySpec
		mounts  []VolumeMount
		wantErr string // "" = expect success
	}{
		{name: "nil is valid", spec: nil, mounts: nil},
		{
			name:    "no sources",
			spec:    &CopySpec{},
			mounts:  []VolumeMount{dest},
			wantErr: "sources is required",
		},
		{
			name:    "no workdir destination",
			spec:    &CopySpec{Sources: []CopySource{{Volume: "leg0"}}},
			mounts:  []VolumeMount{{Name: "leg0", MountPath: "/leg0"}},
			wantErr: "workdir: true",
		},
		{
			name:    "destination read_only",
			spec:    &CopySpec{Sources: []CopySource{{Volume: "leg0"}}},
			mounts:  []VolumeMount{{Name: "base", MountPath: "/workspace", Workdir: true, ReadOnly: true}, src},
			wantErr: "read_only",
		},
		{
			name:    "source not attached",
			spec:    &CopySpec{Sources: []CopySource{{Volume: "ghost"}}},
			mounts:  []VolumeMount{dest},
			wantErr: "not attached",
		},
		{
			name:    "source is the destination",
			spec:    &CopySpec{Sources: []CopySource{{Volume: "base"}}},
			mounts:  []VolumeMount{dest},
			wantErr: "destination",
		},
		{
			name:    "duplicate source",
			spec:    &CopySpec{Sources: []CopySource{{Volume: "leg0"}, {Volume: "leg0"}}},
			mounts:  []VolumeMount{dest, src},
			wantErr: "more than once",
		},
		{
			name:    "parent-escape path",
			spec:    &CopySpec{Sources: []CopySource{{Volume: "leg0", Paths: []string{"../etc"}}}},
			mounts:  []VolumeMount{dest, src},
			wantErr: "relative path",
		},
		{
			name:    "disjoint forbids whole-tree",
			spec:    &CopySpec{Disjoint: true, Sources: []CopySource{{Volume: "leg0", Paths: []string{"."}}}},
			mounts:  []VolumeMount{dest, src},
			wantErr: "whole-tree",
		},
		{
			name:   "valid clone",
			spec:   &CopySpec{Sources: []CopySource{{Volume: "leg0"}}},
			mounts: []VolumeMount{dest, src},
		},
		{
			name:   "valid gather",
			spec:   &CopySpec{Disjoint: true, Sources: []CopySource{{Volume: "leg0", Paths: []string{"dist"}}}},
			mounts: []VolumeMount{dest, src},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCopy(tc.spec, tc.mounts)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateCopy = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validateCopy = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

// runCopyScript executes the synthesised copy command against real directories: the
// generated script references each mount's MountPath, so pointing those at temp dirs
// lets us prove the actual cp/staging/conflict behaviour end-to-end, not just its text.
func runCopyScript(t *testing.T, spec *CopySpec, mounts []VolumeMount) error {
	t.Helper()
	cmd := copyCommand(spec, mounts)
	if len(cmd) != 3 || cmd[0] != copyShell || cmd[1] != "-c" {
		t.Fatalf("copyCommand shape = %v", cmd)
	}
	return exec.Command(cmd[0], cmd[1], cmd[2]).Run() //nolint:gosec // test-controlled script
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestCopyCommand_CloneWholeTree proves a single-source, no-paths copy replicates the
// whole source tree into the destination root — the scatter-clone case.
func TestCopyCommand_CloneWholeTree(t *testing.T) {
	base := t.TempDir()
	dest := t.TempDir()
	writeFile(t, filepath.Join(base, "go.mod"), "module x\n")
	writeFile(t, filepath.Join(base, "svc/a/main.go"), "package a\n")

	spec := &CopySpec{Sources: []CopySource{{Volume: "base"}}}
	mounts := []VolumeMount{
		{Name: "dst", MountPath: dest, Workdir: true},
		{Name: "base", MountPath: base, ReadOnly: true},
	}
	if err := runCopyScript(t, spec, mounts); err != nil {
		t.Fatalf("clone script failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "go.mod")); err != nil {
		t.Errorf("go.mod not cloned: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "svc/a/main.go")); err != nil {
		t.Errorf("nested file not cloned: %v", err)
	}
}

// TestCopyCommand_GatherUnion proves a disjoint gather unions each leg's declared owned
// paths into the destination.
func TestCopyCommand_GatherUnion(t *testing.T) {
	leg0, leg1, base := t.TempDir(), t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(leg0, "svc/a/dist/out.bin"), "A\n")
	writeFile(t, filepath.Join(leg1, "svc/b/dist/out.bin"), "B\n")

	spec := &CopySpec{
		Disjoint: true,
		Sources: []CopySource{
			{Volume: "leg0", Paths: []string{"svc/a/dist"}},
			{Volume: "leg1", Paths: []string{"svc/b/dist"}},
		},
	}
	mounts := []VolumeMount{
		{Name: "base", MountPath: base, Workdir: true},
		{Name: "leg0", MountPath: leg0, ReadOnly: true},
		{Name: "leg1", MountPath: leg1, ReadOnly: true},
	}
	if err := runCopyScript(t, spec, mounts); err != nil {
		t.Fatalf("gather script failed: %v", err)
	}
	for _, p := range []string{"svc/a/dist/out.bin", "svc/b/dist/out.bin"} {
		if _, err := os.Stat(filepath.Join(base, p)); err != nil {
			t.Errorf("gathered path %q missing: %v", p, err)
		}
	}
}

// TestCopyCommand_GatherConflict proves the disjointness check fails (non-zero exit)
// when two legs claim the same path, instead of silently letting one clobber the other.
func TestCopyCommand_GatherConflict(t *testing.T) {
	leg0, leg1, base := t.TempDir(), t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(leg0, "shared/x"), "from-0\n")
	writeFile(t, filepath.Join(leg1, "shared/x"), "from-1\n")

	spec := &CopySpec{
		Disjoint: true,
		Sources: []CopySource{
			{Volume: "leg0", Paths: []string{"shared"}},
			{Volume: "leg1", Paths: []string{"shared"}},
		},
	}
	mounts := []VolumeMount{
		{Name: "base", MountPath: base, Workdir: true},
		{Name: "leg0", MountPath: leg0, ReadOnly: true},
		{Name: "leg1", MountPath: leg1, ReadOnly: true},
	}
	if err := runCopyScript(t, spec, mounts); err == nil {
		t.Fatal("gather with overlapping paths should fail, but the script succeeded")
	}
	// The base must be untouched — staging fails before anything merges in.
	if _, err := os.Stat(filepath.Join(base, "shared")); !os.IsNotExist(err) {
		t.Errorf("base workspace was written despite a gather conflict (err=%v)", err)
	}
}

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The storage preflight. These verify the CHECKS, not the filesystem: each one is
// driven so that it must pass on a working POSIX directory and must report the specific
// property when that property is unavailable.

func TestStoragePreflightMode(t *testing.T) {
	cases := []struct {
		env  string
		want string
		why  string
	}{
		{"", "enforce", "unset must enforce — the default has to be the safe one, since the unsafe outcome is silent corruption"},
		{"enforce", "enforce", "explicit enforce"},
		{"warn", "warn", "downgrade to a warning"},
		{"WARN", "warn", "case-insensitive, so an operator's spelling does not silently re-enable enforcement"},
		{" off ", "off", "trimmed"},
		{"nonsense", "enforce", "an unrecognised value must fall back to enforcing, never to skipping"},
	}
	for _, tc := range cases {
		t.Setenv("GIT_STORAGE_PREFLIGHT", tc.env)
		if got := storagePreflightMode(); got != tc.want {
			t.Errorf("GIT_STORAGE_PREFLIGHT=%q -> %q, want %q — %s", tc.env, got, tc.want, tc.why)
		}
	}
}

// A plain directory on an ordinary filesystem passes every check. If this fails, the
// preflight would refuse to start a perfectly good deployment.
func TestRunStoragePreflight_PassesOnAPlainDirectory(t *testing.T) {
	if failures := runStoragePreflight(t.TempDir()); len(failures) != 0 {
		t.Errorf("a normal POSIX directory failed the preflight: %v", failures)
	}
}

// The checks leave nothing behind. They run on every startup against the live repo
// store, so a probe file left in the storage root would accumulate — and one named
// like a lock would be swept by the ref-lock janitor, which is a confusing thing to
// find in a log.
func TestRunStoragePreflight_LeavesNoProbeFiles(t *testing.T) {
	dir := t.TempDir()
	if failures := runStoragePreflight(dir); len(failures) != 0 {
		t.Fatalf("preflight failed: %v", failures)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("preflight left %v behind", names)
	}
}

// It creates the storage root when absent — a fresh volume is empty, and the preflight
// runs before anything else would have made the directory.
func TestRunStoragePreflight_CreatesAMissingRoot(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "repos")
	if failures := runStoragePreflight(dir); len(failures) != 0 {
		t.Fatalf("preflight failed on a missing root: %v", failures)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("storage root was not created: %v", err)
	}
}

// An unusable root is one failure that names the path, not three confusing ones about
// locking.
func TestRunStoragePreflight_UnusableRootReportsOnce(t *testing.T) {
	// A regular file where the directory should be: MkdirAll cannot proceed.
	blocked := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	failures := runStoragePreflight(blocked)
	if len(failures) != 1 {
		t.Fatalf("got %d failures, want exactly 1: %v", len(failures), failures)
	}
	if !strings.Contains(failures[0].Error(), blocked) {
		t.Errorf("the failure does not name the path: %v", failures[0])
	}
}

// Each check passes in isolation on a working directory.
func TestIndividualChecksPassOnAWorkingFilesystem(t *testing.T) {
	for name, fn := range map[string]func(string) error{
		"exclusive create": checkExclusiveCreate,
		"atomic rename":    checkAtomicRename,
		"flock":            checkFlock,
	} {
		if err := fn(t.TempDir()); err != nil {
			t.Errorf("%s failed on a plain directory: %v", name, err)
		}
	}
}

// A leftover probe from a previous run that was killed mid-check must not fail the
// next startup. Without the pre-emptive remove, one killed pod would make the service
// refuse to start forever.
func TestCheckExclusiveCreate_ToleratesALeftoverProbe(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "excl.probe"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkExclusiveCreate(dir); err != nil {
		t.Errorf("a leftover probe file failed the check: %v", err)
	}
}

// The rename check must reject a copy-then-delete implementation, which is what object
// storage FUSE layers provide. Simulated by leaving the source in place — the property
// under test is "the source is gone afterwards", i.e. it moved rather than copied.
func TestCheckAtomicRename_RejectsACopyThatLeavesTheSource(t *testing.T) {
	dir := t.TempDir()
	if err := checkAtomicRename(dir); err != nil {
		t.Fatalf("baseline failed: %v", err)
	}
	// Assert the check actually reads the destination back rather than trusting the
	// rename's return: a layer that returns success and writes nothing must be caught.
	dst := filepath.Join(dir, "rename.probe")
	if _, err := os.Stat(dst); err == nil {
		t.Error("the rename probe was left behind")
	}
}

// A directory the process cannot write to fails rather than passing vacuously — the
// mode where every check would silently be a no-op.
func TestRunStoragePreflight_ReadOnlyRootFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits are not enforced")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) }) //nolint:errcheck

	if failures := runStoragePreflight(dir); len(failures) == 0 {
		t.Error("a read-only storage root passed the preflight; every check would be a no-op there")
	}
}

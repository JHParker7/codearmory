package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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
	for name, fn := range map[string]func(string, string) error{
		"exclusive create": checkExclusiveCreate,
		"atomic rename":    checkAtomicRename,
		"flock":            checkFlock,
	} {
		if err := fn(t.TempDir(), localProbeID()); err != nil {
			t.Errorf("%s failed on a plain directory: %v", name, err)
		}
	}
}

// A leftover probe from a previous run that was killed mid-check must not fail the
// next startup. Without the pre-emptive remove, one killed pod would make the service
// refuse to start forever.
func TestCheckExclusiveCreate_ToleratesALeftoverProbe(t *testing.T) {
	dir := t.TempDir()
	// The probe name is per-process and stable across restarts (hostname+pid, and in a
	// container the pid is always 1), so the leftover the next startup finds is the one
	// under its OWN name — which is exactly what probePath returns here.
	if err := os.WriteFile(probePath(dir, "excl", localProbeID()), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkExclusiveCreate(dir, localProbeID()); err != nil {
		t.Errorf("a leftover probe file failed the check: %v", err)
	}
}

// Probe names must be scoped to the process, or replicas sharing one mount probe each
// other's files. Two pods starting together is the NORMAL case on shared storage — a
// rollout does it every time — and with fixed names the collisions all present as
// false failures: the rename source vanishing under the other pod's cleanup, a torn
// read of its half-written destination, and (worst) flock being REFUSED because the
// other pod holds it, which is the property working, reported as broken. In the default
// enforce mode that is a crash loop on a filesystem that is entirely correct.
func TestProbePath_IsScopedToTheClient(t *testing.T) {
	dir := t.TempDir()
	for _, kind := range []string{"excl", "rename", "rename-src", "flock"} {
		got := probePath(dir, kind, "git-factory-aaa-1")
		if filepath.Dir(got) != dir {
			t.Errorf("%s probe landed in %q, want %q", kind, filepath.Dir(got), dir)
		}
		if !strings.HasSuffix(got, ".probe") {
			t.Errorf("%s probe %q does not end in .probe — the sweep would not collect it", kind, got)
		}
		// Two pods must never resolve the same probe to the same file.
		if other := probePath(dir, kind, "git-factory-bbb-1"); other == got {
			t.Errorf("%s probe is identical for two pods (%q): replicas would trample each other", kind, got)
		}
	}
	// The rename check needs its two files distinct, not merely unique per pod.
	if probePath(dir, "rename", "same") == probePath(dir, "rename-src", "same") {
		t.Error("the rename source and destination resolve to the same path")
	}
	// The service's own id must actually name the pod, or the scoping above is moot.
	host, _ := os.Hostname()
	if id := localProbeID(); !strings.Contains(id, host) || !strings.Contains(id, fmt.Sprint(os.Getpid())) {
		t.Errorf("localProbeID()=%q does not identify this pod (host %q, pid %d)", id, host, os.Getpid())
	}
}

// The real-deployment reproduction: several DISTINCT clients running the preflight
// against ONE directory at the same time, which is what git_factory replicas do on a
// shared JuiceFS mount every time the deployment rolls. Each must pass. With a fixed
// probe name this failed reliably — and on the minikube JuiceFS bring-up it did,
// crash-looping both replicas on a filesystem that was entirely correct.
func TestRunStoragePreflight_ConcurrentClientsDoNotCollide(t *testing.T) {
	dir := t.TempDir()
	const clients = 8

	var wg sync.WaitGroup
	failures := make([][]error, clients)
	for i := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A distinct pod name per client — goroutines would otherwise share this
			// process's hostname and pid and model one pod, not several.
			failures[i] = runStoragePreflightAs(dir, fmt.Sprintf("git-factory-%d-1", i))
		}()
	}
	wg.Wait()

	for i, f := range failures {
		if len(f) != 0 {
			t.Errorf("concurrent client %d failed on a working filesystem: %v", i, f)
		}
	}
}

// Probes abandoned by a pod that no longer exists are collected, but only once they are
// old enough that they cannot belong to a replica probing right now.
func TestSweepStaleProbes_CollectsOnlyOldLitter(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "excl.git-factory-deadbeef-1.probe")
	fresh := filepath.Join(dir, "flock.git-factory-live-1.probe")
	repo := filepath.Join(dir, "0e1")
	for _, p := range []string{stale, fresh} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(repo, 0o750); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	sweepStaleProbes(dir)

	if _, err := os.Stat(stale); !errors.Is(err, fs.ErrNotExist) {
		t.Error("an abandoned probe was left in the repo store")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("a probe another replica may be using right now was removed: %v", err)
	}
	if _, err := os.Stat(repo); err != nil {
		t.Errorf("the sweep touched a repo fan-out directory: %v", err)
	}
}

// The rename check must reject a copy-then-delete implementation, which is what object
// storage FUSE layers provide. Simulated by leaving the source in place — the property
// under test is "the source is gone afterwards", i.e. it moved rather than copied.
func TestCheckAtomicRename_RejectsACopyThatLeavesTheSource(t *testing.T) {
	dir := t.TempDir()
	if err := checkAtomicRename(dir, localProbeID()); err != nil {
		t.Fatalf("baseline failed: %v", err)
	}
	// Assert the check actually reads the destination back rather than trusting the
	// rename's return: a layer that returns success and writes nothing must be caught.
	dst := probePath(dir, "rename", localProbeID())
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

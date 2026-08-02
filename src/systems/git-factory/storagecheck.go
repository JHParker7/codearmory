package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Storage preflight (ARCHITECTURE §5).
//
// Moving GIT_STORAGE_ROOT off local disk is a configuration change with no application
// code behind it — it is still a path on a filesystem. That is what makes Step 2 cheap,
// and it is also what makes it dangerous: nothing in the service notices that the path
// now means something completely different, and a filesystem that merely LOOKS POSIX
// will take every write and corrupt repositories rather than fail.
//
// The doc is explicit that three properties are non-negotiable and "must be verified on
// the real deployment, not assumed from documentation":
//
//	O_CREAT|O_EXCL is atomic     — git's ref-lock primitive
//	rename() is atomic           — how every ref update commits
//	flock/fcntl work             — advisory locking git and our maintenance rely on
//
// It names the class that fails them: object storage via FUSE (s3fs, gcsfuse,
// Mountpoint, rclone), which has no atomic rename and no real locking, and so
// "corrupts repositories rather than merely being slow". Someone pointing
// GIT_STORAGE_ROOT at an s3fs mount because it is the S3 bucket they already had is a
// realistic mistake, and this is the check that catches it before it takes a push.
//
// WHAT THIS CAN AND CANNOT PROVE. It runs in one process on one node, so it verifies
// these properties LOCALLY. Genuine cross-CLIENT atomicity — two pods on two nodes —
// cannot be established from inside a single pod, and this does not claim to. What it
// does catch is a filesystem that fails the properties outright, which is the actual
// failure mode the doc warns about: s3fs does not become atomic when there is only one
// writer. Treat a pass as "not obviously unsafe", not as certification for multi-writer
// use. Verifying the cross-client half needs two pods against one mount and belongs in
// the deployment checklist, not here.

// storagePreflightMode selects what a failed check does, from GIT_STORAGE_PREFLIGHT:
//
//	enforce (default) — refuse to start
//	warn              — log loudly and continue
//	off               — skip the checks entirely
//
// It defaults to ENFORCE because the two outcomes are not symmetric. A false negative
// stops the service with an error naming the failed property and this override — loud,
// immediate, recoverable in one env var. Continuing on a filesystem that really lacks
// these properties corrupts repositories silently, and a corrupted repo is not
// recoverable from anywhere, since for pushed source this store is the only copy.
func storagePreflightMode() string {
	switch mode := strings.ToLower(strings.TrimSpace(envOrDefault("GIT_STORAGE_PREFLIGHT", "enforce"))); mode {
	case "warn", "off":
		return mode
	case "", "enforce":
		return "enforce"
	default:
		slog.Warn("storage preflight: unrecognised GIT_STORAGE_PREFLIGHT, enforcing",
			"value", mode)
		return "enforce"
	}
}

// checkExclusiveCreate verifies O_CREAT|O_EXCL refuses an existing path.
//
// This is git's ref-lock primitive: every ref update begins by creating
// refs/heads/<name>.lock this way, and the whole serialisation scheme rests on the
// SECOND creator being refused. A filesystem that reports success twice hands the same
// ref to two writers at once, which is a lost update with nothing logged anywhere.
func checkExclusiveCreate(dir string) error {
	path := filepath.Join(dir, "excl.probe")
	os.Remove(path) //nolint:errcheck // a leftover from a killed probe must not fail the run

	first, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("could not create a probe file with O_CREAT|O_EXCL: %w", err)
	}
	first.Close()
	defer os.Remove(path) //nolint:errcheck

	second, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		second.Close()
		return errors.New("O_CREAT|O_EXCL succeeded twice on the same path: " +
			"this filesystem cannot serialise git's ref locks, and concurrent pushes will lose updates")
	}
	if !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("O_CREAT|O_EXCL on an existing path failed with %w, want EEXIST", err)
	}
	return nil
}

// checkAtomicRename verifies rename() replaces an existing file in one step.
//
// Every ref update commits by renaming the .lock file over the ref, so a rename that is
// really copy-then-delete (what object-storage FUSE layers do) leaves a window where the
// ref is absent or half-written. A reader in that window sees a repository with a
// missing branch.
func checkAtomicRename(dir string) error {
	src := filepath.Join(dir, "rename.probe.tmp")
	dst := filepath.Join(dir, "rename.probe")
	defer func() {
		os.Remove(src) //nolint:errcheck
		os.Remove(dst) //nolint:errcheck
	}()

	if err := os.WriteFile(dst, []byte("old"), 0o600); err != nil {
		return fmt.Errorf("could not write the rename target: %w", err)
	}
	if err := os.WriteFile(src, []byte("new"), 0o600); err != nil {
		return fmt.Errorf("could not write the rename source: %w", err)
	}
	// Renaming ONTO an existing path is the case that matters — some layers support
	// rename only when the destination is absent, which is not what a ref update does.
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("rename onto an existing path failed: %w — "+
			"ref updates commit this way and cannot work without it", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		return fmt.Errorf("could not read back the renamed file: %w", err)
	}
	if string(got) != "new" {
		return fmt.Errorf("after rename the target holds %q, want %q", got, "new")
	}
	if _, err := os.Stat(src); !errors.Is(err, fs.ErrNotExist) {
		return errors.New("the rename source still exists afterwards: rename was a copy, not a move")
	}
	return nil
}

// checkFlock verifies advisory locking is enforced between separate open file
// descriptions.
//
// Two descriptions of the same file, one holding LOCK_EX, the other asking for
// LOCK_EX|LOCK_NB: a working implementation refuses the second with EWOULDBLOCK. This
// runs in one process on purpose — flock conflicts between distinct open file
// descriptions regardless of process, so the check is meaningful without forking, and a
// no-op implementation (FUSE layers that accept every lock request and enforce nothing)
// grants both.
func checkFlock(dir string) error {
	path := filepath.Join(dir, "flock.probe")
	defer os.Remove(path) //nolint:errcheck

	held, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("could not create the lock probe: %w", err)
	}
	defer held.Close()

	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("could not take an exclusive flock: %w", err)
	}
	defer syscall.Flock(int(held.Fd()), syscall.LOCK_UN) //nolint:errcheck

	contender, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("could not reopen the lock probe: %w", err)
	}
	defer contender.Close()

	err = syscall.Flock(int(contender.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		syscall.Flock(int(contender.Fd()), syscall.LOCK_UN) //nolint:errcheck
		return errors.New("a second exclusive flock was granted while the first was held: " +
			"locking on this filesystem is accepted but not enforced")
	}
	if !errors.Is(err, syscall.EWOULDBLOCK) {
		return fmt.Errorf("the second flock failed with %w, want EWOULDBLOCK", err)
	}
	return nil
}

// runStoragePreflight runs every check against dir and returns the failures.
//
// All of them run even after one fails: an operator debugging a storage backend wants
// the full picture in one restart, not a new failure revealed on each attempt.
func runStoragePreflight(dir string) []error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return []error{fmt.Errorf("storage root %q is not usable: %w", dir, err)}
	}

	checks := []struct {
		name string
		fn   func(string) error
	}{
		{"O_CREAT|O_EXCL exclusivity (git's ref-lock primitive)", checkExclusiveCreate},
		{"atomic rename onto an existing path (how ref updates commit)", checkAtomicRename},
		{"advisory locking via flock", checkFlock},
	}

	var failures []error
	for _, c := range checks {
		if err := c.fn(dir); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", c.name, err))
		}
	}
	return failures
}

// verifyStorage runs the preflight against GIT_STORAGE_ROOT and applies the configured
// mode. Called from main before anything can serve a push.
func verifyStorage() {
	mode := storagePreflightMode()
	dir := secretOrDefault("GIT_STORAGE_ROOT", "temp/repos")

	if mode == "off" {
		slog.Warn("storage preflight: skipped by GIT_STORAGE_PREFLIGHT=off; "+
			"the properties git depends on for correctness are unverified", "path", dir)
		return
	}

	failures := runStoragePreflight(dir)
	if len(failures) == 0 {
		slog.Info("storage preflight: passed", "path", dir,
			"verified", "O_CREAT|O_EXCL, atomic rename, flock",
			"note", "verified locally; cross-pod atomicity needs two pods against one mount")
		return
	}

	for _, err := range failures {
		slog.Error("storage preflight: FAILED", "path", dir, "error", err)
	}
	if mode == "warn" {
		slog.Warn("storage preflight: continuing anyway because GIT_STORAGE_PREFLIGHT=warn — "+
			"this filesystem can corrupt repositories rather than merely being slow", "path", dir)
		return
	}
	slog.Error("storage preflight: refusing to start. "+
		"git's correctness rests on the properties above, and a filesystem missing them corrupts "+
		"repositories silently instead of returning errors. Object storage mounted through FUSE "+
		"(s3fs, gcsfuse, Mountpoint, rclone) fails these by design — use a JuiceFS mount, which "+
		"splits metadata into a transactional engine, or a POSIX filesystem. "+
		"Set GIT_STORAGE_PREFLIGHT=warn to override once you understand the risk.",
		"path", dir, "failures", len(failures))
	os.Exit(1)
}

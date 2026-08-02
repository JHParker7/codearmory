package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The stale ref-lock janitor. The property under test throughout is the asymmetry it is
// built around: leaving a stale lock in place costs a tick, removing a live one loses a
// push silently. Every case below is written so that a janitor which got more eager
// would fail, not just one that stopped working.

// writeLock creates a lock file whose mtime is `age` in the past.
func writeLock(t *testing.T, path string, age time.Duration) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte("ref: whatever\n"), 0o640); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
	return path
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	return err == nil
}

func TestRefLockMaxAge(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration
		why  string
	}{
		{"", time.Hour, "unset means the default, not disabled — the janitor has to be on by default to be worth having"},
		{"15m", 15 * time.Minute, "a Go duration, as the name says"},
		{"nonsense", time.Hour, "an unparseable value must fall back to the default, never to a tiny threshold"},
		{"0", 0, "explicit zero disables the sweep"},
		{"-5m", -5 * time.Minute, "a negative value is passed through and disables the sweep at the caller"},
	}
	for _, tc := range cases {
		t.Setenv("GIT_REF_LOCK_MAX_AGE", tc.env)
		if got := refLockMaxAge(); got != tc.want {
			t.Errorf("GIT_REF_LOCK_MAX_AGE=%q -> %v, want %v — %s", tc.env, got, tc.want, tc.why)
		}
	}
}

// The core case: an abandoned lock goes, a lock from a push in flight stays.
func TestSweepStaleLocksIn_ClearsOnlyTheAbandoned(t *testing.T) {
	dir := t.TempDir()
	stale := writeLock(t, filepath.Join(dir, "refs", "heads", "main.lock"), 2*time.Hour)
	fresh := writeLock(t, filepath.Join(dir, "refs", "heads", "feature.lock"), 5*time.Second)

	if got := sweepStaleLocksIn(context.Background(), dir, time.Hour); got != 1 {
		t.Errorf("cleared %d, want 1", got)
	}
	if exists(t, stale) {
		t.Error("the abandoned lock survived — this is the ref that stays unpushable from every pod")
	}
	if !exists(t, fresh) {
		t.Error("a lock five seconds old was cleared; that is a push in flight, and removing it loses one of two concurrent updates")
	}
}

// Nested refs are found: the janitor mirrors the ref namespace rather than looking only
// at refs/heads.
func TestSweepStaleLocksIn_WalksNestedRefs(t *testing.T) {
	dir := t.TempDir()
	deep := writeLock(t, filepath.Join(dir, "refs", "heads", "team", "feature", "x.lock"), 3*time.Hour)
	tag := writeLock(t, filepath.Join(dir, "refs", "tags", "v1.lock"), 3*time.Hour)

	if got := sweepStaleLocksIn(context.Background(), dir, time.Hour); got != 2 {
		t.Errorf("cleared %d, want 2", got)
	}
	if exists(t, deep) || exists(t, tag) {
		t.Error("a nested ref lock survived; hierarchical branch names are ordinary and must not be a blind spot")
	}
}

// The named non-ref locks are covered. packed-refs.lock is the one that matters most:
// it is taken by our own gc, so a stale one blocks every future repack as well.
func TestSweepStaleLocksIn_ClearsNamedRepoLocks(t *testing.T) {
	dir := t.TempDir()
	var paths []string
	for _, rel := range repoLockFiles {
		paths = append(paths, writeLock(t, filepath.Join(dir, rel), 4*time.Hour))
	}
	if got := sweepStaleLocksIn(context.Background(), dir, time.Hour); got != len(repoLockFiles) {
		t.Errorf("cleared %d, want %d", got, len(repoLockFiles))
	}
	for _, p := range paths {
		if exists(t, p) {
			t.Errorf("%s survived", p)
		}
	}
}

// Nothing that is not a lock is ever touched. A janitor loose enough to take a ref file
// itself would destroy branches rather than unblock them.
func TestSweepStaleLocksIn_LeavesNonLockFilesAlone(t *testing.T) {
	dir := t.TempDir()
	ref := writeLock(t, filepath.Join(dir, "refs", "heads", "main"), 30*24*time.Hour)
	packed := writeLock(t, filepath.Join(dir, "packed-refs"), 30*24*time.Hour)
	cfg := writeLock(t, filepath.Join(dir, "config"), 30*24*time.Hour)
	// A directory that ends in .lock is still not a lock file.
	lockDir := filepath.Join(dir, "refs", "heads", "odd.lock")
	if err := os.MkdirAll(lockDir, 0o750); err != nil {
		t.Fatal(err)
	}

	if got := sweepStaleLocksIn(context.Background(), dir, time.Hour); got != 0 {
		t.Errorf("cleared %d, want 0", got)
	}
	for _, p := range []string{ref, packed, cfg, lockDir} {
		if !exists(t, p) {
			t.Errorf("%s was removed; only *.lock FILES are the janitor's business", p)
		}
	}
}

// A zero or negative threshold turns the sweep off. It must never be read as "every
// lock is stale" — that reading turns one bad config value into lost pushes across the
// whole plane at once.
func TestSweepStaleLocksIn_DisabledThresholdClearsNothing(t *testing.T) {
	for _, maxAge := range []time.Duration{0, -time.Hour} {
		dir := t.TempDir()
		ancient := writeLock(t, filepath.Join(dir, "refs", "heads", "main.lock"), 30*24*time.Hour)
		if got := sweepStaleLocksIn(context.Background(), dir, maxAge); got != 0 {
			t.Errorf("maxAge=%v cleared %d, want 0", maxAge, got)
		}
		if !exists(t, ancient) {
			t.Errorf("maxAge=%v deleted a lock; a disabled janitor must be inert, not maximally eager", maxAge)
		}
	}
}

// A repo with no refs/ directory at all (freshly initialised, never pushed to) is not
// an error — the walk skips the missing subtree and the named locks are still checked.
func TestSweepStaleLocksIn_MissingRefsDirIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	stale := writeLock(t, filepath.Join(dir, "packed-refs.lock"), 2*time.Hour)
	if got := sweepStaleLocksIn(context.Background(), dir, time.Hour); got != 1 {
		t.Errorf("cleared %d, want 1 — a missing refs/ must not abort the rest of the sweep", got)
	}
	if exists(t, stale) {
		t.Error("packed-refs.lock survived")
	}
}

// Two janitors over the same repo: the second finds the files already gone. That is the
// deployed shape — every pod runs this — so ENOENT has to be an ordinary outcome rather
// than an error, and the second pass must simply report nothing.
func TestSweepStaleLocksIn_ConcurrentSweepsAreSafe(t *testing.T) {
	dir := t.TempDir()
	writeLock(t, filepath.Join(dir, "refs", "heads", "main.lock"), 2*time.Hour)

	if got := sweepStaleLocksIn(context.Background(), dir, time.Hour); got != 1 {
		t.Fatalf("first sweep cleared %d, want 1", got)
	}
	if got := sweepStaleLocksIn(context.Background(), dir, time.Hour); got != 0 {
		t.Errorf("second sweep cleared %d, want 0 — a lock already taken by another pod is not this one's to count", got)
	}
}

// A cancelled context stops the sweep instead of running it to completion during
// shutdown.
func TestSweepStaleLocksIn_RespectsCancellation(t *testing.T) {
	dir := t.TempDir()
	stale := writeLock(t, filepath.Join(dir, "refs", "heads", "main.lock"), 2*time.Hour)
	named := writeLock(t, filepath.Join(dir, "packed-refs.lock"), 2*time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if got := sweepStaleLocksIn(ctx, dir, time.Hour); got != 0 {
		t.Errorf("cleared %d under a cancelled context, want 0", got)
	}
	if !exists(t, stale) || !exists(t, named) {
		t.Error("the sweep kept deleting after cancellation")
	}
}

// The boundary itself: a lock exactly at the threshold is NOT yet stale. Ties go to the
// holder, consistent with erring long everywhere else here.
func TestSweepStaleLocksIn_ThresholdIsExclusive(t *testing.T) {
	dir := t.TempDir()
	at := filepath.Join(dir, "refs", "heads", "main.lock")
	writeLock(t, at, time.Hour)

	// Just inside the threshold by a margin larger than filesystem mtime granularity.
	if got := sweepStaleLocksIn(context.Background(), dir, time.Hour+time.Minute); got != 0 {
		t.Errorf("cleared %d for a lock younger than the threshold, want 0", got)
	}
	if !exists(t, at) {
		t.Error("a lock younger than the threshold was cleared")
	}
}

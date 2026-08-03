package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Repo maintenance: the accounting a fixed-size volume needs, and the two things that
// act on it — a quota that refuses a push, and a repack that must never run while one
// is in flight.

func TestRepoQuotaBytes(t *testing.T) {
	cases := []struct {
		env  string
		want int64
		why  string
	}{
		{"", 0, "unset means unlimited — the feature is opt-in"},
		{"0", 0, "explicit zero is unlimited"},
		{"-5", 0, "a negative limit is meaningless; treat it as unset rather than as 'block everything'"},
		{"nonsense", 0, "an unparseable value must not silently become a tiny quota"},
		{"10", 10 * 1024 * 1024, "megabytes, as the name says"},
	}
	for _, tc := range cases {
		t.Setenv("GIT_REPO_QUOTA_MB", tc.env)
		if got := repoQuotaBytes(); got != tc.want {
			t.Errorf("GIT_REPO_QUOTA_MB=%q -> %d, want %d — %s", tc.env, got, tc.want, tc.why)
		}
	}
}

func TestMaintenanceInterval(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration
	}{
		{"", time.Hour},
		{"15m", 15 * time.Minute},
		{"0", 0},
		{"not-a-duration", time.Hour},
	}
	for _, tc := range cases {
		t.Setenv("GIT_MAINTENANCE_INTERVAL", tc.env)
		if got := maintenanceInterval(); got != tc.want {
			t.Errorf("GIT_MAINTENANCE_INTERVAL=%q -> %v, want %v", tc.env, got, tc.want)
		}
	}
}

// The size that gets stored has to come from the repo on disk, not from a constant.
func TestRepoSizeBytes_MeasuresARealRepo(t *testing.T) {
	setupTestDB(t)
	ctx := context.Background()

	id := uuid.New().String()
	re := seedRepoVisible(t, id, "user-1", "admin", "sized", visibilityPrivate)
	dir, err := localDirFor(ctx, re.ID)
	if err != nil {
		t.Fatalf("localDirFor: %v", err)
	}

	empty, err := repoSizeBytes(ctx, dir)
	if err != nil {
		t.Fatalf("repoSizeBytes: %v", err)
	}

	// Write an object into the bare repo so there is something to measure.
	blob := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(blob, make([]byte, 256*1024), 0o600); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	if out, err := exec.CommandContext(ctx, gitBinary, "-C", dir, "hash-object", "-w", blob).CombinedOutput(); err != nil {
		t.Fatalf("hash-object: %v (%s)", err, out)
	}

	after, err := repoSizeBytes(ctx, dir)
	if err != nil {
		t.Fatalf("repoSizeBytes: %v", err)
	}
	if after <= empty {
		t.Errorf("size after writing an object = %d, was %d — the measurement is not tracking the repo", after, empty)
	}
}

// recordRepoSize stores what it measured, and getRepoSize reads it back — the pair the
// quota check depends on.
func TestRecordRepoSize_RoundTrips(t *testing.T) {
	setupTestDB(t)
	ctx := context.Background()

	re := seedRepoVisible(t, uuid.New().String(), "user-1", "admin", "accounted", visibilityPrivate)
	if err := recordRepoSize(ctx, re.ID); err != nil {
		t.Fatalf("recordRepoSize: %v", err)
	}
	got, err := getRepoSize(ctx, re.ID)
	if err != nil {
		t.Fatalf("getRepoSize: %v", err)
	}
	if got < 0 {
		t.Fatalf("size = %d, want a non-negative measurement", got)
	}

	// And it is exposed, since a quota nobody can see coming is a support ticket.
	loaded, err := getRepo(ctx, "user-1", re.ID)
	if err != nil {
		t.Fatalf("getRepo: %v", err)
	}
	if loaded.SizeBytes != got {
		t.Errorf("Repo.SizeBytes = %d, want %d", loaded.SizeBytes, got)
	}
}

func TestQuotaHeadroom(t *testing.T) {
	setupTestDB(t)
	ctx := context.Background()
	re := seedRepoVisible(t, uuid.New().String(), "user-1", "admin", "quota", visibilityPrivate)

	t.Run("no quota configured means unlimited", func(t *testing.T) {
		t.Setenv("GIT_REPO_QUOTA_MB", "")
		if _, limited := quotaHeadroom(ctx, re.ID); limited {
			t.Error("quotaHeadroom reported a limit with none configured")
		}
	})

	t.Run("headroom is what is left", func(t *testing.T) {
		t.Setenv("GIT_REPO_QUOTA_MB", "10")
		if err := setRepoSize(ctx, re.ID, 4*1024*1024); err != nil {
			t.Fatalf("setRepoSize: %v", err)
		}
		headroom, limited := quotaHeadroom(ctx, re.ID)
		if !limited {
			t.Fatal("quotaHeadroom reported no limit with one configured")
		}
		if want := int64(6 * 1024 * 1024); headroom != want {
			t.Errorf("headroom = %d, want %d", headroom, want)
		}
	})

	// Zero headroom is the refusal signal: git reads maxInputSize=0 as UNLIMITED, so a
	// full repo must be rejected outright rather than handed to receive-pack.
	t.Run("a full repo has no headroom", func(t *testing.T) {
		t.Setenv("GIT_REPO_QUOTA_MB", "10")
		if err := setRepoSize(ctx, re.ID, 10*1024*1024); err != nil {
			t.Fatalf("setRepoSize: %v", err)
		}
		headroom, limited := quotaHeadroom(ctx, re.ID)
		if !limited || headroom != 0 {
			t.Errorf("headroom = %d, limited = %v; want 0, true", headroom, limited)
		}
	})
}

// gc must never repack a repo that is being pushed to. The push holds the lock shared;
// the sweep gives up rather than waiting, and says so by returning ran=false.
func TestGCRepo_SkipsABusyRepo(t *testing.T) {
	setupTestDB(t)
	ctx := context.Background()
	re := seedRepoVisible(t, uuid.New().String(), "user-1", "admin", "busy", visibilityPrivate)

	lock := repoLocks.get(re.ID)
	lock.RLock() // stand in for an in-flight push
	ran, err := gcRepo(ctx, re.ID)
	lock.RUnlock()
	if err != nil {
		t.Fatalf("gcRepo: %v", err)
	}
	if ran {
		t.Error("gcRepo repacked a repo with a transfer in flight")
	}

	// Once the transfer is done the same repo is repacked normally.
	ran, err = gcRepo(ctx, re.ID)
	if err != nil {
		t.Fatalf("gcRepo (idle): %v", err)
	}
	if !ran {
		t.Error("gcRepo skipped an idle repo")
	}
}

func TestRunMaintenance_SweepsEveryRepo(t *testing.T) {
	setupTestDB(t)
	ctx := context.Background()
	for _, name := range []string{"one", "two", "three"} {
		seedRepoVisible(t, uuid.New().String(), "user-1", "admin", name, visibilityPrivate)
	}
	swept, skipped, locks := runMaintenanceWhile(ctx, func() bool { return true })
	if swept != 3 || skipped != 0 {
		t.Errorf("the sweep repacked %d, skipped %d; want 3 and 0", swept, skipped)
	}
	if locks != 0 {
		t.Errorf("the sweep cleared %d locks on freshly created repos; want 0", locks)
	}
}

package main

import (
	"testing"
)

func TestDefaultQuotaBytes(t *testing.T) {
	// Unset: the built-in default applies, so a fresh install has a sane cap rather
	// than unlimited storage.
	t.Setenv("ARTIFACTS_DEFAULT_QUOTA_MB", "")
	if got, want := defaultQuotaBytes(), int64(defaultQuotaMB)*1024*1024; got != want {
		t.Errorf("default = %d, want %d", got, want)
	}
	// The admin's global knob.
	t.Setenv("ARTIFACTS_DEFAULT_QUOTA_MB", "100")
	if got := defaultQuotaBytes(); got != 100*1024*1024 {
		t.Errorf("configured default = %d, want 100 MB", got)
	}
	// Zero is a real setting — "nobody may store anything by default" — not an
	// accident to be read as unlimited.
	t.Setenv("ARTIFACTS_DEFAULT_QUOTA_MB", "0")
	if got := defaultQuotaBytes(); got != 0 {
		t.Errorf("default = %d, want 0 to mean blocked", got)
	}
	// Garbage falls back rather than silently meaning 0 (which would lock everyone
	// out) or unlimited (which would ignore the cap entirely).
	t.Setenv("ARTIFACTS_DEFAULT_QUOTA_MB", "not-a-number")
	if got, want := defaultQuotaBytes(), int64(defaultQuotaMB)*1024*1024; got != want {
		t.Errorf("invalid value gave %d, want the built-in default %d", got, want)
	}
	t.Setenv("ARTIFACTS_DEFAULT_QUOTA_MB", "-5")
	if got, want := defaultQuotaBytes(), int64(defaultQuotaMB)*1024*1024; got != want {
		t.Errorf("negative value gave %d, want the built-in default %d", got, want)
	}
}

func TestResolveMax(t *testing.T) {
	i64 := func(v int64) *int64 { return &v }

	// MB is the unit an admin thinks in; bytes are there for exactness.
	got, msg := resolveMax(quotaRequest{MaxMB: i64(10)})
	if msg != "" || got != 10*1024*1024 {
		t.Errorf("max_mb=10 -> (%d, %q), want 10 MB", got, msg)
	}
	got, msg = resolveMax(quotaRequest{MaxBytes: i64(1234)})
	if msg != "" || got != 1234 {
		t.Errorf("max_bytes=1234 -> (%d, %q)", got, msg)
	}
	// Zero is a deliberate lockout, so it must be accepted rather than treated as
	// "unset".
	if got, msg := resolveMax(quotaRequest{MaxMB: i64(0)}); msg != "" || got != 0 {
		t.Errorf("max_mb=0 -> (%d, %q), want an accepted 0", got, msg)
	}

	// Ambiguous or absent units are rejected rather than guessed.
	if _, msg := resolveMax(quotaRequest{MaxBytes: i64(1), MaxMB: i64(1)}); msg == "" {
		t.Error("both units given must be rejected")
	}
	if _, msg := resolveMax(quotaRequest{}); msg == "" {
		t.Error("neither unit given must be rejected")
	}
	if _, msg := resolveMax(quotaRequest{MaxMB: i64(-1)}); msg == "" {
		t.Error("a negative cap must be rejected")
	}
	if _, msg := resolveMax(quotaRequest{MaxBytes: i64(-1)}); msg == "" {
		t.Error("a negative cap must be rejected")
	}
}

func TestHumanMB(t *testing.T) {
	if got := humanMB(5 * 1024 * 1024); got != "5.0 MB" {
		t.Errorf("humanMB = %q", got)
	}
	if got := humanMB(0); got != "0.0 MB" {
		t.Errorf("humanMB(0) = %q", got)
	}
}

// The over-quota message names the numbers an operator needs to act, mirroring
// forge's volume-cap message.
func TestQuotaMsg(t *testing.T) {
	msg := quotaMsg(512*1024*1024, 10240*1024*1024, 10240*1024*1024)
	for _, want := range []string{"quota exceeded", "512.0 MB", "10240.0 MB"} {
		if !contains(msg, want) {
			t.Errorf("quotaMsg = %q, want it to mention %q", msg, want)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

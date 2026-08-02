package main

import (
	"strings"
	"testing"
)

// The rendered detail is the part of an audit entry a human actually reads, so its
// shape is worth pinning: the repo by clone path first, then facts, no empty fragments.
func TestAuditDetail(t *testing.T) {
	re := Repo{Namespace: "alice", Name: "widgets"}

	if got, want := auditDetail(re), "alice/widgets"; got != want {
		t.Errorf("auditDetail() = %q, want %q", got, want)
	}
	if got, want := auditDetail(re, "refs=refs/heads/main", "outcome=ok"), "alice/widgets refs=refs/heads/main outcome=ok"; got != want {
		t.Errorf("auditDetail() = %q, want %q", got, want)
	}
	// An empty fragment would leave a double space and a trailing gap in the log.
	if got, want := auditDetail(re, "", "outcome=ok"), "alice/widgets outcome=ok"; got != want {
		t.Errorf("auditDetail() with an empty part = %q, want %q", got, want)
	}
}

func TestRefsSummary(t *testing.T) {
	if got, want := refsSummary(nil), "none"; got != want {
		t.Errorf("refsSummary(nil) = %q, want %q — a push that moved nothing should say so", got, want)
	}
	if got, want := refsSummary([]string{"refs/heads/main"}), "refs/heads/main"; got != want {
		t.Errorf("refsSummary = %q, want %q", got, want)
	}

	// A 200-branch push must not write 200 refs into a table nobody prunes, but the
	// count is worth keeping.
	many := make([]string, 25)
	for i := range many {
		many[i] = "refs/heads/branch"
	}
	got := refsSummary(many)
	if !strings.HasSuffix(got, ",+15 more") {
		t.Errorf("refsSummary(25 refs) = %q, want it to end with the remaining count", got)
	}
	if strings.Count(got, "refs/heads/branch") != 10 {
		t.Errorf("refsSummary(25 refs) listed %d refs, want 10", strings.Count(got, "refs/heads/branch"))
	}
}

// Auditing must never be able to break the operation it describes. With no service key
// (tests, or before rotation starts) the call is a no-op rather than a panic.
func TestAuditEvent_NoOpWithoutAServiceKey(t *testing.T) {
	prev := serviceKey
	serviceKey = nil
	defer func() { serviceKey = prev }()

	auditEvent(t.Context(), "user-1", auditActionPush, "repo-1", "detail")
}

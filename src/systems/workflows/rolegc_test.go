package main

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestRoleInUseByActiveRun covers the guard that keeps a workflow role alive while a
// run still authenticates with it — the core of the fix for updating a workflow
// deleting an in-flight run's role. Only pending/running runs pin a role; terminal
// and awaiting_approval runs do not.
func TestRoleInUseByActiveRun(t *testing.T) {
	if !testDBReady {
		t.Skip("db not ready")
	}
	ctx := context.Background()

	mk := func(status, role string) string {
		id := uuid.NewString()
		run := WorkflowRun{RunID: id, WorkflowID: "wf-role-test", Status: status, RoleID: role, CreatedAt: time.Now().UTC()}
		if err := connect().Create(&run).Error; err != nil {
			t.Fatalf("insert run: %v", err)
		}
		return id
	}
	t.Cleanup(func() { connect().Exec(`DELETE FROM workflow_runs WHERE workflow_id = 'wf-role-test'`) }) //nolint:errcheck

	running := mk("running", "role-A")
	mk("pending", "role-A")           // a second active run on the same role
	mk("completed", "role-B")         // terminal — does not pin
	mk("failed", "role-C")            // terminal — does not pin
	mk("awaiting_approval", "role-D") // paused — re-mints on resume, does not pin

	for _, tc := range []struct {
		role string
		want bool
	}{
		{"role-A", true},  // has running + pending runs
		{"role-B", false}, // only completed
		{"role-C", false}, // only failed
		{"role-D", false}, // only awaiting_approval
		{"role-Z", false}, // no runs
		{"", false},       // empty role never in use
	} {
		got, err := roleInUseByActiveRun(ctx, tc.role)
		if err != nil {
			t.Fatalf("roleInUseByActiveRun(%q): %v", tc.role, err)
		}
		if got != tc.want {
			t.Errorf("roleInUseByActiveRun(%q) = %v, want %v", tc.role, got, tc.want)
		}
	}

	// runRoleID round-trips the stored role and is empty for an unknown run.
	if got := runRoleID(ctx, running); got != "role-A" {
		t.Errorf("runRoleID(running) = %q, want role-A", got)
	}
	if got := runRoleID(ctx, "no-such-run"); got != "" {
		t.Errorf("runRoleID(unknown) = %q, want empty", got)
	}
}

package main

import (
	"encoding/json"
	"os"
	"testing"
)

func TestSyncOutcome(t *testing.T) {
	cases := []struct {
		sync, health, phase, want string
	}{
		{"Synced", "Healthy", "Succeeded", SyncSynced},
		{"OutOfSync", "Progressing", "Running", SyncRunning},
		{"Synced", "Degraded", "Failed", SyncFailed},
		{"OutOfSync", "Missing", "Error", SyncFailed},
		{"Synced", "Healthy", "", SyncSynced},               // no operation, already converged
		{"OutOfSync", "Healthy", "", SyncRunning},           // not synced yet
		{"Synced", "Progressing", "Succeeded", SyncRunning}, // op done but not healthy yet
	}
	for _, c := range cases {
		if got := syncOutcome(c.sync, c.health, c.phase); got != c.want {
			t.Errorf("syncOutcome(%q,%q,%q) = %q, want %q", c.sync, c.health, c.phase, got, c.want)
		}
	}
}

// TestManifestSyncContract verifies the argo/sync action's success/failure
// states in the live manifest match what this service writes.
func TestManifestSyncContract(t *testing.T) {
	raw, err := os.ReadFile("../../../infra/local/registry-manifest.json")
	if err != nil {
		t.Skipf("manifest not available: %v", err)
	}
	var doc []struct {
		Actions []struct {
			Name  string `json:"name"`
			Async *struct {
				SuccessStates []string `json:"success_states"`
				FailureStates []string `json:"failure_states"`
			} `json:"async"`
		} `json:"actions"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	for _, svc := range doc {
		for _, a := range svc.Actions {
			if a.Name == "argo/sync" && a.Async != nil {
				if len(a.Async.SuccessStates) != 1 || a.Async.SuccessStates[0] != SyncSynced {
					t.Errorf("argo/sync success_states = %v, want [%s]", a.Async.SuccessStates, SyncSynced)
				}
				if len(a.Async.FailureStates) != 1 || a.Async.FailureStates[0] != SyncFailed {
					t.Errorf("argo/sync failure_states = %v, want [%s]", a.Async.FailureStates, SyncFailed)
				}
				return
			}
		}
	}
	t.Skip("argo/sync action not registered in manifest")
}

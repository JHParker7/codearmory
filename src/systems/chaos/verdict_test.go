package main

import (
	"encoding/json"
	"os"
	"testing"
)

func TestVerdictToStatus(t *testing.T) {
	cases := []struct {
		verdict string
		phase   string
		want    string
	}{
		{"Pass", "Completed", StatusPass},
		{"pass", "completed", StatusPass},
		{"Fail", "Completed", StatusFail},
		{"Error", "Completed", StatusError},
		{"Stopped", "Stopped", StatusError},
		{"Awaited", "Running", StatusRunning},
		{"", "Running", StatusRunning},
		{"", "Completed", StatusError}, // finished with no verdict is a failure
		{"  ", "  ", StatusRunning},
	}
	for _, c := range cases {
		if got := verdictToStatus(c.verdict, c.phase); got != c.want {
			t.Errorf("verdictToStatus(%q,%q) = %q, want %q", c.verdict, c.phase, got, c.want)
		}
	}
}

func TestIsTerminal(t *testing.T) {
	terminal := []string{StatusPass, StatusFail, StatusError, StatusStopped}
	for _, s := range terminal {
		if !isTerminal(s) {
			t.Errorf("isTerminal(%q) = false, want true", s)
		}
	}
	for _, s := range []string{StatusPending, StatusRunning, "weird"} {
		if isTerminal(s) {
			t.Errorf("isTerminal(%q) = true, want false", s)
		}
	}
}

// TestStatusContract guarantees the status strings this service writes line up
// with the success/failure states advertised for chaos/run-experiment in the
// registry manifest. If they drift, the workflows poller would never see a
// terminal state and steps would hang until timeout.
func TestStatusContract(t *testing.T) {
	for _, s := range runExperimentSuccessStates {
		if !isTerminal(s) {
			t.Errorf("declared success state %q is not terminal", s)
		}
	}
	for _, s := range runExperimentFailureStates {
		if !isTerminal(s) {
			t.Errorf("declared failure state %q is not terminal", s)
		}
	}
	// Every terminal verdict-mapped status (excluding the user-initiated
	// stopped) must be claimed by exactly one of the success/failure sets.
	claimed := map[string]bool{}
	for _, s := range append(append([]string{}, runExperimentSuccessStates...), runExperimentFailureStates...) {
		if claimed[s] {
			t.Errorf("status %q declared in both success and failure sets", s)
		}
		claimed[s] = true
	}
	for _, s := range []string{StatusPass, StatusFail, StatusError} {
		if !claimed[s] {
			t.Errorf("terminal status %q is not covered by the run-experiment contract", s)
		}
	}
}

// TestManifestStatusContract cross-checks the live registry manifest so a manual
// edit there cannot silently break the contract. It is best-effort: if the
// manifest is not reachable from the test working dir, it skips.
func TestManifestStatusContract(t *testing.T) {
	path := "../../../infra/local/registry-manifest.json"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("manifest not available: %v", err)
	}
	var doc []struct {
		Name    string `json:"name"`
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
	var found bool
	for _, svc := range doc {
		for _, a := range svc.Actions {
			if a.Name != "chaos/run-experiment" || a.Async == nil {
				continue
			}
			found = true
			assertSameSet(t, "success_states", a.Async.SuccessStates, runExperimentSuccessStates)
			assertSameSet(t, "failure_states", a.Async.FailureStates, runExperimentFailureStates)
		}
	}
	if !found {
		t.Skip("chaos/run-experiment action not yet registered in manifest")
	}
}

func assertSameSet(t *testing.T, label string, got, want []string) {
	t.Helper()
	gm := map[string]bool{}
	for _, g := range got {
		gm[g] = true
	}
	wm := map[string]bool{}
	for _, w := range want {
		wm[w] = true
	}
	if len(gm) != len(wm) {
		t.Errorf("%s: manifest %v != code %v", label, got, want)
		return
	}
	for w := range wm {
		if !gm[w] {
			t.Errorf("%s: code state %q missing from manifest %v", label, w, got)
		}
	}
}

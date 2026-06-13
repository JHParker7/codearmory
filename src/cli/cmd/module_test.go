package cmd

import "testing"

// pickProvider chooses the configured provider for a slot, falling back to the
// first registered when the choice is empty or unknown.
func TestPickProvider(t *testing.T) {
	opts := []Module{{Name: "gitea"}, {Name: "github"}}

	if got := pickProvider(opts, "github").Name; got != "github" {
		t.Errorf("explicit choice: got %q, want github", got)
	}
	if got := pickProvider(opts, "").Name; got != "gitea" {
		t.Errorf("empty choice: got %q, want first (gitea)", got)
	}
	if got := pickProvider(opts, "nope").Name; got != "gitea" {
		t.Errorf("unknown choice: got %q, want first (gitea)", got)
	}
}

// repos and containers are capability slots so a deployment can swap the backing
// provider (e.g. GitHub instead of Gitea) without touching the hub.
func TestReposAndContainersAreSlots(t *testing.T) {
	found := map[string]bool{"repos": false, "containers": false}
	for _, m := range moduleRegistry {
		if _, ok := found[m.Slot]; ok {
			found[m.Slot] = true
		}
	}
	for slot, ok := range found {
		if !ok {
			t.Errorf("expected a registered module providing the %q slot", slot)
		}
	}
}

// activeModules collapses each slot to exactly one provider.
func TestActiveModules_OneProviderPerSlot(t *testing.T) {
	isolateHome(t) // default config: no provider overrides

	counts := map[string]int{}
	for _, m := range activeModules() {
		if m.Slot != "" {
			counts[m.Slot]++
		}
	}
	for slot, n := range counts {
		if n != 1 {
			t.Errorf("slot %q wired %d providers, want exactly 1", slot, n)
		}
	}
}

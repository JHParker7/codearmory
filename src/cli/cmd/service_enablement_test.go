package cmd

import "testing"

// titlesPresent reports whether every wanted screen title appears in screens.
func titlesPresent(screens []HubScreen, titles ...string) bool {
	have := map[string]bool{}
	for _, s := range screens {
		have[s.Title] = true
	}
	for _, t := range titles {
		if !have[t] {
			return false
		}
	}
	return true
}

// TestFilterScreens_HidesDisabledService verifies that a module whose Service is
// disabled drops out of the hub, while account/core modules (empty Service) and
// other still-enabled services remain.
func TestFilterScreens_HidesDisabledService(t *testing.T) {
	full := filterScreens(false, nil)
	if !titlesPresent(full, "Forge", "Hooks") {
		t.Fatalf("expected Forge and Hooks in the unfiltered user hub, got %v", screenTitles(full))
	}

	filtered := filterScreens(false, map[string]bool{"forge": true})
	if titlesPresent(filtered, "Forge") {
		t.Errorf("Forge screen should be hidden when the forge service is disabled")
	}
	if !titlesPresent(filtered, "Hooks") {
		t.Errorf("Hooks screen should remain when only forge is disabled")
	}
	if len(filtered) != len(full)-1 {
		t.Errorf("disabling forge should remove exactly one screen: full=%d filtered=%d", len(full), len(filtered))
	}
}

// TestFilterScreens_AdminServiceHidden verifies the same applies to the admin
// hub: disabling forge hides the Forge Runtimes admin screen, but never the
// always-on Org Services screen (builder is core, and that screen has no
// Service, so it must survive — it's how you re-enable things).
func TestFilterScreens_AdminServiceHidden(t *testing.T) {
	full := filterScreens(true, nil)
	if !titlesPresent(full, "Forge Runtimes", "Org Services") {
		t.Fatalf("expected Forge Runtimes and Org Services in the admin hub, got %v", screenTitles(full))
	}

	filtered := filterScreens(true, map[string]bool{"forge": true})
	if titlesPresent(filtered, "Forge Runtimes") {
		t.Errorf("Forge Runtimes should be hidden when the forge service is disabled")
	}
	if !titlesPresent(filtered, "Org Services") {
		t.Errorf("Org Services must always remain — it's how a disabled service gets re-enabled")
	}
}

// TestDisabledServices_FailsOpenUnderTest documents the hermetic-test guard:
// disabledServices never reaches a builder during `go test`, so it yields an
// empty set and the live entry points behave exactly like the unfiltered hub.
func TestDisabledServices_FailsOpenUnderTest(t *testing.T) {
	if got := disabledServices(); len(got) != 0 {
		t.Errorf("disabledServices() should be empty under test, got %v", got)
	}
	if len(enabledScreensFor(false)) != len(hubScreens()) {
		t.Errorf("with nothing disabled, the user hub should match hubScreens()")
	}
	if len(enabledScreensFor(true)) != len(adminScreens()) {
		t.Errorf("with nothing disabled, the admin hub should match adminScreens()")
	}
}

func screenTitles(screens []HubScreen) []string {
	out := make([]string, len(screens))
	for i, s := range screens {
		out[i] = s.Title
	}
	return out
}

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

// registeredAll returns every backing Service of an active module on the given hub
// (admin or user) — the registered set under which nothing is service-hidden.
func registeredAll(admin bool) map[string]bool {
	out := map[string]bool{}
	for _, m := range activeModules() {
		if m.Admin == admin && m.Service != "" {
			out[m.Service] = true
		}
	}
	return out
}

// TestFilterScreens_HidesUnregisteredService verifies that a module whose Service
// is NOT in the registered routing table drops out of the hub, while account/core
// modules (empty Service) and other registered services remain.
func TestFilterScreens_HidesUnregisteredService(t *testing.T) {
	full := filterScreens(false, nil) // nil = unknown → fail open, nothing hidden
	if !titlesPresent(full, "Forge", "Hooks") {
		t.Fatalf("expected Forge and Hooks in the unfiltered user hub, got %v", screenTitles(full))
	}

	// Registered = everything except forge → forge is hidden, hooks remain.
	registered := registeredAll(false)
	delete(registered, "forge")
	filtered := filterScreens(false, registered)
	if titlesPresent(filtered, "Forge") {
		t.Errorf("Forge screen should be hidden when the forge service is not registered")
	}
	if !titlesPresent(filtered, "Hooks") {
		t.Errorf("Hooks screen should remain when only forge is unregistered")
	}
	if len(filtered) != len(full)-1 {
		t.Errorf("dropping forge should remove exactly one screen: full=%d filtered=%d", len(full), len(filtered))
	}
}

// TestFilterScreens_AdminUnregisteredHidden verifies the same applies to the admin
// hub: an unregistered forge hides the Forge Runtimes admin screen, but never the
// always-on Org Services screen (it has no Service, so it must survive — it's how
// you enable things).
func TestFilterScreens_AdminUnregisteredHidden(t *testing.T) {
	full := filterScreens(true, nil)
	if !titlesPresent(full, "Forge Runtimes", "Org Services") {
		t.Fatalf("expected Forge Runtimes and Org Services in the admin hub, got %v", screenTitles(full))
	}

	registered := registeredAll(true)
	delete(registered, "forge")
	filtered := filterScreens(true, registered)
	if titlesPresent(filtered, "Forge Runtimes") {
		t.Errorf("Forge Runtimes should be hidden when the forge service is not registered")
	}
	if !titlesPresent(filtered, "Org Services") {
		t.Errorf("Org Services must always remain — it has no Service and is how you enable things")
	}
}

// TestRegisteredServices_FailsOpenUnderTest documents the hermetic-test guard:
// registeredServices never reaches conductor during `go test`, so it yields nil
// (unknown) and the live entry points behave exactly like the unfiltered hub.
func TestRegisteredServices_FailsOpenUnderTest(t *testing.T) {
	if got := registeredServices(); got != nil {
		t.Errorf("registeredServices() should be nil (fail open) under test, got %v", got)
	}
	if len(enabledScreensFor(false)) != len(hubScreens()) {
		t.Errorf("with the routing table unknown, the user hub should match hubScreens()")
	}
	if len(enabledScreensFor(true)) != len(adminScreens()) {
		t.Errorf("with the routing table unknown, the admin hub should match adminScreens()")
	}
}

// knownPlatformServices is the set of builder/registry service names a CLI module
// may declare as its backing Service. It mirrors builder's catalog plus the core
// control-plane services. The guard test below fails if a module's Service drifts
// from a real name (e.g. "outpost" instead of "outpost-gateway") — the failure mode
// the module→service mapping otherwise hides until a user notices a module that
// won't hide. Keep this in sync with builder's service catalog.
var knownPlatformServices = map[string]bool{
	"gatekeeper": true, "conductor": true, "registry": true, "builder": true,
	"blueprints": true, "forge": true, "workflows": true, "tickets": true,
	"hooks": true, "containers": true, "gitea_integration": true, "git_connector": true,
	"chaos": true, "argo": true, "outpost-gateway": true, "notifications": true,
}

// TestModuleServices_KnownNames asserts every registered module's Service (when set)
// names a real platform service, catching a typo or stale name in the CLI half of the
// module→service mapping that the portal/manifest copies wouldn't surface.
func TestModuleServices_KnownNames(t *testing.T) {
	for _, m := range moduleRegistry {
		if m.Service == "" {
			continue // account/core modules are never service-gated
		}
		if !knownPlatformServices[m.Service] {
			t.Errorf("module %q declares Service %q, not a known platform service "+
				"(fix the typo, or update knownPlatformServices to match the builder catalog)", m.Name, m.Service)
		}
	}
}

func screenTitles(screens []HubScreen) []string {
	out := make([]string, len(screens))
	for i, s := range screens {
		out[i] = s.Title
	}
	return out
}

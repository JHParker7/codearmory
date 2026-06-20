package cmd

import "testing"

// The admin hub holds the platform-administration screens; the user hub must not.
func TestAdminScreens_SeparateFromUserHub(t *testing.T) {
	isolateHome(t)

	adminTitles := map[string]bool{}
	for _, s := range adminScreens() {
		adminTitles[s.Title] = true
	}
	for _, want := range []string{"Gatekeeper", "Forge Runtimes", "Audit Log"} {
		if !adminTitles[want] {
			t.Errorf("admin hub missing screen %q", want)
		}
	}

	// None of the admin screens should leak into the user hub.
	for _, s := range hubScreens() {
		if adminTitles[s.Title] {
			t.Errorf("admin screen %q must not appear in the user hub", s.Title)
		}
	}
}

// The user hub still keeps the day-to-day developer screens.
func TestUserHub_KeepsUserScreens(t *testing.T) {
	isolateHome(t)
	userTitles := map[string]bool{}
	for _, s := range hubScreens() {
		userTitles[s.Title] = true
	}
	for _, want := range []string{"Forge", "Tickets Board", "Pipelines"} {
		if !userTitles[want] {
			t.Errorf("user hub missing expected screen %q", want)
		}
	}
}

func TestAdminCmd_LaunchesHub(t *testing.T) {
	if adminCmd.Use != "admin" {
		t.Errorf("adminCmd.Use = %q, want admin", adminCmd.Use)
	}
	if adminCmd.RunE == nil {
		t.Error("adminCmd.RunE should launch the admin hub")
	}
}

// newAdminAppModel is seeded from the admin screens and never shows the
// first-use welcome (that belongs to the user hub).
func TestNewAdminAppModel_UsesAdminScreens(t *testing.T) {
	isolateHome(t)
	m := newAdminAppModel()
	if len(m.screens) != len(adminScreens()) {
		t.Errorf("admin app has %d screens, want %d", len(m.screens), len(adminScreens()))
	}
	if m.subtitle != "admin" {
		t.Errorf("admin app subtitle = %q, want admin", m.subtitle)
	}
	if m.active != nil {
		t.Error("admin hub should not open the first-use welcome")
	}
}

// activeModules marks the platform-administration modules as Admin, so
// wireModules hangs their commands off `armory admin`.
func TestAdminModules_FlaggedAdmin(t *testing.T) {
	isolateHome(t)
	wantAdmin := map[string]bool{
		"gatekeeper": true, "forge-runtimes": true, "audit": true,
		"orgs": true, "teams": true, "users": true, "roles": true,
		"permissions": true, "service-requests": true,
	}
	seen := map[string]bool{}
	for _, m := range activeModules() {
		if wantAdmin[m.Name] {
			seen[m.Name] = true
			if !m.Admin {
				t.Errorf("module %q should be flagged Admin", m.Name)
			}
		}
	}
	for name := range wantAdmin {
		if !seen[name] {
			t.Errorf("expected admin module %q to be registered/active", name)
		}
	}
}

package main

import "testing"

func viewsByName(vs []serviceView) map[string]serviceView {
	m := make(map[string]serviceView, len(vs))
	for _, v := range vs {
		m[v.Service] = v
	}
	return m
}

// A service the registry advertises as live must show enabled even with no builder
// baseline row — the registry is the source of truth for what conductor routes. This
// covers a non-core service registered out-of-band: it would otherwise be falsely shown
// disabled when builder seeds every catalog service default-OFF.
func TestMergeViews_RegistryLiveMarksEnabled(t *testing.T) {
	catalog := []catalogEntry{
		{Name: "containers", Description: "Container registry"},
		{Name: "notifications", Description: "Notifications"},
		{Name: "blueprints", Description: "IaC state"},
	}
	live := map[string]bool{"containers": true, "notifications": true} // registered out-of-band
	views := viewsByName(mergeViews(catalog, live, nil))               // no builder baseline rows

	for _, name := range []string{"containers", "notifications"} {
		v := views[name]
		if !v.Enabled {
			t.Errorf("%s: enabled = false, want true (live in registry)", name)
		}
		if v.Source != "registry" {
			t.Errorf("%s: source = %q, want registry", name, v.Source)
		}
		if v.Core {
			t.Errorf("%s: core = true, want false (not a control-plane service)", name)
		}
	}

	// A catalog service the registry does NOT advertise and with no baseline row stays
	// default-OFF — builder could deploy it but hasn't.
	if bp := views["blueprints"]; bp.Enabled || bp.Source != "catalog" {
		t.Errorf("blueprints = {enabled:%v source:%q}, want {false catalog}", bp.Enabled, bp.Source)
	}

	// Core control-plane services are always present and on.
	if gk := views["gatekeeper"]; !gk.Enabled || !gk.Core {
		t.Errorf("gatekeeper = {enabled:%v core:%v}, want {true true}", gk.Enabled, gk.Core)
	}
}

// An explicit baseline row is the admin's desired state and overrides the registry-live
// signal in both directions.
func TestMergeViews_BaselineRowOverridesRegistryLive(t *testing.T) {
	catalog := []catalogEntry{{Name: "containers"}, {Name: "blueprints"}}
	live := map[string]bool{"containers": true}

	views := viewsByName(mergeViews(catalog, live, []OrgService{
		{ServiceName: "containers", Enabled: false}, // admin disables a live service
		{ServiceName: "blueprints", Enabled: true},  // admin enables one not yet live
	}))

	if f := views["containers"]; f.Enabled || f.Source != "default" {
		t.Errorf("containers = {enabled:%v source:%q}, want {false default}", f.Enabled, f.Source)
	}
	if b := views["blueprints"]; !b.Enabled || b.Source != "default" {
		t.Errorf("blueprints = {enabled:%v source:%q}, want {true default}", b.Enabled, b.Source)
	}
}

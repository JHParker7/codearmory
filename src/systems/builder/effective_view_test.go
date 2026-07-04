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
		{Name: "hooks", Description: "Webhook receiver"},
		{Name: "outpost-gateway", Description: "Outpost gateway"},
		{Name: "myservice", Description: "Some service"},
	}
	live := map[string]bool{"hooks": true, "outpost-gateway": true} // registered out-of-band
	views := viewsByName(mergeViews(catalog, live, nil))            // no builder baseline rows

	for _, name := range []string{"hooks", "outpost-gateway"} {
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
	if ms := views["myservice"]; ms.Enabled || ms.Source != "catalog" {
		t.Errorf("myservice = {enabled:%v source:%q}, want {false catalog}", ms.Enabled, ms.Source)
	}

	// Core control-plane services are always present and on.
	if gk := views["gatekeeper"]; !gk.Enabled || !gk.Core {
		t.Errorf("gatekeeper = {enabled:%v core:%v}, want {true true}", gk.Enabled, gk.Core)
	}
}

// An explicit baseline row is the admin's desired state and overrides the registry-live
// signal in both directions.
func TestMergeViews_BaselineRowOverridesRegistryLive(t *testing.T) {
	catalog := []catalogEntry{{Name: "hooks"}, {Name: "outpost-gateway"}}
	live := map[string]bool{"hooks": true}

	views := viewsByName(mergeViews(catalog, live, []OrgService{
		{ServiceName: "hooks", Enabled: false},          // admin disables a live service
		{ServiceName: "outpost-gateway", Enabled: true}, // admin enables one not yet live
	}))

	if f := views["hooks"]; f.Enabled || f.Source != "default" {
		t.Errorf("hooks = {enabled:%v source:%q}, want {false default}", f.Enabled, f.Source)
	}
	if b := views["outpost-gateway"]; !b.Enabled || b.Source != "default" {
		t.Errorf("outpost-gateway = {enabled:%v source:%q}, want {true default}", b.Enabled, b.Source)
	}
}

// Coming-soon services (source not in this repo) are forced disabled and flagged no
// matter what the catalog, registry-live signal, or an admin's baseline row say — this
// repo's CI can't build their images yet, so they must never appear enabled.
func TestMergeViews_ComingSoonForcedDisabled(t *testing.T) {
	catalog := []catalogEntry{{Name: "chaos"}, {Name: "blueprints"}, {Name: "notifications"}}
	live := map[string]bool{"chaos": true} // even a live registration must not win

	views := viewsByName(mergeViews(catalog, live, []OrgService{
		{ServiceName: "blueprints", Enabled: true}, // even an explicit enable must not win
	}))

	for _, name := range []string{"chaos", "blueprints", "notifications"} {
		v := views[name]
		if v.Enabled {
			t.Errorf("%s: enabled = true, want false (coming soon)", name)
		}
		if !v.ComingSoon {
			t.Errorf("%s: coming_soon = false, want true", name)
		}
	}
}

package main

import (
	"sort"
	"testing"
)

func ptr(o OrgService) *OrgService { return &o }

func TestEnabledFrom(t *testing.T) {
	tests := []struct {
		name     string
		service  string
		override *OrgService
		dflt     *OrgService
		want     bool
	}{
		{"core always enabled", "gatekeeper", ptr(OrgService{Enabled: false}), ptr(OrgService{Enabled: false}), true},
		{"override wins (off)", "blueprints", ptr(OrgService{Enabled: false}), ptr(OrgService{Enabled: true}), false},
		{"override wins (on)", "blueprints", ptr(OrgService{Enabled: true}), ptr(OrgService{Enabled: false}), true},
		{"default fallback (off)", "blueprints", nil, ptr(OrgService{Enabled: false}), false},
		{"default fallback (on)", "blueprints", nil, ptr(OrgService{Enabled: true}), true},
		{"nothing configured is default-on", "blueprints", nil, nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := enabledFrom(tc.service, tc.override, tc.dflt); got != tc.want {
				t.Fatalf("enabledFrom(%q) = %v, want %v", tc.service, got, tc.want)
			}
		})
	}
}

func TestComputeDisabled(t *testing.T) {
	defaults := []OrgService{
		{ServiceName: "blueprints", Enabled: false}, // disabled in baseline
		{ServiceName: "containers", Enabled: true},  // enabled in baseline
		{ServiceName: "gatekeeper", Enabled: false}, // core: must be ignored
	}
	overrides := []OrgService{
		{ServiceName: "blueprints", Enabled: true},  // org re-enables blueprints
		{ServiceName: "tickets", Enabled: false},    // org disables tickets
		{ServiceName: "containers", Enabled: true},  // no change
	}

	got := computeDisabled(defaults, overrides)
	sort.Strings(got)
	want := []string{"tickets"}
	if len(got) != len(want) || (len(got) > 0 && got[0] != want[0]) {
		t.Fatalf("computeDisabled = %v, want %v", got, want)
	}
}

func TestComputeDisabled_DefaultOnlyInheritsBaseline(t *testing.T) {
	defaults := []OrgService{{ServiceName: "blueprints", Enabled: false}}
	got := computeDisabled(defaults, nil) // an org with no overrides inherits the baseline
	if len(got) != 1 || got[0] != "blueprints" {
		t.Fatalf("computeDisabled = %v, want [blueprints]", got)
	}
}

func TestComputeDisabled_CoreNeverDisabled(t *testing.T) {
	overrides := []OrgService{{ServiceName: "registry", Enabled: false}}
	if got := computeDisabled(nil, overrides); len(got) != 0 {
		t.Fatalf("computeDisabled = %v, want empty (core excluded)", got)
	}
}

// A catalog service with no baseline row must read DISABLED in the admin view: it is
// not deployed or registered until an admin enables it, so claiming enabled-by-default
// would contradict the (hidden) portal tab. Enabling/disabling rows then drive it.
func TestCatalogView_DefaultDisabledThenRowDrives(t *testing.T) {
	seed := newCatalogView(catalogEntry{Name: "blueprints", Description: "OpenTofu state"})
	if seed.Enabled {
		t.Fatalf("catalog seed for %q is enabled; want disabled by default", seed.Service)
	}
	if seed.Source != "catalog" || seed.Kind != kindPlatform {
		t.Fatalf("catalog seed = %+v, want source=catalog kind=platform", seed)
	}

	views := map[string]*serviceView{"blueprints": seed}
	applyRow(views, OrgService{ServiceName: "blueprints", Enabled: true, Kind: kindPlatform}, "default")
	if !views["blueprints"].Enabled || views["blueprints"].Source != "default" {
		t.Fatalf("after enabling row = %+v, want enabled from default", views["blueprints"])
	}

	applyRow(views, OrgService{ServiceName: "blueprints", Enabled: false, Kind: kindPlatform}, "default")
	if views["blueprints"].Enabled {
		t.Fatalf("after disabling row = %+v, want disabled", views["blueprints"])
	}
}


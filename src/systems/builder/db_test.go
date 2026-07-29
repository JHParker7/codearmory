package main

import (
	"encoding/json"
	"sort"
	"testing"
	"time"
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
		{ServiceName: "chaos", Enabled: true},       // enabled in baseline
		{ServiceName: "gatekeeper", Enabled: false}, // core: must be ignored
	}
	overrides := []OrgService{
		{ServiceName: "blueprints", Enabled: true}, // org re-enables blueprints
		{ServiceName: "argo", Enabled: false},      // org disables argo
		{ServiceName: "chaos", Enabled: true},      // no change
	}

	got := computeDisabled(defaults, overrides)
	sort.Strings(got)
	want := []string{"argo"}
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

// A config update must reach the driver as JSON TEXT, not as a Go map. GORM applies
// `serializer:json` only to struct writes, and upsertOrgService updates an explicit
// column map (so a false Enabled survives), so the map must be marshalled by hand.
// Regression: passing it through raw made pgx fail with "cannot find encode plan" on
// EVERY config change — even one writing back an identical value — which silently
// froze each service's config at whatever it was created with.
func TestOrgServiceUpdates_ConfigIsJSONText(t *testing.T) {
	now := time.Now().UTC()
	up, err := orgServiceUpdates(OrgService{
		Enabled: false, // must survive as an explicit column, not be dropped as a zero value
		Kind:    kindPlatform,
		Config:  map[string]any{"GIT_REPO_QUOTA_MB": "512", "LOG_LEVEL": "info"},
	}, now)
	if err != nil {
		t.Fatalf("orgServiceUpdates: %v", err)
	}

	raw, ok := up["config"].(string)
	if !ok {
		t.Fatalf("config is %T, want string — a map reaches pgx unserialized and the update fails", up["config"])
	}
	var back map[string]any
	if err := json.Unmarshal([]byte(raw), &back); err != nil {
		t.Fatalf("config is not valid JSON (%q): %v", raw, err)
	}
	if back["GIT_REPO_QUOTA_MB"] != "512" || back["LOG_LEVEL"] != "info" {
		t.Errorf("config round-trip lost values: %v", back)
	}
	if up["enabled"] != false {
		t.Errorf("enabled = %v, want false to be written explicitly", up["enabled"])
	}
	if up["updated_at"] != now {
		t.Errorf("updated_at = %v, want %v", up["updated_at"], now)
	}
}

// A cleared config must still encode rather than error, so an admin can remove every
// key from a service without the write failing. It encodes to SQL NULL, not to the
// text "null": GORM's JSONSerializer stores NULL for a nil map on the Create path, so
// writing the string here would leave one cleared config represented two ways
// depending on which path wrote it.
func TestOrgServiceUpdates_NilConfigEncodes(t *testing.T) {
	up, err := orgServiceUpdates(OrgService{Kind: kindPlatform}, time.Now().UTC())
	if err != nil {
		t.Fatalf("nil config must not error: %v", err)
	}
	if up["config"] != nil {
		t.Fatalf("config = %#v, want nil (SQL NULL, matching the Create path)", up["config"])
	}
}

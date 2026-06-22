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
		{"override wins (off)", "forge", ptr(OrgService{Enabled: false}), ptr(OrgService{Enabled: true}), false},
		{"override wins (on)", "forge", ptr(OrgService{Enabled: true}), ptr(OrgService{Enabled: false}), true},
		{"default fallback (off)", "forge", nil, ptr(OrgService{Enabled: false}), false},
		{"default fallback (on)", "forge", nil, ptr(OrgService{Enabled: true}), true},
		{"nothing configured is default-on", "forge", nil, nil, true},
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
		{ServiceName: "forge", Enabled: false},      // disabled in baseline
		{ServiceName: "workflows", Enabled: true},   // enabled in baseline
		{ServiceName: "gatekeeper", Enabled: false}, // core: must be ignored
	}
	overrides := []OrgService{
		{ServiceName: "forge", Enabled: true},     // org re-enables forge
		{ServiceName: "tickets", Enabled: false},  // org disables tickets
		{ServiceName: "workflows", Enabled: true}, // no change
	}

	got := computeDisabled(defaults, overrides)
	sort.Strings(got)
	want := []string{"tickets"}
	if len(got) != len(want) || (len(got) > 0 && got[0] != want[0]) {
		t.Fatalf("computeDisabled = %v, want %v", got, want)
	}
}

func TestComputeDisabled_DefaultOnlyInheritsBaseline(t *testing.T) {
	defaults := []OrgService{{ServiceName: "forge", Enabled: false}}
	got := computeDisabled(defaults, nil) // an org with no overrides inherits the baseline
	if len(got) != 1 || got[0] != "forge" {
		t.Fatalf("computeDisabled = %v, want [forge]", got)
	}
}

func TestComputeDisabled_CoreNeverDisabled(t *testing.T) {
	overrides := []OrgService{{ServiceName: "registry", Enabled: false}}
	if got := computeDisabled(nil, overrides); len(got) != 0 {
		t.Fatalf("computeDisabled = %v, want empty (core excluded)", got)
	}
}

func TestScopeFromPath(t *testing.T) {
	if got := scopeFromPath("default"); got != defaultOrgID {
		t.Fatalf("scopeFromPath(default) = %q, want %q", got, defaultOrgID)
	}
	if got := scopeFromPath("org-123"); got != "org-123" {
		t.Fatalf("scopeFromPath(org-123) = %q, want org-123", got)
	}
}

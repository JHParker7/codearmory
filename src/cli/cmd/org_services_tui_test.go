package cmd

import "testing"

// The org-services screen is an admin module backed by the builder service. We
// assert against the command var and the registry (not rootCmd): wireModules
// only runs in Execute(), so the registry is the source of truth at test time.
func TestOrgServicesModuleRegistered(t *testing.T) {
	var found *Module
	for i := range moduleRegistry {
		if moduleRegistry[i].Name == "org-services" {
			found = &moduleRegistry[i]
			break
		}
	}
	if found == nil {
		t.Fatal("expected an 'org-services' module to be registered")
	}
	if !found.Admin {
		t.Error("org-services must be an admin module")
	}
	if found.Command == nil || found.Command.Use != "org-services" {
		t.Error("org-services module should carry the org-services command")
	}
	if len(found.Screens) != 1 {
		t.Errorf("expected 1 hub screen, got %d", len(found.Screens))
	}
}

func TestOrgServicesCmdHasTUISub(t *testing.T) {
	var hasTUI bool
	for _, c := range orgServicesCmd.Commands() {
		if c.Use == "tui" {
			hasTUI = true
		}
	}
	if !hasTUI {
		t.Error("org-services command should have a 'tui' subcommand")
	}
}

func TestParseConfigJSON(t *testing.T) {
	cfg, err := parseConfigJSON(`{"a":1,"b":"x"}`)
	if err != nil {
		t.Fatalf("valid object: unexpected error %v", err)
	}
	if cfg["b"] != "x" {
		t.Errorf("expected b=x, got %v", cfg["b"])
	}

	if cfg, err := parseConfigJSON("   "); err != nil || cfg != nil {
		t.Errorf("blank should be (nil,nil), got (%v,%v)", cfg, err)
	}

	if _, err := parseConfigJSON(`[1,2,3]`); err == nil {
		t.Error("a JSON array is not an object; expected an error")
	}
	if _, err := parseConfigJSON(`{not json`); err == nil {
		t.Error("invalid JSON should error")
	}
}

func TestOSVConfigJSONRoundTrip(t *testing.T) {
	rec := osvRecord{"config": map[string]any{"k": "v"}}
	if got := osvConfigJSON(rec); got == "" {
		t.Fatal("expected non-empty JSON for a populated config")
	}
	if got := osvConfigJSON(osvRecord{}); got != "" {
		t.Errorf("absent config should render empty, got %q", got)
	}
}

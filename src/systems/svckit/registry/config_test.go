package registry

import (
	"testing"
)

func TestLoadConfigValid(t *testing.T) {
	data := []byte(`{
		"name": "blueprints",
		"description": "Manages OpenTofu state",
		"roles": [{"name": "admin", "description": "Full access"}],
		"endpoints": [{"method": "GET", "path": "/states", "action": "read", "resource": "state", "public": false, "description": "List states"}]
	}`)

	cfg, err := LoadConfig(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Name != "blueprints" {
		t.Errorf("Name = %q, want %q", cfg.Name, "blueprints")
	}
	if cfg.Description != "Manages OpenTofu state" {
		t.Errorf("Description = %q, want %q", cfg.Description, "Manages OpenTofu state")
	}
	if len(cfg.Roles) != 1 || cfg.Roles[0].Name != "admin" {
		t.Errorf("Roles = %v, want [{admin Full access}]", cfg.Roles)
	}
	if len(cfg.Endpoints) != 1 || cfg.Endpoints[0].Method != "GET" {
		t.Errorf("Endpoints = %v, want one GET endpoint", cfg.Endpoints)
	}
	if cfg.Endpoints[0].Public != false {
		t.Error("Endpoints[0].Public should be false")
	}
}

func TestLoadConfigPartialFields(t *testing.T) {
	data := []byte(`{"name": "minimal"}`)

	cfg, err := LoadConfig(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Name != "minimal" {
		t.Errorf("Name = %q, want %q", cfg.Name, "minimal")
	}
	if cfg.Roles != nil {
		t.Errorf("Roles should be nil, got %v", cfg.Roles)
	}
	if cfg.Endpoints != nil {
		t.Errorf("Endpoints should be nil, got %v", cfg.Endpoints)
	}
}

func TestLoadConfigInvalidJSON(t *testing.T) {
	_, err := LoadConfig([]byte(`{not valid json`))
	if err == nil {
		t.Error("expected error for invalid JSON, got nil")
	}
}

func TestLoadConfigEmpty(t *testing.T) {
	_, err := LoadConfig([]byte{})
	if err == nil {
		t.Error("expected error for empty input, got nil")
	}
}

func TestLoadConfigPublicEndpoint(t *testing.T) {
	data := []byte(`{
		"name": "svc",
		"endpoints": [{"method": "POST", "path": "/login", "public": true}]
	}`)
	cfg, err := LoadConfig(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Endpoints[0].Public {
		t.Error("expected Public=true")
	}
}

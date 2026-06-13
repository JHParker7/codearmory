package main

import (
	"encoding/json"
	"testing"
)

// ---------------------------------------------------------------------------
// Manifest parsing — the registry's core responsibility.
//
// loadManifest reads a file and json.Unmarshal's it into []manifestEntry
// before calling loadManifestEntry (which writes to the DB) per entry. These
// tests exercise the pure parse/transform layer — the JSON → manifestEntry
// decode plus the jsonbBytes / hashServiceKey normalisation helpers — with no
// database. The DB upsert in loadManifestEntry is covered by the integration
// suite (requireDB).
// ---------------------------------------------------------------------------

// manifestFixture mirrors the canonical infra/local/registry-manifest.json
// shape: one service with endpoints (routes), actions (with body_transforms +
// async), and default_grants.
const manifestFixture = `[
  {
    "name": "forge",
    "url": "http://forge:8083",
    "description": "Sandboxed execution",
    "forward_auth": false,
    "service_key": "forge-secret",
    "endpoints": [
      { "method": "POST",   "path": "/executions",      "action": "createExecution", "resource": "forge/executions"     },
      { "method": "GET",    "path": "/executions/{id}", "action": "getExecution",    "resource": "forge/executions/{id}" },
      { "method": "GET",    "path": "/healthz",         "action": "health",          "resource": "forge/health", "public": true }
    ],
    "actions": [
      {
        "name": "forge/run",
        "method": "POST",
        "path": "/executions",
        "body_transforms": [{ "from_key": "run", "to_key": "command", "wrap": ["bash", "-c"] }],
        "async": { "id_field": "execution_id", "poll_path": "/executions/{id}" }
      },
      { "name": "forge/plain", "method": "GET", "path": "/executions/{id}" }
    ],
    "default_grants": [
      {
        "grant_on": "user",
        "actions": ["createExecution", "listExecution", "getExecution"],
        "resources": ["{username}/forge/executions", "{username}/forge/executions/*"]
      }
    ]
  }
]`

// parseManifestFixture decodes the fixture the same way loadManifest does and
// returns the single entry, failing the test on a decode error.
func parseManifestFixture(t *testing.T) manifestEntry {
	t.Helper()
	var entries []manifestEntry
	if err := json.Unmarshal([]byte(manifestFixture), &entries); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	return entries[0]
}

// TestParseManifest_ServiceMetadata: top-level service fields (name, url,
// description, forward_auth, service_key) decode from JSON.
func TestParseManifest_ServiceMetadata(t *testing.T) {
	e := parseManifestFixture(t)
	if e.Name != "forge" {
		t.Errorf("name = %q, want %q", e.Name, "forge")
	}
	if e.URL != "http://forge:8083" {
		t.Errorf("url = %q, want %q", e.URL, "http://forge:8083")
	}
	if e.Description != "Sandboxed execution" {
		t.Errorf("description = %q, want %q", e.Description, "Sandboxed execution")
	}
	if e.ForwardAuth {
		t.Error("forward_auth = true, want false")
	}
	if e.ServiceKey != "forge-secret" {
		t.Errorf("service_key = %q, want %q", e.ServiceKey, "forge-secret")
	}
}

// TestParseManifest_EndpointsExtracted: endpoints (routes) parse with correct
// method, path, action, resource, and the public flag (defaulting to false).
func TestParseManifest_EndpointsExtracted(t *testing.T) {
	e := parseManifestFixture(t)
	if len(e.Endpoints) != 3 {
		t.Fatalf("endpoints = %d, want 3", len(e.Endpoints))
	}

	ep := e.Endpoints[0]
	if ep.Method != "POST" || ep.Path != "/executions" {
		t.Errorf("endpoint[0] method/path = %q %q, want POST /executions", ep.Method, ep.Path)
	}
	if ep.Action != "createExecution" || ep.Resource != "forge/executions" {
		t.Errorf("endpoint[0] action/resource = %q %q, want createExecution forge/executions", ep.Action, ep.Resource)
	}
	if ep.Public {
		t.Error("endpoint[0] public = true, want false (default)")
	}

	param := e.Endpoints[1]
	if param.Method != "GET" || param.Path != "/executions/{id}" || param.Resource != "forge/executions/{id}" {
		t.Errorf("endpoint[1] = %q %q %q, want GET /executions/{id} forge/executions/{id}", param.Method, param.Path, param.Resource)
	}

	if !e.Endpoints[2].Public {
		t.Error("endpoint[2] public = false, want true")
	}
}

// TestParseManifest_ActionsExtracted: actions parse with name/method/path; the
// optional body_transforms and async raw-JSON blobs are preserved when present
// and left empty when absent.
func TestParseManifest_ActionsExtracted(t *testing.T) {
	e := parseManifestFixture(t)
	if len(e.Actions) != 2 {
		t.Fatalf("actions = %d, want 2", len(e.Actions))
	}

	run := e.Actions[0]
	if run.Name != "forge/run" || run.Method != "POST" || run.Path != "/executions" {
		t.Errorf("action[0] = %q %q %q, want forge/run POST /executions", run.Name, run.Method, run.Path)
	}
	if len(run.BodyTransforms) == 0 {
		t.Error("action[0] body_transforms should be populated")
	}
	if len(run.Async) == 0 {
		t.Error("action[0] async should be populated")
	}

	plain := e.Actions[1]
	if plain.Name != "forge/plain" || plain.Method != "GET" || plain.Path != "/executions/{id}" {
		t.Errorf("action[1] = %q %q %q, want forge/plain GET /executions/{id}", plain.Name, plain.Method, plain.Path)
	}
	if len(plain.BodyTransforms) != 0 {
		t.Errorf("action[1] body_transforms = %q, want empty", plain.BodyTransforms)
	}
	if len(plain.Async) != 0 {
		t.Errorf("action[1] async = %q, want empty", plain.Async)
	}
}

// TestParseManifest_DefaultGrantsExtracted: default_grants parse with grant_on
// plus the full actions/resources string slices.
func TestParseManifest_DefaultGrantsExtracted(t *testing.T) {
	e := parseManifestFixture(t)
	if len(e.DefaultGrants) != 1 {
		t.Fatalf("default_grants = %d, want 1", len(e.DefaultGrants))
	}

	g := e.DefaultGrants[0]
	if g.GrantOn != "user" {
		t.Errorf("grant_on = %q, want %q", g.GrantOn, "user")
	}
	wantActions := []string{"createExecution", "listExecution", "getExecution"}
	if len(g.Actions) != len(wantActions) {
		t.Fatalf("grant actions = %d, want %d", len(g.Actions), len(wantActions))
	}
	for i, a := range wantActions {
		if g.Actions[i] != a {
			t.Errorf("grant action[%d] = %q, want %q", i, g.Actions[i], a)
		}
	}
	wantResources := []string{"{username}/forge/executions", "{username}/forge/executions/*"}
	if len(g.Resources) != len(wantResources) {
		t.Fatalf("grant resources = %d, want %d", len(g.Resources), len(wantResources))
	}
	for i, r := range wantResources {
		if g.Resources[i] != r {
			t.Errorf("grant resource[%d] = %q, want %q", i, g.Resources[i], r)
		}
	}
}

// TestParseManifest_MultipleEntries: a manifest is a JSON array; multiple
// services decode independently.
func TestParseManifest_MultipleEntries(t *testing.T) {
	const multi = `[
	  {"name": "a", "url": "http://a:1", "forward_auth": true},
	  {"name": "b", "url": "http://b:2", "forward_auth": false}
	]`
	var entries []manifestEntry
	if err := json.Unmarshal([]byte(multi), &entries); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	if entries[0].Name != "a" || !entries[0].ForwardAuth {
		t.Errorf("entry[0] = %q forward_auth=%v, want a true", entries[0].Name, entries[0].ForwardAuth)
	}
	if entries[1].Name != "b" || entries[1].ForwardAuth {
		t.Errorf("entry[1] = %q forward_auth=%v, want b false", entries[1].Name, entries[1].ForwardAuth)
	}
}

// ---------------------------------------------------------------------------
// jsonbBytes — normalises an action's raw-JSON blob into the []byte stored on
// the model. loadManifestEntry applies this to BodyTransforms and Async.
// ---------------------------------------------------------------------------

func TestJSONBBytes(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantNil bool
		want    string
	}{
		{"empty", "", true, ""},
		{"null literal", "null", true, ""},
		{"object", `{"k":"v"}`, false, `{"k":"v"}`},
		{"array", `[1,2]`, false, `[1,2]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := jsonbBytes(json.RawMessage(tt.in))
			if tt.wantNil {
				if got != nil {
					t.Errorf("jsonbBytes(%q) = %q, want nil", tt.in, got)
				}
				return
			}
			if string(got) != tt.want {
				t.Errorf("jsonbBytes(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestParseManifest_ActionAsyncRoundTrips checks the async blob parsed from the
// manifest survives the jsonbBytes normalisation that loadManifestEntry applies
// before storing it (and re-decodes to the same structure).
func TestParseManifest_ActionAsyncRoundTrips(t *testing.T) {
	e := parseManifestFixture(t)
	stored := jsonbBytes(e.Actions[0].Async)
	if len(stored) == 0 {
		t.Fatal("expected async to survive jsonbBytes")
	}
	var decoded map[string]any
	if err := json.Unmarshal(stored, &decoded); err != nil {
		t.Fatalf("stored async not valid JSON: %v", err)
	}
	if decoded["id_field"] != "execution_id" {
		t.Errorf("async id_field = %v, want execution_id", decoded["id_field"])
	}
}

// ---------------------------------------------------------------------------
// hashServiceKey — pure helper applied to each entry's service_key before the
// (DB) upsert. Empty in → empty out; non-empty → a verifiable bcrypt hash.
// ---------------------------------------------------------------------------

func TestHashServiceKey_Empty(t *testing.T) {
	h, err := hashServiceKey("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if h != "" {
		t.Errorf("hash = %q, want empty for empty key", h)
	}
}

func TestHashServiceKey_NonEmpty(t *testing.T) {
	h, err := hashServiceKey("forge-secret")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if h == "" {
		t.Fatal("expected non-empty hash")
	}
	if h == "forge-secret" {
		t.Fatal("hash must not equal the plaintext key")
	}
}

// TestParseAndHashManifestEntry exercises the full pre-DB path: decode an entry
// and hash its service_key, the two pure steps loadManifestEntry performs
// before touching the database.
func TestParseAndHashManifestEntry(t *testing.T) {
	e := parseManifestFixture(t)
	hash, err := hashServiceKey(e.ServiceKey)
	if err != nil {
		t.Fatalf("hashServiceKey: %v", err)
	}
	if hash == "" || hash == e.ServiceKey {
		t.Fatalf("expected bcrypt hash distinct from plaintext, got %q", hash)
	}
}

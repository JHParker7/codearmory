package main

// Unit spec for the Repo JSON contract (types.go), per ARCHITECTURE §4. The wire
// shape is part of the create/get contract that tests/test_repos.py depends on:
// Owner is the ownership filter and must never be serialized; http_url is derived
// and returned so clients can build clone commands.

import (
	"encoding/json"
	"testing"
)

func repoJSONKeys(t *testing.T, re Repo) map[string]any {
	t.Helper()
	raw, err := json.Marshal(re)
	if err != nil {
		t.Fatalf("marshal Repo: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal Repo JSON: %v", err)
	}
	return m
}

// Passes today: the stable id, the clone-URL segment (namespace), name,
// description and default_branch are all serialized.
func TestRepoJSON_ExposesPublicFields(t *testing.T) {
	m := repoJSONKeys(t, Repo{ID: "x", Namespace: "alice", Name: "widgets"})
	for _, k := range []string{"id", "namespace", "name", "description", "default_branch", "created_at", "updated_at"} {
		if _, ok := m[k]; !ok {
			t.Errorf("Repo JSON missing field %q", k)
		}
	}
}

// Passes today: Owner (the gatekeeper user_id used for the ownership filter) and
// the internal Shards routing field are json:"-" and must not leak (ARCHITECTURE
// §3, §4).
func TestRepoJSON_HidesInternalFields(t *testing.T) {
	m := repoJSONKeys(t, Repo{ID: "x", Owner: "user-1", Namespace: "alice", Name: "widgets"})
	if _, ok := m["owner"]; ok {
		t.Error("Repo JSON leaked owner (must be json:\"-\" — it is the ownership filter, not client data)")
	}
	if _, ok := m["shards"]; ok {
		t.Error("Repo JSON leaked shards (internal routing, must be json:\"-\")")
	}
}

// RED until Step 1: http_url is a derived field the git wire and UI depend on —
// the clone URL ending in /{namespace}/{name}.git (ARCHITECTURE §4, mirrors
// test_repos.py::test_create_response_has_git_fields). types.go must add a
// derived, unstored HTTPURL field (gorm:"-", json:"http_url") that api_repo.go
// populates from GIT_HTTP_BASE_URL / the request Host.
func TestRepoJSON_HasDerivedHTTPURL(t *testing.T) {
	m := repoJSONKeys(t, Repo{ID: "x", Namespace: "alice", Name: "widgets"})
	if _, ok := m["http_url"]; !ok {
		t.Error("Repo JSON missing http_url — add a derived, unstored HTTPURL field (ARCHITECTURE §4)")
	}
}

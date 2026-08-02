package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestBuilderResourceMatchesTheManifests guards the one drift that silently breaks every
// builder endpoint: conductor authorises against the resource the REGISTRY MANIFEST
// declares, while builder re-checks with its own builderResource constant. If the two
// diverge, conductor lets a request through and builder refuses it — or the manifest is
// loosened and builder's check becomes the only thing still holding.
//
// This exists because the constant and both manifests had to change together when
// platform-owned resources moved under codearmory/. Nothing but this test would notice
// if a later edit moved only one of them.
func TestBuilderResourceMatchesTheManifests(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	repo := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")

	for _, rel := range []string{
		"infra/helm/codearmory/files/registry-manifest.json",
		"infra/local/registry-manifest.json",
	} {
		raw, err := os.ReadFile(filepath.Join(repo, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		var services []struct {
			Name      string `json:"name"`
			Endpoints []struct {
				Method   string `json:"method"`
				Path     string `json:"path"`
				Resource string `json:"resource"`
			} `json:"endpoints"`
		}
		if err := json.Unmarshal(raw, &services); err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		found := 0
		for _, s := range services {
			if s.Name != "builder" {
				continue
			}
			for _, e := range s.Endpoints {
				found++
				if e.Resource != builderResource {
					t.Errorf("%s: %s %s declares resource %q, but builder checks %q",
						rel, e.Method, e.Path, e.Resource, builderResource)
				}
			}
		}
		if found == 0 {
			t.Errorf("%s: no builder endpoints found — this guard would pass vacuously", rel)
		}
	}
}

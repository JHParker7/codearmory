package main

import (
	"context"
	"strings"
	"testing"
)

// expectedNonCore is every non-core service the chart no longer deploys; builder must
// carry an embedded definition for each so it can deploy + register it at runtime.
var expectedNonCore = []string{
	"blueprints", "notifications",
	"outpost-gateway", "chaos", "argo",
	"gitea_integration",
}

func TestEmbeddedDefs_AllParseAndComplete(t *testing.T) {
	defs, err := loadEmbeddedDefs()
	if err != nil {
		t.Fatalf("loadEmbeddedDefs: %v", err)
	}
	if len(defs) != len(expectedNonCore) {
		t.Fatalf("got %d defs, want %d: %v", len(defs), len(expectedNonCore), keysOf(defs))
	}
	for _, name := range expectedNonCore {
		d, ok := defs[name]
		if !ok {
			t.Errorf("missing embedded def for %q", name)
			continue
		}
		if d.K8sName == "" || d.ImageRepo == "" || d.Port == 0 {
			t.Errorf("%s: incomplete def %+v", name, d)
		}
		if len(d.Endpoints) == 0 {
			t.Errorf("%s: no endpoints (manifest fragment missing)", name)
		}
		if len(d.DefaultGrants) == 0 {
			t.Errorf("%s: no default_grants", name)
		}
	}
}

func TestEmbeddedDefs_K8sNamesAreDNS1123(t *testing.T) {
	defs, _ := loadEmbeddedDefs()
	for name, d := range defs {
		// '_' is valid in a registry name but NOT in a k8s object name.
		if strings.Contains(d.K8sName, "_") {
			t.Errorf("%s: k8sName %q contains '_' (invalid DNS-1123)", name, d.K8sName)
		}
	}
}

func TestEmbeddedDefs_NoCoreServices(t *testing.T) {
	defs, _ := loadEmbeddedDefs()
	for name := range defs {
		if coreServices[name] {
			t.Errorf("core service %q must not be in the builder catalog", name)
		}
	}
}

// git_factory moved into this monorepo and became core: the chart deploys it and the
// registry manifest registers it. Both halves of that demotion have to hold together —
// a def left behind would have builder reconcile a workload the chart already owns,
// and a missing coreServices entry would let the set-service API toggle it off.
func TestGitFactoryIsCoreAndNotInTheCatalog(t *testing.T) {
	if !coreServices["codearmory_git_factory"] {
		t.Error("codearmory_git_factory must be core — it ships with the chart")
	}
	if _, ok := embeddedServiceDef("codearmory_git_factory"); ok {
		t.Error("codearmory_git_factory must have no builder def")
	}
}

func TestServiceCatalog_IncludesEmbeddedWithoutRegistry(t *testing.T) {
	// No registry configured → liveCatalog is empty, but the embedded defs must still
	// surface every non-core service as toggle-able.
	registryURL = ""
	getRegistryKey = nil
	got := serviceCatalog(context.Background())
	names := map[string]bool{}
	for _, e := range got {
		names[e.Name] = true
	}
	for _, want := range expectedNonCore {
		if !names[want] {
			t.Errorf("serviceCatalog missing embedded service %q", want)
		}
	}
}

func keysOf(m map[string]serviceDef) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

package cmd

import (
	"reflect"
	"testing"
)

func TestBuildStepWith_ForgeBuildImage_Nests(t *testing.T) {
	with, err := buildStepWith("forge/build-image", reader(map[string]string{
		withKeyPrefix + "destinations":    "reg.io/acme/app:1.0, reg.io/acme/app:latest",
		withKeyPrefix + "dockerfile":      "docker/Dockerfile",
		withKeyPrefix + "context":         "/workspace",
		withKeyPrefix + "build_args":      "VERSION=1.0 COMMIT=abc",
		withKeyPrefix + "target":          "prod",
		withKeyPrefix + "registry_secret": "my-registry",
		withKeyPrefix + "volumes":         "workspace:/src",
		withKeyPrefix + "runner_class":    "build-kata",
	}))
	if err != nil {
		t.Fatalf("buildStepWith: %v", err)
	}

	build, ok := with["build"].(map[string]any)
	if !ok {
		t.Fatalf("expected a nested build object, got %#v", with["build"])
	}
	if !reflect.DeepEqual(build["destinations"], []string{"reg.io/acme/app:1.0", "reg.io/acme/app:latest"}) {
		t.Errorf("destinations = %#v", build["destinations"])
	}
	if build["dockerfile"] != "docker/Dockerfile" || build["context"] != "/workspace" || build["target"] != "prod" {
		t.Errorf("build = %#v", build)
	}
	if !reflect.DeepEqual(build["build_args"], map[string]string{"VERSION": "1.0", "COMMIT": "abc"}) {
		t.Errorf("build_args = %#v", build["build_args"])
	}
	// The registry secret becomes a secret_ref on REGISTRY_AUTH.
	sr, ok := with["secret_refs"].(map[string]any)
	if !ok || sr[registryAuthEnv] != "secret:my-registry" {
		t.Errorf("secret_refs = %#v", with["secret_refs"])
	}
	// volumes + runner_class stay top-level (not under build).
	if _, ok := with["volumes"]; !ok {
		t.Errorf("volumes should be top-level: %#v", with)
	}
	if with["runner_class"] != "build-kata" {
		t.Errorf("runner_class = %v", with["runner_class"])
	}
	if _, ok := with["destinations"]; ok {
		t.Errorf("flat destinations should have moved under build: %#v", with)
	}
}

func TestBuildImage_RoundTripsThroughFlatten(t *testing.T) {
	orig := map[string]string{
		withKeyPrefix + "destinations":    "reg.io/acme/app:1.0",
		withKeyPrefix + "dockerfile":      "Dockerfile",
		withKeyPrefix + "build_args":      "K=V",
		withKeyPrefix + "registry_secret": "my-registry",
		withKeyPrefix + "runner_class":    "build-kata",
	}
	with, err := buildStepWith("forge/build-image", reader(orig))
	if err != nil {
		t.Fatalf("buildStepWith: %v", err)
	}
	// flattenStepWith should recover the flat form fields from the nested request.
	flat := flattenStepWith("forge/build-image", with)
	if got := formatList(flat["destinations"]); got != "reg.io/acme/app:1.0" {
		t.Errorf("destinations round-trip = %q", got)
	}
	if flat["dockerfile"] != "Dockerfile" {
		t.Errorf("dockerfile round-trip = %v", flat["dockerfile"])
	}
	if flat["registry_secret"] != "my-registry" {
		t.Errorf("registry_secret round-trip = %v", flat["registry_secret"])
	}
	if _, ok := flat["build"]; ok {
		t.Errorf("build object should have been flattened away: %#v", flat)
	}
	if _, ok := flat["secret_refs"]; ok {
		t.Errorf("secret_refs should have been consumed into registry_secret: %#v", flat)
	}
}

func TestSplitList_FieldForDestinations(t *testing.T) {
	with, err := buildStepWith("forge/build-image", reader(map[string]string{
		withKeyPrefix + "destinations": "a:1,  b:2 , c:3",
		withKeyPrefix + "runner_class": "build-kata",
	}))
	if err != nil {
		t.Fatalf("buildStepWith: %v", err)
	}
	build := with["build"].(map[string]any)
	if !reflect.DeepEqual(build["destinations"], []string{"a:1", "b:2", "c:3"}) {
		t.Errorf("destinations = %#v", build["destinations"])
	}
}

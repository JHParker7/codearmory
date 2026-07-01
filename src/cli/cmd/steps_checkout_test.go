package cmd

import (
	"reflect"
	"testing"
)

// forge/git-clone nests path/ref/depth under a `checkout` object, keeps image and
// volumes top-level, and defaults `run` to a no-op the checkout prologue weaves into.
func TestBuildStepWith_GitCloneNestsCheckout(t *testing.T) {
	with, err := buildStepWith("forge/git-clone", reader(map[string]string{
		withKeyPrefix + "image":   "alpine/git",
		withKeyPrefix + "volumes": "workspace:/src",
		withKeyPrefix + "path":    "app",
		withKeyPrefix + "ref":     "main",
		withKeyPrefix + "depth":   "1",
	}))
	if err != nil {
		t.Fatalf("buildStepWith: %v", err)
	}
	if with["image"] != "alpine/git" {
		t.Errorf("image = %v, want alpine/git", with["image"])
	}
	if with["run"] != "true" {
		t.Errorf("run = %v, want default \"true\"", with["run"])
	}
	// path/ref/depth must be lifted out of the top level into checkout.
	for _, k := range checkoutFields {
		if _, ok := with[k]; ok {
			t.Errorf("%q should be nested under checkout, still top-level", k)
		}
	}
	checkout, ok := with["checkout"].(map[string]any)
	if !ok {
		t.Fatalf("checkout = %#v, want map", with["checkout"])
	}
	if checkout["path"] != "app" || checkout["ref"] != "main" || checkout["depth"] != int64(1) {
		t.Errorf("checkout = %#v", checkout)
	}
	want := []any{map[string]any{
		"workflow_id": volumeRunIDRef,
		"name":        "workspace",
		"mount_path":  "/src",
		"workdir":     true,
	}}
	if !reflect.DeepEqual(with["volumes"], want) {
		t.Errorf("volumes = %#v, want %#v", with["volumes"], want)
	}
}

// With no path/ref/depth, checkout is still present (empty) so forge runs the clone
// prologue, and a post-clone command overrides the default no-op run.
func TestBuildStepWith_GitCloneEmptyCheckoutAndCustomRun(t *testing.T) {
	with, err := buildStepWith("forge/git-clone", reader(map[string]string{
		withKeyPrefix + "image":   "alpine/git",
		withKeyPrefix + "volumes": "workspace",
		withKeyPrefix + "run":     "git submodule update --init",
	}))
	if err != nil {
		t.Fatalf("buildStepWith: %v", err)
	}
	checkout, ok := with["checkout"].(map[string]any)
	if !ok || len(checkout) != 0 {
		t.Errorf("checkout = %#v, want empty map", with["checkout"])
	}
	if with["run"] != "git submodule update --init" {
		t.Errorf("run = %v, want the custom command", with["run"])
	}
}

// image and volumes are required.
func TestBuildStepWith_GitCloneRequiresImageAndVolume(t *testing.T) {
	if _, err := buildStepWith("forge/git-clone", reader(map[string]string{
		withKeyPrefix + "volumes": "workspace",
	})); err == nil {
		t.Errorf("expected error when image is missing")
	}
	if _, err := buildStepWith("forge/git-clone", reader(map[string]string{
		withKeyPrefix + "image": "alpine/git",
	})); err == nil {
		t.Errorf("expected error when volume is missing")
	}
}

// A stored git-clone step round-trips through flattenStepWith back to the flat form
// fields (checkout lifted to path/ref/depth), so the edit form pre-populates.
func TestGitCloneFlattenRoundTrip(t *testing.T) {
	orig := map[string]string{
		withKeyPrefix + "image":   "alpine/git",
		withKeyPrefix + "volumes": "workspace:/src",
		withKeyPrefix + "path":    "app",
		withKeyPrefix + "ref":     "release",
		withKeyPrefix + "depth":   "0",
	}
	with, err := buildStepWith("forge/git-clone", reader(orig))
	if err != nil {
		t.Fatalf("buildStepWith: %v", err)
	}
	flat := flattenStepWith("forge/git-clone", with)
	if _, ok := flat["checkout"]; ok {
		t.Errorf("checkout should be lifted away on flatten, got %v", flat["checkout"])
	}
	if flat["path"] != "app" || flat["ref"] != "release" || flat["depth"] != int64(0) {
		t.Errorf("flattened = %#v", flat)
	}
}

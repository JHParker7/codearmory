package cmd

import (
	"reflect"
	"testing"
)

// reader turns a map of prefixed form values into the valueOf func buildStepWith wants.
func reader(vals map[string]string) func(string) string {
	return func(k string) string { return vals[k] }
}

func TestBuildStepWith_CreateVolumeDefaultsRunID(t *testing.T) {
	with, err := buildStepWith("forge/create-volume", reader(map[string]string{
		withKeyPrefix + "name":       "workspace",
		withKeyPrefix + "size_mb":    "1024",
		withKeyPrefix + "medium":     "memory",
		withKeyPrefix + "mount_path": "/workspace",
	}))
	if err != nil {
		t.Fatalf("buildStepWith: %v", err)
	}
	if with["workflow_id"] != volumeRunIDRef {
		t.Errorf("workflow_id = %v, want %q", with["workflow_id"], volumeRunIDRef)
	}
	if with["name"] != "workspace" || with["size_mb"] != int64(1024) || with["medium"] != "memory" {
		t.Errorf("create-volume with = %+v", with)
	}
}

func TestBuildStepWith_CreateVolumeExplicitWorkflowIDWins(t *testing.T) {
	with, err := buildStepWith("forge/create-volume", reader(map[string]string{
		withKeyPrefix + rawWithKey: `{"workflow_id":"custom-scope"}`,
	}))
	if err != nil {
		t.Fatalf("buildStepWith: %v", err)
	}
	if with["workflow_id"] != "custom-scope" {
		t.Errorf("explicit workflow_id should win, got %v", with["workflow_id"])
	}
}

func TestBuildStepWith_ForgeRunVolumeAttach(t *testing.T) {
	with, err := buildStepWith("forge/run", reader(map[string]string{
		withKeyPrefix + "image":   "alpine:3.19",
		withKeyPrefix + "run":     "make build",
		withKeyPrefix + "volumes": "workspace:/src",
	}))
	if err != nil {
		t.Fatalf("buildStepWith: %v", err)
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

func TestBuildStepWith_ForgeRunNoVolumeWhenEmpty(t *testing.T) {
	with, err := buildStepWith("forge/run", reader(map[string]string{
		withKeyPrefix + "image": "alpine:3.19",
		withKeyPrefix + "run":   "make build",
	}))
	if err != nil {
		t.Fatalf("buildStepWith: %v", err)
	}
	if _, ok := with["volumes"]; ok {
		t.Errorf("volumes should be absent when the attach field is empty, got %v", with["volumes"])
	}
}

func TestVolumeAttachRoundTrip(t *testing.T) {
	cases := map[string]string{
		"workspace":      "workspace",      // default mount → name only
		"workspace:/src": "workspace:/src", // custom mount preserved
		"cache:/data":    "cache:/data",
	}
	for in, want := range cases {
		got := volumeAttachString(volumeAttachWith(in))
		if got != want {
			t.Errorf("round-trip %q → %q, want %q", in, got, want)
		}
	}
	// A bare mountless name defaults to /workspace internally.
	v := volumeAttachWith("workspace")
	if m := v[0].(map[string]any); m["mount_path"] != defaultMountPath || m["workdir"] != true {
		t.Errorf("attach = %#v, want mount %s + workdir", m, defaultMountPath)
	}
}

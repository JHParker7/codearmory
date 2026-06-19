package cmd

import "testing"

// buildWith's forge/run convenience flags include --runner-class, which pins a
// forge runner class. When unset the key is omitted so forge applies its default.
func TestBuildWith_ForgeRun_RunnerClass(t *testing.T) {
	with, err := buildWith("forge/run", "", "ubuntu:22.04", "go test ./...", "large", nil)
	if err != nil {
		t.Fatalf("buildWith error: %v", err)
	}
	if with["runner_class"] != "large" {
		t.Errorf("runner_class = %v, want large", with["runner_class"])
	}
}

func TestBuildWith_ForgeRun_OmitsRunnerClassWhenUnset(t *testing.T) {
	with, err := buildWith("forge/run", "", "ubuntu:22.04", "go test ./...", "", nil)
	if err != nil {
		t.Fatalf("buildWith error: %v", err)
	}
	if _, ok := with["runner_class"]; ok {
		t.Errorf("runner_class should be omitted when unset, got %v", with["runner_class"])
	}
}

// The --runner-class flag overlays onto a --with JSON base just like --image/--run.
func TestBuildWith_ForgeRun_RunnerClassOverlaysWithJSON(t *testing.T) {
	with, err := buildWith("forge/run", `{"image":"ubuntu:22.04","run":"true"}`, "", "", "xlarge", nil)
	if err != nil {
		t.Fatalf("buildWith error: %v", err)
	}
	if with["runner_class"] != "xlarge" {
		t.Errorf("runner_class = %v, want xlarge", with["runner_class"])
	}
	if with["image"] != "ubuntu:22.04" {
		t.Errorf("image = %v, want ubuntu:22.04 (base preserved)", with["image"])
	}
}

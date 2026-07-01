package main

import (
	"strings"
	"testing"
)

func TestVolumeResourceName(t *testing.T) {
	// Deterministic: same handle → same name.
	a := volumeResourceName("run-123", "workspace")
	b := volumeResourceName("run-123", "workspace")
	if a != b {
		t.Fatalf("resource name is not deterministic: %q vs %q", a, b)
	}
	// Distinct workflows do not collide even with the same logical name.
	if c := volumeResourceName("run-999", "workspace"); c == a {
		t.Errorf("different workflows produced the same resource name %q", c)
	}
	// DNS-1123 label: lowercase alnum + dashes, starts/ends alnum, ≤63 chars.
	if !volumeNameRe.MatchString(a) {
		t.Errorf("resource name %q is not a DNS-1123 label", a)
	}
	if len(a) > 63 {
		t.Errorf("resource name %q exceeds 63 chars", a)
	}
	if !strings.HasPrefix(a, "fv-") {
		t.Errorf("resource name %q missing fv- prefix", a)
	}
}

func TestNormalizeCreateVolume(t *testing.T) {
	initVolumeConfig() // sets the caps this exercises

	t.Run("defaults applied", func(t *testing.T) {
		req := createVolumeRequest{WorkflowID: "run-1"}
		name, err := normalizeCreateVolume(&req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if req.Name != "workspace" {
			t.Errorf("name default = %q, want workspace", req.Name)
		}
		if req.Medium != mediumMemory {
			t.Errorf("medium default = %q, want %q", req.Medium, mediumMemory)
		}
		if req.MountPath != defaultVolumeMountPath {
			t.Errorf("mount_path default = %q, want %q", req.MountPath, defaultVolumeMountPath)
		}
		if req.SizeMB != minVolumeMB {
			t.Errorf("size floored to %d, got %d", minVolumeMB, req.SizeMB)
		}
		if name != volumeResourceName("run-1", "workspace") {
			t.Errorf("resource name = %q", name)
		}
	})

	tests := []struct {
		name    string
		req     createVolumeRequest
		wantErr string
	}{
		{"bad workflow id", createVolumeRequest{WorkflowID: "no spaces!"}, "workflow_id"},
		{"bad name", createVolumeRequest{WorkflowID: "r1", Name: "Bad_Name"}, "name"},
		{"name too long", createVolumeRequest{WorkflowID: "r1", Name: strings.Repeat("a", maxVolumeNameLen+1)}, "name"},
		{"bad medium", createVolumeRequest{WorkflowID: "r1", Medium: "ssd"}, "medium"},
		{"relative mount path", createVolumeRequest{WorkflowID: "r1", MountPath: "rel/path"}, "mount_path"},
		{"parent escape mount path", createVolumeRequest{WorkflowID: "r1", MountPath: "/a/../b"}, ".."},
		{"reserved mount path", createVolumeRequest{WorkflowID: "r1", MountPath: "/tmp"}, "reserved"},
		{"over per-volume cap", createVolumeRequest{WorkflowID: "r1", SizeMB: maxVolumeMB + 1}, "per-volume limit"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.req
			if _, err := normalizeCreateVolume(&req); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestNormalizeCreateVolume_Disabled(t *testing.T) {
	saved := maxWorkflowVolumeMB
	maxWorkflowVolumeMB = 0
	defer func() { maxWorkflowVolumeMB = saved }()

	req := createVolumeRequest{WorkflowID: "run-1"}
	if _, err := normalizeCreateVolume(&req); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("want disabled error, got %v", err)
	}
}

func TestValidateVolumeMounts(t *testing.T) {
	tests := []struct {
		name    string
		mounts  []VolumeMount
		wantErr string
	}{
		{"empty is fine", nil, ""},
		{"valid single", []VolumeMount{{WorkflowID: "r1", Name: "ws"}}, ""},
		{"bad workflow id", []VolumeMount{{WorkflowID: "bad id", Name: "ws"}}, "workflow_id"},
		{"bad name", []VolumeMount{{WorkflowID: "r1", Name: "Bad"}}, "name"},
		{"duplicate mount path", []VolumeMount{
			{WorkflowID: "r1", Name: "a", MountPath: "/workspace"},
			{WorkflowID: "r1", Name: "b", MountPath: "/workspace"},
		}, "duplicate mount_path"},
		{"two workdirs", []VolumeMount{
			{WorkflowID: "r1", Name: "a", MountPath: "/a", Workdir: true},
			{WorkflowID: "r1", Name: "b", MountPath: "/b", Workdir: true},
		}, "at most one"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateVolumeMounts(tc.mounts)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestValidateVolumeMounts_DefaultsMountPath(t *testing.T) {
	mounts := []VolumeMount{{WorkflowID: "r1", Name: "ws"}}
	if err := validateVolumeMounts(mounts); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mounts[0].MountPath != defaultVolumeMountPath {
		t.Errorf("mount path default = %q, want %q", mounts[0].MountPath, defaultVolumeMountPath)
	}
}

func TestResolveVolumeMounts(t *testing.T) {
	exec := Execution{Volumes: []VolumeMount{
		{WorkflowID: "run-1", Name: "repo", MountPath: "/src", Workdir: true},
		{WorkflowID: "run-1", Name: "cache"}, // default path
	}}
	got := resolveVolumeMounts(exec)
	if len(got) != 2 {
		t.Fatalf("want 2 mounts, got %d", len(got))
	}
	if got[0].resourceName != volumeResourceName("run-1", "repo") || got[0].mountPath != "/src" || !got[0].workdir {
		t.Errorf("mount[0] wrong: %+v", got[0])
	}
	if got[1].mountPath != defaultVolumeMountPath {
		t.Errorf("mount[1] default path = %q, want %q", got[1].mountPath, defaultVolumeMountPath)
	}
}

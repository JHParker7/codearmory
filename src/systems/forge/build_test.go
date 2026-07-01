package main

import (
	"strings"
	"testing"
)

func TestInitBuildConfig_DefaultBuilderImage(t *testing.T) {
	initBuildConfig()
	if builderImage == "" || !strings.Contains(builderImage, "kaniko") {
		t.Errorf("default builder image = %q, want the kaniko executor", builderImage)
	}
}

func TestValidateBuild(t *testing.T) {
	refs := map[string]string{"REGISTRY_AUTH": "secret:reg"}
	tests := []struct {
		name    string
		spec    *BuildSpec
		refs    map[string]string
		env     map[string]string
		wantErr string
	}{
		{name: "nil is not a build", spec: nil},
		{name: "push with creds ok", spec: &BuildSpec{Destinations: []string{"reg.io/acme/app:1"}}, refs: refs},
		{name: "no-push needs no creds", spec: &BuildSpec{NoPush: true}},
		{name: "custom auth env from env", spec: &BuildSpec{Destinations: []string{"reg.io/a:1"}, RegistryAuth: "DOCKERCFG"}, env: map[string]string{"DOCKERCFG": "{}"}},

		{name: "push without destinations", spec: &BuildSpec{}, wantErr: "destinations is required"},
		{name: "push without creds", spec: &BuildSpec{Destinations: []string{"reg.io/a:1"}}, wantErr: "REGISTRY_AUTH is not set"},
		{name: "relative context rejected", spec: &BuildSpec{NoPush: true, Context: "rel"}, wantErr: "build.context"},
		{name: "context parent escape", spec: &BuildSpec{NoPush: true, Context: "/a/../b"}, wantErr: "build.context"},
		{name: "absolute dockerfile rejected", spec: &BuildSpec{NoPush: true, Dockerfile: "/etc/passwd"}, wantErr: "build.dockerfile"},
		{name: "dockerfile parent escape", spec: &BuildSpec{NoPush: true, Dockerfile: "../Dockerfile"}, wantErr: "build.dockerfile"},
		{name: "bad destination", spec: &BuildSpec{Destinations: []string{"bad ref!"}}, refs: refs, wantErr: "not a valid image reference"},
		{name: "bad build-arg key", spec: &BuildSpec{NoPush: true, BuildArgs: map[string]string{"bad-key": "v"}}, wantErr: "build_args key"},
		{name: "bad target", spec: &BuildSpec{NoPush: true, Target: "bad target"}, wantErr: "build.target"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateBuild(tc.spec, tc.refs, tc.env)
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

func TestKanikoCommand(t *testing.T) {
	initBuildConfig()
	spec := &BuildSpec{
		Destinations: []string{"reg.io/acme/app:1.2", "reg.io/acme/app:latest"},
		BuildArgs:    map[string]string{"VERSION": "1.2", "COMMIT": "abc"},
		Target:       "prod",
	}
	cmd := kanikoCommand(spec)
	if cmd[0] != "/busybox/sh" || cmd[1] != "-c" {
		t.Fatalf("kaniko shell wrapper = %v", cmd[:2])
	}
	s := cmd[2]
	for _, want := range []string{
		"/kaniko/executor",
		"--context=dir://'/workspace'",
		"--dockerfile='Dockerfile'",
		"--destination='reg.io/acme/app:1.2'",
		"--destination='reg.io/acme/app:latest'",
		"--build-arg 'COMMIT=abc'", // sorted before VERSION
		"--build-arg 'VERSION=1.2'",
		"--target='prod'",
		"/kaniko/.docker/config.json", // writes auth (pushing)
	} {
		if !strings.Contains(s, want) {
			t.Errorf("kaniko script missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "--no-push") {
		t.Errorf("should push when destinations set:\n%s", s)
	}
}

func TestKanikoCommand_NoPush(t *testing.T) {
	initBuildConfig()
	s := kanikoCommand(&BuildSpec{NoPush: true})[2]
	if !strings.Contains(s, "--no-push") {
		t.Errorf("expected --no-push:\n%s", s)
	}
	if strings.Contains(s, "config.json") {
		t.Errorf("no-push should not write registry auth:\n%s", s)
	}
}

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

// Kaniko's own --image-download-retry default is 0, so one transient TOOMANYREQUESTS
// from a public registry kills a build whose code is fine. Forge must not inherit that.
func TestKanikoCommand_RetriesRegistryOperations(t *testing.T) {
	initBuildConfig()

	// A no-push build still PULLS its base image, which is the throttle-exposed step.
	s := kanikoCommand(&BuildSpec{NoPush: true})[2]
	if !strings.Contains(s, "--image-download-retry=3") {
		t.Errorf("a no-push build must still retry its base-image pull:\n%s", s)
	}
	if strings.Contains(s, "--push-retry") {
		t.Errorf("a no-push build has nothing to push:\n%s", s)
	}

	// A pushing build is exposed at both ends.
	s = kanikoCommand(&BuildSpec{Destinations: []string{"reg.io/a/b:1"}})[2]
	for _, want := range []string{"--image-download-retry=3", "--push-retry=3"} {
		if !strings.Contains(s, want) {
			t.Errorf("pushing build missing %q:\n%s", want, s)
		}
	}
}

func TestBuildRetries_OperatorOverride(t *testing.T) {
	t.Setenv("FORGE_BUILD_RETRIES", "7")
	if got := buildRetries(); got != 7 {
		t.Errorf("buildRetries = %d, want the env override 7", got)
	}
	if s := kanikoCommand(&BuildSpec{NoPush: true})[2]; !strings.Contains(s, "--image-download-retry=7") {
		t.Errorf("override not threaded into the command:\n%s", s)
	}

	// 0 means "restore kaniko's default": emit no flag rather than an explicit =0.
	t.Setenv("FORGE_BUILD_RETRIES", "0")
	if s := kanikoCommand(&BuildSpec{NoPush: true})[2]; strings.Contains(s, "--image-download-retry") {
		t.Errorf("0 must omit the flag entirely, not pass =0:\n%s", s)
	}

	// A garbage value must not silently disable retries.
	t.Setenv("FORGE_BUILD_RETRIES", "not-a-number")
	if got := buildRetries(); got != defaultBuildRetries {
		t.Errorf("buildRetries = %d, want fallback to %d on an unparseable value", got, defaultBuildRetries)
	}
}

// A mirror is operator config: it must reach the kaniko command, and it must never be
// something a caller can set (a build pointing its own base pull at an arbitrary host).
func TestKanikoCommand_RegistryMirror(t *testing.T) {
	initBuildConfig()

	// Unset: no flag at all, so an operator who configures nothing gets today's behaviour.
	if s := kanikoCommand(&BuildSpec{NoPush: true})[2]; strings.Contains(s, "--registry-map") {
		t.Errorf("no mirror configured must emit no --registry-map:\n%s", s)
	}

	t.Setenv("FORGE_REGISTRY_MAP", "public.ecr.aws=mirror.svc:5000")
	t.Setenv("FORGE_INSECURE_REGISTRIES", "mirror.svc:5000")
	s := kanikoCommand(&BuildSpec{NoPush: true})[2]
	for _, want := range []string{
		"--registry-map='public.ecr.aws=mirror.svc:5000'",
		"--insecure-registry='mirror.svc:5000'", // an in-cluster mirror has no TLS
	} {
		if !strings.Contains(s, want) {
			t.Errorf("kaniko script missing %q:\n%s", want, s)
		}
	}
	// Fallback to the original registry must stay ON: a cold or broken mirror should
	// degrade to a direct pull, not fail every build in the platform.
	if strings.Contains(s, "--skip-default-registry-fallback") {
		t.Error("must not disable registry fallback: a mirror miss has to fall back to the origin")
	}
}

func TestInsecureRegistries_Parsing(t *testing.T) {
	t.Setenv("FORGE_INSECURE_REGISTRIES", " a.io:5000 , , b.io ")
	got := insecureRegistries()
	if len(got) != 2 || got[0] != "a.io:5000" || got[1] != "b.io" {
		t.Errorf("insecureRegistries = %#v, want trimmed entries with blanks dropped", got)
	}
	t.Setenv("FORGE_INSECURE_REGISTRIES", "")
	if got := insecureRegistries(); got != nil {
		t.Errorf("unset must yield nil, got %#v", got)
	}
}

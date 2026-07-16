package main

import (
	"strings"
	"testing"
)

func workdirMounts() []VolumeMount {
	return []VolumeMount{{Name: "workspace", MountPath: "/workspace", Workdir: true}}
}

func TestValidateArtifact(t *testing.T) {
	refs := map[string]string{"ARTIFACTS_TOKEN": "secret:tok"}
	ok := &ArtifactSpec{Mode: "save", Name: "gocache", Path: "cache"}
	if err := validateArtifact(ok, refs, nil, workdirMounts()); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}

	cases := []struct {
		name   string
		spec   *ArtifactSpec
		refs   map[string]string
		mounts []VolumeMount
		want   string
	}{
		{"bad mode", &ArtifactSpec{Mode: "sync", Name: "c"}, refs, workdirMounts(), "mode must be"},
		{"no name", &ArtifactSpec{Mode: "save"}, refs, workdirMounts(), "name is required"},
		// The name lands in a URL; a separator would address a different artifact.
		{"name with slash", &ArtifactSpec{Mode: "save", Name: "a/b"}, refs, workdirMounts(), "path separator"},
		{"name with query", &ArtifactSpec{Mode: "save", Name: "a?x=1"}, refs, workdirMounts(), "path separator"},
		// The path is interpolated into a shell script.
		{"path escapes quoting", &ArtifactSpec{Mode: "save", Name: "c", Path: "x; rm -rf /"}, refs, workdirMounts(), "shell metacharacters"},
		{"path traversal", &ArtifactSpec{Mode: "save", Name: "c", Path: "../etc"}, refs, workdirMounts(), ".."},
		// Nowhere to read from or write to.
		{"no workdir volume", &ArtifactSpec{Mode: "save", Name: "c"}, refs,
			[]VolumeMount{{Name: "w", MountPath: "/w"}}, "workdir: true"},
		// A helper with no token would 401 from inside the sandbox — fail at submit.
		{"token wired nowhere", &ArtifactSpec{Mode: "save", Name: "c"}, map[string]string{}, workdirMounts(), "secret_refs"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateArtifact(tc.spec, tc.refs, nil, tc.mounts)
			if err == nil {
				t.Fatalf("want an error mentioning %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}

	// A plain env var is an acceptable alternative to a secret_ref.
	if err := validateArtifact(&ArtifactSpec{Mode: "restore", Name: "c"}, nil,
		map[string]string{"ARTIFACTS_TOKEN": "x"}, workdirMounts()); err != nil {
		t.Errorf("a token supplied via env must be accepted: %v", err)
	}
	// A custom token env is honoured.
	if err := validateArtifact(&ArtifactSpec{Mode: "save", Name: "c", TokenEnv: "MY_TOK"},
		map[string]string{"MY_TOK": "secret:t"}, nil, workdirMounts()); err != nil {
		t.Errorf("custom token_env rejected: %v", err)
	}
}

func TestArtifactCommand_Save(t *testing.T) {
	t.Setenv("FORGE_ARTIFACTS_URL", "http://artifacts:8097")
	cmd := artifactCommand(&ArtifactSpec{Mode: "save", Name: "gocache", Path: "cache"}, workdirMounts())
	if len(cmd) != 3 || cmd[0] != "sh" || cmd[1] != "-c" {
		t.Fatalf("cmd = %v, want a sh -c script", cmd)
	}
	s := cmd[2]
	for _, want := range []string{
		"cd /workspace",              // runs in the attached volume
		"tar czf - cache",            // archives the requested path
		"-X PUT",                     // uploads
		"--data-binary @-",           // STREAMS: a multi-GB cache never lands on disk
		"Authorization: Bearer $ARTIFACTS_TOKEN",
		"http://artifacts:8097/artifacts/gocache",
		"--fail", // curl is silent on 4xx by default: without this a quota rejection would look like success
	} {
		if !strings.Contains(s, want) {
			t.Errorf("save script missing %q:\n%s", want, s)
		}
	}
}

func TestArtifactCommand_Restore(t *testing.T) {
	t.Setenv("FORGE_ARTIFACTS_URL", "http://artifacts:8097")
	cmd := artifactCommand(&ArtifactSpec{Mode: "restore", Name: "gocache", Path: "cache"}, workdirMounts())
	s := cmd[2]
	for _, want := range []string{
		"/artifacts/gocache/content", // the download endpoint, not the metadata one
		"tar xzf",                    // extracts
		"-C cache",                   // into the requested path
		`if [ "$code" != "200" ]`,    // a non-200 fails the step
	} {
		if !strings.Contains(s, want) {
			t.Errorf("restore script missing %q:\n%s", want, s)
		}
	}
	// Not optional: a missing artifact must FAIL, so a typo'd name is not silently ignored.
	if strings.Contains(s, `"404"`) {
		t.Error("a non-optional restore must not treat 404 as success")
	}
}

// The first run of a caching pipeline has nothing stored yet; an optional restore
// must be a no-op rather than failing the run.
func TestArtifactCommand_OptionalRestoreSkipsWhenAbsent(t *testing.T) {
	cmd := artifactCommand(&ArtifactSpec{Mode: "restore", Name: "c", Optional: true}, workdirMounts())
	s := cmd[2]
	if !strings.Contains(s, `if [ "$code" = "404" ]`) || !strings.Contains(s, "exit 0") {
		t.Errorf("optional restore must exit 0 on a missing artifact:\n%s", s)
	}
}

// The store URL is forge config, never a request field: a sandbox must not be able to
// aim a workspace upload at an arbitrary host.
func TestArtifactsURL_IsForgeConfig(t *testing.T) {
	t.Setenv("FORGE_ARTIFACTS_URL", "http://elsewhere:9000/")
	if got := artifactsURL(); got != "http://elsewhere:9000" {
		t.Errorf("artifactsURL = %q, want the trailing slash trimmed", got)
	}
	t.Setenv("FORGE_ARTIFACTS_URL", "")
	if got := artifactsURL(); got != "http://artifacts:8097" {
		t.Errorf("default = %q", got)
	}
}

func TestArtifactSpec_Defaults(t *testing.T) {
	a := &ArtifactSpec{Mode: "save", Name: "c"}
	if a.tokenEnv() != defaultArtifactTokenEnv {
		t.Errorf("tokenEnv = %q", a.tokenEnv())
	}
	if a.path() != "." {
		t.Errorf("path = %q, want the whole volume by default", a.path())
	}
}

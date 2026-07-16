package main

import (
	"fmt"
	"strings"
)

// Artifact save/restore: move a path between a run's shared volume and the
// artifacts service, which stores blobs that OUTLIVE the run.
//
// Only forge can do this: the artifacts service holds the blobs but cannot mount a
// run's volume, and the workflows orchestrator cannot read one either. So — exactly
// as with the scatter clone/gather (CopySpec) — forge synthesises a helper container
// that mounts the volume and streams the transfer, on its own controlled image.
//
// Auth is deliberately NOT invented here. The helper reads a bearer from an env var
// the caller wires with an ordinary secret_ref ("secret:<name>" → a gatekeeper org
// secret). That keeps this from silently handing a sandbox a token it did not have:
// granting artifact access stays an explicit act by the pipeline author, using the
// same mechanism that already carries git and registry credentials.

const (
	// defaultArtifactTokenEnv is the env var the helper reads its bearer from.
	defaultArtifactTokenEnv = "ARTIFACTS_TOKEN"
	// artifactModeSave / artifactModeRestore are the two directions.
	artifactModeSave    = "save"
	artifactModeRestore = "restore"
)

// artifactsURL is where the helper reaches the artifact store. It is forge config
// rather than a per-request field so a sandbox cannot be pointed at an arbitrary
// host and made to POST a workspace there.
func artifactsURL() string {
	return strings.TrimRight(envOrDefault("FORGE_ARTIFACTS_URL", "http://artifacts:8097"), "/")
}

// ArtifactSpec configures an artifact transfer. Like BuildSpec/CopySpec, forge
// derives the image and command from it, so `image` and `command` are ignored.
type ArtifactSpec struct {
	// Mode is "save" (volume → store) or "restore" (store → volume).
	Mode string `json:"mode"`
	// Name is the artifact's name in the store, unique per user. Saving the same
	// name replaces it — which is what makes it usable as a cache.
	Name string `json:"name"`
	// Path is the directory or file, relative to the attached workdir volume, to
	// archive on save / extract into on restore. Default "." (the whole volume).
	Path string `json:"path,omitempty"`
	// TokenEnv names the env var holding the bearer for the artifacts service.
	// Default ARTIFACTS_TOKEN; wire it with secret_refs.
	TokenEnv string `json:"token_env,omitempty"`
	// Optional, restore only: succeed when the artifact does not exist yet, leaving
	// the volume untouched. This is what makes a cache restore safe on the FIRST
	// run — without it every new pipeline fails until something has been saved.
	Optional bool `json:"optional,omitempty"`
}

func (a *ArtifactSpec) tokenEnv() string {
	if a.TokenEnv != "" {
		return a.TokenEnv
	}
	return defaultArtifactTokenEnv
}

func (a *ArtifactSpec) path() string {
	if strings.TrimSpace(a.Path) != "" {
		return a.Path
	}
	return "."
}

// validateArtifact checks an artifact spec's shape. Returns nil when valid.
func validateArtifact(a *ArtifactSpec, secretRefs, env map[string]string, mounts []VolumeMount) error {
	if a.Mode != artifactModeSave && a.Mode != artifactModeRestore {
		return fmt.Errorf("artifact.mode must be %q or %q", artifactModeSave, artifactModeRestore)
	}
	if strings.TrimSpace(a.Name) == "" {
		return fmt.Errorf("artifact.name is required")
	}
	// The name lands in a URL and the store validates it too, but a bad name should
	// fail at submit rather than after a pod has been scheduled.
	if strings.ContainsAny(a.Name, "/ \t?&#") {
		return fmt.Errorf("artifact.name must not contain a path separator, whitespace or URL syntax")
	}
	// The path is interpolated into a shell script, so refuse anything that could
	// terminate the quoting and run something else.
	if strings.ContainsAny(a.path(), "'\"`$;|&\n") {
		return fmt.Errorf("artifact.path must not contain shell metacharacters")
	}
	if strings.Contains(a.path(), "..") {
		return fmt.Errorf("artifact.path must not contain ..")
	}
	// The transfer needs somewhere to read from / write to.
	if !hasWorkdirMount(mounts) {
		return fmt.Errorf("artifact requires an attached volume marked workdir: true")
	}
	// Fail fast when the token is wired nowhere: the alternative is a helper that
	// runs, 401s, and reports a confusing failure from inside the sandbox.
	tok := a.tokenEnv()
	if _, ok := secretRefs[tok]; !ok {
		if _, ok := env[tok]; !ok {
			return fmt.Errorf("artifact needs %s: wire it with secret_refs (e.g. %q: \"secret:my-artifacts-token\")", tok, tok)
		}
	}
	return nil
}

func hasWorkdirMount(mounts []VolumeMount) bool {
	for i := range mounts {
		if mounts[i].Workdir {
			return true
		}
	}
	return false
}

// artifactCommand synthesises the transfer script.
//
// tar streams straight to/from curl, so a multi-gigabyte cache never lands on the
// helper's own disk — only in the volume it belongs in. --fail makes curl exit
// non-zero on an HTTP error (it is silent about 4xx by default), which is what turns
// a quota rejection into a failed step rather than a "successful" empty artifact.
func artifactCommand(a *ArtifactSpec, mounts []VolumeMount) []string {
	dest := ""
	for i := range mounts {
		if mounts[i].Workdir {
			dest = mountPathOrDefault(mounts[i].MountPath)
		}
	}
	url := fmt.Sprintf("%s/artifacts/%s", artifactsURL(), a.Name)
	tok := "$" + a.tokenEnv()

	var sb strings.Builder
	sb.WriteString("set -eu\n")
	sb.WriteString(fmt.Sprintf("cd %s\n", dest))

	if a.Mode == artifactModeSave {
		sb.WriteString(fmt.Sprintf("echo '==> saving %s from %s'\n", a.Name, a.path()))
		// Stream the tar directly into the PUT body.
		sb.WriteString(fmt.Sprintf(
			"tar czf - %s | curl --fail --silent --show-error -X PUT "+
				"-H \"Authorization: Bearer %s\" -H 'Content-Type: application/gzip' "+
				"--data-binary @- %q\n", a.path(), tok, url))
		sb.WriteString(fmt.Sprintf("echo '==> saved %s'\n", a.Name))
		return []string{"sh", "-c", sb.String()}
	}

	// Restore. An optional restore treats "not stored yet" as success, so a cache
	// warms on the first run instead of failing it.
	sb.WriteString(fmt.Sprintf("echo '==> restoring %s into %s'\n", a.Name, a.path()))
	sb.WriteString(fmt.Sprintf("code=$(curl --silent --show-error -o /tmp/a.tgz -w '%%{http_code}' "+
		"-H \"Authorization: Bearer %s\" %q) || true\n", tok, url+"/content"))
	if a.Optional {
		sb.WriteString("if [ \"$code\" = \"404\" ]; then echo '==> no artifact stored yet, skipping'; exit 0; fi\n")
	}
	sb.WriteString("if [ \"$code\" != \"200\" ]; then echo \"restore failed: HTTP $code\"; cat /tmp/a.tgz 2>/dev/null; exit 1; fi\n")
	sb.WriteString(fmt.Sprintf("mkdir -p %s\n", a.path()))
	sb.WriteString(fmt.Sprintf("tar xzf /tmp/a.tgz -C %s\n", a.path()))
	sb.WriteString(fmt.Sprintf("echo '==> restored %s'\n", a.Name))
	return []string{"sh", "-c", sb.String()}
}

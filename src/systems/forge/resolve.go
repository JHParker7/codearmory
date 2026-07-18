package main

import (
	"fmt"
	"regexp"
	"strings"
)

// A resolve-paths execution scans an attached shared volume and captures the entries —
// directories or files, relative to the volume root — whose path matches a regex. It is
// the fan-out set generator behind a scatter: each matched path becomes one parallel
// leg. Forge derives the image (its minimal runner image) and command (a synthesised
// find | grep) from the spec, and the newline-separated match list is captured as the
// `output` variable through the normal output_env mechanism, so a workflow reads it as
// the step's structured output. Like build and copy, this bypasses the image/command
// allowlist because forge fully supplies both.

const (
	resolveShell         = "/bin/sh"
	defaultResolveOutput = "paths"
	resolveModeDir       = "dir"
	resolveModeFile      = "file"
)

// validateResolve checks a resolve spec against the attached mounts. A nil spec is
// valid (not a resolve). The regex must compile, the mode must be dir/file, and a
// volume to scan must be resolvable.
func validateResolve(r *ResolveSpec, mounts []VolumeMount) error {
	if r == nil {
		return nil
	}
	if strings.TrimSpace(r.Regex) == "" {
		return fmt.Errorf("resolve.regex is required")
	}
	if _, err := regexp.Compile(r.Regex); err != nil {
		return fmt.Errorf("resolve.regex: %w", err)
	}
	switch r.Mode {
	case "", resolveModeDir, resolveModeFile:
	default:
		return fmt.Errorf("resolve.mode %q: must be %q or %q", r.Mode, resolveModeDir, resolveModeFile)
	}
	if r.MaxDepth < 0 {
		return fmt.Errorf("resolve.max_depth must be >= 0")
	}
	if r.Output != "" && !envKeyRe.MatchString(r.Output) {
		return fmt.Errorf("resolve.output %q: must be a valid env var name", r.Output)
	}
	_, err := resolveMountPath(r, mounts)
	return err
}

// resolveMountPath picks the mount to scan: the named volume, else the workdir mount,
// else the single attached mount.
func resolveMountPath(r *ResolveSpec, mounts []VolumeMount) (string, error) {
	if r.Volume != "" {
		for _, m := range mounts {
			if m.Name == r.Volume {
				return mountPathOrDefault(m.MountPath), nil
			}
		}
		return "", fmt.Errorf("resolve.volume %q is not attached (add it to `volumes`)", r.Volume)
	}
	for _, m := range mounts {
		if m.Workdir {
			return mountPathOrDefault(m.MountPath), nil
		}
	}
	if len(mounts) == 1 {
		return mountPathOrDefault(mounts[0].MountPath), nil
	}
	return "", fmt.Errorf("resolve requires a volume to scan: set resolve.volume or mark one attached volume workdir: true")
}

// resolveOutputVar is the env var / output key the matched paths are captured into.
func resolveOutputVar(r *ResolveSpec) string {
	if r.Output != "" {
		return r.Output
	}
	return defaultResolveOutput
}

// containsString reports whether s is in xs.
func containsString(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// resolveCommand assembles the scan: list matching entries relative to the mount root,
// filter by the regex, sort for determinism, and capture the newline-separated list
// into the output variable (read back via output_env). An empty match set is valid
// (zero legs) — the trailing `|| true` keeps a no-match grep from failing the run.
func resolveCommand(r *ResolveSpec, mounts []VolumeMount) []string {
	mount, _ := resolveMountPath(r, mounts) // already validated
	typ := "d"
	if r.Mode == resolveModeFile {
		typ = "f"
	}
	depth := ""
	if r.MaxDepth > 0 {
		depth = fmt.Sprintf(" -maxdepth %d", r.MaxDepth)
	}
	out := resolveOutputVar(r)

	var sb strings.Builder
	sb.WriteString("cd " + shellSingleQuote(mount) + " || { echo 'forge: resolve volume mount not found' >&2; exit 1; }\n")
	sb.WriteString(out + "=\"$({ find . -mindepth 1" + depth + " -type " + typ +
		" -print 2>/dev/null | sed 's#^\\./##' | grep -E " + shellSingleQuote(r.Regex) + " || true; } | sort)\"\n")
	return []string{resolveShell, "-c", sb.String()}
}

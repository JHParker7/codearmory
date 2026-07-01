package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Image building runs as an ordinary sandboxed execution whose image and command
// forge derives from a BuildSpec — no Docker daemon, no host socket. Forge builds
// with Kaniko, which does daemonless userspace layer extraction and so runs under
// both kernel-isolated backends with only root + a writable rootfs (no elevated
// capabilities, no privileged container): the gVisor Sentry ("gvisor") and the kata
// microVM ("kata"). That root + writable rootfs comes from a Privileged runner class,
// which forge only honours on those kernel-isolated backends — so "builds require a
// Privileged runner class" is the single guard, and the shared-kernel backends
// (docker/plain-k8s) can never build. The build context is normally a shared
// workspace volume a prior checkout step populated.

const (
	defaultBuildContext    = "/workspace"
	defaultDockerfile      = "Dockerfile"
	defaultRegistryAuthEnv = "REGISTRY_AUTH"
	// builds are minutes-long, not seconds; default well above a command step.
	defaultBuildTimeoutSecs = int64(1800)

	kanikoShell = "/busybox/sh" // kaniko :debug ships busybox at this path
)

var (
	// imageRefRe restricts a push destination to the characters of a registry image
	// reference; it is single-quoted into the build command as defence in depth.
	imageRefRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]*$`)
	// buildTargetRe restricts a multi-stage target name.
	buildTargetRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

// builderImage is the operator-overridable Kaniko image, read once at startup. It is
// forge-controlled (not user-supplied) and bypasses ALLOWED_IMAGES.
var builderImage string

func initBuildConfig() {
	builderImage = envOrDefault("FORGE_BUILDER_IMAGE", "gcr.io/kaniko-project/executor:debug")
}

// validateBuild checks a build spec's shape against the request's secret_refs/env.
// A nil spec is valid (not a build). Push destinations and registry credentials are
// required unless NoPush. Every interpolated value is charset-restricted here and
// single-quoted at assembly, so it cannot break out of the build command.
func validateBuild(b *BuildSpec, refs, env map[string]string) error {
	if b == nil {
		return nil
	}
	if err := validateAbsBuildPath(orDefault(b.Context, defaultBuildContext)); err != nil {
		return fmt.Errorf("build.context: %w", err)
	}
	if err := validateRelBuildPath(orDefault(b.Dockerfile, defaultDockerfile)); err != nil {
		return fmt.Errorf("build.dockerfile: %w", err)
	}
	if !b.NoPush {
		if len(b.Destinations) == 0 {
			return fmt.Errorf("build.destinations is required unless no_push is set")
		}
		authEnv := orDefault(b.RegistryAuth, defaultRegistryAuthEnv)
		if !envKeyRe.MatchString(authEnv) {
			return fmt.Errorf("build.registry_auth %q: must be a valid env var name", authEnv)
		}
		if _, ok := refs[authEnv]; !ok {
			if _, ok := env[authEnv]; !ok {
				return fmt.Errorf("build pushes but %s is not set by secret_refs or env; supply registry credentials (a Docker config.json) under that name", authEnv)
			}
		}
	}
	for i, d := range b.Destinations {
		if !imageRefRe.MatchString(d) {
			return fmt.Errorf("build.destinations[%d] %q: not a valid image reference", i, d)
		}
	}
	for k := range b.BuildArgs {
		if !envKeyRe.MatchString(k) {
			return fmt.Errorf("build.build_args key %q: must match [A-Za-z_][A-Za-z0-9_]*", k)
		}
	}
	if b.Target != "" && !buildTargetRe.MatchString(b.Target) {
		return fmt.Errorf("build.target %q: invalid stage name", b.Target)
	}
	return nil
}

// kanikoCommand assembles the Kaniko invocation. It writes the registry Docker config
// from the auth env before executing (when pushing), then builds from the dir://
// context and pushes each destination (or --no-push).
func kanikoCommand(b *BuildSpec) []string {
	ctx := orDefault(b.Context, defaultBuildContext)
	df := orDefault(b.Dockerfile, defaultDockerfile)

	var sb strings.Builder
	sb.WriteString("set -e\n")
	if pushing(b) {
		sb.WriteString("mkdir -p /kaniko/.docker\n")
		sb.WriteString("printf '%s' \"$" + orDefault(b.RegistryAuth, defaultRegistryAuthEnv) + "\" > /kaniko/.docker/config.json\n")
	}
	sb.WriteString("exec /kaniko/executor")
	sb.WriteString(" --context=dir://" + shellSingleQuote(ctx))
	sb.WriteString(" --dockerfile=" + shellSingleQuote(df))
	if pushing(b) {
		for _, d := range b.Destinations {
			sb.WriteString(" --destination=" + shellSingleQuote(d))
		}
	} else {
		sb.WriteString(" --no-push")
	}
	for _, kv := range sortedBuildArgs(b.BuildArgs) {
		sb.WriteString(" --build-arg " + shellSingleQuote(kv))
	}
	if b.Target != "" {
		sb.WriteString(" --target=" + shellSingleQuote(b.Target))
	}
	return []string{kanikoShell, "-c", sb.String()}
}

// pushing reports whether the build should push (has destinations and NoPush unset).
func pushing(b *BuildSpec) bool { return !b.NoPush && len(b.Destinations) > 0 }

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// sortedBuildArgs renders build args as sorted "KEY=VALUE" tokens — deterministic for
// stable commands (and tests).
func sortedBuildArgs(args map[string]string) []string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = k + "=" + args[k]
	}
	return out
}

// validateAbsBuildPath requires an absolute path with no parent escapes.
func validateAbsBuildPath(p string) error {
	if !strings.HasPrefix(p, "/") || strings.Contains(p, "..") || strings.Contains(p, "\x00") {
		return fmt.Errorf("%q: must be an absolute path with no %q", p, "..")
	}
	return nil
}

// validateRelBuildPath requires a non-empty relative path (the Dockerfile, relative
// to the context) with no parent escapes.
func validateRelBuildPath(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "..") || strings.Contains(p, "\x00") {
		return fmt.Errorf("%q: must be a relative path with no %q", p, "..")
	}
	return nil
}

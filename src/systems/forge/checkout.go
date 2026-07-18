package main

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
)

// Defaults for a CheckoutSpec left partially unset.
const (
	defaultCheckoutEnv   = "GIT_CLONE_URL" // env var carrying the clone URL
	defaultCheckoutDir   = "repo"          // fallback dir when a repo name can't be derived
	defaultCheckoutDepth = 1               // shallow clone, matching actions/checkout
)

// gitImage is the forge-controlled minimal git image used for a checkout step that
// supplies no image of its own (the forge/git-clone action). Like the Kaniko builder
// image it is operator-overridable and bypasses ALLOWED_IMAGES, so users never pick
// or maintain a git-capable image just to clone a repo. Read once at startup.
var gitImage string

func initCheckoutConfig() {
	gitImage = envOrDefault("FORGE_GIT_IMAGE", "ghcr.io/code-armory-app/runner-git:latest")
}

var (
	// checkoutPathRe restricts the clone directory to a safe relative path. It is
	// interpolated into a shell script, so the allowlist keeps out shell
	// metacharacters; parent-dir escapes ("..") are rejected separately.
	checkoutPathRe = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._/-]*$`)
	// checkoutRefRe restricts a branch/tag name to safe characters and forbids a
	// leading '-' so it can't be read as a git flag.
	checkoutRefRe = regexp.MustCompile(`^[A-Za-z0-9_.][A-Za-z0-9._/-]*$`)
)

// isShellCommand reports whether cmd is a `[<shell> -c <script>]` invocation — the
// only shape into which a checkout prologue (or the output-env trailer) can be
// woven. Matches wrapOutputEnv's assumption.
func isShellCommand(cmd []string) bool {
	return len(cmd) >= 3 && cmd[1] == "-c"
}

// validateCheckout checks a submitted checkout spec against the command and the
// env the request also sets. It enforces that: the command is a shell (`-c`) form;
// the source env var is a valid name and is actually provided (via secret_refs or
// env), so the job can't silently run against an empty URL; and path/ref/depth are
// well-formed. A nil spec is valid (no checkout).
func validateCheckout(c *CheckoutSpec, command []string, refs, env map[string]string) error {
	if c == nil {
		return nil
	}
	if !isShellCommand(command) {
		return fmt.Errorf(`checkout requires a shell command (["sh","-c","..."])`)
	}
	src := c.Env
	if src == "" {
		src = defaultCheckoutEnv
	}
	if !envKeyRe.MatchString(src) {
		return fmt.Errorf("checkout.env %q: must match [A-Za-z_][A-Za-z0-9_]*", src)
	}
	if blockedEnvKeys[strings.ToUpper(src)] {
		return fmt.Errorf("checkout.env %q is not permitted", src)
	}
	if _, ok := refs[src]; !ok {
		if _, ok := env[src]; !ok {
			return fmt.Errorf("checkout.env %q is not set by secret_refs or env; add a git: repo reference (or an env var) with that name", src)
		}
	}
	if c.Path != "" {
		if !checkoutPathRe.MatchString(c.Path) || strings.Contains(c.Path, "..") {
			return fmt.Errorf("checkout.path %q: must be a relative path of [A-Za-z0-9._/-] with no \"..\"", c.Path)
		}
	}
	if c.Ref != "" && !checkoutRefRe.MatchString(c.Ref) {
		return fmt.Errorf("checkout.ref %q: must match [A-Za-z0-9._/-] and not start with '-'", c.Ref)
	}
	if c.Depth != nil && *c.Depth < 0 {
		return fmt.Errorf("checkout.depth %d: must be >= 0 (0 = full clone)", *c.Depth)
	}
	return nil
}

// applyCheckout weaves a `git clone … && cd …` prologue into a shell command so
// the command runs inside a freshly checked-out repo. It is a no-op for non-shell
// commands (submit rejects those, so this is defensive). refs supplies the git:/
// gitea: reference used only to derive a default clone directory. intoWorkdirRoot
// is set when the execution's working directory is itself a shared workspace volume
// (a mount with workdir: true): the clone then defaults into that volume's root so
// the volume *is* the working tree, rather than a repo-name subdirectory of it.
// The returned slice is a copy; the persisted command is never modified.
func applyCheckout(cmd []string, c *CheckoutSpec, refs map[string]string, intoWorkdirRoot bool) []string {
	if c == nil || !isShellCommand(cmd) {
		return cmd
	}
	out := append([]string(nil), cmd...)
	out[2] = c.script(refs, intoWorkdirRoot) + "\n" + cmd[2]
	return out
}

// tmpCheckoutDir is the temporary subdirectory the volume-root checkout clones into
// before relocating the tree up to the working dir. A repo won't contain this name at
// its top level, so the relocation can't collide.
const tmpCheckoutDir = ".forge-checkout"

// script builds the POSIX-sh prologue that clones the repo and cd's into it. All
// interpolated values (env name, path, ref) are validated at submit time and
// single-quoted here, so they cannot break out of the script. intoWorkdirRoot is set
// when the working dir is itself a shared volume (workdir: true), which switches the
// no-path default to landing the tree at the volume root (see below).
func (c *CheckoutSpec) script(refs map[string]string, intoWorkdirRoot bool) string {
	env := c.Env
	if env == "" {
		env = defaultCheckoutEnv
	}

	depth := defaultCheckoutDepth
	if c.Depth != nil {
		depth = *c.Depth
	}
	flags := ""
	if depth > 0 {
		flags += fmt.Sprintf(" --depth=%d", depth)
	}
	if c.Ref != "" {
		flags += " --branch=" + shellSingleQuote(c.Ref)
	}

	var b strings.Builder
	b.WriteString("# forge: checkout — clone repo into working dir\n")
	// Preflight: without git the raw `git clone` fails with a bare "git: not found"
	// that the clone guard below then masks as "git clone failed" — misleading, since
	// the real cause is the image. Name it explicitly and point at the fix.
	b.WriteString("command -v git >/dev/null 2>&1 || { echo 'forge: checkout: git is not installed in this image; use a runner image that includes git' >&2; exit 1; }\n")
	// ${env:-} guards against `set -u` while treating unset as empty.
	b.WriteString("if [ -z \"${" + env + ":-}\" ]; then echo 'forge: checkout: " + env + " is not set' >&2; exit 1; fi\n")

	// Volume-root checkout: the working dir is a shared volume that downstream steps
	// mount at their root, so the tree must land at the volume root — not a repo-name
	// subdirectory of it (forge/build-image's default /workspace context and a
	// downstream forge/run's workdir both look at the root). `git clone … .` refuses a
	// non-empty target, and a disk-backed PVC can carry a lost+found, so clone into a
	// temp subdir on the volume and relocate its entries (dotfiles included) up to the
	// root. Only for the no-explicit-path case; an explicit path is an ordinary subdir.
	if intoWorkdirRoot && c.Path == "" {
		tmp := shellSingleQuote(tmpCheckoutDir)
		b.WriteString("git clone" + flags + " -- \"$" + env + "\" " + tmp +
			" || { echo 'forge: checkout: git clone failed' >&2; exit 1; }\n")
		b.WriteString("find " + tmp + " -mindepth 1 -maxdepth 1 -exec mv -- {} . ';'" +
			" || { echo 'forge: checkout: could not move the checkout into the workspace' >&2; exit 1; }\n")
		b.WriteString("rmdir " + tmp + " 2>/dev/null || true\n")
		return b.String()
	}

	dir := c.Path
	if dir == "" {
		dir = c.defaultDir(refs)
	}
	b.WriteString("git clone" + flags + " -- \"$" + env + "\" " + shellSingleQuote(dir) +
		" || { echo 'forge: checkout: git clone failed' >&2; exit 1; }\n")
	b.WriteString("cd " + shellSingleQuote(dir) + " || exit 1\n")
	return b.String()
}

// defaultDir derives the clone directory from the git:/gitea: reference the same
// job sets on the source env var (e.g. git:https://host/acme/widgets.git → widgets).
// Falls back to "repo" when no reference is present or a safe name can't be derived.
// Used only for the subdirectory checkout; the volume-root case (see script) never
// calls it.
func (c *CheckoutSpec) defaultDir(refs map[string]string) string {
	env := c.Env
	if env == "" {
		env = defaultCheckoutEnv
	}
	if name := repoBasename(refs[env]); name != "" {
		return name
	}
	return defaultCheckoutDir
}

// checkoutIntoWorkdirRoot reports whether any attached volume is made the execution's
// working directory (workdir: true). When so, an auto-checkout with no explicit path
// clones into the volume root rather than a subdirectory of it, so the shared volume
// itself holds the working tree.
func checkoutIntoWorkdirRoot(mounts []VolumeMount) bool {
	for _, m := range mounts {
		if m.Workdir {
			return true
		}
	}
	return false
}

// repoBasename extracts a repository directory name from a secret_ref value such as
// "git:https://host/acme/widgets.git" or "gitea:acme/widgets". Returns "" when the
// name can't be derived or isn't a safe directory name.
func repoBasename(ref string) string {
	_, arg, ok := strings.Cut(ref, ":")
	if !ok || arg == "" {
		return ""
	}
	// git: refs are URLs; gitea: refs are owner/repo. Parsing as a URL and taking
	// the last path segment handles both.
	name := arg
	if u, err := url.Parse(arg); err == nil && u.Path != "" {
		name = u.Path
	}
	name = strings.TrimSuffix(path.Base(strings.Trim(name, "/")), ".git")
	if name == "" || name == "." || name == "/" || !checkoutPathRe.MatchString(name) || strings.Contains(name, "..") {
		return ""
	}
	return name
}

// shellSingleQuote wraps s in single quotes for safe POSIX-sh interpolation,
// escaping any embedded single quote. Inputs here are already regex-restricted, so
// this is defence in depth.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

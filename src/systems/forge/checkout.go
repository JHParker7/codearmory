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
// gitea: reference used only to derive a default clone directory. The returned
// slice is a copy; the persisted command is never modified.
func applyCheckout(cmd []string, c *CheckoutSpec, refs map[string]string) []string {
	if c == nil || !isShellCommand(cmd) {
		return cmd
	}
	out := append([]string(nil), cmd...)
	out[2] = c.script(refs) + "\n" + cmd[2]
	return out
}

// script builds the POSIX-sh prologue that clones the repo and cd's into it. All
// interpolated values (env name, path, ref) are validated at submit time and
// single-quoted here, so they cannot break out of the script.
func (c *CheckoutSpec) script(refs map[string]string) string {
	env := c.Env
	if env == "" {
		env = defaultCheckoutEnv
	}
	dir := c.Path
	if dir == "" {
		dir = c.defaultDir(refs)
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
	b.WriteString("git clone" + flags + " -- \"$" + env + "\" " + shellSingleQuote(dir) +
		" || { echo 'forge: checkout: git clone failed' >&2; exit 1; }\n")
	b.WriteString("cd " + shellSingleQuote(dir) + " || exit 1\n")
	return b.String()
}

// defaultDir derives the clone directory from the git:/gitea: reference the same
// job sets on the source env var (e.g. git:https://host/acme/widgets.git → widgets).
// Falls back to "repo" when no reference is present or a safe name can't be derived.
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

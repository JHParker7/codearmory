package main

import (
	"fmt"
	"strings"
)

// A volume-copy execution copies files between attached shared volumes inside the
// sandbox — no CSI clone, no ReadWriteMany. It is the primitive behind scatter/gather:
//
//   - scatter-clone: one source (the base workspace, whole tree) → a fresh per-leg
//     volume, so each parallel leg gets its own independent RWO copy and never
//     contends on a shared PVC (which on block storage would Multi-Attach across nodes).
//   - gather: N source volumes (the finished legs), each contributing its declared
//     owned paths → the base workspace, unioned with a disjointness check so two legs
//     writing the same path is a hard error rather than a silent last-writer-wins.
//
// It runs as an ordinary sandboxed Execution whose image (forge's minimal git/runner
// image) and command (a synthesised `cp` script) forge derives from the CopySpec, so
// the usual image/command/allowlist checks do not apply — like build and git-clone.
// Because every source plus the destination is mounted into the one copy pod, they all
// attach to a single node, which RWO permits; portability is total (any RWO backend:
// Ceph RBD, OpenEBS, docker).

const copyShell = "/bin/sh"

// validateCopy checks a copy spec against the execution's attached volume mounts. A
// nil spec is valid (not a copy). It requires at least one source, exactly one
// destination (the mount marked workdir: true), that every source names a distinct
// non-destination mount, and that every path is a clean relative path. Disjoint mode
// (gather) additionally forbids the whole-tree "." path, since the union is defined at
// the granularity of the declared owned paths.
func validateCopy(c *CopySpec, mounts []VolumeMount) error {
	if c == nil {
		return nil
	}
	if len(c.Sources) == 0 {
		return fmt.Errorf("copy.sources is required")
	}

	// The destination is the single workdir mount; sources are looked up by name.
	byName := map[string]*VolumeMount{}
	var dest *VolumeMount
	for i := range mounts {
		m := &mounts[i]
		byName[m.Name] = m
		if m.Workdir {
			dest = m
		}
	}
	if dest == nil {
		return fmt.Errorf("copy requires the destination volume attached with workdir: true")
	}
	if dest.ReadOnly {
		return fmt.Errorf("copy destination volume %q is mounted read_only", dest.Name)
	}

	seen := map[string]bool{}
	for i, s := range c.Sources {
		if s.Volume == "" {
			return fmt.Errorf("copy.sources[%d].volume is required", i)
		}
		src, ok := byName[s.Volume]
		if !ok {
			return fmt.Errorf("copy.sources[%d]: volume %q is not attached (add it to `volumes`)", i, s.Volume)
		}
		if src == dest {
			return fmt.Errorf("copy.sources[%d]: %q is the destination (workdir) volume", i, s.Volume)
		}
		if seen[s.Volume] {
			return fmt.Errorf("copy.sources[%d]: volume %q listed more than once", i, s.Volume)
		}
		seen[s.Volume] = true
		for j, p := range copyPaths(s.Paths) {
			if err := validateRelCopyPath(p); err != nil {
				return fmt.Errorf("copy.sources[%d].paths[%d]: %w", i, j, err)
			}
			if c.Disjoint && (p == "." || p == "") {
				return fmt.Errorf("copy.sources[%d].paths[%d]: the whole-tree %q path is not allowed when disjoint is set — declare the specific owned paths", i, j, ".")
			}
		}
	}
	return nil
}

// copyCommand assembles the `cp` script. For each source it copies the declared paths
// (default: the whole tree) into the destination, preserving each path's relative
// location. Disjoint mode stages into a scratch dir and fails on the first collision,
// so a gather across legs is a checked union; non-disjoint mode (a clone) copies
// straight into the destination.
func copyCommand(c *CopySpec, mounts []VolumeMount) []string {
	byName := map[string]string{} // volume name -> mount path
	dest := ""
	for i := range mounts {
		m := &mounts[i]
		byName[m.Name] = mountPathOrDefault(m.MountPath)
		if m.Workdir {
			dest = mountPathOrDefault(m.MountPath)
		}
	}

	var sb strings.Builder
	sb.WriteString("set -e\n")

	if c.Disjoint {
		// Stage every source into a scratch dir first: a path that already exists there
		// was produced by an earlier source, so two legs claimed it — fail loudly. Only
		// once staging is conflict-free do we merge it into the destination, so a
		// rejected gather never leaves the base workspace half-written.
		sb.WriteString("stage=\"$(mktemp -d)\"\n")
		for _, s := range c.Sources {
			srcRoot := byName[s.Volume]
			for _, p := range copyPaths(s.Paths) {
				srcPath := joinRel(srcRoot, p)
				stagePath := joinRel("$stage", p)
				sb.WriteString("if [ -e " + shellDoubleQuoteVar(stagePath) + " ]; then echo 'forge: gather conflict: " + p + " is written by more than one leg' >&2; exit 1; fi\n")
				sb.WriteString("mkdir -p \"$(dirname " + shellDoubleQuoteVar(stagePath) + ")\"\n")
				sb.WriteString("cp -a " + shellSingleQuote(srcPath) + " " + shellDoubleQuoteVar(stagePath) + "\n")
			}
		}
		sb.WriteString("cp -a \"$stage/.\" " + shellSingleQuote(dest+"/") + "\n")
		return []string{copyShell, "-c", sb.String()}
	}

	for _, s := range c.Sources {
		srcRoot := byName[s.Volume]
		for _, p := range copyPaths(s.Paths) {
			if p == "." || p == "" {
				// Whole tree: copy the source's contents into the destination root.
				sb.WriteString("cp -a " + shellSingleQuote(srcRoot+"/.") + " " + shellSingleQuote(dest+"/") + "\n")
				continue
			}
			srcPath := joinRel(srcRoot, p)
			destPath := joinRel(dest, p)
			sb.WriteString("mkdir -p \"$(dirname " + shellSingleQuote(destPath) + ")\"\n")
			sb.WriteString("cp -a " + shellSingleQuote(srcPath) + " " + shellSingleQuote(destPath) + "\n")
		}
	}
	return []string{copyShell, "-c", sb.String()}
}

// copyPaths defaults an empty path list to the whole tree.
func copyPaths(paths []string) []string {
	if len(paths) == 0 {
		return []string{"."}
	}
	return paths
}

// joinRel joins a mount root and a relative path, mapping the whole-tree "." to
// "<root>/." (copy contents) so the caller doesn't special-case it.
func joinRel(root, p string) string {
	if p == "." || p == "" {
		return root + "/."
	}
	return root + "/" + p
}

func mountPathOrDefault(p string) string {
	if p == "" {
		return defaultVolumeMountPath
	}
	return p
}

// validateRelCopyPath requires a relative path with no parent escape or NUL. The
// whole-tree "." (and empty, treated as ".") is allowed.
func validateRelCopyPath(p string) error {
	if p == "." || p == "" {
		return nil
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, "..") || strings.Contains(p, "\x00") {
		return fmt.Errorf("%q: must be a relative path with no %q", p, "..")
	}
	return nil
}

// shellDoubleQuoteVar double-quotes a string that intentionally contains a shell
// variable ($stage) so the variable still expands, while the literal remainder is
// protected. The path segments themselves are charset-restricted by
// validateRelCopyPath, so this is defence in depth around the interpolation.
func shellDoubleQuoteVar(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// A scatter step turns "partition a workspace, build the parts in parallel, recombine"
// into one pipeline block. It runs entirely on forge primitives so no ReadWriteMany or
// CSI clone is needed:
//
//  1. resolve  — forge/resolve-paths lists the base workspace paths matching a regex;
//     each becomes one leg (bound to ${scatter.path}).
//  2. clone    — forge/create-volume + forge/volume-copy give each leg its OWN copy of
//     the workspace, so parallel legs never share a PVC (which on block storage would
//     Multi-Attach across nodes — the very failure this feature avoids).
//  3. run      — the step's own action (e.g. forge/run) executes per leg on its clone.
//  4. gather   — forge/volume-copy unions each leg's declared owned outputs back into the
//     base workspace, failing loudly if two legs claim the same path (disjoint union).
//
// Every forge call goes through the normal executeStep path (HTTP + async poll + token),
// so scatter adds orchestration, not a new transport.

const (
	actionForgeResolvePaths = "forge/resolve-paths"
	actionForgeVolumeCopy   = "forge/volume-copy"

	defaultScatterVolume    = "workspace"
	defaultScatterMountPath = "/workspace"
	// scatterPathsOutput is the output_env key forge/resolve-paths captures the matched
	// path list into, read back from the resolve step's output.
	scatterPathsOutput = "paths"
	// scatterSrcMount / scatterGatherMount are the read-only mount points the clone and
	// gather use for their source volume(s); distinct from the workspace mount path so a
	// mount does not collide with the destination.
	scatterSrcMount    = "/scatter-src"
	scatterGatherMount = "/scatter-gather"
)

func (c *ScatterConfig) baseVolume() string {
	if c.Volume != "" {
		return c.Volume
	}
	return defaultScatterVolume
}

func (c *ScatterConfig) mount() string {
	if c.MountPath != "" {
		return c.MountPath
	}
	return defaultScatterMountPath
}

// validateScatter checks a scatter config's shape: exactly one path source (regex or
// paths_from) and, for the regex source, a valid mode. Returns "" when valid.
func validateScatter(c *ScatterConfig) string {
	hasRegex := strings.TrimSpace(c.Regex) != ""
	hasFrom := strings.TrimSpace(c.PathsFrom) != ""
	if hasRegex == hasFrom {
		return "scatter requires exactly one of regex or paths_from"
	}
	switch c.Mode {
	case "", "dir", "file":
	default:
		return fmt.Sprintf("scatter.mode %q must be \"dir\" or \"file\"", c.Mode)
	}
	if c.MaxDepth < 0 {
		return "scatter.max_depth must be >= 0"
	}
	if c.MaxConcurrent < 0 {
		return "scatter.max_concurrent must be >= 0"
	}
	// share_base gives legs no volume of their own, so a gather has nothing to read from.
	// Fail loudly rather than silently dropping the outputs the caller asked for.
	if c.ShareBase && len(c.Outputs) > 0 {
		return "scatter.share_base mounts the base read-only and gives legs no volume to own, so scatter.outputs cannot be gathered — drop outputs, or unset share_base to clone per leg"
	}
	return ""
}

// volMount builds a forge VolumeMount entry for a step's `volumes` With key.
func volMount(runID, name, mountPath string, workdir, readOnly bool) map[string]any {
	m := map[string]any{"workflow_id": runID, "name": name, "mount_path": mountPath}
	if workdir {
		m["workdir"] = true
	}
	if readOnly {
		m["read_only"] = true
	}
	return m
}

// scatterShardName is the deterministic per-leg clone volume name, scoped to the run so
// run-end teardown (DELETE /volumes?workflow_id=runID) reaps it.
func scatterShardName(base string, i int) string {
	return fmt.Sprintf("%s-s%d", base, i)
}

// scatterResolveStep lists the base workspace paths matching the regex, capturing them
// as the `paths` output.
func scatterResolveStep(c *ScatterConfig, runID string) Step {
	return Step{
		Action: actionForgeResolvePaths,
		With: map[string]any{
			"volumes": []any{volMount(runID, c.baseVolume(), c.mount(), false, true)},
			"resolve": map[string]any{
				"volume":    c.baseVolume(),
				"regex":     c.Regex,
				"mode":      c.Mode,
				"max_depth": c.MaxDepth,
				"output":    scatterPathsOutput,
			},
		},
	}
}

// scatterCreateShardStep provisions one leg's clone volume.
func scatterCreateShardStep(c *ScatterConfig, runID, shard string) Step {
	with := map[string]any{"workflow_id": runID, "name": shard}
	if c.SizeMB > 0 {
		with["size_mb"] = c.SizeMB
	}
	if c.Medium != "" {
		with["medium"] = c.Medium
	}
	return Step{Action: ActionForgeCreateVolume, With: with}
}

// scatterCloneStep copies the whole base workspace into a leg's freshly-created clone
// volume (mounted workdir), so the leg starts from an exact copy.
func scatterCloneStep(c *ScatterConfig, runID, shard string) Step {
	return Step{
		Action: actionForgeVolumeCopy,
		With: map[string]any{
			"volumes": []any{
				volMount(runID, shard, c.mount(), true, false),
				volMount(runID, c.baseVolume(), scatterSrcMount, false, true),
			},
			"copy": map[string]any{
				"sources": []any{map[string]any{"volume": c.baseVolume()}},
			},
		},
	}
}

// scatterLegStep is the user's step run on a leg's clone: its own action/With, with the
// clone volume attached as the workspace (workdir). ${scatter.path} in the With is
// resolved by the caller's subst context.
func scatterLegStep(ws WorkflowStep, c *ScatterConfig, runID, shard string) Step {
	with := make(map[string]any, len(ws.With)+1)
	for k, v := range ws.With {
		with[k] = v
	}
	with["volumes"] = scatterLegVolumes(ws.With, c, runID, shard)
	return Step{Name: ws.Name, Action: ws.Action, With: with, Timeout: ws.Timeout}
}

// scatterLegVolumes builds a leg's volume list: its OWN clone at the workspace mount
// (workdir), plus every other volume the step declared, carried through unchanged.
//
// Carrying the extras through is what makes a SHARED dependency cache possible. Cloning
// the workspace per leg is the point of scatter (parallel legs must never share a PVC),
// but it also means anything seeded into the base workspace is duplicated N times — a
// 1.4GB warm Go cache across 15 legs is 21GB of disk and blows the workflow volume cap.
// A cache wants the opposite treatment: provisioned ONCE and mounted into every leg, so
// legs share compiled artifacts instead of each starting cold. Replacing `volumes`
// outright made that impossible to express.
//
// Dropped: any declared volume naming the base volume, or mounting at the workspace mount
// path — the leg's clone replaces the base there, and two mounts at one path is invalid.
func scatterLegVolumes(with map[string]any, c *ScatterConfig, runID, shard string) []any {
	// share_base: the base itself is the leg's workspace, mounted read-only. No clone, so
	// no shard volume exists to mount.
	workspace := volMount(runID, shard, c.mount(), true, false)
	if c.ShareBase {
		workspace = volMount(runID, c.baseVolume(), c.mount(), true, true)
	}
	vols := []any{workspace}
	declared, _ := with["volumes"].([]any)
	for _, v := range declared {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if name, _ := m["name"].(string); name == c.baseVolume() {
			continue
		}
		// An empty mount_path would land on forge's default — the same place the clone
		// mounts — so it is dropped rather than silently colliding with the workspace.
		mp, _ := m["mount_path"].(string)
		if mp == "" || mp == c.mount() {
			continue
		}
		// The clone is the leg's working directory; a second workdir is ambiguous, so
		// strip it from the extras (on a copy — the step definition is shared across legs
		// and must not be mutated).
		extra := make(map[string]any, len(m))
		for k, val := range m {
			if k == "workdir" {
				continue
			}
			extra[k] = val
		}
		vols = append(vols, extra)
	}
	return vols
}

// scatterGatherStep unions each leg's declared owned outputs back into the base
// workspace (disjoint — two legs claiming the same path fails). Returns ok=false when
// there is nothing to gather (no Outputs declared), so the caller skips it.
func scatterGatherStep(c *ScatterConfig, runID string, shards, paths []string) (Step, bool) {
	if len(c.Outputs) == 0 || len(shards) == 0 {
		return Step{}, false
	}
	volumes := []any{volMount(runID, c.baseVolume(), c.mount(), true, false)}
	sources := make([]any, 0, len(shards))
	for i, shard := range shards {
		volumes = append(volumes, volMount(runID, shard, fmt.Sprintf("%s-%d", scatterGatherMount, i), false, true))
		owned := make([]any, 0, len(c.Outputs))
		for _, o := range c.Outputs {
			owned = append(owned, substitute(o, substContext{scatterPath: paths[i]}))
		}
		sources = append(sources, map[string]any{"volume": shard, "paths": owned})
	}
	return Step{
		Action: actionForgeVolumeCopy,
		With: map[string]any{
			"volumes": volumes,
			"copy":    map[string]any{"disjoint": true, "sources": sources},
		},
	}, true
}

// parseScatterPaths pulls the matched-path list out of the resolve step's output. The
// output is the forge step's captured output_env map (JSON), so the paths live under
// the `paths` key; a bare string (already the value) is tolerated. The list is parsed
// with the same splitter as a matrix values_from.
func parseScatterPaths(resolveOutput string) []string {
	if v, ok := jsonField(resolveOutput, []string{scatterPathsOutput}); ok {
		return parseMatrixList(v)
	}
	return parseMatrixList(resolveOutput)
}

// runScatterGroup executes a scatter step end to end and returns its aggregated leg
// output (a JSON array, like a matrix) and the group status. It records a step run per
// leg for the run view, plus a synthetic step run for a resolve/gather failure so the
// cause is visible rather than a silently failed run.
// legSem is the run-wide leg budget shared with every other node in the frontier;
// cfg.MaxConcurrent still caps this scatter's own fan-out within it.
func (p *WorkerPool) runScatterGroup(ctx context.Context, store *tokenStore, runID, workflowID string, ws WorkflowStep, stepIndex int, inputs, visible map[string]string, depth int, legSem chan struct{}) (string, string) {
	c := ws.Scatter
	base := substContext{inputs: inputs, outputs: visible, runID: runID, workflowID: workflowID, depth: depth}

	// Produce the fan-out path set. Both sources are substituted for run-level
	// references (${inputs.*}/${steps.*}) first: paths_from takes the list straight from
	// a reference (like a matrix values_from, no workspace scan); otherwise the regex
	// drives a forge/resolve-paths scan of the base workspace.
	cfg := *c
	var paths []string
	if strings.TrimSpace(cfg.PathsFrom) != "" {
		paths = parseMatrixList(substitute(cfg.PathsFrom, base))
		if len(paths) == 0 {
			return "", p.scatterFail(runID, stepIndex, ws.Name, fmt.Sprintf("scatter paths_from %q produced no paths", cfg.PathsFrom))
		}
	} else {
		cfg.Regex = substitute(cfg.Regex, base)
		resolveOut, err := p.executeStep(ctx, store, scatterResolveStep(&cfg, runID), base)
		if err != nil {
			return "", p.scatterFail(runID, stepIndex, ws.Name, "resolve paths: "+err.Error())
		}
		paths = parseScatterPaths(resolveOut.Output)
		if len(paths) == 0 {
			return "", p.scatterFail(runID, stepIndex, ws.Name, fmt.Sprintf("scatter regex %q matched no paths", cfg.Regex))
		}
	}
	if len(paths) > maxMatrixValues {
		return "", p.scatterFail(runID, stepIndex, ws.Name, fmt.Sprintf("scatter expanded to %d paths, exceeding the limit of %d", len(paths), maxMatrixValues))
	}

	// One leg per matched path: create a clone volume, seed it from the base, then run
	// the user's step on it — all concurrently, capped by MaxConcurrent.
	shards := make([]string, len(paths))
	results := make([]taskResult, len(paths))
	stepRunIDs := make([]string, len(paths))
	for i := range paths {
		shards[i] = scatterShardName(cfg.baseVolume(), i)
		sid := uuid.New().String()
		stepRunIDs[i] = sid
		if serr := p.startStepRun(runID, sid, stepIndex, scatterLegName(ws.Name, paths[i])); serr != nil {
			slog.ErrorContext(ctx, "worker: start scatter leg run", "run_id", runID, "error", serr)
			return "", StatusFailed
		}
	}

	// Default the fan-out to defaultFanoutConcurrency; an explicit max_concurrent
	// raises it up to the maxParallelSteps ceiling.
	limit := defaultFanoutConcurrency
	if c.MaxConcurrent > 0 {
		limit = min(c.MaxConcurrent, maxParallelSteps)
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := range paths {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if !acquireLeg(ctx, legSem) {
				results[i] = taskResult{idx: i, err: context.Canceled}
				return
			}
			defer releaseLeg(legSem)
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				// Cancelled while still queued: the leg never ran, so leave its
				// started_at NULL (zero duration) and just record the terminal state.
				results[i] = taskResult{idx: i, err: context.Canceled}
				p.finishStepRun(stepRunIDs[i], StatusCancelled, nil, nil, nil, nil)
				return
			}
			r := p.runScatterLeg(withStepRunID(ctx, stepRunIDs[i]), store, runID, workflowID, ws, &cfg, shards[i], paths[i], inputs, visible, depth)
			results[i] = r
			// Record this leg's own terminal time the moment it finishes, so each leg
			// shows its real end instead of the instant the whole group's barrier was
			// reached (which made every leg display an identical end time). started_at is
			// stamped by the withStepRunID poller path (queued→running), so no begin here.
			st := StatusCompleted
			switch {
			case r.err != nil && ctx.Err() != nil:
				st = StatusCancelled
			case r.err != nil:
				st = StatusFailed
			}
			p.finishStepRun(stepRunIDs[i], st, strPtrOrNil(r.output), strPtrOrNil(r.logs), r.usedMB, r.limitMB)
		}(i)
	}
	wg.Wait()

	// Aggregate the group's overall status from the per-leg results; each leg already
	// recorded its own step-run terminal state above.
	status := StatusCompleted
	for _, r := range results {
		switch {
		case r.err != nil && ctx.Err() != nil:
			status = worstStatus(status, StatusCancelled)
		case r.err != nil:
			status = StatusFailed
		}
	}
	if status != StatusCompleted {
		return "", status
	}

	// All legs succeeded — union their owned outputs back into the base workspace.
	if gather, ok := scatterGatherStep(&cfg, runID, shards, paths); ok {
		if _, gerr := p.executeStep(ctx, store, gather, base); gerr != nil {
			return "", p.scatterFail(runID, stepIndex, ws.Name, "gather outputs: "+gerr.Error())
		}
	}
	return aggregateTaskOutputs(results), StatusCompleted
}

// runScatterLeg provisions one leg's clone volume, seeds it from the base workspace,
// and runs the user's step on it bound to its matched path.
func (p *WorkerPool) runScatterLeg(ctx context.Context, store *tokenStore, runID, workflowID string, ws WorkflowStep, cfg *ScatterConfig, shard, path string, inputs, visible map[string]string, depth int) taskResult {
	base := substContext{inputs: inputs, outputs: visible, runID: runID, workflowID: workflowID, depth: depth}
	// share_base skips both the clone volume and the copy that seeds it — the leg mounts
	// the base read-only instead. That is two fewer sandbox pods per leg (on kata, two
	// fewer microVM boots).
	if !cfg.ShareBase {
		if _, err := p.executeStep(ctx, store, scatterCreateShardStep(cfg, runID, shard), base); err != nil {
			return taskResult{err: fmt.Errorf("create clone volume: %w", err)}
		}
		if _, err := p.executeStep(ctx, store, scatterCloneStep(cfg, runID, shard), base); err != nil {
			return taskResult{err: fmt.Errorf("clone workspace: %w", err)}
		}
	}
	legCtx := substContext{inputs: inputs, outputs: visible, scatterPath: path, runID: runID, workflowID: workflowID, depth: depth}
	res, err := p.executeStep(ctx, store, scatterLegStep(ws, cfg, runID, shard), legCtx)
	return taskResult{output: res.Output, logs: res.Logs, usedMB: res.MemoryUsedMB, limitMB: res.MemoryLimitMB, err: err}
}

// scatterFail records a single failed step run naming the cause (resolve/gather errors,
// or an empty match set) and returns StatusFailed, so the failure is visible in the run
// view rather than a run that silently fails with no step.
func (p *WorkerPool) scatterFail(runID string, stepIndex int, name, msg string) string {
	if sid := uuid.New().String(); p.startStepRun(runID, sid, stepIndex, name) == nil {
		p.finishStepRun(sid, StatusFailed, strPtr(msg), nil, nil, nil)
	}
	return StatusFailed
}

func scatterLegName(base, path string) string {
	return fmt.Sprintf("%s [path=%s]", base, path)
}

// worstStatus keeps a failed status dominant over cancelled over completed.
func worstStatus(a, b string) string {
	if a == StatusFailed || b == StatusFailed {
		return StatusFailed
	}
	if a == StatusCancelled || b == StatusCancelled {
		return StatusCancelled
	}
	return StatusCompleted
}

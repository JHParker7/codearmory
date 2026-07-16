/**
 * Pure mapping between the Scratch-style block builder and the workflows backend
 * model — the portal twin of the CLI's tuiPipelineStages.
 *
 * The backend stores a pipeline as an *ordered* list of steps, each with an
 * optional parallel_group: consecutive steps sharing a non-nil parallel_group run
 * concurrently (one "stage"); every other step is its own sequential stage. The
 * builder is a vertical stack of blocks (one per step occurrence) where a block
 * can be "linked" to the block above it to run in the same stage. This module is
 * the (pure, unit-tested) bridge between the two:
 *
 *   blocksFromSteps  ordered steps  -> blocks (first block of each stage starts a
 *                                       stage; the rest are parallelWithPrev)
 *   stepsFromBlocks  blocks         -> ordered steps with parallel_group
 *   stagesOf         blocks         -> blocks grouped into stage bands (rendering)
 *
 * Node/library types are kept out of here so the mapping can be tested under
 * mocha without the UI; the block component adapts these shapes. (stepSchema is
 * likewise a pure, UI-free module, so importing its pipeline-field split keeps the
 * mocha-testability.)
 */
import { splitPipelineWith } from './stepSchema';

/** Fans a step out into one execution per value in a list, binding
 * ${matrix.<var>} per execution. Mutually exclusive with parallel_group. Mirrors
 * the workflows API MatrixConfig; kept local so this module stays UI/library-free. */
export interface MatrixConfig {
  var: string;
  values?: string[];
  values_from?: string;
  /** Caps how many fan-out executions run at once (0/undefined = the service
   * default). Lower it when each value spins up a resource-heavy runner. */
  max_concurrent?: number;
  /** Runs the values one at a time instead of in parallel (the simple form of
   * max_concurrent: 1); takes precedence over max_concurrent. */
  sequential?: boolean;
}

/** Fans a step out over the regex-matched paths of a shared workspace: each match is
 * one parallel leg running on its OWN clone (bound to ${scatter.path}), and owned
 * outputs are gathered back into the base afterward. Only valid on an inline step. */
export interface ScatterConfig {
  /** Base workspace volume to scan/clone (default "workspace"). */
  volume?: string;
  /** Mount path of the (cloned) workspace in each leg (default "/workspace"). */
  mount_path?: string;
  /** POSIX ERE matched against each entry's path relative to the volume root. */
  regex: string;
  /** Match directories ("dir", default) or files ("file"). */
  mode?: string;
  max_depth?: number;
  /** Paths each leg owns, unioned back into the base after all legs finish (may use
   * ${scatter.path}). Overlap across legs fails. Empty = gather nothing. */
  outputs?: string[];
  size_mb?: number;
  medium?: string;
  /** Caps how many legs run at once (0/undefined = the service default). */
  max_concurrent?: number;
}

/** An inline manual-approval gate: a pipeline pause point that needs no Step row.
 * A ref with an approval has no step_id and is always solo (no group/matrix). */
export interface ApprovalGate {
  message?: string;
  approvers?: string[];
}

/** A pipeline-level run parameter. Mirrors the workflows API WorkflowInputDef; kept
 * local so this module stays UI/library-free. */
export interface WorkflowInputDef {
  name: string;
  default?: string;
  required?: boolean;
  description?: string;
}

/** A pipeline-level output published on completion. `value` is a `${...}` template
 * (typically `${steps.STEP.output.KEY}`). Mirrors the API WorkflowOutputDef. */
export interface WorkflowOutputDef {
  name: string;
  value: string;
}

/** A pipeline step reference as the workflows API stores/accepts it — exactly one of
 * three kinds: a reference to a stored step (step_id), an INLINE step whose whole
 * definition lives on the ref (action, no step_id), or an inline approval gate. */
export interface StepRef {
  step_id?: string;
  /** Inline step only: the action it runs. When set (and no step_id), `name` is the
   * step name and `with` is its full config (not an override — nothing to merge over). */
  action?: string;
  /** Inline step only: per-step timeout in seconds (0/absent = service default). */
  timeout?: number;
  /** Inline step: the step name. Stored-step reference: per-occurrence name override
   * (empty/absent = use the step definition's name). */
  name?: string;
  /** Inline step: the full `with` config. Stored-step reference: per-occurrence
   * overrides merged over the step's own with at run time — how a pipeline wires a
   * step's inputs to upstream outputs. */
  with?: Record<string, unknown>;
  parallel_group?: number | null;
  matrix?: MatrixConfig | null;
  scatter?: ScatterConfig | null;
  approval?: ApprovalGate | null;
}

/** A directed edge between two steps, identified by STEP NAME — the same identity
 * `${steps.<name>.output}` uses, and the workflows API's WorkflowRoute.
 *
 * `when` is an expression (NOT `${...}` templating) deciding whether the edge is
 * taken, evaluated once `from` reaches a terminal state; empty means "taken iff
 * `from` completed". A step with no inbound route is an entry step. */
export interface Route {
  from: string;
  to: string;
  when?: string;
}

/** One step occurrence in the builder. `parallelWithPrev` links it into the same
 * stage as the block above (they run concurrently). The first block is always a
 * stage start, so its flag is forced false. `uid` is unique per occurrence so the
 * same step can appear more than once. `matrix` fans a solo block out over a list;
 * `approval` makes the block an inline manual-approval gate (no stepId).
 *
 * `parallelWithPrev` is the LEGACY ordered-list encoding: it is still read when
 * loading a pipeline authored before routes existed (so its edges can be derived
 * for display), but the graph editor writes routes instead and never sets it. */
export interface Block {
  uid: string;
  /** '' for an inline step and for a gate; the stored step id otherwise. */
  stepId: string;
  /** An INLINE step's definition, held on the block instead of the shared catalog
   * (so `stepId` is ''). `inline.with` is the definition config (edited by the step
   * form); `block.with` stays the per-occurrence override (input wiring) — exactly
   * mirroring a stored step's def/override split, so the same editors work. The two
   * layers are merged into one `with` when serialised (inline refs are single-layer). */
  inline?: { action: string; timeout?: number; with?: Record<string, unknown> };
  parallelWithPrev: boolean;
  /** Inline step: the step name. Stored reference: per-occurrence name override
   * (undefined = use the step definition's name). */
  name?: string;
  /** Per-occurrence `with` overrides (input wiring). undefined/empty = none. */
  with?: Record<string, unknown>;
  matrix?: MatrixConfig | null;
  scatter?: ScatterConfig | null;
  approval?: ApprovalGate | null;
}

/**
 * Collapse an ordered step list into sequential stages of step_ids. Mirrors the
 * CLI's tuiPipelineStages: only *consecutive* steps with the same non-nil
 * parallel_group merge into one stage.
 */
export function stagesFromSteps(steps: StepRef[]): string[][] {
  const stages: string[][] = [];
  let prevGroup: number | null | undefined = undefined;
  for (const s of steps) {
    const g = s.parallel_group ?? null;
    const id = s.step_id ?? ''; // inline gates have no step_id
    if (g !== null && g === prevGroup) {
      stages[stages.length - 1].push(id);
    } else {
      stages.push([id]);
    }
    prevGroup = g;
  }
  return stages;
}

/** Stored steps -> builder blocks. The first block of each stage starts the stage
 * (parallelWithPrev=false); any further blocks in a parallel stage link upward.
 * Block order matches the input order 1:1, so each block carries its step's matrix
 * by position (matrix only ever appears on solo, non-parallel steps). */
export function blocksFromSteps(steps: StepRef[]): Block[] {
  const blocks: Block[] = [];
  let i = 0;
  for (const stage of stagesFromSteps(steps)) {
    stage.forEach((stepId, idx) => {
      const s = steps[i];
      // An inline ref (action, no step_id, no gate) becomes an inline block. Its `with`
      // is split so the definition (config + input defaults) goes into the definition
      // layer (inline.with) while any per-occurrence PIPELINE fields (e.g. an attached
      // volume) go into the block override — mirroring a stored-step reference, which
      // keeps such fields in its override. Left in inline.with they would be dropped
      // the next time the inline step's definition is edited (the def form rebuilds
      // inline.with without pipeline fields).
      const isInline = !!s?.action && !s?.step_id && !s?.approval;
      let inline: Block['inline'];
      let blockWith: Record<string, unknown> | undefined;
      if (isInline) {
        const { def, pipeline } = splitPipelineWith(s!.action!, (s!.with ?? {}) as Record<string, unknown>);
        inline = { action: s!.action!, timeout: s!.timeout, with: Object.keys(def).length ? def : undefined };
        blockWith = Object.keys(pipeline).length ? pipeline : {};
      } else {
        blockWith = s?.with;
      }
      blocks.push({
        uid: `b${i}`, stepId, parallelWithPrev: idx > 0,
        name: s?.name || undefined,
        with: blockWith,
        inline,
        matrix: s?.matrix ?? null, scatter: s?.scatter ?? null, approval: s?.approval ?? null,
      });
      i++;
    });
  }
  return blocks;
}

/** Group blocks into stage bands: a run of [start, linked, linked…] is one stage.
 * The first block always starts a stage regardless of its flag. */
export function stagesOf(blocks: Block[]): Block[][] {
  const stages: Block[][] = [];
  blocks.forEach((b, idx) => {
    if (idx === 0 || !b.parallelWithPrev) stages.push([b]);
    else stages[stages.length - 1].push(b);
  });
  return stages;
}

/** Builder blocks -> ordered steps with parallel_group. Each stage band of >1
 * block gets a shared group; a solo block gets null. Order follows the blocks. A
 * solo block's matrix (mutually exclusive with parallel_group) is carried through
 * when it names a var, so an incomplete in-progress matrix is dropped silently. */
export function stepsFromBlocks(blocks: Block[]): StepRef[] {
  const out: StepRef[] = [];
  let group = 0;
  // The base ref for a block, before parallel_group/matrix. An inline block collapses
  // its two edit layers (inline.with def + block.with override) into one `with`.
  const baseRef = (b: Block): StepRef => {
    if (b.approval) return { approval: b.approval };
    if (b.inline) {
      const cfg = { ...(b.inline.with ?? {}), ...(b.with ?? {}) };
      const ref: StepRef = { action: b.inline.action, name: b.name };
      if (Object.keys(cfg).length > 0) ref.with = cfg;
      if (b.inline.timeout && b.inline.timeout > 0) ref.timeout = b.inline.timeout;
      return ref;
    }
    const ref: StepRef = { step_id: b.stepId };
    if (b.name) ref.name = b.name;
    if (b.with && Object.keys(b.with).length > 0) ref.with = b.with;
    return ref;
  };
  for (const stage of stagesOf(blocks)) {
    if (stage.length > 1) {
      const g = group++;
      for (const b of stage) {
        const ref = baseRef(b);
        // A gate can never be parallel, so it stays a solo gate even if grouped.
        if (!b.approval) ref.parallel_group = g;
        out.push(ref);
      }
    } else {
      const b = stage[0];
      const ref = baseRef(b);
      if (!b.approval) {
        if (!b.inline) ref.parallel_group = null;
        if (b.scatter && b.scatter.regex.trim()) ref.scatter = b.scatter;
        else if (b.matrix && b.matrix.var.trim()) ref.matrix = b.matrix;
      }
      out.push(ref);
    }
  }
  return out;
}

/** The resolved definition of a step node — from its own inline def (inline step)
 * or the shared catalog (stored reference). Undefined for a gate. */
export interface BlockDef { name: string; action: string; with?: Record<string, unknown>; timeout?: number }

export function blockDef(b: Block, catalog: Record<string, StepLike>): BlockDef | undefined {
  if (b.approval) return undefined;
  if (b.inline) return { name: b.name ?? '', action: b.inline.action, with: b.inline.with, timeout: b.inline.timeout };
  const s = catalog[b.stepId];
  return s ? { name: s.name, action: s.action, with: (s.with ?? {}) as Record<string, unknown>, timeout: s.timeout ?? undefined } : undefined;
}

/** The catalog shape blockDef needs — structurally the API's Step, declared here so
 * this module stays free of the API client and remains testable under mocha. */
export interface StepLike { name: string; action: string; with?: Record<string, unknown> | null; timeout?: number | null }

/** What the editor reports up when a node is selected — enough for the host's right
 * panel to render the step editor (an inline step edits its own def; a reference
 * edits the shared step) without reaching back into node state. */
export interface BlockSelection { uid: string; kind: 'gate' | 'ref' | 'inline'; stepId: string; name?: string; def?: BlockDef }

// ── The graph model ─────────────────────────────────────────────────────────────
// A pipeline is a graph: the steps are its nodes (identified by name) and `routes`
// are the edges. The ordered steps[]/parallel_group encoding is the legacy shape —
// still accepted by the backend, which derives the same edges from it at run time.
// These helpers mirror the Go engine (graph.go) so the editor draws exactly what
// the worker will execute.

/** The name a block is addressed by in the graph — its own name, or the referenced
 * step definition's name. This IS the node identity (it is already the
 * `${steps.<name>.output}` key), so the editor never invents a separate node id. */
export function nodeName(b: Block, defName: (id: string) => string | undefined): string {
  if (b.name && b.name.trim()) return b.name.trim();
  if (b.approval) return 'approval';
  return defName(b.stepId) ?? '';
}

/** Legacy ordered blocks -> the routes they imply, so a pipeline authored before
 * routes existed opens in the graph editor as the graph it already was.
 *
 * Mirrors deriveRoutes in the Go engine: the cross product between adjacent stages
 * IS the barrier — every node of a stage depends on every node of the one before.
 * Reuses stagesOf, so it decodes parallel_group exactly as the backend does. */
export function routesFromBlocks(blocks: Block[], defName: (id: string) => string | undefined): Route[] {
  const routes: Route[] = [];
  let prev: string[] = [];
  for (const stage of stagesOf(blocks)) {
    const cur = stage.map((b) => nodeName(b, defName)).filter(Boolean);
    for (const from of prev) for (const to of cur) routes.push({ from, to });
    if (cur.length > 0) prev = cur;
  }
  return routes;
}

/** A node placed on the canvas: `layer` is its column (depth from an entry step)
 * and `row` its position within that column. */
export interface Placed {
  uid: string;
  name: string;
  layer: number;
  row: number;
}

/**
 * Assign every node a layer and a row for rendering.
 *
 * layer(n) = 0 when n has no inbound route, else max(layer(pred)) + 1 — the
 * longest path from an entry, so an edge always points rightward and a join sits
 * past every branch that feeds it. Rows pack nodes within a layer in block order,
 * keeping the layout stable as the graph is edited.
 *
 * Cycles cannot be laid out; the iteration is bounded by the node count and any
 * node left in a cycle simply stops advancing (the editor flags the cycle
 * separately, and the backend rejects it).
 */
export function layoutGraph(blocks: Block[], routes: Route[], defName: (id: string) => string | undefined): Placed[] {
  const names = blocks.map((b) => nodeName(b, defName));
  const layer = new Map<string, number>();
  names.forEach((n) => layer.set(n, 0));
  // Relax layers until stable, bounded by the node count so a cycle terminates.
  for (let i = 0; i < names.length; i++) {
    let changed = false;
    for (const r of routes) {
      if (!layer.has(r.from) || !layer.has(r.to)) continue;
      const want = (layer.get(r.from) as number) + 1;
      if (want > (layer.get(r.to) as number)) {
        layer.set(r.to, want);
        changed = true;
      }
    }
    if (!changed) break;
  }
  const rowOf = new Map<number, number>();
  return blocks.map((b, i) => {
    const name = names[i];
    const l = layer.get(name) ?? 0;
    const row = rowOf.get(l) ?? 0;
    rowOf.set(l, row + 1);
    return { uid: b.uid, name, layer: l, row };
  });
}

/** Reports the cycle-forming routes, if any: a graph is acyclic exactly when a
 * topological sort (Kahn) can emit every node. Mirrors the backend's check so the
 * editor can refuse to save before the API does. Returns the names left unemitted. */
export function findCycle(blocks: Block[], routes: Route[], defName: (id: string) => string | undefined): string[] {
  const names = blocks.map((b) => nodeName(b, defName)).filter(Boolean);
  const known = new Set(names);
  const indeg = new Map<string, number>();
  names.forEach((n) => indeg.set(n, 0));
  const out = new Map<string, string[]>();
  for (const r of routes) {
    if (!known.has(r.from) || !known.has(r.to)) continue;
    indeg.set(r.to, (indeg.get(r.to) ?? 0) + 1);
    out.set(r.from, [...(out.get(r.from) ?? []), r.to]);
  }
  const queue = names.filter((n) => (indeg.get(n) ?? 0) === 0);
  let emitted = 0;
  while (queue.length > 0) {
    const n = queue.shift() as string;
    emitted++;
    for (const to of out.get(n) ?? []) {
      indeg.set(to, (indeg.get(to) as number) - 1);
      if (indeg.get(to) === 0) queue.push(to);
    }
  }
  if (emitted === names.length) return [];
  return names.filter((n) => (indeg.get(n) ?? 0) > 0);
}

/** Builder blocks -> step refs for a GRAPH pipeline: the same shape as
 * stepsFromBlocks minus parallel_group, which routes replace (the backend rejects
 * the two together). Order is preserved because it is still the step index the run
 * records join on. */
export function stepsFromNodes(blocks: Block[]): StepRef[] {
  return stepsFromBlocks(blocks.map((b) => ({ ...b, parallelWithPrev: false }))).map((ref) => {
    const { parallel_group: _drop, ...rest } = ref;
    return rest as StepRef;
  });
}

/** Drops routes whose endpoints no longer exist — e.g. after a step is deleted or
 * renamed — so a stale edge can never be saved. */
export function pruneRoutes(routes: Route[], names: string[]): Route[] {
  const known = new Set(names.filter(Boolean));
  return routes.filter((r) => known.has(r.from) && known.has(r.to));
}

// ── Pipeline config ⇄ JSON ──────────────────────────────────────────────────────
// The editor's right-hand panel shows the pipeline as the exact create/update API
// payload and lets it be edited back. These pure helpers are the bridge, shared by
// the panel and the save path so what you see is what is saved.

/** Maps builder StepRefs to the API's step payload shape: a parallel step keeps its
 * group; a solo step keeps a matrix only when it names a var; everything else is a
 * bare {step_id}. */
export function stepsToPayload(steps: StepRef[]): StepRef[] {
  return steps.map((s) => {
    let ref: StepRef;
    if (s.approval) ref = { approval: s.approval };
    else if (s.action) {
      // Inline step: action + full config (+ optional timeout), plus group/matrix.
      ref = { action: s.action };
      if (s.timeout && s.timeout > 0) ref.timeout = s.timeout;
      if (s.parallel_group != null) ref.parallel_group = s.parallel_group;
      else if (s.scatter && s.scatter.regex.trim()) ref.scatter = s.scatter;
      else if (s.matrix && s.matrix.var.trim()) ref.matrix = s.matrix;
    }
    else if (s.parallel_group != null) ref = { step_id: s.step_id, parallel_group: s.parallel_group };
    else if (s.matrix && s.matrix.var.trim()) ref = { step_id: s.step_id, matrix: s.matrix };
    else ref = { step_id: s.step_id };
    if (s.name) ref.name = s.name;
    if (s.with && Object.keys(s.with).length > 0) ref.with = s.with;
    return ref;
  });
}

/** Scans a step's `with` config for the ${...} references it consumes, so the
 * builder can show what a step pulls in: run inputs (`${inputs.X}` / bare `${X}`)
 * and upstream step outputs (`${steps.Y.output...}`). Matrix bindings are ignored
 * (they are supplied per-execution, not wired by the user). Recurses into nested
 * maps and arrays; returns deduped, order-preserved name lists. */
export function collectRefs(withMap: Record<string, unknown>): { inputs: string[]; steps: string[] } {
  const inputs = new Set<string>();
  const steps = new Set<string>();
  const walk = (v: unknown): void => {
    if (typeof v === 'string') {
      for (const m of v.matchAll(/\$\{([^}]+)\}/g)) {
        const expr = m[1].trim();
        if (expr.startsWith('steps.')) {
          const rest = expr.slice('steps.'.length);
          const dot = rest.indexOf('.output');
          if (dot > 0) steps.add(rest.slice(0, dot));
        } else if (expr.startsWith('inputs.')) {
          inputs.add(expr.slice('inputs.'.length));
        } else if (!expr.startsWith('matrix.')) {
          inputs.add(expr); // bare ${NAME} resolves to a run input
        }
      }
    } else if (Array.isArray(v)) {
      v.forEach(walk);
    } else if (v && typeof v === 'object') {
      Object.values(v as Record<string, unknown>).forEach(walk);
    }
  };
  walk(withMap);
  return { inputs: [...inputs], steps: [...steps] };
}

/** The effective ${steps.<name>.output} key for a step ref: its per-occurrence
 * name if set, else the step definition's name (resolved via defName). Approval
 * gates carry only an override name (they produce no output). '' when unnamed. */
export function effectiveStepName(ref: StepRef, defName: (stepId: string) => string | undefined): string {
  if (ref.name) return ref.name;
  if (ref.step_id) return defName(ref.step_id) ?? '';
  return '';
}

/** Effective names shared by more than one step. Such steps collide in a run's
 * output map (the later shadows the earlier), so ${steps.<name>.output} wiring is
 * ambiguous — the backend rejects it, and the builder blocks save. Order-preserved. */
export function duplicateStepNames(steps: StepRef[], defName: (stepId: string) => string | undefined): string[] {
  const counts = new Map<string, number>();
  for (const s of steps) {
    const n = effectiveStepName(s, defName);
    if (n) counts.set(n, (counts.get(n) ?? 0) + 1);
  }
  return [...counts.entries()].filter(([, c]) => c > 1).map(([n]) => n);
}

/** Sanitises pipeline-level input declarations for the payload/JSON: keeps only
 * named rows and drops empty optional fields. Returns undefined when none remain,
 * so the `inputs` key is omitted entirely. */
export function inputsToPayload(inputs: WorkflowInputDef[]): WorkflowInputDef[] | undefined {
  const out: WorkflowInputDef[] = [];
  for (const i of inputs) {
    const name = i.name.trim();
    if (!name) continue;
    const def: WorkflowInputDef = { name };
    if (i.default != null && i.default !== '') def.default = i.default;
    if (i.required) def.required = true;
    if (i.description != null && i.description !== '') def.description = i.description;
    out.push(def);
  }
  return out.length > 0 ? out : undefined;
}

/** Sanitises pipeline-level output declarations: keeps only rows with both a name
 * and a value. Returns undefined when none remain, so the `outputs` key is omitted. */
export function outputsToPayload(outputs: WorkflowOutputDef[]): WorkflowOutputDef[] | undefined {
  const out: WorkflowOutputDef[] = [];
  for (const o of outputs) {
    const name = o.name.trim();
    const value = o.value.trim();
    if (!name || !value) continue;
    out.push({ name, value });
  }
  return out.length > 0 ? out : undefined;
}

/** Renders the pipeline as the canonical config JSON (the saved payload).
 * description/inputs/outputs are omitted when empty. */
export function configToJson(name: string, description: string, steps: StepRef[], inputs: WorkflowInputDef[] = [], outputs: WorkflowOutputDef[] = []): string {
  const obj: Record<string, unknown> = { name };
  if (description) obj.description = description;
  const ins = inputsToPayload(inputs);
  if (ins) obj.inputs = ins;
  const outs = outputsToPayload(outputs);
  if (outs) obj.outputs = outs;
  obj.steps = stepsToPayload(steps);
  return JSON.stringify(obj, null, 2);
}

/** Reads pipeline-level input declarations back out of a parsed config object.
 * Skips malformed rows; returns undefined when there are none, so a config with no
 * inputs parses to an object without the key (matching an unset declaration). */
function parseInputDefs(v: unknown): WorkflowInputDef[] | undefined {
  if (!Array.isArray(v)) return undefined;
  const out: WorkflowInputDef[] = [];
  for (const item of v) {
    if (!item || typeof item !== 'object' || Array.isArray(item)) continue;
    const o = item as Record<string, unknown>;
    if (typeof o.name !== 'string' || !o.name) continue;
    const def: WorkflowInputDef = { name: o.name };
    if (typeof o.default === 'string') def.default = o.default;
    if (typeof o.required === 'boolean' && o.required) def.required = true;
    if (typeof o.description === 'string') def.description = o.description;
    out.push(def);
  }
  return out.length > 0 ? out : undefined;
}

/** Reads pipeline-level output declarations back out of a parsed config object.
 * Skips rows missing a name or value; returns undefined when there are none. */
function parseOutputDefs(v: unknown): WorkflowOutputDef[] | undefined {
  if (!Array.isArray(v)) return undefined;
  const out: WorkflowOutputDef[] = [];
  for (const item of v) {
    if (!item || typeof item !== 'object' || Array.isArray(item)) continue;
    const o = item as Record<string, unknown>;
    if (typeof o.name !== 'string' || !o.name || typeof o.value !== 'string') continue;
    out.push({ name: o.name, value: o.value });
  }
  return out.length > 0 ? out : undefined;
}

/** Parses an edited config JSON back into builder state, throwing a user-facing
 * Error on malformed JSON or an unexpected shape so the panel can surface it. The
 * inputs/outputs keys are present only when the config declares them. */
export function parseConfig(raw: string): { name: string; description: string; steps: StepRef[]; inputs?: WorkflowInputDef[]; outputs?: WorkflowOutputDef[] } {
  const obj: unknown = JSON.parse(raw);
  if (!obj || typeof obj !== 'object' || Array.isArray(obj)) throw new Error('config must be a JSON object');
  const o = obj as Record<string, unknown>;
  const rawSteps = o.steps ?? [];
  if (!Array.isArray(rawSteps)) throw new Error('"steps" must be an array');
  const steps: StepRef[] = rawSteps.map((s, i) => {
    if (!s || typeof s !== 'object' || Array.isArray(s)) throw new Error(`steps[${i}]: must be an object`);
    const so = s as Record<string, unknown>;
    const name = typeof so.name === 'string' && so.name ? so.name : undefined;
    const withOverride = (so.with && typeof so.with === 'object' && !Array.isArray(so.with)) ? so.with as Record<string, unknown> : undefined;
    // An inline approval gate has no step_id.
    if (so.approval && typeof so.approval === 'object' && !Array.isArray(so.approval)) {
      const a = so.approval as Record<string, unknown>;
      const gate: ApprovalGate = {};
      if (typeof a.message === 'string') gate.message = a.message;
      if (Array.isArray(a.approvers)) gate.approvers = a.approvers.filter((x): x is string => typeof x === 'string');
      return { parallel_group: null, approval: gate, ...(name ? { name } : {}) };
    }
    // An inline step carries its definition (action) instead of a step_id.
    if (typeof so.action === 'string' && so.action) {
      const ref: StepRef = { action: so.action, parallel_group: typeof so.parallel_group === 'number' ? so.parallel_group : null };
      if (name) ref.name = name;
      if (withOverride && Object.keys(withOverride).length > 0) ref.with = withOverride;
      if (typeof so.timeout === 'number' && so.timeout > 0) ref.timeout = so.timeout;
      if (so.matrix && typeof so.matrix === 'object' && !Array.isArray(so.matrix)) ref.matrix = so.matrix as MatrixConfig;
      if (so.scatter && typeof so.scatter === 'object' && !Array.isArray(so.scatter)) ref.scatter = so.scatter as ScatterConfig;
      return ref;
    }
    if (typeof so.step_id !== 'string' || !so.step_id) throw new Error(`steps[${i}]: a "step_id" string, an inline "action", or an "approval" gate is required`);
    const ref: StepRef = { step_id: so.step_id, parallel_group: typeof so.parallel_group === 'number' ? so.parallel_group : null };
    if (name) ref.name = name;
    if (withOverride && Object.keys(withOverride).length > 0) ref.with = withOverride;
    if (so.matrix && typeof so.matrix === 'object' && !Array.isArray(so.matrix)) ref.matrix = so.matrix as MatrixConfig;
    return ref;
  });
  const result: { name: string; description: string; steps: StepRef[]; inputs?: WorkflowInputDef[]; outputs?: WorkflowOutputDef[] } = {
    name: typeof o.name === 'string' ? o.name : '',
    description: typeof o.description === 'string' ? o.description : '',
    steps,
  };
  const inputs = parseInputDefs(o.inputs);
  if (inputs) result.inputs = inputs;
  const outputs = parseOutputDefs(o.outputs);
  if (outputs) result.outputs = outputs;
  return result;
}

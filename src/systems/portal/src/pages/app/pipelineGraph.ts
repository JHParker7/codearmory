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
 * mocha without the UI; the block component adapts these shapes.
 */

/** Fans a step out into one execution per value in a list, binding
 * ${matrix.<var>} per execution. Mutually exclusive with parallel_group. Mirrors
 * the workflows API MatrixConfig; kept local so this module stays UI/library-free. */
export interface MatrixConfig {
  var: string;
  values?: string[];
  values_from?: string;
}

/** An inline manual-approval gate: a pipeline pause point that needs no Step row.
 * A ref with an approval has no step_id and is always solo (no group/matrix). */
export interface ApprovalGate {
  message?: string;
  approvers?: string[];
}

/** A pipeline step reference as the workflows API stores/accepts it — EITHER a
 * reference to a stored step (step_id) OR an inline approval gate. */
export interface StepRef {
  step_id?: string;
  /** Per-occurrence name override; empty/absent = use the step definition's name. */
  name?: string;
  /** Per-occurrence overrides for the step's `with` config (merged over the step's
   * own with at run time) — how a pipeline wires a step's inputs to upstream outputs. */
  with?: Record<string, unknown>;
  parallel_group?: number | null;
  matrix?: MatrixConfig | null;
  approval?: ApprovalGate | null;
}

/** One step occurrence in the builder. `parallelWithPrev` links it into the same
 * stage as the block above (they run concurrently). The first block is always a
 * stage start, so its flag is forced false. `uid` is unique per occurrence so the
 * same step can appear more than once. `matrix` fans a solo block out over a list;
 * `approval` makes the block an inline manual-approval gate (no stepId). */
export interface Block {
  uid: string;
  stepId: string;
  parallelWithPrev: boolean;
  /** Per-occurrence name override (undefined = use the step definition's name). */
  name?: string;
  /** Per-occurrence `with` overrides (input wiring). undefined/empty = none. */
  with?: Record<string, unknown>;
  matrix?: MatrixConfig | null;
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
      blocks.push({ uid: `b${i}`, stepId, parallelWithPrev: idx > 0, name: steps[i]?.name || undefined, with: steps[i]?.with, matrix: steps[i]?.matrix ?? null, approval: steps[i]?.approval ?? null });
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
  const withExtras = (b: Block, ref: StepRef): StepRef => {
    const r = { ...ref };
    if (b.name) r.name = b.name;
    if (b.with && Object.keys(b.with).length > 0) r.with = b.with;
    return r;
  };
  for (const stage of stagesOf(blocks)) {
    if (stage.length > 1) {
      const g = group++;
      // A gate can never be parallel, so it stays a solo gate even if grouped.
      for (const b of stage) out.push(withExtras(b, b.approval ? { approval: b.approval } : { step_id: b.stepId, parallel_group: g }));
    } else {
      const b = stage[0];
      if (b.approval) { out.push(withExtras(b, { approval: b.approval })); continue; }
      const ref: StepRef = { step_id: b.stepId, parallel_group: null };
      if (b.matrix && b.matrix.var.trim()) ref.matrix = b.matrix;
      out.push(withExtras(b, ref));
    }
  }
  return out;
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

/** Renders the pipeline as the canonical config JSON (the saved payload).
 * description is omitted when empty. */
export function configToJson(name: string, description: string, steps: StepRef[]): string {
  const obj: Record<string, unknown> = { name };
  if (description) obj.description = description;
  obj.steps = stepsToPayload(steps);
  return JSON.stringify(obj, null, 2);
}

/** Parses an edited config JSON back into builder state, throwing a user-facing
 * Error on malformed JSON or an unexpected shape so the panel can surface it. */
export function parseConfig(raw: string): { name: string; description: string; steps: StepRef[] } {
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
    if (typeof so.step_id !== 'string' || !so.step_id) throw new Error(`steps[${i}]: a "step_id" string or an "approval" gate is required`);
    const ref: StepRef = { step_id: so.step_id, parallel_group: typeof so.parallel_group === 'number' ? so.parallel_group : null };
    if (name) ref.name = name;
    if (withOverride && Object.keys(withOverride).length > 0) ref.with = withOverride;
    if (so.matrix && typeof so.matrix === 'object' && !Array.isArray(so.matrix)) ref.matrix = so.matrix as MatrixConfig;
    return ref;
  });
  return {
    name: typeof o.name === 'string' ? o.name : '',
    description: typeof o.description === 'string' ? o.description : '',
    steps,
  };
}

/**
 * Pure mapping between the Scratch-style block builder and the workflows backend
 * model — the portal twin of the CLI's tuiPipelineStages.
 *
 * The backend stores a pipeline as an ordered list of steps plus `routes` — the
 * edges between them, which are the ONLY encoding of parallelism between steps
 * (two edges out of one node is a fork). A pipeline with no routes is a plain
 * sequence: the backend derives a linear chain in steps[] order. Step order still
 * matters because it is the step index a run's records join on.
 *
 * The builder is a set of blocks (one per step occurrence) wired by routes. This
 * module is the (pure, unit-tested) bridge between the two:
 *
 *   blocksFromSteps  ordered steps  -> blocks (1:1, in order)
 *   stepsFromBlocks  blocks         -> ordered steps
 *   routesFromBlocks blocks         -> the linear chain a route-less pipeline implies
 *
 * Node/library types are kept out of here so the mapping can be tested under
 * mocha without the UI; the block component adapts these shapes. (stepSchema is
 * likewise a pure, UI-free module, so importing its pipeline-field split keeps the
 * mocha-testability.)
 */
import { splitPipelineWith } from './stepSchema';

/** Fans a step out into one execution per value in a list, binding
 * ${matrix.<var>} per execution — a fan-out WITHIN one step, as distinct from
 * `routes`, which is parallelism BETWEEN steps. Mirrors the workflows API
 * MatrixConfig; kept local so this module stays UI/library-free. */
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
 * A ref with an approval has no step_id and never fans out (no matrix/scatter). */
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
  // Nullable because the API returns an enriched step with `with: null` when it has
  // no config; the editor treats null and absent alike.
  with?: Record<string, unknown> | null;
  matrix?: MatrixConfig | null;
  scatter?: ScatterConfig | null;
  approval?: ApprovalGate | null;
  /** The map region this step belongs to — see MapDef. Mutually exclusive with
   * matrix/scatter, which are the step's OWN fan-out. */
  map_id?: string;
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
  /** Optional human label for the edge (e.g. "if_discover_failed"), shown on the
   * branch instead of the raw condition. Display-only — the engine ignores it. */
  name?: string;
}

/** A map region: a SUBGRAPH repeated once per value. Steps join it by naming its id
 * in `mapId`, and the region's own routes are its body — so unlike a matrix (which
 * repeats ONE step) an iteration can build, then test, then conditionally push.
 *
 * `volume`, when set, gives each iteration its own CLONE of that workspace. That is
 * the reason the region exists rather than being composed from a matrix over
 * sub-pipelines: volumes are ReadWriteOnce, so parallel iterations cannot share a
 * checkout. Mirrors the workflows API MapDef. */
export interface MapDef {
  id: string;
  /** Each iteration sees its value as ${map.<var>}. */
  var: string;
  /** Exactly one of values / values_from — the latter is a ${...} reference resolved
   * when the region starts, which is what makes the fan-out dynamic. */
  values?: string[];
  values_from?: string;
  max_concurrent?: number;
  sequential?: boolean;
  /** Base workspace cloned per iteration; empty = no clone. */
  volume?: string;
  mount_path?: string;
  size_mb?: number;
  medium?: string;
  /** Paths each iteration owns, gathered back into the base afterward. */
  outputs?: string[];
}

/** One step occurrence in the builder. `uid` is unique per occurrence so the same
 * step can appear more than once. Concurrency with other blocks is not a property
 * of the block at all — it is the shape of the `routes` graph. `matrix`/`scatter`
 * fan the block out over a list (a fan-out within the one node); `approval` makes
 * the block an inline manual-approval gate (no stepId). */
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
  /** Inline step: the step name. Stored reference: per-occurrence name override
   * (undefined = use the step definition's name). */
  name?: string;
  /** Per-occurrence `with` overrides (input wiring). undefined/empty = none. */
  with?: Record<string, unknown>;
  matrix?: MatrixConfig | null;
  scatter?: ScatterConfig | null;
  approval?: ApprovalGate | null;
  /** The map region this node belongs to (undefined = none). */
  mapId?: string;
}

/** Stored steps -> builder blocks, 1:1 and in order. Each block carries its step's
 * matrix/scatter by position; the graph's shape lives in `routes`, not here. */
export function blocksFromSteps(steps: StepRef[]): Block[] {
  return steps.map((s, i) => {
    // An inline ref (action, no step_id, no gate) becomes an inline block. Its `with`
    // is split so the definition (config + input defaults) goes into the definition
    // layer (inline.with) while any per-occurrence PIPELINE fields (e.g. an attached
    // volume) go into the block override — mirroring a stored-step reference, which
    // keeps such fields in its override. Left in inline.with they would be dropped
    // the next time the inline step's definition is edited (the def form rebuilds
    // inline.with without pipeline fields).
    const isInline = !!s.action && !s.step_id && !s.approval;
    let inline: Block['inline'];
    let blockWith: Record<string, unknown> | undefined;
    if (isInline) {
      const { def, pipeline } = splitPipelineWith(s.action!, (s.with ?? {}) as Record<string, unknown>);
      inline = { action: s.action!, timeout: s.timeout, with: Object.keys(def).length ? def : undefined };
      blockWith = Object.keys(pipeline).length ? pipeline : {};
    } else {
      // The API returns `with: null` for a step with no config; the block model
      // uses undefined for "none", so normalise here.
      blockWith = s.with ?? undefined;
    }
    return {
      uid: `b${i}`,
      stepId: s.step_id ?? '', // inline steps and gates have no step_id
      name: s.name || undefined,
      with: blockWith,
      inline,
      matrix: s.matrix ?? null, scatter: s.scatter ?? null, approval: s.approval ?? null,
      mapId: s.map_id || undefined,
    };
  });
}

/** Builder blocks -> ordered steps, 1:1 and in order. A block's matrix/scatter is
 * carried through only when it is complete (a matrix names a var, a scatter has a
 * regex), so an incomplete in-progress fan-out is dropped silently. */
export function stepsFromBlocks(blocks: Block[]): StepRef[] {
  // The base ref for a block, before matrix/scatter. An inline block collapses its
  // two edit layers (inline.with def + block.with override) into one `with`.
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
  return blocks.map((b) => {
    const ref = baseRef(b);
    // A gate is a pause point, not an execution, so it never fans out.
    if (!b.approval) {
      if (b.scatter && b.scatter.regex.trim()) ref.scatter = b.scatter;
      else if (b.matrix && b.matrix.var.trim()) ref.matrix = b.matrix;
    }
    return ref;
  });
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
// are the edges. A pipeline that declares no routes is a plain sequence, whose
// linear chain the backend derives from steps[] order at run time.
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

/** The routes an ordered, route-less pipeline implies: the linear chain through its
 * blocks. Mirrors deriveRoutes in the Go engine, so a pipeline that declares no
 * routes opens in the graph editor as exactly the sequence the worker will run —
 * and saving it writes those edges out explicitly. */
export function routesFromBlocks(blocks: Block[], defName: (id: string) => string | undefined): Route[] {
  const names = blocks.map((b) => nodeName(b, defName)).filter(Boolean);
  const routes: Route[] = [];
  for (let i = 1; i < names.length; i++) routes.push({ from: names[i - 1], to: names[i] });
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

/** The prefix for a synthetic decision node's id — see displayGraph. A choice is not
 * a step; the canvas draws it as a diamond derived from a step's conditional routes,
 * so its id is namespaced to never collide with a real block uid. */
const DECISION_PREFIX = ' dec:';
export function decisionId(sourceName: string): string { return DECISION_PREFIX + sourceName; }
export function isDecisionId(id: string): boolean { return id.startsWith(DECISION_PREFIX); }

/** A node in the DRAWN graph: either a real step (kind 'step', carrying its block
 * uid) or a synthetic decision diamond (kind 'decision', carrying the name of the
 * step it branches from). layer/row place it on the canvas grid. */
export interface DisplayNode {
  id: string;
  kind: 'step' | 'decision' | 'map';
  uid?: string;
  name?: string;
  sourceName?: string;
  /** kind 'map' only: the region's id and the step names of its members, in order —
   * so the canvas can draw the collapsed region as one block listing its body. */
  mapId?: string;
  members?: string[];
  layer: number;
  row: number;
}

/** An edge in the DRAWN graph. routeIndex ties a drawn edge back to the underlying
 * Route (for selection/deletion); it is null for the plain step→decision link, which
 * is synthetic. `arm` marks the branch edges leaving a decision, so the renderer can
 * label them (the condition, or "else" for the unconditional default). */
export interface DisplayEdge {
  from: string;
  to: string;
  when?: string;
  name?: string;
  routeIndex: number | null;
  arm?: boolean;
}

/**
 * The graph as DRAWN, not as stored. A step whose out-routes include a conditional one
 * "branches": its routes are re-drawn through a synthetic decision diamond, so the
 * runner step never appears to carry the branch — mirroring the state-machine model,
 * where a choice is its own state. A step with only unconditional routes (a plain
 * sequence, or a parallel fork) is drawn directly, unchanged.
 *
 * This is a pure VIEW over (blocks, routes): the stored model is untouched, and every
 * drawn branch edge still carries the index of the Route it came from, so selecting or
 * editing a condition works exactly as before.
 */
export function displayGraph(blocks: Block[], routes: Route[], defName: (id: string) => string | undefined, collapseMaps = false): { nodes: DisplayNode[]; edges: DisplayEdge[] } {
  const uidOf = new Map<string, string>(); // step name -> block uid
  blocks.forEach((b) => uidOf.set(nodeName(b, defName), b.uid));

  // When collapsing, a whole map region is drawn as ONE node: its member steps share a
  // single node id, the routes wholly inside the region vanish (they are its body, not
  // top-level flow), and only the edges crossing the boundary remain — so the pipeline's
  // routes no longer thread through and around the region's individual steps.
  const mapOfName = new Map<string, string>();     // member step name -> map id
  const membersByMap = new Map<string, string[]>(); // map id -> member step names, in order
  if (collapseMaps) {
    blocks.forEach((b) => {
      if (!b.mapId) return;
      const nm = nodeName(b, defName);
      mapOfName.set(nm, b.mapId);
      if (!membersByMap.has(b.mapId)) membersByMap.set(b.mapId, []);
      membersByMap.get(b.mapId)!.push(nm);
    });
  }
  const mapNodeId = (mapId: string) => 'map:' + mapId;
  // The display-node id a step name resolves to (its map's node if collapsed, else its block).
  const resolve = (name: string): string | undefined => {
    const mid = mapOfName.get(name);
    return mid ? mapNodeId(mid) : uidOf.get(name);
  };

  // Resolved out-edges per SOURCE node id: internal-to-a-region edges are dropped, and
  // duplicate boundary edges (several members → the same outside step) are collapsed to one.
  const outByNode = new Map<string, { to: string; when?: string; name?: string; i: number }[]>();
  routes.forEach((r, i) => {
    const s = resolve(r.from), t = resolve(r.to);
    if (!s || !t || s === t) return;
    if (!outByNode.has(s)) outByNode.set(s, []);
    const arr = outByNode.get(s)!;
    if (arr.some((e) => e.to === t && e.when === r.when && e.name === r.name)) return;
    arr.push({ to: t, when: r.when, name: r.name, i });
  });
  const branchesNode = (id: string) => (outByNode.get(id) ?? []).some((e) => !!e.when);

  // Ordered node list (a decision follows its source) for stable row packing. A region is
  // emitted once, at its first member's position; the other members are skipped.
  const ordered: Omit<DisplayNode, 'layer' | 'row'>[] = [];
  const emittedMap = new Set<string>();
  blocks.forEach((b) => {
    const name = nodeName(b, defName);
    const mid = collapseMaps ? b.mapId : undefined;
    if (mid) {
      if (emittedMap.has(mid)) return;
      emittedMap.add(mid);
      const id = mapNodeId(mid);
      const members = membersByMap.get(mid) ?? [];
      ordered.push({ id, kind: 'map', mapId: mid, name: mid, members });
      if (branchesNode(id)) ordered.push({ id: decisionId(id), kind: 'decision', sourceName: mid });
    } else {
      ordered.push({ id: b.uid, kind: 'step', uid: b.uid, name });
      if (branchesNode(b.uid)) ordered.push({ id: decisionId(b.uid), kind: 'decision', sourceName: name });
    }
  });

  const edges: DisplayEdge[] = [];
  ordered.forEach((n) => {
    if (n.kind === 'decision') return;
    const outs = outByNode.get(n.id) ?? [];
    if (outs.length === 0) return;
    if (branchesNode(n.id)) {
      edges.push({ from: n.id, to: decisionId(n.id), routeIndex: null });
      outs.forEach((e) => edges.push({ from: decisionId(n.id), to: e.to, when: e.when, name: e.name, routeIndex: e.i, arm: true }));
    } else {
      outs.forEach((e) => edges.push({ from: n.id, to: e.to, when: e.when, name: e.name, routeIndex: e.i }));
    }
  });

  // Longest-path layering (mirrors layoutGraph), bounded by node count so a cycle
  // terminates; a decision sits one layer below its source, pushing its targets down.
  const ids = ordered.map((n) => n.id);
  const layer = new Map<string, number>();
  ids.forEach((id) => layer.set(id, 0));
  for (let k = 0; k < ids.length; k++) {
    let changed = false;
    for (const e of edges) {
      if (!layer.has(e.from) || !layer.has(e.to)) continue;
      const want = (layer.get(e.from) as number) + 1;
      if (want > (layer.get(e.to) as number)) { layer.set(e.to, want); changed = true; }
    }
    if (!changed) break;
  }
  // Order nodes WITHIN each layer to reduce edge crossings (a barycenter sweep, the
  // classic Sugiyama heuristic): a node drifts toward the average position of its
  // neighbours in the adjacent layer, so e.g. two siblings feeding the same joins end
  // up on the side that doesn't make their edges cross. Seeded from block order and
  // run a few down/up passes; stable, and a no-op when nothing crosses.
  const byLayer = new Map<number, string[]>();
  ordered.forEach((n) => {
    const l = layer.get(n.id) ?? 0;
    if (!byLayer.has(l)) byLayer.set(l, []);
    byLayer.get(l)!.push(n.id);
  });
  const maxLayer = Math.max(0, ...byLayer.keys());
  const preds = new Map<string, string[]>();
  const succs = new Map<string, string[]>();
  edges.forEach((e) => {
    if (!succs.has(e.from)) succs.set(e.from, []);
    succs.get(e.from)!.push(e.to);
    if (!preds.has(e.to)) preds.set(e.to, []);
    preds.get(e.to)!.push(e.from);
  });
  const pos = new Map<string, number>();
  const reindex = () => byLayer.forEach((ids) => ids.forEach((id, i) => pos.set(id, i)));
  reindex();
  const bary = (id: string, neigh: Map<string, string[]>): number => {
    const ns = neigh.get(id) ?? [];
    if (ns.length === 0) return pos.get(id) ?? 0;
    return ns.reduce((s, n) => s + (pos.get(n) ?? 0), 0) / ns.length;
  };
  for (let iter = 0; iter < 4; iter++) {
    for (let l = 1; l <= maxLayer; l++) {
      const ids = byLayer.get(l);
      if (ids) { ids.sort((a, b) => bary(a, preds) - bary(b, preds)); reindex(); }
    }
    for (let l = maxLayer - 1; l >= 0; l--) {
      const ids = byLayer.get(l);
      if (ids) { ids.sort((a, b) => bary(a, succs) - bary(b, succs)); reindex(); }
    }
  }
  // When maps are collapsed there are no member nodes to fence off, so column = barycenter
  // order and we skip the band machinery entirely.
  const col = new Map<string, number>();
  if (collapseMaps) {
    byLayer.forEach((ids) => ids.forEach((id) => col.set(id, pos.get(id) ?? 0)));
    const nodes: DisplayNode[] = ordered.map((n) => ({ ...n, layer: layer.get(n.id) ?? 0, row: col.get(n.id) ?? 0 }));
    return { nodes, edges };
  }

  // Reserve a column BAND for each map region so its enclosure — drawn as a plain
  // rectangle over the member cells — never wraps a non-member. A map's members occupy the
  // same columns on every layer, and the band is reserved across the region's WHOLE layer
  // span (min..max member layer, not only the layers that hold members), so any non-member
  // that falls between them is pushed sideways out of the rectangle rather than being
  // enclosed. Bands are ordered by their barycenter column so a region stays roughly where
  // the flow naturally places it. (Keeping members column-aligned is what lets the box stay
  // a clean rectangle once ranks are centred — see the shared per-group shift in the canvas.)
  const mapOf = new Map<string, string>();
  blocks.forEach((b) => {
    if (b.mapId) mapOf.set(b.uid, b.mapId);
  });
  const membersOf = new Map<string, string[]>();
  mapOf.forEach((m, id) => {
    if (!membersOf.has(m)) membersOf.set(m, []);
    membersOf.get(m)!.push(id);
  });
  const bandWidth = new Map<string, number>();
  const mapLo = new Map<string, number>();
  const mapHi = new Map<string, number>();
  membersOf.forEach((ids, m) => {
    const perLayer = new Map<number, number>();
    let lo = Infinity, hi = -Infinity;
    ids.forEach((id) => {
      const l = layer.get(id) ?? 0;
      perLayer.set(l, (perLayer.get(l) ?? 0) + 1);
      lo = Math.min(lo, l);
      hi = Math.max(hi, l);
    });
    bandWidth.set(m, Math.max(1, ...perLayer.values()));
    mapLo.set(m, lo);
    mapHi.set(m, hi);
  });
  const targetCol = (m: string): number => {
    const ids = membersOf.get(m)!;
    return ids.reduce((s, id) => s + (pos.get(id) ?? 0), 0) / ids.length;
  };
  const bandStart = new Map<string, number>();
  let cursor = 0;
  [...membersOf.keys()]
    .sort((a, b) => targetCol(a) - targetCol(b))
    .forEach((m) => {
      const start = Math.max(cursor, Math.round(targetCol(m)));
      bandStart.set(m, start);
      cursor = start + bandWidth.get(m)!;
    });

  byLayer.forEach((ids, l) => {
    const inOrder = [...ids].sort((a, b) => (pos.get(a) ?? 0) - (pos.get(b) ?? 0));
    const used = new Set<number>();
    const reserved = new Set<number>();
    // Reserve every band whose region spans this layer (even with no member here), so the
    // box's rectangle can hold no free node anywhere within its vertical extent.
    membersOf.forEach((_ids, m) => {
      if ((mapLo.get(m) ?? 0) <= l && l <= (mapHi.get(m) ?? 0)) {
        const base = bandStart.get(m) ?? 0;
        for (let i = 0; i < (bandWidth.get(m) ?? 1); i++) reserved.add(base + i);
      }
    });
    const memberIdx = new Map<string, number>();
    inOrder.forEach((id) => {
      const m = mapOf.get(id);
      if (!m) return;
      const idx = memberIdx.get(m) ?? 0;
      memberIdx.set(m, idx + 1);
      const c = (bandStart.get(m) ?? 0) + idx;
      col.set(id, c);
      used.add(c);
    });
    // Free nodes fill the remaining columns, skipping any reserved band column.
    let c = 0;
    inOrder.forEach((id) => {
      if (mapOf.has(id)) return;
      while (used.has(c) || reserved.has(c)) c++;
      col.set(id, c);
      used.add(c);
      c++;
    });
  });

  const nodes: DisplayNode[] = ordered.map((n) => ({ ...n, layer: layer.get(n.id) ?? 0, row: col.get(n.id) ?? pos.get(n.id) ?? 0 }));
  return { nodes, edges };
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

/** Builder blocks -> step refs for a GRAPH pipeline: stepsFromBlocks plus each
 * node's map region, which is carried on the block rather than the step ref. Order
 * is preserved because it is still the step index the run records join on. */
export function stepsFromNodes(blocks: Block[]): StepRef[] {
  return stepsFromBlocks(blocks).map((ref, i) => {
    if (blocks[i]?.mapId) ref.map_id = blocks[i].mapId;
    return ref;
  });
}

/** The step names belonging to each map region, in step order. */
export function regionMembers(blocks: Block[], defName: (id: string) => string | undefined): Record<string, string[]> {
  const out: Record<string, string[]> = {};
  for (const b of blocks) {
    if (!b.mapId) continue;
    (out[b.mapId] ??= []).push(nodeName(b, defName));
  }
  return out;
}

/** Drops regions no step belongs to, so removing the last member of a map removes
 * the map — the backend rejects a declared region with no members. */
export function pruneMaps(maps: MapDef[], blocks: Block[]): MapDef[] {
  const used = new Set(blocks.map((b) => b.mapId).filter(Boolean));
  return maps.filter((m) => used.has(m.id));
}

/** Client-side mirror of the backend's validateMaps, so the editor can show what a
 * save would reject rather than surfacing a 400 after the fact. Returns [] when valid. */
export function mapIssues(blocks: Block[], maps: MapDef[], routes: Route[], defName: (id: string) => string | undefined): string[] {
  const issues: string[] = [];
  const byID = new Map(maps.map((m) => [m.id, m]));
  for (const m of maps) {
    if (!m.var.trim()) issues.push(`map ${m.id}: needs a variable name`);
    const hasValues = (m.values ?? []).length > 0;
    const hasFrom = !!m.values_from?.trim();
    if (hasValues === hasFrom) issues.push(`map ${m.id}: set exactly one of values or values from`);
  }
  const regionOf: Record<string, string> = {};
  for (const b of blocks) {
    if (!b.mapId) continue;
    const name = nodeName(b, defName);
    regionOf[name] = b.mapId;
    if (!byID.has(b.mapId)) {
      issues.push(`${name}: belongs to unknown map ${b.mapId}`);
      continue;
    }
    // These would nest a fan-out inside the region's, or (for a gate) require
    // pausing each iteration independently — both rejected by the backend.
    if (b.matrix) issues.push(`${name}: a step in a map cannot also have a matrix`);
    if (b.scatter) issues.push(`${name}: a step in a map cannot also have a scatter`);
    if (b.approval) issues.push(`${name}: an approval gate cannot be inside a map`);
  }
  for (const r of routes) {
    const a = regionOf[r.from];
    const b = regionOf[r.to];
    if (a && b && a !== b) issues.push(`route ${r.from} → ${r.to}: cannot cross between maps ${a} and ${b}`);
  }
  return issues;
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

/** Maps builder StepRefs to the API's step payload shape: a step keeps a matrix or
 * scatter only when it is complete (a var / a regex); everything else is a bare
 * {step_id}. */
export function stepsToPayload(steps: StepRef[]): StepRef[] {
  return steps.map((s) => {
    let ref: StepRef;
    if (s.approval) ref = { approval: s.approval };
    else if (s.action) {
      // Inline step: action + full config (+ optional timeout), plus any fan-out.
      ref = { action: s.action };
      if (s.timeout && s.timeout > 0) ref.timeout = s.timeout;
      if (s.scatter && s.scatter.regex.trim()) ref.scatter = s.scatter;
      else if (s.matrix && s.matrix.var.trim()) ref.matrix = s.matrix;
    }
    else if (s.matrix && s.matrix.var.trim()) ref = { step_id: s.step_id, matrix: s.matrix };
    else ref = { step_id: s.step_id };
    if (s.name) ref.name = s.name;
    if (s.with && Object.keys(s.with).length > 0) ref.with = s.with;
    // Map membership is part of the pipeline's shape. This function rebuilds a ref
    // field by field, so anything not copied here is silently dropped on save —
    // omitting map_id would dissolve the region the moment the pipeline is re-saved.
    if (s.map_id) ref.map_id = s.map_id;
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
      return { approval: gate, ...(name ? { name } : {}) };
    }
    // An inline step carries its definition (action) instead of a step_id.
    if (typeof so.action === 'string' && so.action) {
      const ref: StepRef = { action: so.action };
      if (name) ref.name = name;
      if (withOverride && Object.keys(withOverride).length > 0) ref.with = withOverride;
      if (typeof so.timeout === 'number' && so.timeout > 0) ref.timeout = so.timeout;
      if (so.matrix && typeof so.matrix === 'object' && !Array.isArray(so.matrix)) ref.matrix = so.matrix as MatrixConfig;
      if (so.scatter && typeof so.scatter === 'object' && !Array.isArray(so.scatter)) ref.scatter = so.scatter as ScatterConfig;
      return ref;
    }
    if (typeof so.step_id !== 'string' || !so.step_id) throw new Error(`steps[${i}]: a "step_id" string, an inline "action", or an "approval" gate is required`);
    const ref: StepRef = { step_id: so.step_id };
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

/**
 * The pipeline graph editor: nodes are steps, edges ("routes") are what runs next.
 *
 * This replaces the Scratch-style block stack, where a pipeline was an ordered list
 * and a block could only be "linked" to the block directly above it. That encoding
 * is a linked list: it cannot express a step depending on two non-adjacent steps, a
 * diamond join, or a conditional branch — the reason routes exist.
 *
 * Layout is derived, not dragged: a node's ROW is its depth from an entry step
 * (layoutGraph), so the run flows DOWNWARD — an edge always points down the page and
 * a join always sits below every branch feeding it, with siblings spread across a
 * row. There are no free-floating coordinates to persist, and the drawing is a pure
 * function of the graph — what you see is what the worker executes, because the
 * layout mirrors the engine's own derivation.
 *
 * Editing:
 *   click a palette entry     add a node (an entry step until you route into it)
 *   click out-port -> in-port draw a route
 *   click a route             select it, to set a condition or delete it
 *   click a node              select it (the host shows the step editor)
 */
import { forwardRef, useCallback, useEffect, useImperativeHandle, useMemo, useRef, useState } from 'react';
import { T } from '../../theme';
import { useResizablePane } from '../../components/ResizeHandle';
import {
  layoutGraph, findCycle, nodeName, pruneRoutes, stepsFromNodes, routesFromBlocks, blocksFromSteps, blockDef,
  regionMembers, pruneMaps, mapIssues,
} from './pipelineGraph';
import type {
  Block, Route, StepRef, BlockSelection, MatrixConfig, ScatterConfig, ApprovalGate, MapDef,
} from './pipelineGraph';
import type { Step, WorkflowAction, GitRepo } from '../../api/bff';

/** Node box geometry. Kept here (not in theme) because the edge maths depends on it.
 * The flow is top-down, so GAP_Y is the tall one: that is where the edges live. */
const NODE_W = 190;
const NODE_H = 62;
const GAP_X = 30; // between siblings across a row
const GAP_Y = 76; // between depths — the edges run through here
const PAD = 28;

/** The canvas reports the same selection shape the block builder did, so the host's
 * step editor is unchanged by the switch to a graph. */
export type CanvasSelection = BlockSelection;

export interface PipelineCanvasHandle {
  patchBlock: (uid: string, patch: Partial<Block>) => void;
  getBlock: (uid: string) => Block | undefined;
}

interface PipelineCanvasProps {
  initialSteps: StepRef[];
  /** Stored routes. Empty for a legacy pipeline, whose edges are derived from its
   * ordered steps so it opens as the graph it already implicitly was. */
  initialRoutes?: Route[];
  /** The pipeline's map regions — see MapDef. */
  initialMaps?: MapDef[];
  catalog: Record<string, Step>;
  editable?: boolean;
  palette?: Step[];
  actions?: WorkflowAction[];
  /** The git-broker repo catalog, threaded through to the host's step editor. */
  repos?: GitRepo[];
  token?: string;
  onChange?: (steps: StepRef[], routes: Route[], maps: MapDef[]) => void;
  onInspect?: (selection: CanvasSelection | null) => void;
  onPickAction?: (action: WorkflowAction) => void;
  pendingAdd?: Step | null;
  onPendingConsumed?: () => void;
  /** Per-step run state, keyed by step NAME. When set the canvas draws a RUN: each
   * node takes its status colour and shows how many executions it fanned out into
   * (a matrix or map step has several). The same renderer draws the editor, the
   * pipeline detail and the run, so all three agree on the shape. */
  runStatus?: Record<string, { status: string; legs: number }>;
  /** Highlights the node whose logs the host is showing. */
  activeNode?: string | null;
}

const field: React.CSSProperties = {
  background: T.cardHi, border: `1px solid ${T.border}`, color: T.text,
  fontFamily: T.mono, fontSize: 11, padding: '4px 6px', outline: 'none',
};

/** Fans the step out over a list: a comma-separated literal, or a ${...} reference
 * resolved at run time. Typing in one source clears the other (the backend accepts
 * exactly one). The fan-out is internal to the node — its legs share one step index
 * and recombine into one output — so a matrix node is still a single node here. */
function MatrixEditor({ uid, matrix, onSet }: { uid: string; matrix: MatrixConfig; onSet: (uid: string, m: MatrixConfig | null) => void }) {
  return (
    <div style={{ padding: 8, background: T.bg, border: `1px dashed ${T.border}`, borderLeft: `3px solid ${T.amber}`, display: 'flex', flexDirection: 'column', gap: 6 }}>
      <div style={{ fontFamily: T.mono, fontSize: 9, color: T.amber, letterSpacing: 1, textTransform: 'uppercase' }}>⊞ matrix · one run per value</div>
      <input value={matrix.var} placeholder="var (e.g. region) → ${matrix.region}"
        onChange={(e) => onSet(uid, { ...matrix, var: e.target.value })} style={field} />
      <input value={(matrix.values ?? []).join(', ')} placeholder="values, comma-separated (a, b, c)"
        onChange={(e) => onSet(uid, { ...matrix, values: e.target.value.split(',').map((v) => v.trim()).filter(Boolean), values_from: undefined })} style={field} />
      <input value={matrix.values_from ?? ''} placeholder="or values from a reference (${inputs.regions})"
        onChange={(e) => onSet(uid, { ...matrix, values_from: e.target.value, values: e.target.value ? [] : matrix.values })} style={field} />
      <input type="number" min={0} value={matrix.max_concurrent ?? ''} placeholder="max concurrent (blank = default 3)" disabled={!!matrix.sequential}
        onChange={(e) => { const n = parseInt(e.target.value, 10); onSet(uid, { ...matrix, max_concurrent: Number.isFinite(n) && n > 0 ? n : undefined }); }}
        style={{ ...field, opacity: matrix.sequential ? 0.5 : 1 }} />
      <label style={{ display: 'flex', alignItems: 'center', gap: 6, fontFamily: T.mono, fontSize: 11, color: T.text, cursor: 'pointer' }}>
        <input type="checkbox" checked={!!matrix.sequential}
          onChange={(e) => onSet(uid, { ...matrix, sequential: e.target.checked || undefined })} />
        run sequentially (one at a time)
      </label>
    </div>
  );
}

/** Fans the step out over the workspace paths matching a regex — each leg on its own
 * clone (bound to ${scatter.path}), with owned outputs gathered back afterward. */
function ScatterEditor({ uid, scatter, onSet }: { uid: string; scatter: ScatterConfig; onSet: (uid: string, s: ScatterConfig | null) => void }) {
  return (
    <div style={{ padding: 8, background: T.bg, border: `1px dashed ${T.border}`, borderLeft: `3px solid ${T.green}`, display: 'flex', flexDirection: 'column', gap: 6 }}>
      <div style={{ fontFamily: T.mono, fontSize: 9, color: T.green, letterSpacing: 1, textTransform: 'uppercase' }}>⊟ scatter · one leg per matched path (own clone)</div>
      <input value={scatter.regex} placeholder="regex matching workspace paths (^services/[^/]+$)"
        onChange={(e) => onSet(uid, { ...scatter, regex: e.target.value })} style={field} />
      <div style={{ display: 'flex', gap: 6 }}>
        <select value={scatter.mode ?? 'dir'} onChange={(e) => onSet(uid, { ...scatter, mode: e.target.value })} style={{ ...field, flex: 1 }}>
          <option value="dir">dir</option>
          <option value="file">file</option>
        </select>
        <input value={scatter.volume ?? ''} placeholder="volume (default workspace)"
          onChange={(e) => onSet(uid, { ...scatter, volume: e.target.value || undefined })} style={{ ...field, flex: 1 }} />
      </div>
      <input value={(scatter.outputs ?? []).join(', ')} placeholder="owned outputs, comma-separated (${scatter.path}/dist) — gathered back"
        onChange={(e) => onSet(uid, { ...scatter, outputs: e.target.value.split(',').map((v) => v.trim()).filter(Boolean) })} style={field} />
      <input type="number" min={0} value={scatter.max_concurrent ?? ''} placeholder="max concurrent legs (blank = default 3)"
        onChange={(e) => { const n = parseInt(e.target.value, 10); onSet(uid, { ...scatter, max_concurrent: Number.isFinite(n) && n > 0 ? n : undefined }); }} style={field} />
    </div>
  );
}

/** A map region's fan-out: the value list its body repeats over, and the workspace
 * each iteration gets its own clone of. Unlike a matrix — which repeats one step —
 * every step assigned to this map repeats together, with the routes between them, so
 * an iteration can build then test then conditionally push. */
function MapEditor({ def, members, onSet }: { def: MapDef; members: string[]; onSet: (m: MapDef) => void }) {
  return (
    <div style={{ padding: 8, background: T.bg, border: `1px dashed ${T.border}`, borderLeft: `3px solid ${T.blue}`, display: 'flex', flexDirection: 'column', gap: 6 }}>
      <div style={{ fontFamily: T.mono, fontSize: 9, color: T.blue, letterSpacing: 1, textTransform: 'uppercase' }}>
        ⟳ map {def.id} · repeats {members.join(' → ') || '(no steps)'} per value
      </div>
      <input value={def.var} placeholder="var (e.g. dir) → ${map.dir}"
        onChange={(e) => onSet({ ...def, var: e.target.value })} style={field} />
      <input value={(def.values ?? []).join(', ')} placeholder="values, comma-separated (a, b, c)"
        onChange={(e) => onSet({ ...def, values: e.target.value.split(',').map((v) => v.trim()).filter(Boolean), values_from: undefined })} style={field} />
      <input value={def.values_from ?? ''} placeholder="or values from a step's output (${steps.discover.output.DIRS})"
        onChange={(e) => onSet({ ...def, values_from: e.target.value, values: e.target.value ? [] : def.values })} style={field} />
      <div style={{ display: 'flex', gap: 6 }}>
        <input value={def.volume ?? ''} placeholder="workspace to clone per iteration (blank = none)"
          onChange={(e) => onSet({ ...def, volume: e.target.value || undefined })} style={{ ...field, flex: 2 }} />
        <input type="number" min={0} value={def.max_concurrent ?? ''} placeholder="max at once (3)" disabled={!!def.sequential}
          onChange={(e) => { const n = parseInt(e.target.value, 10); onSet({ ...def, max_concurrent: Number.isFinite(n) && n > 0 ? n : undefined }); }}
          style={{ ...field, flex: 1, opacity: def.sequential ? 0.5 : 1 }} />
      </div>
      {def.volume && (
        <input value={(def.outputs ?? []).join(', ')} placeholder="owned outputs, comma-separated (${map.dir}/dist) — gathered back"
          onChange={(e) => onSet({ ...def, outputs: e.target.value.split(',').map((v) => v.trim()).filter(Boolean) })} style={field} />
      )}
      <label style={{ display: 'flex', alignItems: 'center', gap: 6, fontFamily: T.mono, fontSize: 11, color: T.text, cursor: 'pointer' }}>
        <input type="checkbox" checked={!!def.sequential}
          onChange={(e) => onSet({ ...def, sequential: e.target.checked || undefined })} />
        run iterations one at a time
      </label>
      <div style={{ fontSize: 10, color: T.faint, fontFamily: T.mono, lineHeight: 1.45 }}>
        Steps after the map wait for <b>every</b> iteration, and read all of them as a
        JSON array via <code>{'${steps.<step>.output}'}</code>. A cloned workspace gives each
        iteration its own checkout — parallel iterations cannot share one.
      </div>
    </div>
  );
}

/** An approval gate's prompt and optional approver allow-list. */
function GateEditor({ uid, gate, onSet }: { uid: string; gate: ApprovalGate; onSet: (uid: string, g: ApprovalGate) => void }) {
  return (
    <div style={{ padding: 8, background: T.bg, border: `1px dashed ${T.border}`, borderLeft: `3px solid ${T.blue}`, display: 'flex', flexDirection: 'column', gap: 6 }}>
      <div style={{ fontFamily: T.mono, fontSize: 9, color: T.blue, letterSpacing: 1, textTransform: 'uppercase' }}>⏸ pauses until approved</div>
      <input value={gate.message ?? ''} placeholder="message shown to approvers (e.g. deploy to prod?)"
        onChange={(e) => onSet(uid, { ...gate, message: e.target.value })} style={field} />
      <input value={(gate.approvers ?? []).join(', ')} placeholder="approvers (usernames, comma-separated; empty = anyone with permission)"
        onChange={(e) => onSet(uid, { ...gate, approvers: e.target.value.split(',').map((v) => v.trim()).filter(Boolean) })} style={field} />
    </div>
  );
}

/** A starting condition for a new route: the commonest branch is on the source
 * step's outcome, and it compiles, so the editor never seeds something the backend
 * would reject. */
function seedCondition(from: string): string {
  return `steps.${from}.status == "completed"`;
}

/** A cubic bezier from one node's bottom port to another's top port, bulging
 * vertically so sibling edges stay distinguishable rather than overlapping. */
function edgePath(x1: number, y1: number, x2: number, y2: number): string {
  const dy = Math.max(28, Math.abs(y2 - y1) * 0.5);
  return `M ${x1} ${y1} C ${x1} ${y1 + dy}, ${x2} ${y2 - dy}, ${x2} ${y2}`;
}

/** The outcome a route's condition selects, so a branch reads as pass / fail / other
 * at a glance instead of every conditional edge looking identical. A `status`
 * comparison against a success-y value (or `!=` a failure-y one) is the taken-on-
 * success branch; the opposite is the failure branch; anything else is a neutral
 * data condition. */
function conditionColor(when?: string): string {
  if (!when) return T.faint; // unconditional — taken when the source succeeds
  const m = when.match(/status\s*(==|!=)\s*["']([a-zA-Z_]+)["']/);
  if (m) {
    const pass = /^(completed|complete|success|succeeded|ok|passed)$/i.test(m[2]);
    const isEq = m[1] === '==';
    return (isEq && pass) || (!isEq && !pass) ? T.green : T.red;
  }
  return T.violet;
}

/** A short label drawn ON a conditional edge — the crux of the redesign: the branch
 * shows what it tests, not a generic badge. A `steps.X.status == "failed"` becomes
 * `failed`; other expressions are truncated. The full text stays in the tooltip and
 * the route inspector. */
function conditionLabel(when: string): string {
  const eq = when.match(/status\s*==\s*["']([a-zA-Z_]+)["']/);
  if (eq) return eq[1];
  const ne = when.match(/status\s*!=\s*["']([a-zA-Z_]+)["']/);
  if (ne) return `≠ ${ne[1]}`;
  const s = when.trim();
  return s.length > 22 ? s.slice(0, 21) + '…' : s;
}

type NodeShape = 'terminator' | 'gate' | 'decision' | 'fanout' | 'step';

/** The SVG outline for a node's box, so the graph uses real flow-chart symbols:
 * a rounded stadium for a run's entry/exit, a diamond for a decision (an approval
 * gate or a step that branches on a condition), a double-barred box for a fan-out,
 * and a plain rounded rect for an ordinary step. */
function shapeOutline(shape: NodeShape, x: number, y: number, w: number, h: number): string {
  const cx = x + w / 2, cy = y + h / 2;
  if (shape === 'gate' || shape === 'decision') {
    return `M ${cx} ${y} L ${x + w} ${cy} L ${cx} ${y + h} L ${x} ${cy} Z`;
  }
  // terminator = stadium (fully rounded ends); step/fanout = lightly rounded rect.
  const r = shape === 'terminator' ? h / 2 : 5;
  return `M ${x + r} ${y} L ${x + w - r} ${y} A ${r} ${r} 0 0 1 ${x + w} ${y + r}`
    + ` L ${x + w} ${y + h - r} A ${r} ${r} 0 0 1 ${x + w - r} ${y + h}`
    + ` L ${x + r} ${y + h} A ${r} ${r} 0 0 1 ${x} ${y + h - r}`
    + ` L ${x} ${y + r} A ${r} ${r} 0 0 1 ${x + r} ${y} Z`;
}

export const PipelineCanvas = forwardRef<PipelineCanvasHandle, PipelineCanvasProps>(function PipelineCanvas({
  initialSteps, initialRoutes, initialMaps, catalog, editable = false, palette = [], actions = [],
  onChange, onInspect, onPickAction, pendingAdd, onPendingConsumed, runStatus, activeNode,
}: PipelineCanvasProps, ref) {
  const defName = useCallback((id: string) => catalog[id]?.name, [catalog]);

  const [blocks, setBlocks] = useState<Block[]>(() => blocksFromSteps(initialSteps));
  const [routes, setRoutes] = useState<Route[]>(() =>
    initialRoutes && initialRoutes.length > 0
      ? initialRoutes
      : routesFromBlocks(blocksFromSteps(initialSteps), (id) => catalog[id]?.name));
  const [maps, setMaps] = useState<MapDef[]>(() => initialMaps ?? []);
  const [selectedUid, setSelectedUid] = useState<string | null>(null);
  const [selectedEdge, setSelectedEdge] = useState<number | null>(null);
  /** The out-port awaiting a target: the first half of drawing a route. */
  const [linkFrom, setLinkFrom] = useState<string | null>(null);
  /** A palette fan-out armed for the NEXT step added (only when nothing is selected).
   * Mirrors the old block palette's matrix/parallel "modes". */
  const [pendingFanout, setPendingFanout] = useState<'matrix' | 'scatter' | 'map' | null>(null);
  /** True when the palette's if/else is armed: the next route drawn gets a condition
   * and opens for editing, so branching does not require drawing then hunting. */
  const [pendingIf, setPendingIf] = useState(false);
  const seq = useRef(0);

  // Mirror blocks in a ref so the imperative handle reads current state (not a
  // stale closure) when the host converts / localises / edits an inline step's def.
  const blocksRef = useRef<Block[]>(blocks);
  blocksRef.current = blocks;
  useImperativeHandle(ref, () => ({
    patchBlock: (uid, patch) => setBlocks((bs) => bs.map((b) => (b.uid === uid ? { ...b, ...patch } : b))),
    getBlock: (uid) => blocksRef.current.find((b) => b.uid === uid),
  }), []);

  const splitRef = useRef<HTMLDivElement>(null);
  const [paletteW, paletteHandle] = useResizablePane('split.pipelinecanvas.palette.w', 260, {
    min: 200, max: 460, side: 'left', direction: 'horizontal', containerRef: splitRef, otherMin: 320,
  });

  useEffect(() => {
    const bs = blocksFromSteps(initialSteps);
    setBlocks(bs);
    setRoutes(initialRoutes && initialRoutes.length > 0 ? initialRoutes : routesFromBlocks(bs, (id) => catalog[id]?.name));
    setMaps(initialMaps ?? []);
    setSelectedUid(null); setSelectedEdge(null); setLinkFrom(null); setPendingFanout(null); setPendingIf(false);
  }, [initialSteps, initialRoutes, initialMaps, catalog]);

  // Report the graph up on every edit. Routes are pruned first so a deleted or
  // renamed step can never leave an edge pointing at nothing.
  useEffect(() => {
    if (!editable || !onChange) return;
    const names = blocks.map((b) => nodeName(b, defName));
    // Prune first: a deleted or renamed step must not leave an edge pointing at
    // nothing, and dropping a map's last member drops the map (the backend rejects
    // a declared region with no steps).
    onChange(stepsFromNodes(blocks), pruneRoutes(routes, names), pruneMaps(maps, blocks));
  }, [blocks, routes, maps, editable, onChange, defName]);

  const placed = useMemo(() => layoutGraph(blocks, routes, defName), [blocks, routes, defName]);
  const posOf = useMemo(() => {
    const m = new Map<string, { x: number; y: number }>();
    placed.forEach((p) => m.set(p.uid, {
      x: PAD + p.row * (NODE_W + GAP_X),
      y: PAD + p.layer * (NODE_H + GAP_Y),
    }));
    return m;
  }, [placed]);
  const byName = useMemo(() => {
    const m = new Map<string, string>(); // name -> uid
    blocks.forEach((b) => m.set(nodeName(b, defName), b.uid));
    return m;
  }, [blocks, defName]);

  /** Edges leaving one node are spread across its lower edge (and edges entering a
   * join spread across its upper edge) so siblings fan out to distinct points rather
   * than stacking on the exact centre — the main reason the old drawing was hard to
   * read. Each name maps to the ordered route indices out of / into it. */
  const edgeGeom = useMemo(() => {
    const outByFrom = new Map<string, number[]>();
    const inByTo = new Map<string, number[]>();
    routes.forEach((r, i) => {
      if (!outByFrom.has(r.from)) outByFrom.set(r.from, []);
      outByFrom.get(r.from)!.push(i);
      if (!inByTo.has(r.to)) inByTo.set(r.to, []);
      inByTo.get(r.to)!.push(i);
    });
    return { outByFrom, inByTo };
  }, [routes]);

  /** Names that branch on a condition (an if/else): drawn as a decision diamond. */
  const decisionNames = useMemo(() => {
    const s = new Set<string>();
    routes.forEach((r) => { if (r.when) s.add(r.from); });
    return s;
  }, [routes]);

  /** The endpoint x for a route: slotted across the source's lower / target's upper
   * edge by this route's position among its siblings. A lone edge stays centred. */
  const slotX = (nodeX: number, siblings: number[], routeIdx: number): number => {
    const k = siblings.indexOf(routeIdx);
    const n = siblings.length;
    return nodeX + (NODE_W * (k + 1)) / (n + 1);
  };

  const selectedNode = useMemo(() => blocks.find((b) => b.uid === selectedUid) ?? null, [blocks, selectedUid]);
  const cycle = useMemo(() => findCycle(blocks, routes, defName), [blocks, routes, defName]);
  const members = useMemo(() => regionMembers(blocks, defName), [blocks, defName]);
  const issues = useMemo(() => mapIssues(blocks, maps, routes, defName), [blocks, maps, routes, defName]);

  /** The bounding box of each region's nodes, so a map reads as an enclosure rather
   * than a per-node label — its body is a subgraph, and the drawing should say so. */
  const regionBoxes = useMemo(() => {
    return maps.map((m) => {
      const pts = blocks.filter((b) => b.mapId === m.id).map((b) => posOf.get(b.uid)).filter(Boolean) as { x: number; y: number }[];
      if (pts.length === 0) return null;
      const x = Math.min(...pts.map((p) => p.x)) - 14;
      const y = Math.min(...pts.map((p) => p.y)) - 22;
      const x2 = Math.max(...pts.map((p) => p.x)) + NODE_W + 14;
      const y2 = Math.max(...pts.map((p) => p.y)) + NODE_H + 14;
      return { def: m, x, y, w: x2 - x, h: y2 - y };
    }).filter(Boolean) as { def: MapDef; x: number; y: number; w: number; h: number }[];
  }, [maps, blocks, posOf]);
  const width = Math.max(...placed.map((p) => PAD * 2 + (p.row + 1) * (NODE_W + GAP_X)), 400);
  const height = Math.max(...placed.map((p) => PAD * 2 + (p.layer + 1) * (NODE_H + GAP_Y)), 260);

  const addStep = (stepId: string, name: string) => {
    const uid = `n${seq.current++}-${Date.now()}`;
    setBlocks((bs) => [...bs, { uid, stepId, name: undefined }]);
    consumeFanout(uid);
    void name;
  };

  const addInline = (action: string, name: string, w?: Record<string, unknown>, timeout?: number) => {
    const uid = `n${seq.current++}-${Date.now()}`;
    // Names are node identities, so a collision would silently merge two nodes.
    const taken = new Set(blocks.map((b) => nodeName(b, defName)));
    let unique = name; let n = 2;
    while (taken.has(unique)) unique = `${name}-${n++}`;
    setBlocks((bs) => [...bs, { uid, stepId: '', name: unique, inline: { action, with: w, timeout } }]);
    consumeFanout(uid);
  };

  useEffect(() => {
    if (!pendingAdd) return;
    addInline(pendingAdd.action, pendingAdd.name, pendingAdd.with ?? undefined, pendingAdd.timeout ?? undefined);
    onPendingConsumed?.();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [pendingAdd]);

  /** Add a manual-approval gate. A gate carries an `approval` config and no action:
   * the backend rejects an inline step whose action is "approval", so it must not be
   * built as one. */
  const addGate = () => {
    const uid = `n${seq.current++}-${Date.now()}`;
    const taken = new Set(blocks.map((b) => nodeName(b, defName)));
    let unique = 'approval'; let n = 2;
    while (taken.has(unique)) unique = `approval-${n++}`;
    setBlocks((bs) => [...bs, { uid, stepId: '', name: unique, approval: {} }]);
  };

  // Matrix and scatter are both a fan-out OF ONE NODE, so they are mutually
  // exclusive with each other — but no longer with parallelism, which is now the
  // graph's shape rather than a field on the step.
  const setMatrix = (uid: string, m: MatrixConfig | null) =>
    setBlocks((bs) => bs.map((b) => (b.uid === uid ? { ...b, matrix: m, scatter: m ? null : b.scatter } : b)));
  const setScatter = (uid: string, s: ScatterConfig | null) =>
    setBlocks((bs) => bs.map((b) => (b.uid === uid ? { ...b, scatter: s, matrix: s ? null : b.matrix } : b)));
  const setApproval = (uid: string, g: ApprovalGate) =>
    setBlocks((bs) => bs.map((b) => (b.uid === uid ? { ...b, approval: g } : b)));

  /** Put a node in a map region (or take it out). Assigning clears the node's own
   * matrix/scatter: those are the STEP's fan-out and would nest inside the region's,
   * which the backend rejects. */
  const setNodeMap = (uid: string, mapID: string | undefined) => {
    setBlocks((bs) => bs.map((b) => (b.uid === uid ? { ...b, mapId: mapID, matrix: mapID ? null : b.matrix, scatter: mapID ? null : b.scatter } : b)));
  };

  /** Create a region and put the node in it. Ids are internal, so they are generated
   * rather than typed — the user names the VARIABLE, which is what they reference. */
  const addMapWithNode = (uid: string) => {
    const taken = new Set(maps.map((m) => m.id));
    let id = 'map1'; let n = 2;
    while (taken.has(id)) id = `map${n++}`;
    setMaps((ms) => [...ms, { id, var: '', values: [] }]);
    setNodeMap(uid, id);
  };

  const setMapDef = (def: MapDef) => setMaps((ms) => ms.map((m) => (m.id === def.id ? def : m)));

  /** Give a step a fan-out from the palette. With a step selected it toggles on that
   * step; with nothing selected it ARMS, and the next step added takes it — which is
   * how the old block palette's modes behaved.
   *
   * A map is the exception to "toggle": joining an EXISTING map is the common case
   * (that is what makes a multi-step body), so it adds to the most recent map rather
   * than always creating a new one. */
  const applyFanout = (kind: 'matrix' | 'scatter' | 'map') => {
    if (!selectedNode) {
      setPendingFanout((cur) => (cur === kind ? null : kind));
      return;
    }
    const uid = selectedNode.uid;
    if (kind === 'matrix') {
      setMatrix(uid, selectedNode.matrix ? null : { var: '', values: [] });
    } else if (kind === 'scatter') {
      if (!selectedNode.inline) return; // scatter needs a pipeline-local step
      setScatter(uid, selectedNode.scatter ? null : { regex: '', mode: 'dir' });
    } else if (selectedNode.mapId) {
      setNodeMap(uid, undefined);
    } else if (maps.length > 0) {
      setNodeMap(uid, maps[maps.length - 1].id);
    } else {
      addMapWithNode(uid);
    }
  };

  /** Give a route a condition from the palette. With a route selected it seeds one
   * there; otherwise it ARMS, and the next route drawn gets one — mirroring how the
   * fan-out buttons arm.
   *
   * A condition belongs to an EDGE, not a step: "if/else" in a graph is two routes
   * out of one step, each with a condition. So this seeds the condition rather than
   * adding an "if" node.
   */
  const addIfElse = () => {
    if (selectedEdge != null && routes[selectedEdge]) {
      if (!routes[selectedEdge].when) patchRoute(selectedEdge, { when: seedCondition(routes[selectedEdge].from) });
      return;
    }
    setPendingIf((v) => !v);
  };

  /** Apply an armed palette fan-out to a freshly added step, then disarm. */
  const consumeFanout = (uid: string) => {
    if (!pendingFanout) return;
    const kind = pendingFanout;
    setPendingFanout(null);
    if (kind === 'matrix') setMatrix(uid, { var: '', values: [] });
    else if (kind === 'scatter') setScatter(uid, { regex: '', mode: 'dir' });
    else if (maps.length > 0) setNodeMap(uid, maps[maps.length - 1].id);
    else addMapWithNode(uid);
  };

  const removeNode = (uid: string) => {
    const name = nodeName(blocks.find((b) => b.uid === uid) as Block, defName);
    setBlocks((bs) => bs.filter((b) => b.uid !== uid));
    setRoutes((rs) => rs.filter((r) => r.from !== name && r.to !== name));
    if (selectedUid === uid) { setSelectedUid(null); onInspect?.(null); }
  };

  /** Complete a link: from's out-port was clicked, now to's in-port. Rejects the
   * self-edge and the duplicate here so the canvas never holds a graph the backend
   * would refuse. */
  const completeLink = (toUid: string) => {
    if (!linkFrom) return;
    const from = nodeName(blocks.find((b) => b.uid === linkFrom) as Block, defName);
    const to = nodeName(blocks.find((b) => b.uid === toUid) as Block, defName);
    setLinkFrom(null);
    if (!from || !to || from === to) return;
    setRoutes((rs) => {
      if (rs.some((r) => r.from === from && r.to === to)) return rs;
      const next = [...rs, pendingIf ? { from, to, when: seedCondition(from) } : { from, to }];
      if (pendingIf) {
        // Select the new route so its condition is there to edit straight away.
        setSelectedEdge(next.length - 1);
        setSelectedUid(null);
        setPendingIf(false);
      }
      return next;
    });
  };

  /** Mirrors PipelineBlocks' selectionFor: a gate has no step to edit, an inline
   * step edits its own def, a reference edits the shared step. */
  const select = (b: Block) => {
    setSelectedUid(b.uid);
    setSelectedEdge(null);
    if (b.approval) {
      onInspect?.({ uid: b.uid, kind: 'gate', stepId: '' });
      return;
    }
    onInspect?.({
      uid: b.uid,
      kind: b.inline ? 'inline' : 'ref',
      stepId: b.stepId,
      name: nodeName(b, defName),
      def: blockDef(b, catalog),
    });
  };

  const patchRoute = (i: number, patch: Partial<Route>) =>
    setRoutes((rs) => rs.map((r, j) => (j === i ? { ...r, ...patch } : r)));

  return (
    <div ref={splitRef} style={{ display: 'flex', height: '100%', minHeight: 0, border: `1px solid ${T.border}`, background: T.bg }}>
      {editable && (
        <>
          <div style={{ width: paletteW, flexShrink: 0, overflowY: 'auto', borderRight: `1px solid ${T.border}`, background: T.bgAlt, padding: 10 }}>
            {/* Fan-out is a property OF a step, not a step you add — so these apply to
                the selected step, or arm the next one you add (as the old block
                palette's modes did). Kept on the palette because that is where they
                are looked for. */}
            <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 0.5, marginBottom: 8 }}>
              FAN OUT {selectedNode ? `· ${nodeName(selectedNode, defName) || 'selected step'}` : '· next step added'}
            </div>
            {([
              { kind: 'matrix' as const, label: '⊞ matrix', hint: 'one run per value, of this step alone', color: T.amber, on: !!selectedNode?.matrix },
              { kind: 'scatter' as const, label: '⊟ scatter', hint: 'one leg per workspace path, each on its own clone', color: T.green, on: !!selectedNode?.scatter },
              { kind: 'map' as const, label: '⟳ map', hint: 'repeat SEVERAL steps per value — add others to the same map', color: T.blue, on: !!selectedNode?.mapId },
            ]).map((f) => {
              const armed = pendingFanout === f.kind;
              const active = f.on || armed;
              return (
                <button key={f.kind} onClick={() => applyFanout(f.kind)} title={f.hint}
                  style={{ ...paletteBtn, borderColor: active ? f.color : T.border, color: active ? f.color : T.text }}>
                  {f.label}{armed ? ' · arming…' : ''}
                </button>
              );
            })}
            <div style={{ fontSize: 10, color: T.faint, lineHeight: 1.4, fontFamily: T.mono, margin: '2px 0 12px' }}>
              {selectedNode ? 'applies to the selected step' : 'select a step, or click one of these then add a step'}
            </div>

            {/* Flow control: the two things that change what runs NEXT, rather than
                what a step does. Grouped because they are reached for together. */}
            <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 0.5, marginBottom: 8 }}>FLOW</div>
            <button onClick={addGate} style={paletteBtn} title="pause the run here until someone approves">
              ⏸ approval gate
            </button>
            <button onClick={addIfElse} style={{ ...paletteBtn, borderColor: pendingIf ? T.green : T.border, color: pendingIf ? T.green : T.text }}
              title={selectedEdge != null ? 'give this route a condition' : 'the next route you draw gets a condition'}>
              ⑂ if / else{pendingIf ? ' · arming…' : ''}
            </button>
            <div style={{ fontSize: 10, color: T.faint, lineHeight: 1.4, fontFamily: T.mono, margin: '2px 0 12px' }}>
              {selectedEdge != null
                ? 'adds a condition to the selected route'
                : 'route a step to two steps, then give each route a condition'}
            </div>

            <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 0.5, marginBottom: 8 }}>ADD STEP</div>
            {actions.map((a) => (
              <button key={a.name} onClick={() => onPickAction?.(a)} style={paletteBtn} title={a.summary ?? undefined}>
                {a.name}
              </button>
            ))}
            {palette.map((p) => (
              <button key={p.step_id} onClick={() => addStep(p.step_id, p.name)} style={paletteBtn}>
                {p.name}
              </button>
            ))}
            <div style={{ marginTop: 14, fontSize: 10.5, color: T.faint, lineHeight: 1.5, fontFamily: T.mono }}>
              Click a step's lower <span style={{ color: T.green }}>▾</span> then another step's
              upper <span style={{ color: T.green }}>▾</span> to route between them. A step with no
              incoming route starts the run; the flow runs top to bottom.
              <div style={{ marginTop: 8 }}>
                <span style={{ color: T.green }}>∥ parallel</span> is the shape of the graph:
                route one step to <b>two</b> steps and they run at the same time. Route both
                into a third and it waits for both.
              </div>
              <div style={{ marginTop: 8 }}>
                Select a step to fan it out (<span style={{ color: T.amber }}>⊞ matrix</span> /{' '}
                <span style={{ color: T.green }}>⊟ scatter</span>), or a route to give it a condition.
              </div>
            </div>
          </div>
          <div {...paletteHandle} />
        </>
      )}

      <div style={{ flex: 1, minWidth: 0, overflow: 'auto', position: 'relative' }}>
        {/* What a save would be rejected for, shown before the API says so. */}
        {(cycle.length > 0 || issues.length > 0) && (
          <div style={{ position: 'sticky', top: 0, zIndex: 5, background: T.redSoft, color: T.red, padding: '6px 10px', fontFamily: T.mono, fontSize: 11, lineHeight: 1.5 }}>
            {cycle.length > 0 && <div>routes form a cycle involving: {cycle.join(', ')} — the run would never start</div>}
            {issues.map((m) => <div key={m}>{m}</div>)}
          </div>
        )}
        {blocks.length === 0 && (
          <div style={{ padding: 24, fontFamily: T.mono, fontSize: 12, color: T.faint }}>
            No steps yet — add one from the palette.
          </div>
        )}

        <div style={{ position: 'relative', width, height }}>
          {/* Region enclosures, behind everything: a map's body is a subgraph, so it
              is drawn as a box around its steps rather than a badge on each one. */}
          {regionBoxes.map((r) => (
            <div key={r.def.id} style={{
              position: 'absolute', left: r.x, top: r.y, width: r.w, height: r.h,
              border: `1px dashed ${T.blue}`, background: T.blueSoft, pointerEvents: 'none',
            }}>
              <span style={{ position: 'absolute', top: -8, left: 8, background: T.bg, padding: '0 4px', fontFamily: T.mono, fontSize: 9, color: T.blue }}>
                ⟳ map · per {r.def.var || '?'}
                {r.def.volume ? ' · own workspace' : ''}
                {r.def.sequential ? ' · one at a time' : ''}
              </span>
            </div>
          ))}
          {/* Node shapes + routing edges, under the interactive node layer so a route
              never covers a step's text and the shapes read as flow-chart symbols
              behind the labels and ports. */}
          <svg width={width} height={height} style={{ position: 'absolute', inset: 0, pointerEvents: 'none' }}>
            <defs>
              {([['faint', T.faint], ['green', T.green], ['red', T.red], ['violet', T.violet]] as const).map(([id, col]) => (
                <marker key={id} id={`arr-${id}`} viewBox="0 0 8 8" refX="7" refY="4" markerWidth="7.5" markerHeight="7.5" orient="auto-start-reverse">
                  <path d="M 0 0 L 8 4 L 0 8 z" fill={col} />
                </marker>
              ))}
            </defs>

            {/* Node outlines: stadium = a run's start/end, diamond = a decision (an
                approval gate or a step that branches on a condition), double-barred
                box = a fan-out, rounded rect = an ordinary step. Drawn first so the
                edges, labels and ports sit on top. */}
            {blocks.map((b) => {
              const p = posOf.get(b.uid);
              if (!p) return null;
              const name = nodeName(b, defName);
              const action = b.inline?.action ?? catalog[b.stepId]?.action ?? '';
              const isGate = action === 'approval' || !!b.approval;
              const isDecision = decisionNames.has(name);
              const isEntry = !routes.some((r) => r.to === name);
              const isTerminal = !routes.some((r) => r.from === name);
              const isFanout = !!b.matrix || !!b.scatter;
              const shape: NodeShape = isGate ? 'gate' : isDecision ? 'decision'
                : isEntry || isTerminal ? 'terminator' : isFanout ? 'fanout' : 'step';
              const run = runStatus?.[name];
              const on = selectedUid === b.uid || activeNode === name;
              const stroke = on ? T.green
                : run ? runColor(run.status)
                : isGate ? T.blue
                : isDecision ? T.violet
                : isEntry ? T.green
                : b.matrix ? T.amber
                : b.scatter ? T.green
                : T.border;
              const fill = on ? T.greenSoft : isGate ? T.blueSoft : isDecision ? T.violetSoft : T.bgAlt;
              return (
                <g key={`shape-${b.uid}`}>
                  <path d={shapeOutline(shape, p.x, p.y, NODE_W, NODE_H)}
                    fill={fill} stroke={stroke} strokeWidth={on ? 2 : 1.4} />
                  {shape === 'fanout' && (
                    <>
                      <line x1={p.x + 7} y1={p.y} x2={p.x + 7} y2={p.y + NODE_H} stroke={stroke} strokeWidth={1} />
                      <line x1={p.x + NODE_W - 7} y1={p.y} x2={p.x + NODE_W - 7} y2={p.y + NODE_H} stroke={stroke} strokeWidth={1} />
                    </>
                  )}
                </g>
              );
            })}

            {routes.map((r, i) => {
              const a = posOf.get(byName.get(r.from) ?? '');
              const b = posOf.get(byName.get(r.to) ?? '');
              if (!a || !b) return null; // endpoint gone; pruned on save
              const outs = edgeGeom.outByFrom.get(r.from) ?? [i];
              const ins = edgeGeom.inByTo.get(r.to) ?? [i];
              const x1 = slotX(a.x, outs, i), y1 = a.y + NODE_H;
              const x2 = slotX(b.x, ins, i), y2 = b.y;
              const sel = selectedEdge === i;
              const col = sel ? T.green : conditionColor(r.when);
              const markerId = sel ? 'green' : !r.when ? 'faint' : col === T.green ? 'green' : col === T.red ? 'red' : 'violet';
              const lab = r.when ? conditionLabel(r.when) : '';
              const lx = x1 + (x2 - x1) * 0.42, ly = y1 + (y2 - y1) * 0.42;
              return (
                <g key={`${r.from}->${r.to}-${i}`}>
                  <path d={edgePath(x1, y1, x2, y2)} fill="none"
                    stroke={col} strokeWidth={sel ? 2.2 : 1.5}
                    strokeDasharray={r.when ? '5 3' : undefined}
                    markerEnd={`url(#arr-${markerId})`} />
                  {/* Fat invisible stroke = clickable target for selecting the edge. */}
                  <path d={edgePath(x1, y1, x2, y2)} fill="none" stroke="transparent" strokeWidth={14}
                    style={{ pointerEvents: editable ? 'stroke' : 'none', cursor: 'pointer' }}
                    onClick={() => { setSelectedEdge(i); setSelectedUid(null); }} />
                  {/* The condition, ON the edge: a labelled pill coloured by outcome, so
                      a branch says what it tests without opening the inspector. The full
                      expression stays in the tooltip and the route inspector. */}
                  {lab && (
                    <g style={{ pointerEvents: 'none' }}>
                      <title>{r.when}</title>
                      <rect x={lx - (lab.length * 3.4 + 7)} y={ly - 8} width={lab.length * 6.8 + 14} height={16} rx={8}
                        fill={T.bg} stroke={col} strokeWidth={sel ? 1.4 : 1} />
                      <text x={lx} y={ly + 3.5} textAnchor="middle" fill={col} fontSize={10} fontFamily={T.mono} fontWeight={600}>{lab}</text>
                    </g>
                  )}
                </g>
              );
            })}
          </svg>

          {blocks.map((b) => {
            const p = posOf.get(b.uid);
            if (!p) return null;
            const name = nodeName(b, defName);
            const action = b.inline?.action ?? catalog[b.stepId]?.action ?? '';
            const isEntry = !routes.some((r) => r.to === name);
            const isGate = action === 'approval' || !!b.approval;
            const isDecision = decisionNames.has(name);
            const diamond = isGate || isDecision;
            const linking = linkFrom === b.uid;
            const run = runStatus?.[name];
            return (
              // Transparent: the SVG layer draws this node's outline + fill, so the box
              // can be a stadium / diamond / fan-out rather than only a rectangle. This
              // layer carries the label, the ports and the click target.
              <div key={b.uid} style={{
                position: 'absolute', left: p.x, top: p.y, width: NODE_W, height: NODE_H,
                boxSizing: 'border-box', background: 'transparent',
                display: 'flex', flexDirection: 'column', justifyContent: 'center',
                textAlign: diamond ? 'center' : 'left',
                // A diamond's points eat the corners, so pad the label inward.
                padding: diamond ? '6px 26px' : '6px 10px', cursor: 'pointer',
              }} onClick={() => select(b)}>
                <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 6 }}>
                  <span style={{ fontFamily: T.mono, fontSize: 12, color: T.textHi, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                    {name || <em style={{ color: T.faint }}>unnamed</em>}
                  </span>
                  {editable && (
                    <button title="remove step" onClick={(e) => { e.stopPropagation(); removeNode(b.uid); }}
                      style={{ background: 'none', border: 'none', color: T.faint, cursor: 'pointer', fontSize: 11, padding: 0 }}>✕</button>
                  )}
                </div>
                <div style={{ fontFamily: T.mono, fontSize: 9.5, color: T.faint, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                  {run ? (
                    <span style={{ color: runColor(run.status) }}>
                      {run.status}{run.legs > 1 ? ` · ${run.legs}×` : ''}
                    </span>
                  ) : (
                    <>
                      {isGate ? 'manual approval' : action}
                      {b.matrix?.var ? ` · ⊞ ${b.matrix.var}` : ''}
                      {b.scatter?.regex ? ' · ⊟ scatter' : ''}
                      {isDecision && !isGate ? ' · ⑂ branches' : ''}
                    </>
                  )}
                </div>
                {isEntry && !runStatus && <div style={{ position: 'absolute', top: -7, left: 6, fontSize: 8, fontFamily: T.mono, color: T.green, background: T.bg, padding: '0 3px' }}>START</div>}

                {editable && (
                  <>
                    {/* in-port: completes a link started elsewhere */}
                    <button title="route into this step" onClick={(e) => { e.stopPropagation(); completeLink(b.uid); }}
                      disabled={!linkFrom || linkFrom === b.uid}
                      style={{ ...port, top: -9, borderColor: linkFrom && linkFrom !== b.uid ? T.green : T.border, cursor: linkFrom ? 'pointer' : 'default' }}>▾</button>
                    {/* out-port: starts a link */}
                    <button title="route out of this step" onClick={(e) => { e.stopPropagation(); setLinkFrom(linking ? null : b.uid); }}
                      style={{ ...port, bottom: -9, borderColor: linking ? T.green : T.border, color: linking ? T.green : T.faint }}>▾</button>
                  </>
                )}
              </div>
            );
          })}
        </div>

        {/* Node inspector: a step's fan-out (matrix/scatter) and gate config. The
            step's own action/inputs are edited in the host's right-hand panel via
            onInspect; this is only what the graph itself owns. */}
        {editable && selectedNode && (
          <div style={{ position: 'sticky', bottom: 0, background: T.bgAlt, borderTop: `1px solid ${T.border}`, padding: 10, display: 'flex', flexDirection: 'column', gap: 8 }}>
            <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
              <span style={{ fontFamily: T.mono, fontSize: 11, color: T.textHi }}>
                {nodeName(selectedNode, defName) || 'unnamed'}
              </span>
              {!selectedNode.approval && (
                <>
                  <button onClick={() => setMatrix(selectedNode.uid, selectedNode.matrix ? null : { var: '', values: [] })}
                    disabled={!!selectedNode.mapId}
                    title={selectedNode.mapId ? 'a step in a map cannot also have its own matrix' : 'fan this step out over a list of values'}
                    style={{ ...toggleBtn, borderColor: selectedNode.matrix ? T.amber : T.border, color: selectedNode.matrix ? T.amber : T.faint, opacity: selectedNode.mapId ? 0.4 : 1 }}>
                    ⊞ matrix
                  </button>
                  <button onClick={() => setScatter(selectedNode.uid, selectedNode.scatter ? null : { regex: '', mode: 'dir' })}
                    disabled={!selectedNode.inline || !!selectedNode.mapId}
                    title={selectedNode.mapId ? 'a step in a map cannot also have its own scatter' : selectedNode.inline ? 'fan this step out over matching workspace paths' : 'scatter needs a pipeline-local (inline) step'}
                    style={{ ...toggleBtn, borderColor: selectedNode.scatter ? T.green : T.border, color: selectedNode.scatter ? T.green : T.faint, opacity: selectedNode.inline && !selectedNode.mapId ? 1 : 0.4 }}>
                    ⊟ scatter
                  </button>
                  {/* Map membership: a region repeats EVERY step assigned to it,
                      together with the routes between them. */}
                  <select value={selectedNode.mapId ?? ''} style={{ ...toggleBtn, color: selectedNode.mapId ? T.blue : T.faint, borderColor: selectedNode.mapId ? T.blue : T.border }}
                    title="repeat this step (with the others in the same map) once per value"
                    onChange={(e) => {
                      const v = e.target.value;
                      if (v === '__new') addMapWithNode(selectedNode.uid);
                      else setNodeMap(selectedNode.uid, v || undefined);
                    }}>
                    <option value="">⟳ not in a map</option>
                    {maps.map((m) => (
                      <option key={m.id} value={m.id}>⟳ in map · per {m.var || m.id}</option>
                    ))}
                    <option value="__new">⟳ + new map…</option>
                  </select>
                </>
              )}
            </div>
            {selectedNode.approval && <GateEditor uid={selectedNode.uid} gate={selectedNode.approval} onSet={setApproval} />}
            {selectedNode.matrix && <MatrixEditor uid={selectedNode.uid} matrix={selectedNode.matrix} onSet={setMatrix} />}
            {selectedNode.scatter && <ScatterEditor uid={selectedNode.uid} scatter={selectedNode.scatter} onSet={setScatter} />}
            {selectedNode.mapId && maps.find((m) => m.id === selectedNode.mapId) && (
              <MapEditor def={maps.find((m) => m.id === selectedNode.mapId) as MapDef}
                members={members[selectedNode.mapId] ?? []} onSet={setMapDef} />
            )}
            {!selectedNode.approval && !selectedNode.matrix && !selectedNode.scatter && !selectedNode.mapId && (
              <div style={{ fontSize: 10, color: T.faint, fontFamily: T.mono, lineHeight: 1.45 }}>
                Runs once. Fan it out with ⊞ matrix (one run per value) or ⊟ scatter (one leg per
                workspace path), or put it in a ⟳ map to repeat it — <b>together with the other steps
                in that map</b> — once per value. To run steps <b>in parallel</b>, route into them
                from the same step.
              </div>
            )}
          </div>
        )}

        {/* Route inspector: the only place a condition is authored. */}
        {editable && selectedEdge != null && routes[selectedEdge] && (
          <div style={{ position: 'sticky', bottom: 0, background: T.bgAlt, borderTop: `1px solid ${T.border}`, padding: 10 }}>
            <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 6 }}>
              <span style={{ fontFamily: T.mono, fontSize: 11, color: T.textHi }}>
                {routes[selectedEdge].from} <span style={{ color: T.green }}>→</span> {routes[selectedEdge].to}
              </span>
              <button onClick={() => { setRoutes((rs) => rs.filter((_, j) => j !== selectedEdge)); setSelectedEdge(null); }}
                style={{ marginLeft: 'auto', background: 'none', border: `1px solid ${T.border}`, color: T.faint, fontFamily: T.mono, fontSize: 10, padding: '2px 6px', cursor: 'pointer' }}>
                remove route
              </button>
            </div>
            <input
              value={routes[selectedEdge].when ?? ''}
              onChange={(e) => patchRoute(selectedEdge, { when: e.target.value || undefined })}
              placeholder={'always, when the step above succeeds — or e.g. steps.build.status == "failed"'}
              style={{ width: '100%', boxSizing: 'border-box', background: T.bg, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 11, padding: '5px 7px' }}
            />
            <div style={{ fontSize: 10, color: T.faint, marginTop: 5, lineHeight: 1.45, fontFamily: T.mono }}>
              Leave empty to follow this route only when <b>{routes[selectedEdge].from}</b> succeeds.
              Available: steps.NAME.status / .output / .json.FIELD, inputs.NAME, run.id.
            </div>
          </div>
        )}
      </div>
    </div>
  );
});

const paletteBtn: React.CSSProperties = {
  display: 'block', width: '100%', textAlign: 'left', marginBottom: 4,
  background: T.bg, border: `1px solid ${T.border}`, color: T.text,
  fontFamily: T.mono, fontSize: 11, padding: '5px 7px', cursor: 'pointer',
};

/** A run status's accent colour, mirroring the run page's own palette. A step that
 * never ran (a not-taken branch) has no step run at all, so it stays neutral. */
function runColor(status: string): string {
  switch (status) {
    case 'completed': return T.green;
    case 'failed': return T.red;
    case 'running': return T.blue;
    case 'awaiting_approval': return T.amber;
    case 'cancelled': return T.dim;
    default: return T.dim;
  }
}

const toggleBtn: React.CSSProperties = {
  background: 'none', border: `1px solid ${T.border}`, fontFamily: T.mono,
  fontSize: 10, padding: '2px 6px', cursor: 'pointer',
};

const port: React.CSSProperties = {
  position: 'absolute', left: NODE_W / 2 - 9, width: 18, height: 18,
  background: T.bg, border: `1px solid ${T.border}`, borderRadius: '50%',
  color: T.faint, fontSize: 9, lineHeight: '1', padding: 0,
  display: 'flex', alignItems: 'center', justifyContent: 'center',
};


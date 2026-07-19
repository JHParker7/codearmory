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
  displayGraph, findCycle, nodeName, pruneRoutes, stepsFromNodes, routesFromBlocks, blocksFromSteps, blockDef,
  regionMembers, pruneMaps, mapIssues,
} from './pipelineGraph';
import type {
  Block, Route, StepRef, BlockSelection, MatrixConfig, ScatterConfig, ApprovalGate, MapDef, DisplayNode,
} from './pipelineGraph';
import type { Step, WorkflowAction, GitRepo } from '../../api/bff';

/** Node box geometry. Kept here (not in theme) because the edge maths depends on it.
 * The flow is top-down, so GAP_Y is the tall one: that is where the edges live. */
const NODE_W = 190;
const NODE_H = 62;
const GAP_X = 30; // between siblings across a row
const GAP_Y = 76; // between depths — the edges run through here
const PAD = 28;
// A decision diamond occupies one grid cell but is drawn smaller than a step box,
// centred, so it reads as a distinct flow-chart symbol rather than another step.
const DEC_W = 104;
const DEC_H = 48;
// A collapsed map region is drawn as a CONTAINER box: it is one node in the outer flow
// (so routes attach to the group, not its members) but its member step blocks are laid
// out stacked INSIDE it. These size the box around that inner stack.
const MAP_LABEL_H = 22; // label strip at the top of the box
const MAP_INNER_GAP = 24; // vertical gap between stacked members
const MAP_PAD_B = 12; // padding below the last member
const MAP_PAD_X = 16; // horizontal inset of members from the box sides (box stays grid-aligned)
const mapBoxHeight = (memberCount: number) =>
  MAP_LABEL_H + Math.max(1, memberCount) * NODE_H + (Math.max(1, memberCount) - 1) * MAP_INNER_GAP + MAP_PAD_B;

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

type EdgeSide = 'top' | 'bottom' | 'left' | 'right';

const SIDE_N: Record<EdgeSide, [number, number]> = { top: [0, -1], bottom: [0, 1], left: [-1, 0], right: [1, 0] };

/** An ORTHOGONAL connector between two attachment points that respects BOTH the side it
 * leaves and the side it enters (so the arrowhead meets the target square-on), and keeps
 * its long cross-run down in the inter-rank GAP — right next to the target's entry stub —
 * rather than at a node's own y-level where it would cut across neighbours. Each node is
 * left/entered via a short perpendicular stub; `stagger` shifts the transfer lane toward
 * the source so sibling edges don't share one. */
function sidePath(a: { x: number; y: number }, aSide: EdgeSide, b: { x: number; y: number }, bSide: EdgeSide, stagger = 0): string {
  const s = 16;
  const a1 = { x: a.x + SIDE_N[aSide][0] * s, y: a.y + SIDE_N[aSide][1] * s };
  const b1 = { x: b.x + SIDE_N[bSide][0] * s, y: b.y + SIDE_N[bSide][1] * s };
  const verticalEntry = bSide === 'top' || bSide === 'bottom';
  if (verticalEntry) {
    // Horizontal transfer lane hugging the target's entry stub (in the gap), staggered.
    const yT = b1.y - (b1.y >= a1.y ? 1 : -1) * stagger;
    return `M ${a.x} ${a.y} L ${a1.x} ${a1.y} L ${a1.x} ${yT} L ${b1.x} ${yT} L ${b1.x} ${b1.y} L ${b.x} ${b.y}`;
  }
  // Horizontal entry (a same-rank peer): vertical transfer lane hugging the entry stub.
  const xT = b1.x - (b1.x >= a1.x ? 1 : -1) * stagger;
  return `M ${a.x} ${a.y} L ${a1.x} ${a1.y} L ${xT} ${a1.y} L ${xT} ${b1.y} L ${b1.x} ${b1.y} L ${b.x} ${b.y}`;
}

/** An orthogonal path for a long edge that goes AROUND intermediate ranks: a stub out
 * of the source, across to a vertical channel at `cx` (left/right of the columns),
 * straight down it, then across and into the target — all straight lines. */
function sideChannelPath(x1: number, y1: number, x2: number, y2: number, cx: number, sOut = 16, sIn = 16): string {
  return `M ${x1} ${y1} L ${x1} ${y1 + sOut} L ${cx} ${y1 + sOut} L ${cx} ${y2 - sIn} L ${x2} ${y2 - sIn} L ${x2} ${y2}`;
}

/** A short, readable NAME for a branch arm leaving a decision, so each path says what
 * it is rather than a bare value: `steps.X.status == "failed"` reads as `if failed`,
 * `!= "ok"` as `if not ok`; other expressions are truncated. The full text stays in
 * the tooltip and the route inspector. The unconditional default is labelled `else`
 * by the caller. */
function conditionLabel(when: string): string {
  const eq = when.match(/status\s*==\s*["']([a-zA-Z_]+)["']/);
  if (eq) return `if ${eq[1]}`;
  const ne = when.match(/status\s*!=\s*["']([a-zA-Z_]+)["']/);
  if (ne) return `if not ${ne[1]}`;
  const s = when.trim();
  return s.length > 20 ? `if ${s.slice(0, 19)}…` : `if ${s}`;
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

  // The DRAWN graph: real steps plus a synthetic decision diamond for every step that
  // branches on a condition, so a runner step never carries the branch itself.
  // In the read-only views (pipeline detail, run) each map region collapses to ONE block,
  // so the pipeline's routes stop threading around its individual steps. The editable
  // builder keeps them expanded (with the region box) so members can still be edited.
  const display = useMemo(() => displayGraph(blocks, routes, defName, !editable), [blocks, routes, defName, editable]);
  const nodeById = useMemo(() => {
    const m = new Map<string, DisplayNode>();
    display.nodes.forEach((n) => m.set(n.id, n));
    return m;
  }, [display]);
  /** The widest rank sets the grid; every narrower rank is CENTRED under it, so the flow
   * reads as a balanced tree down the middle rather than hugging the left. The one twist:
   * all the ranks a map region spans must share a SINGLE shift, otherwise centring each of
   * them independently would slide the region's members to different x per layer and its
   * (rectangular) enclosure would no longer be a clean box. So ranks are grouped by the map
   * spans that connect them (union-find over layers), and each group is centred as one unit
   * on its widest rank; map-free ranks are still centred individually. */
  const cols = useMemo(() => {
    const perLayer = new Map<number, number>();
    display.nodes.forEach((n) => perLayer.set(n.layer, Math.max(perLayer.get(n.layer) ?? 0, n.row + 1)));
    const max = Math.max(1, ...perLayer.values());
    // Layer spans of each map region.
    const nodeByUid = new Map(display.nodes.map((n) => [n.id, n] as const));
    const lo = new Map<string, number>(), hi = new Map<string, number>();
    blocks.forEach((b) => {
      if (!b.mapId) return;
      const n = nodeByUid.get(b.uid);
      if (!n) return;
      lo.set(b.mapId, Math.min(lo.get(b.mapId) ?? Infinity, n.layer));
      hi.set(b.mapId, Math.max(hi.get(b.mapId) ?? -Infinity, n.layer));
    });
    // Union the layers each region spans so they share one centring shift.
    const parent = new Map<number, number>();
    const find = (x: number): number => { while (parent.get(x) !== x) { parent.set(x, parent.get(parent.get(x)!)!); x = parent.get(x)!; } return x; };
    [...perLayer.keys()].forEach((l) => parent.set(l, l));
    lo.forEach((a, m) => { const b = hi.get(m)!; for (let l = a; l <= b; l++) { if (!parent.has(l)) parent.set(l, l); parent.set(find(l), find(a)); } });
    const groupW = new Map<number, number>();
    perLayer.forEach((w, l) => { const g = find(l); groupW.set(g, Math.max(groupW.get(g) ?? 0, w)); });
    const shift = new Map<number, number>();
    perLayer.forEach((w, l) => { const g = find(l); shift.set(l, (max - (groupW.get(g) ?? w)) / 2); });
    return { shift, max };
  }, [display, blocks]);
  /** A collapsed map node is taller than a step, so ranks can't share one height. Each
   * rank's LANE height is its tallest node; y accumulates lane by lane. */
  const heightOf = (n: DisplayNode) => (n.kind === 'map' ? mapBoxHeight(n.members?.length ?? 1) : NODE_H);
  const lanes = useMemo(() => {
    const h = new Map<number, number>();
    display.nodes.forEach((n) => h.set(n.layer, Math.max(h.get(n.layer) ?? NODE_H, heightOf(n))));
    const y = new Map<number, number>();
    let acc = PAD;
    const maxL = Math.max(0, ...h.keys());
    for (let l = 0; l <= maxL; l++) { y.set(l, acc); acc += (h.get(l) ?? NODE_H) + GAP_Y; }
    return { h, y, bottom: acc };
  }, [display]);
  const posOf = useMemo(() => {
    const m = new Map<string, { x: number; y: number }>();
    display.nodes.forEach((n) => {
      const laneH = lanes.h.get(n.layer) ?? NODE_H;
      const laneY = lanes.y.get(n.layer) ?? PAD;
      const nh = heightOf(n);
      const x = PAD + ((cols.shift.get(n.layer) ?? 0) + n.row) * (NODE_W + GAP_X);
      const y = laneY + (laneH - nh) / 2; // centre the node within its lane
      m.set(n.id, { x, y });
      // Member step blocks are stacked inside the map box, below its label strip.
      if (n.kind === 'map') {
        (n.memberUids ?? []).forEach((uid, i) => {
          m.set(uid, { x, y: y + MAP_LABEL_H + i * (NODE_H + MAP_INNER_GAP) });
        });
      }
    });
    return m;
  }, [display, cols, lanes]);
  /** Where each drawn edge attaches. An edge meets a node on the SIDE that faces the
   * other end — a step's four sides, a decision's four tips — so a route coming from
   * the right lands on the right and doesn't cross the ones arriving from above.
   * Endpoints sharing a side are then fanned across it, ordered by the other end's
   * position so they don't cross each other either. Returns per edge index the two
   * points and the sides they leave through (for the curve's direction). */
  const endpoints = useMemo(() => {
    type Side = 'top' | 'bottom' | 'left' | 'right';
    const h = (id: string) => { const n = nodeById.get(id); return n ? heightOf(n) : NODE_H; };
    const centre = (id: string) => { const p = posOf.get(id)!; return { x: p.x + NODE_W / 2, y: p.y + h(id) / 2 }; };
    // The side of `nodeId` that faces `otherId`. A decision ARM prefers the bottom tip
    // only when the target is nearly straight below, else a left/right tip.
    const sideFor = (nodeId: string, otherId: string, decisionArm: boolean): Side => {
      const n = centre(nodeId), o = centre(otherId);
      const dx = o.x - n.x, dy = o.y - n.y;
      if (decisionArm) {
        if (dy > 0 && Math.abs(dx) < dy * 0.5) return 'bottom';
        return dx >= 0 ? 'right' : 'left';
      }
      // Attach vertically whenever the other node is in a different RANK, so the edge drops
      // into the inter-rank gap and transfers there — never along its own y-level, where it
      // would run straight through same-rank neighbours. Side attach is only for peers that
      // sit level with each other.
      if (Math.abs(dy) > NODE_H * 0.75) return dy >= 0 ? 'bottom' : 'top';
      return dx >= 0 ? 'right' : 'left';
    };
    const sSide = new Map<number, Side>(), tSide = new Map<number, Side>();
    display.edges.forEach((e, k) => {
      if (!posOf.get(e.from) || !posOf.get(e.to)) return;
      sSide.set(k, sideFor(e.from, e.to, nodeById.get(e.from)?.kind === 'decision'));
      tSide.set(k, nodeById.get(e.to)?.kind === 'decision' ? 'top' : sideFor(e.to, e.from, false));
    });
    // Group endpoints by (node, side, out/in), then order each group by the OTHER
    // end's coordinate along that side, so slots line up with the neighbours.
    const groups = new Map<string, number[]>();
    const add = (key: string, k: number) => { if (!groups.has(key)) groups.set(key, []); groups.get(key)!.push(k); };
    display.edges.forEach((e, k) => {
      if (!sSide.has(k)) return;
      add(`${e.from}|${sSide.get(k)}|o`, k);
      add(`${e.to}|${tSide.get(k)}|i`, k);
    });
    const otherPerp = (k: number, end: 'o' | 'i', side: Side) => {
      const e = display.edges[k];
      const c = centre(end === 'o' ? e.to : e.from);
      return side === 'top' || side === 'bottom' ? c.x : c.y;
    };
    groups.forEach((ks, key) => {
      const [, side, end] = key.split('|') as [string, Side, 'o' | 'i'];
      ks.sort((a, b) => otherPerp(a, end, side) - otherPerp(b, end, side));
    });
    const pointOn = (nodeId: string, side: Side, k: number, end: 'o' | 'i') => {
      const p = posOf.get(nodeId)!;
      const nh = h(nodeId);
      const cx = p.x + NODE_W / 2, cy = p.y + nh / 2;
      if (nodeById.get(nodeId)?.kind === 'decision') {
        if (side === 'top') return { x: cx, y: cy - DEC_H / 2 };
        if (side === 'bottom') return { x: cx, y: cy + DEC_H / 2 };
        if (side === 'left') return { x: cx - DEC_W / 2, y: cy };
        return { x: cx + DEC_W / 2, y: cy };
      }
      const ks = groups.get(`${nodeId}|${side}|${end}`) ?? [k];
      const frac = (Math.max(0, ks.indexOf(k)) + 1) / (ks.length + 1);
      if (side === 'top') return { x: p.x + NODE_W * frac, y: p.y };
      if (side === 'bottom') return { x: p.x + NODE_W * frac, y: p.y + nh };
      if (side === 'left') return { x: p.x, y: p.y + nh * frac };
      return { x: p.x + NODE_W, y: p.y + nh * frac };
    };
    const out = new Map<number, { a: { x: number; y: number }; aSide: Side; b: { x: number; y: number }; bSide: Side; aFan: number; aFanN: number }>();
    display.edges.forEach((e, k) => {
      if (!sSide.has(k)) return;
      const og = groups.get(`${e.from}|${sSide.get(k)}|o`) ?? [k];
      out.set(k, {
        a: pointOn(e.from, sSide.get(k)!, k, 'o'), aSide: sSide.get(k)!,
        b: pointOn(e.to, tSide.get(k)!, k, 'i'), bSide: tSide.get(k)!,
        aFan: Math.max(0, og.indexOf(k)), aFanN: og.length,
      });
    });
    return out;
  }, [display, posOf, nodeById]);

  /** The vertical channel x for each spanning edge. It starts in the gap right next to the
   * target (on the source's side) — so a clear edge stays local — but if that column is
   * blocked by a step at any rank BETWEEN source and target, it steps outward until it
   * finds a corridor free of every box in those ranks. That's what makes the fail-ticket
   * fan-in route AROUND the steps below it rather than straight through them. */
  const longEdgeCx = useMemo(() => {
    const boxesByLayer = new Map<number, { l: number; r: number }[]>();
    display.nodes.forEach((n) => {
      const p = posOf.get(n.id);
      if (!p) return;
      const w = n.kind === 'decision' ? DEC_W : NODE_W;
      const x = n.kind === 'decision' ? p.x + NODE_W / 2 - DEC_W / 2 : p.x;
      if (!boxesByLayer.has(n.layer)) boxesByLayer.set(n.layer, []);
      boxesByLayer.get(n.layer)!.push({ l: x, r: x + w });
    });
    const MARGIN = 10;
    const rightEdge = PAD + cols.max * (NODE_W + GAP_X);
    const clampX = (x: number) => Math.max(8, Math.min(rightEdge + PAD - 8, x));
    const m = new Map<number, number>();
    display.edges.forEach((e, k) => {
      const fl = nodeById.get(e.from)?.layer ?? 0, tl = nodeById.get(e.to)?.layer ?? 0;
      if (tl - fl < 2) return;
      const tp = posOf.get(e.to);
      if (!tp) return;
      const fromCentre = (posOf.get(e.from)?.x ?? tp.x) + NODE_W / 2;
      const goLeft = fromCentre <= tp.x + NODE_W / 2;
      const preferX = goLeft ? tp.x - GAP_X / 2 : tp.x + NODE_W + GAP_X / 2;
      const forbidden: [number, number][] = [];
      for (let r = Math.min(fl, tl) + 1; r <= Math.max(fl, tl) - 1; r++)
        (boxesByLayer.get(r) ?? []).forEach((b) => forbidden.push([b.l - MARGIN, b.r + MARGIN]));
      const free = (x: number) => !forbidden.some(([a, b]) => x >= a && x <= b);
      let cx = preferX;
      if (!free(cx)) {
        const dir = goLeft ? -1 : 1;
        for (let s = 1; s <= 300; s++) { const x = preferX + dir * s * 4; if (free(x)) { cx = x; break; } cx = x; }
      }
      m.set(k, clampX(cx));
    });
    return m;
  }, [display, posOf, nodeById, cols]);

  /** Route-label placement with overlap resolution. Each labelled edge starts at the
   * midpoint of its drawn path, then labels are pushed on the y-axis (some higher, some
   * lower) until they clear BOTH one another AND the step/map/decision boxes — which act
   * as fixed obstacles with a margin — so a pill never sits on top of a node or another
   * pill. Where a label ends up nudged off its route, a leader ties it back. */
  const labelPos = useMemo(() => {
    const M = 5; // clearance kept around every box
    // Fixed obstacles: the node boxes. Members live inside their map box, which covers them.
    const boxes: { x: number; y: number; w: number; h: number }[] = [];
    display.nodes.forEach((n) => {
      const p = posOf.get(n.id);
      if (!p) return;
      if (n.kind === 'decision') boxes.push({ x: p.x + NODE_W / 2 - DEC_W / 2, y: p.y + heightOf(n) / 2 - DEC_H / 2, w: DEC_W, h: DEC_H });
      else boxes.push({ x: p.x, y: p.y, w: NODE_W, h: heightOf(n) });
    });
    const items: { k: number; x: number; y: number; w: number; h: number }[] = [];
    display.edges.forEach((e, k) => {
      const ep = endpoints.get(k);
      if (!ep) return;
      const label = e.name || (e.arm ? (e.when ? conditionLabel(e.when) : 'else') : '');
      if (!label) return;
      const { a, b } = ep;
      const fromLayer = nodeById.get(e.from)?.layer ?? 0;
      const toLayer = nodeById.get(e.to)?.layer ?? 0;
      let x: number, y: number;
      if (toLayer - fromLayer >= 2) {
        x = longEdgeCx.get(k) ?? (a.x + b.x) / 2;
        y = (a.y + b.y) / 2;
      } else {
        x = (a.x + b.x) / 2;
        y = (a.y + b.y) / 2;
      }
      items.push({ k, x, y, w: label.length * 6.6 + 14, h: 16 });
    });
    items.sort((p, q) => p.x - q.x || p.y - q.y);
    // xHit: two horizontal spans (centre ± half-width, both padded by M) overlap.
    const xHit = (ax: number, aw: number, bx: number, bw: number) => Math.abs(ax - bx) < (aw + bw) / 2 + M;
    for (let iter = 0; iter < 40; iter++) {
      let moved = false;
      // Push labels off the node boxes first (out the nearer side).
      for (const A of items) {
        for (const o of boxes) {
          if (!xHit(A.x, A.w, o.x + o.w / 2, o.w)) continue;
          const aTop = A.y - A.h / 2, aBot = A.y + A.h / 2;
          const oTop = o.y - M, oBot = o.y + o.h + M;
          if (aBot <= oTop || aTop >= oBot) continue;
          const up = aBot - oTop, down = oBot - aTop;
          A.y += up < down ? -(up + 0.5) : down + 0.5;
          moved = true;
        }
      }
      // Then separate labels from each other.
      for (let i = 0; i < items.length; i++) {
        for (let j = i + 1; j < items.length; j++) {
          const A = items[i], B = items[j];
          if (!xHit(A.x, A.w, B.x, B.w)) continue;
          const dy = B.y - A.y;
          const gap = (A.h + B.h) / 2 + 3;
          if (Math.abs(dy) >= gap) continue;
          const push = (gap - Math.abs(dy)) / 2 + 0.5;
          const dir = dy === 0 ? (i % 2 === 0 ? 1 : -1) : dy > 0 ? 1 : -1;
          A.y -= dir * push;
          B.y += dir * push;
          moved = true;
        }
      }
      if (!moved) break;
    }
    const m = new Map<number, { x: number; y: number }>();
    items.forEach((it) => m.set(it.k, { x: it.x, y: it.y }));
    return m;
  }, [display, endpoints, nodeById, posOf, longEdgeCx]);

  /** The routes that live WHOLLY inside a collapsed map region — drawn as short
   * connectors between the members stacked in the box, since they aren't part of the
   * outer flow. Empty in the editable (expanded) view, where they are normal edges. */
  const innerEdges = useMemo(() => {
    if (editable) return [] as { from: string; to: string }[];
    const uidByName = new Map(blocks.map((b) => [nodeName(b, defName), b.uid] as const));
    const mapByUid = new Map(blocks.filter((b) => b.mapId).map((b) => [b.uid, b.mapId!] as const));
    const out: { from: string; to: string }[] = [];
    routes.forEach((r) => {
      const fu = uidByName.get(r.from), tu = uidByName.get(r.to);
      if (!fu || !tu) return;
      const fm = mapByUid.get(fu), tm = mapByUid.get(tu);
      if (fm && fm === tm) out.push({ from: fu, to: tu });
    });
    return out;
  }, [blocks, routes, defName, editable]);

  const selectedNode = useMemo(() => blocks.find((b) => b.uid === selectedUid) ?? null, [blocks, selectedUid]);
  const cycle = useMemo(() => findCycle(blocks, routes, defName), [blocks, routes, defName]);
  const members = useMemo(() => regionMembers(blocks, defName), [blocks, defName]);
  const issues = useMemo(() => mapIssues(blocks, maps, routes, defName), [blocks, maps, routes, defName]);

  /** The bounding box of each region's nodes, so a map reads as an enclosure rather than a
   * per-node label — its body is a subgraph, and the drawing should say so. The layout
   * (band reservation in displayGraph + the shared per-group shift above) keeps every
   * member column-aligned and every non-member out of the band, so this plain rectangle
   * wraps only the region's own steps. */
  const regionBoxes = useMemo(() => {
    if (!editable) return []; // read-only collapses each region into a container node instead
    return maps.map((m) => {
      const pts = blocks.filter((b) => b.mapId === m.id).map((b) => posOf.get(b.uid)).filter(Boolean) as { x: number; y: number }[];
      if (pts.length === 0) return null;
      const x = Math.min(...pts.map((p) => p.x)) - 14;
      const y = Math.min(...pts.map((p) => p.y)) - 22;
      const x2 = Math.max(...pts.map((p) => p.x)) + NODE_W + 14;
      const y2 = Math.max(...pts.map((p) => p.y)) + NODE_H + 14;
      return { def: m, x, y, w: x2 - x, h: y2 - y };
    }).filter(Boolean) as { def: MapDef; x: number; y: number; w: number; h: number }[];
  }, [maps, blocks, posOf, editable]);
  const width = Math.max(PAD * 2 + cols.max * (NODE_W + GAP_X), 400);
  const height = Math.max(lanes.bottom + PAD, 260);

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

        <div style={{ position: 'relative', width, height, margin: '0 auto' }}>
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
          {/* Edges are drawn under the nodes so a route never covers a step's text. */}
          <svg width={width} height={height} style={{ position: 'absolute', inset: 0, pointerEvents: 'none' }}>
            <defs>
              <marker id="arrow" viewBox="0 0 8 8" refX="7" refY="4" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
                <path d="M 0 0 L 8 4 L 0 8 z" fill={T.faint} />
              </marker>
              <marker id="arrow-sel" viewBox="0 0 8 8" refX="7" refY="4" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
                <path d="M 0 0 L 8 4 L 0 8 z" fill={T.green} />
              </marker>
            </defs>
            {display.edges.map((e, k) => {
              const ep = endpoints.get(k);
              if (!ep) return null; // endpoint gone; pruned on save
              const { a, b, aSide, bSide } = ep;
              const sel = e.routeIndex != null && selectedEdge === e.routeIndex;
              // The edge's NAME wins if set; otherwise a branch arm is labelled with its
              // condition ("else" for the unconditional default). The plain
              // step→decision link stays unlabelled.
              const label = e.name || (e.arm ? (e.when ? conditionLabel(e.when) : 'else') : '');
              // A route that spans intermediate ranks is drawn AROUND them via a side
              // channel; everything else attaches to the nearest side of each node.
              const fromLayer = nodeById.get(e.from)?.layer ?? 0;
              const toLayer = nodeById.get(e.to)?.layer ?? 0;
              const long = toLayer - fromLayer >= 2;
              let d: string, lx: number, ly: number;
              if (long) {
                // Drop down the target-side channel corridor (clear of the steps in between),
                // so same-target edges merge and none cut through a box. See `longEdgeCx`.
                const cx = longEdgeCx.get(k) ?? (a.x + b.x) / 2;
                d = sideChannelPath(a.x, a.y, b.x, b.y, cx);
                lx = cx; ly = (a.y + b.y) / 2;
              } else {
                // Stagger sibling edges leaving the same node into separate transfer lanes.
                const stagger = ep.aFanN > 1 ? Math.min(ep.aFan * 10, 44) : 0;
                d = sidePath(a, aSide, b, bSide, stagger);
                lx = (a.x + b.x) / 2; ly = (a.y + b.y) / 2;
              }
              return (
                <g key={k}>
                  <path d={d} fill="none"
                    stroke={sel ? T.green : T.faint} strokeWidth={sel ? 2 : 1.2}
                    strokeDasharray={e.when ? '5 3' : undefined}
                    markerEnd={`url(#${sel ? 'arrow-sel' : 'arrow'})`} />
                  {/* A fat invisible stroke gives the thin edge a clickable target,
                      selecting the underlying route so its condition can be edited. */}
                  {e.routeIndex != null && (
                    <path d={d} fill="none" stroke="transparent" strokeWidth={12}
                      style={{ pointerEvents: editable ? 'stroke' : 'none', cursor: 'pointer' }}
                      onClick={() => { setSelectedEdge(e.routeIndex); setSelectedUid(null); }} />
                  )}
                  {label && (() => {
                    const lp = labelPos.get(k) ?? { x: lx, y: ly };
                    return (
                      <g style={{ pointerEvents: 'none' }}>
                        <title>{[e.name, e.when].filter(Boolean).join(' — ') || 'default branch (else)'}</title>
                        {/* A thin leader ties a nudged label back to the point on its route. */}
                        {Math.abs(lp.y - ly) > 10 && (
                          <line x1={lp.x} y1={lp.y} x2={lx} y2={ly} stroke={T.faint} strokeWidth={0.75} strokeDasharray="2 2" />
                        )}
                        <rect x={lp.x - (label.length * 3.3 + 7)} y={lp.y - 8} width={label.length * 6.6 + 14} height={16} rx={8}
                          fill={T.bg} stroke={sel ? T.green : T.faint} strokeWidth={1} />
                        <text x={lp.x} y={lp.y + 3.5} textAnchor="middle" fill={sel ? T.green : T.dim} fontSize={9.5} fontFamily={T.mono}>{label}</text>
                      </g>
                    );
                  })()}
                </g>
              );
            })}
            {/* Internal connectors of a collapsed map region: its members are stacked, so
                each internal route is a short link from one member's bottom to the next's top. */}
            {innerEdges.map((e, k) => {
              const a = posOf.get(e.from), b = posOf.get(e.to);
              if (!a || !b) return null;
              const ax = a.x + NODE_W / 2, ay = a.y + NODE_H;
              const bx = b.x + NODE_W / 2, by = b.y;
              return <path key={`ie${k}`} d={`M ${ax} ${ay} L ${bx} ${by}`} fill="none" stroke={T.faint} strokeWidth={1.2} markerEnd="url(#arrow)" />;
            })}
            {/* Decision diamonds: a step that branches on a condition flows into one
                of these, so the routing reads as a flow chart and the runner step
                never carries the branch. Drawn over the edges, behind the step boxes. */}
            {display.nodes.filter((n) => n.kind === 'decision').map((n) => {
              const p = posOf.get(n.id);
              if (!p) return null;
              const cx = p.x + NODE_W / 2, cy = p.y + NODE_H / 2;
              const d = `M ${cx} ${cy - DEC_H / 2} L ${cx + DEC_W / 2} ${cy} L ${cx} ${cy + DEC_H / 2} L ${cx - DEC_W / 2} ${cy} Z`;
              return (
                <g key={n.id} style={{ pointerEvents: 'none' }}>
                  <title>{`branch on ${n.sourceName}`}</title>
                  <path d={d} fill={T.bgAlt} stroke={T.blue} strokeWidth={1.4} />
                  <text x={cx} y={cy + 4} textAnchor="middle" fill={T.blue} fontSize={13} fontFamily={T.mono}>⑂</text>
                </g>
              );
            })}
          </svg>

          {/* Map-region CONTAINER boxes (read-only): one node in the outer flow, drawn as
              a labelled box behind its member step blocks — which are laid out inside it. */}
          {display.nodes.filter((n) => n.kind === 'map').map((n) => {
            const p = posOf.get(n.id);
            if (!p) return null;
            const def = maps.find((m) => m.id === n.mapId);
            const boxH = mapBoxHeight(n.members?.length ?? 1);
            const memberRuns = (n.members ?? []).map((nm) => runStatus?.[nm]).filter(Boolean) as { status: string; legs: number }[];
            const worst = memberRuns.length
              ? (['failed', 'awaiting_approval', 'running', 'cancelled'].find((s) => memberRuns.some((r) => r.status === s)) ?? memberRuns[0].status)
              : undefined;
            return (
              <div key={n.id} title={`map region · per ${def?.var || '?'}`} style={{
                position: 'absolute', left: p.x, top: p.y, width: NODE_W, height: boxH, boxSizing: 'border-box',
                background: 'transparent', border: `1px dashed ${worst ? runColor(worst) : T.blue}`,
                borderRadius: 4, pointerEvents: 'none',
              }}>
                <span style={{ position: 'absolute', top: 4, left: 8, fontFamily: T.mono, fontSize: 9.5, color: worst ? runColor(worst) : T.blue }}>
                  ⟳ map · per {def?.var || '?'}
                  {def?.volume ? ' · own ws' : ''}
                  {def?.sequential ? ' · seq' : ''}
                </span>
              </div>
            );
          })}

          {blocks.map((b) => {
            const p = posOf.get(b.uid);
            if (!p) return null;
            const name = nodeName(b, defName);
            const action = b.inline?.action ?? catalog[b.stepId]?.action ?? '';
            const isEntry = !routes.some((r) => r.to === name);
            const isGate = action === 'approval' || !!b.approval;
            const linking = linkFrom === b.uid;
            const run = runStatus?.[name];
            // A run colours the node by outcome; the editor colours it by role.
            const bar = run ? runColor(run.status) : isGate ? T.amber : isEntry ? T.green : T.border;
            const isActive = activeNode === name;
            // A member of a collapsed map region is inset inside its container box so it
            // doesn't touch the box border (read-only only; the editor keeps them full width).
            const inMap = !editable && !!b.mapId;
            return (
              <div key={b.uid} style={{
                position: 'absolute', left: p.x + (inMap ? MAP_PAD_X : 0), top: p.y,
                width: inMap ? NODE_W - MAP_PAD_X * 2 : NODE_W, height: NODE_H,
                boxSizing: 'border-box',
                background: selectedUid === b.uid || isActive ? T.greenSoft : T.bgAlt,
                border: `1px solid ${selectedUid === b.uid || isActive ? T.green : T.border}`,
                // A run colours the node by outcome; the editor colours it by role.
                borderLeft: `3px solid ${bar}`,
                display: 'flex', flexDirection: 'column', justifyContent: 'center',
                padding: '6px 10px', cursor: 'pointer',
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
            {/* Optional name: shown on the branch in place of the raw condition. */}
            <input
              value={routes[selectedEdge].name ?? ''}
              onChange={(e) => patchRoute(selectedEdge, { name: e.target.value || undefined })}
              placeholder={'branch name (optional) — e.g. if_discover_failed'}
              style={{ width: '100%', boxSizing: 'border-box', background: T.bg, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 11, padding: '5px 7px', marginBottom: 6 }}
            />
            <input
              value={routes[selectedEdge].when ?? ''}
              onChange={(e) => patchRoute(selectedEdge, { when: e.target.value || undefined })}
              placeholder={'always, when the step above succeeds — or e.g. steps.build.status == "failed"'}
              style={{ width: '100%', boxSizing: 'border-box', background: T.bg, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 11, padding: '5px 7px' }}
            />
            <div style={{ fontSize: 10, color: T.faint, marginTop: 5, lineHeight: 1.45, fontFamily: T.mono }}>
              The <b>name</b> labels this path on the diagram; leave it blank to show the condition.
              Leave the condition empty to follow this route only when <b>{routes[selectedEdge].from}</b> succeeds.
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


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

/** A bezier between two attachment points that leaves each node PERPENDICULAR to the
 * side it exits — so an edge off the right side bows rightward, one off the top rises,
 * etc. — giving the natural flow-chart look instead of everything dropping straight down. */
function sidePath(a: { x: number; y: number }, aSide: EdgeSide, b: { x: number; y: number }, bSide: EdgeSide): string {
  const k = Math.max(24, Math.hypot(b.x - a.x, b.y - a.y) * 0.35);
  const n = (s: EdgeSide): [number, number] => (s === 'top' ? [0, -1] : s === 'bottom' ? [0, 1] : s === 'left' ? [-1, 0] : [1, 0]);
  const [ax, ay] = n(aSide), [bx, by] = n(bSide);
  return `M ${a.x} ${a.y} C ${a.x + ax * k} ${a.y + ay * k}, ${b.x + bx * k} ${b.y + by * k}, ${b.x} ${b.y}`;
}

/** A path that leaves a node, bows out to a vertical channel at `cx` (left or right of
 * the node columns), runs down it, and curves back into the target. Used for a long
 * edge that spans intermediate ranks, so it goes AROUND the steps between its ends
 * instead of straight down through them. */
function sideChannelPath(x1: number, y1: number, x2: number, y2: number, cx: number): string {
  const out = 26; // how far below/above the endpoints the turn happens
  return `M ${x1} ${y1}`
    + ` C ${x1} ${y1 + out}, ${cx} ${y1}, ${cx} ${y1 + out + 8}`
    + ` L ${cx} ${y2 - out - 8}`
    + ` C ${cx} ${y2}, ${x2} ${y2 - out}, ${x2} ${y2}`;
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
  const display = useMemo(() => displayGraph(blocks, routes, defName), [blocks, routes, defName]);
  const nodeById = useMemo(() => {
    const m = new Map<string, DisplayNode>();
    display.nodes.forEach((n) => m.set(n.id, n));
    return m;
  }, [display]);
  /** The widest rank sets the grid; every narrower rank is CENTRED under it, so the
   * flow reads as a balanced tree down the middle rather than hugging the left. */
  const cols = useMemo(() => {
    const perLayer = new Map<number, number>();
    display.nodes.forEach((n) => perLayer.set(n.layer, Math.max(perLayer.get(n.layer) ?? 0, n.row + 1)));
    return { perLayer, max: Math.max(1, ...perLayer.values()) };
  }, [display]);
  const posOf = useMemo(() => {
    const m = new Map<string, { x: number; y: number }>();
    display.nodes.forEach((n) => {
      const offset = (cols.max - (cols.perLayer.get(n.layer) ?? 1)) / 2; // centre this rank
      m.set(n.id, {
        x: PAD + (offset + n.row) * (NODE_W + GAP_X),
        y: PAD + n.layer * (NODE_H + GAP_Y),
      });
    });
    return m;
  }, [display, cols]);
  /** Where each drawn edge attaches. An edge meets a node on the SIDE that faces the
   * other end — a step's four sides, a decision's four tips — so a route coming from
   * the right lands on the right and doesn't cross the ones arriving from above.
   * Endpoints sharing a side are then fanned across it, ordered by the other end's
   * position so they don't cross each other either. Returns per edge index the two
   * points and the sides they leave through (for the curve's direction). */
  const endpoints = useMemo(() => {
    type Side = 'top' | 'bottom' | 'left' | 'right';
    const centre = (id: string) => { const p = posOf.get(id)!; return { x: p.x + NODE_W / 2, y: p.y + NODE_H / 2 }; };
    // The side of `nodeId` that faces `otherId`. A decision ARM prefers the bottom tip
    // only when the target is nearly straight below, else a left/right tip.
    const sideFor = (nodeId: string, otherId: string, decisionArm: boolean): Side => {
      const n = centre(nodeId), o = centre(otherId);
      const dx = o.x - n.x, dy = o.y - n.y;
      if (decisionArm) {
        if (dy > 0 && Math.abs(dx) < dy * 0.5) return 'bottom';
        return dx >= 0 ? 'right' : 'left';
      }
      if (Math.abs(dy) >= Math.abs(dx)) return dy >= 0 ? 'bottom' : 'top';
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
      const cx = p.x + NODE_W / 2, cy = p.y + NODE_H / 2;
      if (nodeById.get(nodeId)?.kind === 'decision') {
        if (side === 'top') return { x: cx, y: cy - DEC_H / 2 };
        if (side === 'bottom') return { x: cx, y: cy + DEC_H / 2 };
        if (side === 'left') return { x: cx - DEC_W / 2, y: cy };
        return { x: cx + DEC_W / 2, y: cy };
      }
      const ks = groups.get(`${nodeId}|${side}|${end}`) ?? [k];
      const frac = (Math.max(0, ks.indexOf(k)) + 1) / (ks.length + 1);
      if (side === 'top') return { x: p.x + NODE_W * frac, y: p.y };
      if (side === 'bottom') return { x: p.x + NODE_W * frac, y: p.y + NODE_H };
      if (side === 'left') return { x: p.x, y: p.y + NODE_H * frac };
      return { x: p.x + NODE_W, y: p.y + NODE_H * frac };
    };
    const out = new Map<number, { a: { x: number; y: number }; aSide: Side; b: { x: number; y: number }; bSide: Side }>();
    display.edges.forEach((e, k) => {
      if (!sSide.has(k)) return;
      out.set(k, {
        a: pointOn(e.from, sSide.get(k)!, k, 'o'), aSide: sSide.get(k)!,
        b: pointOn(e.to, tSide.get(k)!, k, 'i'), bSide: tSide.get(k)!,
      });
    });
    return out;
  }, [display, posOf, nodeById]);

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
  const width = Math.max(PAD * 2 + cols.max * (NODE_W + GAP_X), 400);
  const height = Math.max(...display.nodes.map((n) => PAD * 2 + (n.layer + 1) * (NODE_H + GAP_Y)), 260);

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
    // Editable: fill the flex container and scroll internally. Read-only (a run / a
    // pipeline preview): size to the graph's own width so the ENCLOSING panel scrolls
    // both ways — otherwise a wide flow is clipped with the scrollbar out of reach.
    <div ref={splitRef} style={{ display: 'flex', height: editable ? '100%' : 'auto', width: editable ? undefined : 'max-content', minHeight: 0, border: `1px solid ${T.border}`, background: T.bg }}>
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

      <div style={{ flex: 1, minWidth: 0, overflow: editable ? 'auto' : 'visible', position: 'relative' }}>
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
                const right = (a.x + b.x) / 2 > width / 2;
                const cx = right ? width - 12 - (k % 3) * 13 : 12 + (k % 3) * 13;
                d = sideChannelPath(a.x, a.y, b.x, b.y, cx);
                lx = cx; ly = (a.y + b.y) / 2;
              } else {
                d = sidePath(a, aSide, b, bSide);
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
                  {label && (
                    <g style={{ pointerEvents: 'none' }}>
                      <title>{[e.name, e.when].filter(Boolean).join(' — ') || 'default branch (else)'}</title>
                      <rect x={lx - (label.length * 3.3 + 7)} y={ly - 8} width={label.length * 6.6 + 14} height={16} rx={8}
                        fill={T.bg} stroke={sel ? T.green : T.faint} strokeWidth={1} />
                      <text x={lx} y={ly + 3.5} textAnchor="middle" fill={sel ? T.green : T.dim} fontSize={9.5} fontFamily={T.mono}>{label}</text>
                    </g>
                  )}
                </g>
              );
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
            return (
              <div key={b.uid} style={{
                position: 'absolute', left: p.x, top: p.y, width: NODE_W, height: NODE_H,
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


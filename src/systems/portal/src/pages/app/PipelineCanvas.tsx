/**
 * The pipeline graph editor: nodes are steps, edges ("routes") are what runs next.
 *
 * This replaces the Scratch-style block stack, where a pipeline was an ordered list
 * and a block could only be "linked" to the block directly above it. That encoding
 * is a linked list: it cannot express a step depending on two non-adjacent steps, a
 * diamond join, or a conditional branch — the reason routes exist.
 *
 * Layout is derived, not dragged: a node's column is its depth from an entry step
 * (layoutGraph), so an edge always points rightward and a join always sits past
 * every branch feeding it. There are no free-floating coordinates to persist, and
 * the drawing is a pure function of the graph — what you see is what the worker
 * executes, because the layout mirrors the engine's own derivation.
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
} from './pipelineGraph';
import type { Block, Route, StepRef, BlockSelection } from './pipelineGraph';
import type { Step, WorkflowAction, GitRepo } from '../../api/bff';

/** Node box geometry. Kept here (not in theme) because the edge maths depends on it. */
const NODE_W = 190;
const NODE_H = 62;
const GAP_X = 96; // horizontal room between columns — where the edges live
const GAP_Y = 26;
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
  catalog: Record<string, Step>;
  editable?: boolean;
  palette?: Step[];
  actions?: WorkflowAction[];
  /** The git-broker repo catalog, threaded through to the host's step editor. */
  repos?: GitRepo[];
  token?: string;
  onChange?: (steps: StepRef[], routes: Route[]) => void;
  onInspect?: (selection: CanvasSelection | null) => void;
  onPickAction?: (action: WorkflowAction) => void;
  pendingAdd?: Step | null;
  onPendingConsumed?: () => void;
}

/** A cubic bezier between two ports, bulging horizontally so parallel edges stay
 * distinguishable rather than overlapping as straight lines. */
function edgePath(x1: number, y1: number, x2: number, y2: number): string {
  const dx = Math.max(36, Math.abs(x2 - x1) * 0.5);
  return `M ${x1} ${y1} C ${x1 + dx} ${y1}, ${x2 - dx} ${y2}, ${x2} ${y2}`;
}

export const PipelineCanvas = forwardRef<PipelineCanvasHandle, PipelineCanvasProps>(function PipelineCanvas({
  initialSteps, initialRoutes, catalog, editable = false, palette = [], actions = [],
  onChange, onInspect, onPickAction, pendingAdd, onPendingConsumed,
}: PipelineCanvasProps, ref) {
  const defName = useCallback((id: string) => catalog[id]?.name, [catalog]);

  const [blocks, setBlocks] = useState<Block[]>(() => blocksFromSteps(initialSteps));
  const [routes, setRoutes] = useState<Route[]>(() =>
    initialRoutes && initialRoutes.length > 0
      ? initialRoutes
      : routesFromBlocks(blocksFromSteps(initialSteps), (id) => catalog[id]?.name));
  const [selectedUid, setSelectedUid] = useState<string | null>(null);
  const [selectedEdge, setSelectedEdge] = useState<number | null>(null);
  /** The out-port awaiting a target: the first half of drawing a route. */
  const [linkFrom, setLinkFrom] = useState<string | null>(null);
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
    setSelectedUid(null); setSelectedEdge(null); setLinkFrom(null);
  }, [initialSteps, initialRoutes, catalog]);

  // Report the graph up on every edit. Routes are pruned first so a deleted or
  // renamed step can never leave an edge pointing at nothing.
  useEffect(() => {
    if (!editable || !onChange) return;
    const names = blocks.map((b) => nodeName(b, defName));
    onChange(stepsFromNodes(blocks), pruneRoutes(routes, names));
  }, [blocks, routes, editable, onChange, defName]);

  const placed = useMemo(() => layoutGraph(blocks, routes, defName), [blocks, routes, defName]);
  const posOf = useMemo(() => {
    const m = new Map<string, { x: number; y: number }>();
    placed.forEach((p) => m.set(p.uid, {
      x: PAD + p.layer * (NODE_W + GAP_X),
      y: PAD + p.row * (NODE_H + GAP_Y),
    }));
    return m;
  }, [placed]);
  const byName = useMemo(() => {
    const m = new Map<string, string>(); // name -> uid
    blocks.forEach((b) => m.set(nodeName(b, defName), b.uid));
    return m;
  }, [blocks, defName]);

  const cycle = useMemo(() => findCycle(blocks, routes, defName), [blocks, routes, defName]);
  const width = Math.max(...placed.map((p) => PAD + (p.layer + 1) * (NODE_W + GAP_X)), 400);
  const height = Math.max(...placed.map((p) => PAD * 2 + (p.row + 1) * (NODE_H + GAP_Y)), 260);

  const addStep = (stepId: string, name: string) => {
    const uid = `n${seq.current++}-${Date.now()}`;
    setBlocks((bs) => [...bs, { uid, stepId, parallelWithPrev: false, name: undefined }]);
    void name;
  };

  const addInline = (action: string, name: string, w?: Record<string, unknown>, timeout?: number) => {
    const uid = `n${seq.current++}-${Date.now()}`;
    // Names are node identities, so a collision would silently merge two nodes.
    const taken = new Set(blocks.map((b) => nodeName(b, defName)));
    let unique = name; let n = 2;
    while (taken.has(unique)) unique = `${name}-${n++}`;
    setBlocks((bs) => [...bs, { uid, stepId: '', parallelWithPrev: false, name: unique, inline: { action, with: w, timeout } }]);
  };

  useEffect(() => {
    if (!pendingAdd) return;
    addInline(pendingAdd.action, pendingAdd.name, pendingAdd.with ?? undefined, pendingAdd.timeout ?? undefined);
    onPendingConsumed?.();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [pendingAdd]);

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
    setRoutes((rs) => (rs.some((r) => r.from === from && r.to === to) ? rs : [...rs, { from, to }]));
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
            <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 0.5, marginBottom: 8 }}>ADD STEP</div>
            <button onClick={() => addInline('approval', 'approval')} style={paletteBtn}>
              ⏸ approval gate
            </button>
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
              Click a step's <span style={{ color: T.green }}>▸</span> then another step's{' '}
              <span style={{ color: T.green }}>▸</span> to route between them. A step with no
              incoming route starts the run.
            </div>
          </div>
          <div {...paletteHandle} />
        </>
      )}

      <div style={{ flex: 1, minWidth: 0, overflow: 'auto', position: 'relative' }}>
        {cycle.length > 0 && (
          <div style={{ position: 'sticky', top: 0, zIndex: 5, background: T.redSoft, color: T.red, padding: '6px 10px', fontFamily: T.mono, fontSize: 11 }}>
            routes form a cycle involving: {cycle.join(', ')} — the run would never start
          </div>
        )}
        {blocks.length === 0 && (
          <div style={{ padding: 24, fontFamily: T.mono, fontSize: 12, color: T.faint }}>
            No steps yet — add one from the palette.
          </div>
        )}

        <div style={{ position: 'relative', width, height }}>
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
            {routes.map((r, i) => {
              const a = posOf.get(byName.get(r.from) ?? '');
              const b = posOf.get(byName.get(r.to) ?? '');
              if (!a || !b) return null; // endpoint gone; pruned on save
              const x1 = a.x + NODE_W, y1 = a.y + NODE_H / 2;
              const x2 = b.x, y2 = b.y + NODE_H / 2;
              const sel = selectedEdge === i;
              return (
                <g key={`${r.from}->${r.to}-${i}`}>
                  <path d={edgePath(x1, y1, x2, y2)} fill="none"
                    stroke={sel ? T.green : T.faint} strokeWidth={sel ? 2 : 1.2}
                    strokeDasharray={r.when ? '5 3' : undefined}
                    markerEnd={`url(#${sel ? 'arrow-sel' : 'arrow'})`} />
                  {/* A fat invisible stroke gives the thin edge a clickable target. */}
                  <path d={edgePath(x1, y1, x2, y2)} fill="none" stroke="transparent" strokeWidth={12}
                    style={{ pointerEvents: editable ? 'stroke' : 'none', cursor: 'pointer' }}
                    onClick={() => { setSelectedEdge(i); setSelectedUid(null); }} />
                  {r.when && (
                    <text x={(x1 + x2) / 2} y={(y1 + y2) / 2 - 6} textAnchor="middle"
                      fill={sel ? T.green : T.faint} fontSize={9} fontFamily={T.mono}
                      style={{ pointerEvents: 'none' }}>
                      {r.when.length > 28 ? `${r.when.slice(0, 27)}…` : r.when}
                    </text>
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
            const linking = linkFrom === b.uid;
            return (
              <div key={b.uid} style={{
                position: 'absolute', left: p.x, top: p.y, width: NODE_W, height: NODE_H,
                boxSizing: 'border-box',
                background: selectedUid === b.uid ? T.greenSoft : T.bgAlt,
                border: `1px solid ${selectedUid === b.uid ? T.green : T.border}`,
                borderLeft: `3px solid ${isGate ? (T.amber) : isEntry ? T.green : T.border}`,
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
                  {isGate ? 'manual approval' : action}
                  {b.matrix?.var ? ` · ⊞ ${b.matrix.var}` : ''}
                  {b.scatter?.regex ? ' · ⊟ scatter' : ''}
                </div>
                {isEntry && <div style={{ position: 'absolute', top: -7, left: 6, fontSize: 8, fontFamily: T.mono, color: T.green, background: T.bg, padding: '0 3px' }}>START</div>}

                {editable && (
                  <>
                    {/* in-port: completes a link started elsewhere */}
                    <button title="route into this step" onClick={(e) => { e.stopPropagation(); completeLink(b.uid); }}
                      disabled={!linkFrom || linkFrom === b.uid}
                      style={{ ...port, left: -9, borderColor: linkFrom && linkFrom !== b.uid ? T.green : T.border, cursor: linkFrom ? 'pointer' : 'default' }}>▸</button>
                    {/* out-port: starts a link */}
                    <button title="route out of this step" onClick={(e) => { e.stopPropagation(); setLinkFrom(linking ? null : b.uid); }}
                      style={{ ...port, right: -9, borderColor: linking ? T.green : T.border, color: linking ? T.green : T.faint }}>▸</button>
                  </>
                )}
              </div>
            );
          })}
        </div>

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

const port: React.CSSProperties = {
  position: 'absolute', top: NODE_H / 2 - 9, width: 18, height: 18,
  background: T.bg, border: `1px solid ${T.border}`, borderRadius: '50%',
  color: T.faint, fontSize: 9, lineHeight: '1', padding: 0,
  display: 'flex', alignItems: 'center', justifyContent: 'center',
};


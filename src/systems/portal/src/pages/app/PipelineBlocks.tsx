/**
 * Scratch-style pipeline block builder.
 *
 * Each step is a block; blocks stack top→bottom (= run order). Steps that run
 * concurrently are grouped under a "∥ parallel" header (a shared green left-bar
 * and tint marks the container). While dragging, two kinds of drop target appear:
 *   - sequential GAPS between stages and at the end → drop a block there to make
 *     it its own sequential step (so a block can always leave a parallel container);
 *   - a "∥ add to parallel" JOIN zone under each parallel container → drop a block
 *     there to run it alongside that container's steps.
 * The ∥ toggle and the palette's "parallel block" are the non-drag ways to group.
 *
 * The block list maps 1:1 to the backend's ordered-steps-with-parallel_group
 * model via ./pipelineGraph, so the visual order *is* the pipeline. Editable mode
 * reports the derived StepRef[] through onChange; read-only mode is a static
 * diagram for the pipeline detail page.
 */
import { forwardRef, useCallback, useEffect, useImperativeHandle, useMemo, useRef, useState } from 'react';
import type { ReactNode } from 'react';
import {
  DndContext, PointerSensor, pointerWithin, closestCenter, useSensor, useSensors, useDroppable, MeasuringStrategy,
} from '@dnd-kit/core';
import type { DragEndEvent, DragStartEvent, CollisionDetection } from '@dnd-kit/core';
import { SortableContext, verticalListSortingStrategy, useSortable } from '@dnd-kit/sortable';
import { CSS } from '@dnd-kit/utilities';
import { T } from '../../theme';
import { useResizablePane } from '../../components/ResizeHandle';
import type { Step, GitRepo, WorkflowAction } from '../../api/bff';
import { Block, StepRef, MatrixConfig, ScatterConfig, ApprovalGate, blocksFromSteps, stepsFromBlocks, stagesOf } from './pipelineGraph';
import { StepInputsEditor, UpstreamOutput } from './StepInputsEditor';
import { gitRepoFromWith, stepConfigIssues, actionCreatesVolume, createdVolumeName } from './stepSchema';

export interface PipelineBlocksProps {
  initialSteps: StepRef[];
  catalog: Record<string, Step>;
  editable?: boolean;
  palette?: Step[];
  // The action catalog, shown as blocks in the palette so a step can be created and
  // configured inline from an action (via onPickAction), not only reused from palette.
  actions?: WorkflowAction[];
  // The git-broker repo catalog, for the per-step Git repo picker on forge blocks.
  repos?: GitRepo[];
  // Auth token, so the per-step checkout branch selector can enumerate a repo's branches.
  token?: string;
  onChange?: (steps: StepRef[]) => void;
  // Reports the selected block's identity + resolved def (null = a gate or no
  // selection), so the host's right panel can edit that step. A gate has no step to
  // edit; an inline step edits its own def; a reference edits the shared step.
  onInspect?: (selection: BlockSelection | null) => void;
  // Clicking an action block: the host opens an inline create form for that action.
  onPickAction?: (action: WorkflowAction) => void;
  // A freshly-created step to drop into the pipeline as a new block (honoring the
  // active parallel/matrix mode). The host sets it after a create form is submitted
  // and clears it via onPendingConsumed once added.
  pendingAdd?: Step | null;
  onPendingConsumed?: () => void;
  height?: number | string;
}

// Prefer a droppable the pointer is inside (the gaps, join zones, and blocks under
// the cursor); fall back to nearest centre over empty space.
const collisionDetection: CollisionDetection = (args) => {
  const within = pointerWithin(args);
  return within.length ? within : closestCenter(args);
};

/** Uppercase divider heading for a palette group (actions / steps). */
const paletteSection: React.CSSProperties = {
  padding: '9px 16px 5px', fontFamily: T.mono, fontSize: 9, color: T.faint,
  letterSpacing: 1, textTransform: 'uppercase', background: T.bgAlt, borderBottom: `1px solid ${T.border}`,
};

/** The resolved definition of a block's step — from the block's own inline def
 * (inline step) or the shared catalog (stored reference). Undefined for a gate. */
export interface BlockDef { name: string; action: string; with?: Record<string, unknown>; timeout?: number }

export function blockDef(b: Block, catalog: Record<string, Step>): BlockDef | undefined {
  if (b.approval) return undefined;
  if (b.inline) return { name: b.name ?? '', action: b.inline.action, with: b.inline.with, timeout: b.inline.timeout };
  const s = catalog[b.stepId];
  return s ? { name: s.name, action: s.action, with: (s.with ?? {}) as Record<string, unknown>, timeout: s.timeout ?? undefined } : undefined;
}

/** What the builder reports up when a block is selected — enough for the host's right
 * panel to render the step editor (an inline step edits its own def; a reference
 * edits the shared step) without reaching back into block state. */
export interface BlockSelection { uid: string; kind: 'gate' | 'ref' | 'inline'; stepId: string; name?: string; def?: BlockDef }

/** Imperative handle the host uses to mutate the selected block (inline-def edit,
 * convert-to-general, make-local) — block state lives here, so the host drives these
 * through the handle rather than re-seeding (which would reset uids and selection). */
export interface PipelineBlocksHandle {
  patchBlock: (uid: string, patch: Partial<Block>) => void;
  getBlock: (uid: string) => Block | undefined;
}

function selectionFor(b: Block, catalog: Record<string, Step>): BlockSelection {
  return { uid: b.uid, kind: b.approval ? 'gate' : b.inline ? 'inline' : 'ref', stepId: b.stepId, name: b.name, def: blockDef(b, catalog) };
}

/** Sequential drop target between stages / at the end. id = `gap-<insertIndex>`.
 * Always a droppable (so it is measured), compact when idle, a dashed zone while
 * dragging. */
function Gap({ id, dragging, showArrow }: { id: string; dragging: boolean; showArrow: boolean }) {
  const { setNodeRef, isOver } = useDroppable({ id });
  const idle: React.CSSProperties = showArrow
    ? { display: 'flex', alignItems: 'center', gap: 6, paddingLeft: 16, height: 16 }
    : { height: 2 };
  return (
    <div ref={setNodeRef} style={dragging
      ? { height: 22, margin: '3px 0', display: 'flex', alignItems: 'center', justifyContent: 'center', border: `1px dashed ${isOver ? T.green : T.border}`, background: isOver ? T.greenSoft : 'transparent', fontFamily: T.mono, fontSize: 9, color: isOver ? T.green : T.faint }
      : idle}>
      {dragging
        ? (isOver ? '↓ drop here for a sequential step' : '↓')
        : (showArrow ? <><span style={{ width: 2, height: '100%', background: T.border }} /><span style={{ fontFamily: T.mono, fontSize: 9, color: T.faint }}>↓</span></> : null)}
    </div>
  );
}

interface BlockCardProps {
  block: Block;
  label: string;
  action: string;
  editable: boolean;
  canLink: boolean;
  inParallel: boolean;
  isGate: boolean;
  selected: boolean;
  defWith: Record<string, unknown>;
  upstream: UpstreamOutput[];
  upstreamVolumes: string[];
  repos: GitRepo[];
  token?: string;
  onSelect: () => void;
  onToggle: () => void;
  onRename: (uid: string, name: string | undefined) => void;
  onSetWith: (uid: string, override: Record<string, unknown>) => void;
  onToggleParallel: (uid: string) => void;
  onSetMatrix: (uid: string, matrix: MatrixConfig | null) => void;
  onSetScatter: (uid: string, scatter: ScatterConfig | null) => void;
  onSetApproval: (uid: string, gate: ApprovalGate) => void;
  onRemove: (uid: string) => void;
}

/** Inline editor for an approval gate's prompt and optional approver allow-list. */
function GateEditor({ uid, gate, onSetApproval }: { uid: string; gate: ApprovalGate; onSetApproval: (uid: string, g: ApprovalGate) => void }) {
  const stop = (e: React.PointerEvent) => e.stopPropagation();
  const field: React.CSSProperties = { background: T.cardHi, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 11, padding: '4px 6px', outline: 'none' };
  return (
    <div onPointerDown={stop} style={{ marginTop: 6, padding: 8, background: T.bg, border: `1px dashed ${T.border}`, borderLeft: `3px solid ${T.blue}`, display: 'flex', flexDirection: 'column', gap: 6 }}>
      <div style={{ fontFamily: T.mono, fontSize: 9, color: T.blue, letterSpacing: 1, textTransform: 'uppercase' }}>⏸ pauses until approved</div>
      <input value={gate.message ?? ''} placeholder="message shown to approvers (e.g. deploy to prod?)" onPointerDown={stop}
        onChange={(e) => onSetApproval(uid, { ...gate, message: e.target.value })} style={field} />
      <input value={(gate.approvers ?? []).join(', ')} placeholder="approvers (usernames, comma-separated; empty = anyone with permission)" onPointerDown={stop}
        onChange={(e) => onSetApproval(uid, { ...gate, approvers: e.target.value.split(',').map((v) => v.trim()).filter(Boolean) })} style={field} />
    </div>
  );
}

/** Inline matrix editor shown under a solo block. Fans the step out over a list:
 * a comma-separated literal list, or a ${...} reference resolved at run time.
 * Typing in one source clears the other (the backend accepts exactly one). */
function MatrixEditor({ uid, matrix, onSetMatrix }: { uid: string; matrix: MatrixConfig; onSetMatrix: (uid: string, m: MatrixConfig | null) => void }) {
  const stop = (e: React.PointerEvent) => e.stopPropagation();
  const field: React.CSSProperties = { background: T.cardHi, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 11, padding: '4px 6px', outline: 'none' };
  return (
    <div onPointerDown={stop} style={{ marginTop: 6, padding: 8, background: T.bg, border: `1px dashed ${T.border}`, borderLeft: `3px solid ${T.amber}`, display: 'flex', flexDirection: 'column', gap: 6 }}>
      <div style={{ fontFamily: T.mono, fontSize: 9, color: T.amber, letterSpacing: 1, textTransform: 'uppercase' }}>⊞ matrix · one run per value</div>
      <input value={matrix.var} placeholder="var (e.g. region) → ${matrix.region}" onPointerDown={stop}
        onChange={(e) => onSetMatrix(uid, { ...matrix, var: e.target.value })} style={field} />
      <input value={(matrix.values ?? []).join(', ')} placeholder="values, comma-separated (a, b, c)" onPointerDown={stop}
        onChange={(e) => onSetMatrix(uid, { ...matrix, values: e.target.value.split(',').map((v) => v.trim()).filter(Boolean), values_from: undefined })} style={field} />
      <input value={matrix.values_from ?? ''} placeholder="or values from a reference (${inputs.regions})" onPointerDown={stop}
        onChange={(e) => onSetMatrix(uid, { ...matrix, values_from: e.target.value, values: e.target.value ? [] : matrix.values })} style={field} />
      <input type="number" min={0} value={matrix.max_concurrent ?? ''} placeholder="max concurrent (blank = default 3)" onPointerDown={stop} disabled={!!matrix.sequential}
        onChange={(e) => { const n = parseInt(e.target.value, 10); onSetMatrix(uid, { ...matrix, max_concurrent: Number.isFinite(n) && n > 0 ? n : undefined }); }} style={{ ...field, opacity: matrix.sequential ? 0.5 : 1 }} />
      <label onPointerDown={stop} style={{ display: 'flex', alignItems: 'center', gap: 6, fontFamily: T.mono, fontSize: 11, color: T.text, cursor: 'pointer' }}>
        <input type="checkbox" checked={!!matrix.sequential} onPointerDown={stop}
          onChange={(e) => onSetMatrix(uid, { ...matrix, sequential: e.target.checked || undefined })} />
        run sequentially (one at a time)
      </label>
    </div>
  );
}

/** Inline scatter editor shown under a solo block. Fans the step out over the paths of
 * a shared workspace matching a regex — each leg runs on its own clone (bound to
 * ${scatter.path}), and owned outputs are gathered back into the base afterward. */
function ScatterEditor({ uid, scatter, onSetScatter }: { uid: string; scatter: ScatterConfig; onSetScatter: (uid: string, s: ScatterConfig | null) => void }) {
  const stop = (e: React.PointerEvent) => e.stopPropagation();
  const field: React.CSSProperties = { background: T.cardHi, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 11, padding: '4px 6px', outline: 'none' };
  return (
    <div onPointerDown={stop} style={{ marginTop: 6, padding: 8, background: T.bg, border: `1px dashed ${T.border}`, borderLeft: `3px solid ${T.green}`, display: 'flex', flexDirection: 'column', gap: 6 }}>
      <div style={{ fontFamily: T.mono, fontSize: 9, color: T.green, letterSpacing: 1, textTransform: 'uppercase' }}>⊟ scatter · one leg per matched path (own clone)</div>
      <input value={scatter.regex} placeholder="regex matching workspace paths (^services/[^/]+$)" onPointerDown={stop}
        onChange={(e) => onSetScatter(uid, { ...scatter, regex: e.target.value })} style={field} />
      <div style={{ display: 'flex', gap: 6 }}>
        <select value={scatter.mode ?? 'dir'} onPointerDown={stop} onChange={(e) => onSetScatter(uid, { ...scatter, mode: e.target.value })} style={{ ...field, flex: 1 }}>
          <option value="dir">dir</option>
          <option value="file">file</option>
        </select>
        <input value={scatter.volume ?? ''} placeholder="volume (default workspace)" onPointerDown={stop}
          onChange={(e) => onSetScatter(uid, { ...scatter, volume: e.target.value || undefined })} style={{ ...field, flex: 1 }} />
      </div>
      <input value={(scatter.outputs ?? []).join(', ')} placeholder="owned outputs, comma-separated (${scatter.path}/dist) — gathered back" onPointerDown={stop}
        onChange={(e) => onSetScatter(uid, { ...scatter, outputs: e.target.value.split(',').map((v) => v.trim()).filter(Boolean) })} style={field} />
      <input type="number" min={0} value={scatter.max_concurrent ?? ''} placeholder="max concurrent legs (blank = default 3)" onPointerDown={stop}
        onChange={(e) => { const n = parseInt(e.target.value, 10); onSetScatter(uid, { ...scatter, max_concurrent: Number.isFinite(n) && n > 0 ? n : undefined }); }} style={field} />
    </div>
  );
}

function BlockCard({ block, label, action, editable, canLink, inParallel, isGate, selected, defWith, upstream, upstreamVolumes, repos, token, onSelect, onToggle, onRename, onSetWith, onToggleParallel, onSetMatrix, onSetScatter, onSetApproval, onRemove }: BlockCardProps) {
  const { attributes, listeners, setNodeRef, transform, transition, isDragging } = useSortable({ id: block.uid, disabled: !editable });
  const leftBar = isGate ? T.blue : block.scatter ? T.green : block.matrix ? T.amber : inParallel ? T.green : T.dim;
  // Flag a block whose required config is still unset (e.g. a git-clone with no repo,
  // or a run with no command) so it reads as incomplete — a red border + hint —
  // before the workflow is saved. Gates carry their own config on the card, so they
  // are never flagged here. Only in the editable builder, never the read-only diagram.
  const eff = { ...defWith, ...(block.with ?? {}) };
  const issues = editable && !isGate ? stepConfigIssues(action, eff, gitRepoFromWith(eff)) : [];
  const incomplete = issues.length > 0;
  const style: React.CSSProperties = {
    transform: CSS.Transform.toString(transform),
    transition,
    opacity: isDragging ? 0.6 : 1,
    zIndex: isDragging ? 5 : undefined,
    display: 'flex', flexDirection: 'column', gap: 6,
    // In a parallel stage the cards sit side by side in a flex row; otherwise each
    // is a full-width row in the vertical sequence.
    marginBottom: inParallel ? 0 : 4,
    flex: inParallel ? '1 1 160px' : undefined,
    minWidth: inParallel ? 0 : undefined,
    background: selected ? T.cardHi : inParallel ? T.bgAlt : T.card,
    border: `1px solid ${isDragging ? T.green : incomplete ? T.red : selected ? T.textHi : T.border}`,
    borderLeft: `3px solid ${leftBar}`,
    padding: '8px 10px', fontFamily: T.mono,
    cursor: editable ? 'grab' : 'default', touchAction: editable ? 'none' : undefined, userSelect: 'none',
  };
  const stop = (e: React.PointerEvent) => e.stopPropagation();
  // Matrix fan-out applies only to a solo, non-gate step; it is mutually exclusive
  // A matrix block fans a single step out over a list; it can't also be parallel,
  // so its editor is hidden inside a parallel stage. Matrix is created from the
  // palette ("⊞ matrix block"), not a per-card toggle.
  const isMatrix = !!block.matrix;
  const showMatrix = editable && isMatrix && !inParallel && !isGate;
  const isScatter = !!block.scatter;
  const showScatter = editable && isScatter && !inParallel && !isGate;
  const prefix = isGate ? '⏸ ' : isScatter ? '⊟ ' : isMatrix ? '⊞ ' : '';
  return (
    <div ref={setNodeRef} style={style} {...(editable ? attributes : {})} {...(editable ? listeners : {})}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
        {editable && <span style={{ color: T.faint, fontSize: 13, lineHeight: 1 }}>⠿</span>}
        <div style={{ flex: 1, minWidth: 0 }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 4, minWidth: 0 }}>
            {prefix && <span style={{ fontSize: 12.5, color: isGate ? T.blue : T.amber }}>{prefix.trim()}</span>}
            {editable && !isGate ? (
              <input value={block.name ?? label}
                onChange={(e) => { const v = e.target.value.replace(/[^A-Za-z0-9._-]/g, ''); onRename(block.uid, v === '' ? undefined : v); }}
                onFocus={onSelect} onPointerDown={stop}
                title="name this step (overrides the step's name; referenced as ${steps.<name>.output})"
                style={{ flex: 1, minWidth: 0, background: 'transparent', border: 'none', borderBottom: `1px dashed ${selected ? T.dim : 'transparent'}`, color: isMatrix ? T.amber : T.textHi, fontFamily: T.mono, fontSize: 12.5, fontWeight: 700, padding: '1px 0', outline: 'none' }} />
            ) : (
              <span onClick={onSelect} style={{ cursor: 'pointer', fontSize: 12.5, fontWeight: 700, color: isGate ? T.blue : T.textHi, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{label}</span>
            )}
          </div>
          <div onClick={editable ? onToggle : onSelect} title={editable ? (selected ? 'click to collapse this step' : 'click to edit this step') : undefined} style={{ fontSize: 10, color: T.faint, cursor: 'pointer', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{action}{isScatter ? ' · scatter fan-out' : isMatrix ? ' · matrix fan-out' : ''}</div>
          {incomplete && (
            <div onClick={onSelect} title={`incomplete — ${issues.join('; ')}`}
              style={{ fontSize: 9.5, color: T.red, cursor: 'pointer', marginTop: 2, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
              ⚠ incomplete · {issues.join(' · ')}
            </div>
          )}
        </div>
        {editable && canLink && !isGate && !isMatrix && !isScatter && (
          <button onPointerDown={stop} onClick={() => onToggleParallel(block.uid)}
            title={inParallel ? 'make sequential (run after the stage above)' : 'run in parallel with the stage above'}
            style={{ background: inParallel ? T.greenSoft : 'transparent', border: `1px solid ${inParallel ? T.green : T.border}`, color: inParallel ? T.green : T.dim, fontFamily: T.mono, fontSize: 11, lineHeight: 1, padding: '4px 7px', cursor: 'pointer' }}>∥</button>
        )}
        {/* Per-block matrix toggle: fans this one step out over a list of values. Only
            on a solo (non-parallel) step, since matrix and parallel are mutually
            exclusive. Toggling it on reveals the values editor below immediately. */}
        {editable && !isGate && !inParallel && !isScatter && (
          <button onPointerDown={stop} onClick={() => onSetMatrix(block.uid, isMatrix ? null : { var: '', values: [] })}
            title={isMatrix ? 'remove matrix fan-out' : 'fan this step out over a list of values (matrix)'}
            style={{ background: isMatrix ? T.amberSoft : 'transparent', border: `1px solid ${isMatrix ? T.amber : T.border}`, color: isMatrix ? T.amber : T.dim, fontFamily: T.mono, fontSize: 11, lineHeight: 1, padding: '4px 7px', cursor: 'pointer' }}>⊞</button>
        )}
        {/* Per-block scatter toggle: fans an inline step out over the regex-matched paths
            of a shared workspace, each leg on its own clone. Inline steps only (scatter
            needs an action to run per leg), solo (not parallel), and exclusive with matrix. */}
        {editable && !isGate && !inParallel && !isMatrix && !!block.inline && (
          <button onPointerDown={stop} onClick={() => onSetScatter(block.uid, isScatter ? null : { regex: '', mode: 'dir' })}
            title={isScatter ? 'remove scatter fan-out' : 'fan this step out over workspace paths matching a regex (scatter)'}
            style={{ background: isScatter ? T.greenSoft : 'transparent', border: `1px solid ${isScatter ? T.green : T.border}`, color: isScatter ? T.green : T.dim, fontFamily: T.mono, fontSize: 11, lineHeight: 1, padding: '4px 7px', cursor: 'pointer' }}>⊟</button>
        )}
        {editable && (
          <button onPointerDown={stop} onClick={() => onRemove(block.uid)} title={isGate ? 'remove gate' : 'remove step'}
            style={{ background: 'transparent', border: 'none', color: T.faint, cursor: 'pointer', fontFamily: T.mono, fontSize: 12, lineHeight: 1, padding: 0 }}
            onMouseEnter={(e) => { (e.currentTarget as HTMLButtonElement).style.color = T.red; }}
            onMouseLeave={(e) => { (e.currentTarget as HTMLButtonElement).style.color = T.faint; }}>✕</button>
        )}
      </div>
      {showMatrix && block.matrix && <MatrixEditor uid={block.uid} matrix={block.matrix} onSetMatrix={onSetMatrix} />}
      {showScatter && block.scatter && <ScatterEditor uid={block.uid} scatter={block.scatter} onSetScatter={onSetScatter} />}
      {editable && isGate && block.approval && <GateEditor uid={block.uid} gate={block.approval} onSetApproval={onSetApproval} />}
      {editable && !isGate && selected && (
        <StepInputsEditor action={action} defWith={defWith} override={block.with ?? {}} upstream={upstream} upstreamVolumes={upstreamVolumes} repos={repos} token={token}
          onChange={(o) => onSetWith(block.uid, o)} />
      )}
    </div>
  );
}

export const PipelineBlocks = forwardRef<PipelineBlocksHandle, PipelineBlocksProps>(function PipelineBlocks({ initialSteps, catalog, editable = false, palette = [], actions = [], repos = [], token, onChange, onInspect, onPickAction, pendingAdd, onPendingConsumed, height }, ref) {
  const [blocks, setBlocks] = useState<Block[]>(() => blocksFromSteps(initialSteps));
  // Mirror blocks in a ref so the imperative handle reads current state (not a stale
  // closure) when the host converts / makes-local / edits an inline block's def.
  const blocksRef = useRef<Block[]>(blocks);
  blocksRef.current = blocks;
  useImperativeHandle(ref, () => ({
    patchBlock: (uid, patch) => setBlocks((bs) => bs.map((b) => (b.uid === uid ? { ...b, ...patch } : b))),
    getBlock: (uid) => blocksRef.current.find((b) => b.uid === uid),
  }), []);
  const [parallelMode, setParallelMode] = useState(false);
  const [parallelOpen, setParallelOpen] = useState(false);
  // matrixMode: the next step clicked from the palette becomes a matrix fan-out.
  const [matrixMode, setMatrixMode] = useState(false);
  const [selectedUid, setSelectedUid] = useState<string | null>(null);
  const [dragging, setDragging] = useState(false);
  const seq = useRef(0);
  const sensors = useSensors(useSensor(PointerSensor, { activationConstraint: { distance: 4 } }));
  // The step palette is the draggable (and persisted) side of the builder split;
  // the canvas takes the rest. Clamped so neither the palette nor the canvas shuts.
  const splitRef = useRef<HTMLDivElement>(null);
  const [paletteW, paletteHandle] = useResizablePane('split.pipelineblocks.palette.w', 260, {
    min: 200, max: 460, side: 'left', direction: 'horizontal', containerRef: splitRef, otherMin: 320,
  });

  useEffect(() => { setBlocks(blocksFromSteps(initialSteps)); setParallelMode(false); setParallelOpen(false); setMatrixMode(false); setSelectedUid(null); }, [initialSteps]);
  useEffect(() => { if (editable && onChange) onChange(stepsFromBlocks(blocks)); }, [blocks, editable, onChange]);

  const onDragStart = useCallback((e: DragStartEvent) => { void e; setDragging(true); }, []);

  const onDragEnd = useCallback((e: DragEndEvent) => {
    setDragging(false);
    const { active, over } = e;
    if (!over) return;
    const overId = String(over.id);
    setBlocks((bs) => {
      const from = bs.findIndex((b) => b.uid === active.id);
      if (from < 0) return bs;

      const work = bs.map((b) => ({ ...b }));
      // Dragging a container's first member out: promote the next member to start.
      if (!work[from].parallelWithPrev && from + 1 < work.length && work[from + 1].parallelWithPrev) {
        work[from + 1].parallelWithPrev = false;
      }
      const item = { ...work[from] };
      const rest = work.filter((_, i) => i !== from);
      const normalize = (arr: Block[]) => { if (arr.length) arr[0] = { ...arr[0], parallelWithPrev: false }; return arr; };
      const clamp = (n: number) => Math.max(0, Math.min(n, rest.length));

      if (overId.startsWith('gap-')) {
        const t = parseInt(overId.slice(4), 10);
        if (Number.isNaN(t)) return bs;
        item.parallelWithPrev = false; // a gap is a sequential slot
        rest.splice(clamp(t > from ? t - 1 : t), 0, item);
        return normalize(rest);
      }

      // Dropped onto a block. If that block belongs to a parallel container, the
      // dragged block JOINS it — inserted after the container's last member and
      // linked, so it groups with that container (not the stage above it). This is
      // how a step is dragged into a parallel block. Onto a sequential block it
      // just reorders there.
      if (overId === String(active.id)) return bs;
      const overStage = stagesOf(rest).find((st) => st.some((b) => b.uid === overId));
      // A gate can never be parallel, so it never joins a parallel container.
      if (overStage && overStage.length > 1 && !item.approval) {
        const lastIdx = rest.findIndex((b) => b.uid === overStage[overStage.length - 1].uid);
        item.parallelWithPrev = true;
        item.matrix = null; // joining a parallel group drops the matrix (exclusive)
        rest.splice(lastIdx + 1, 0, item);
        return normalize(rest);
      }
      const to = rest.findIndex((b) => b.uid === overId);
      if (to < 0) return bs;
      item.parallelWithPrev = false;
      rest.splice(clamp(to), 0, item);
      return normalize(rest);
    });
  }, []);

  // Select a block and report its identity + def so the inspector updates. A gate is
  // reported as a selection with kind 'gate' (its config lives on the card).
  const select = useCallback((b: Block) => {
    setSelectedUid(b.uid);
    onInspect?.(b.approval ? null : selectionFor(b, catalog));
  }, [onInspect, catalog]);

  // Clear the current selection so its inline editor collapses and the inspector
  // returns to its neutral state — no need to close the whole builder to stop
  // editing a step (Escape, or clicking the empty canvas, also lands here).
  const deselect = useCallback(() => {
    setSelectedUid(null);
    onInspect?.(null);
  }, [onInspect]);

  // Clicking a block toggles it: open its editor, or collapse it if already open.
  const toggle = useCallback((b: Block) => {
    setSelectedUid((cur) => {
      if (cur === b.uid) { onInspect?.(null); return null; }
      onInspect?.(b.approval ? null : selectionFor(b, catalog));
      return b.uid;
    });
  }, [onInspect, catalog]);

  // Escape deselects the open step — only bound while one is selected, so it never
  // fights any parent-level Escape when nothing is being edited. Ignored while a
  // form field has focus so it can't discard in-progress typing (clicking the block
  // again or the empty canvas still deselects regardless of focus).
  useEffect(() => {
    if (!editable || selectedUid === null) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return;
      const el = document.activeElement;
      const tag = el?.tagName;
      if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || (el as HTMLElement | null)?.isContentEditable) return;
      deselect();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [editable, selectedUid, deselect]);

  const addStep = useCallback((s: Step) => {
    // A palette Step with an empty step_id is an INLINE step (created from an action):
    // its definition rides on the block (inline.*), not the shared catalog.
    const isInline = s.step_id === '';
    const uid = `add${seq.current++}`;
    const mk = (extra: Partial<Block>): Block => isInline
      ? { uid, stepId: '', parallelWithPrev: false, name: s.name, inline: { action: s.action, timeout: s.timeout ?? undefined, with: (s.with ?? {}) as Record<string, unknown> }, ...extra }
      : { uid, stepId: s.step_id, parallelWithPrev: false, ...extra };
    // In matrix mode the clicked step becomes a solo matrix fan-out (one block, then
    // the mode ends — a matrix wraps a single step).
    if (matrixMode) {
      setBlocks((bs) => [...bs, mk({ matrix: { var: '', values: [] } })]);
      setMatrixMode(false);
    } else {
      setBlocks((bs) => [...bs, mk({ parallelWithPrev: parallelMode && parallelOpen })]);
      if (parallelMode) setParallelOpen(true);
    }
    setSelectedUid(uid);
    // Show the freshly-added step's inputs/output (inline → its own def).
    onInspect?.(isInline
      ? { uid, kind: 'inline', stepId: '', name: s.name, def: { name: s.name, action: s.action, with: (s.with ?? {}) as Record<string, unknown>, timeout: s.timeout ?? undefined } }
      : { uid, kind: 'ref', stepId: s.step_id, name: undefined, def: blockDef({ uid, stepId: s.step_id, parallelWithPrev: false }, catalog) });
  }, [parallelMode, parallelOpen, matrixMode, onInspect, catalog]);

  // Drop a host-supplied freshly-created step in as a new block (honoring the active
  // parallel/matrix mode), each pendingAdd object consumed exactly once.
  const consumedAdd = useRef<Step | null>(null);
  useEffect(() => {
    if (pendingAdd && consumedAdd.current !== pendingAdd) {
      consumedAdd.current = pendingAdd;
      addStep(pendingAdd);
      onPendingConsumed?.();
    }
  }, [pendingAdd, addStep, onPendingConsumed]);

  const toggleParallelMode = useCallback(() => { setParallelMode((m) => !m); setParallelOpen(false); setMatrixMode(false); }, []);
  const toggleMatrixMode = useCallback(() => { setMatrixMode((m) => !m); setParallelMode(false); setParallelOpen(false); }, []);
  const toggleParallel = useCallback((uid: string) => {
    // Joining a parallel group drops any matrix (the two are mutually exclusive).
    setBlocks((bs) => bs.map((b) => (b.uid === uid ? { ...b, parallelWithPrev: !b.parallelWithPrev, matrix: b.parallelWithPrev ? b.matrix : null, scatter: b.parallelWithPrev ? b.scatter : null } : b)));
  }, []);
  const setMatrix = useCallback((uid: string, matrix: MatrixConfig | null) => {
    // Matrix and scatter are mutually exclusive fan-outs on a solo step.
    setBlocks((bs) => bs.map((b) => (b.uid === uid ? { ...b, matrix, scatter: matrix ? null : b.scatter } : b)));
  }, []);
  const setScatter = useCallback((uid: string, scatter: ScatterConfig | null) => {
    setBlocks((bs) => bs.map((b) => (b.uid === uid ? { ...b, scatter, matrix: scatter ? null : b.matrix } : b)));
  }, []);
  const setApproval = useCallback((uid: string, approval: ApprovalGate) => {
    setBlocks((bs) => bs.map((b) => (b.uid === uid ? { ...b, approval } : b)));
  }, []);
  const setWith = useCallback((uid: string, override: Record<string, unknown>) => {
    setBlocks((bs) => bs.map((b) => (b.uid === uid ? { ...b, with: Object.keys(override).length > 0 ? override : undefined } : b)));
  }, []);
  const setName = useCallback((uid: string, name: string | undefined) => {
    setBlocks((bs) => bs.map((b) => (b.uid === uid ? { ...b, name } : b)));
    // Keep the inspector's output reference (${steps.<name>.output}) in sync as the
    // selected block is renamed.
    if (selectedUid === uid) {
      const b = blocks.find((x) => x.uid === uid);
      onInspect?.(b?.approval ? null : b ? selectionFor({ ...b, name }, catalog) : null);
    }
  }, [selectedUid, blocks, onInspect, catalog]);
  // An approval gate is always its own sequential block (no step, no parallel).
  const addGate = useCallback(() => {
    const uid = `add${seq.current++}`;
    setBlocks((bs) => [...bs, { uid, stepId: '', parallelWithPrev: false, approval: { message: '' } }]);
    setParallelMode(false); setParallelOpen(false); setMatrixMode(false);
    setSelectedUid(uid);
    onInspect?.(null); // a gate has no step to inspect; its config is on the card
  }, [onInspect]);
  const remove = useCallback((uid: string) => {
    setBlocks((bs) => bs.filter((b) => b.uid !== uid));
    if (selectedUid === uid) { setSelectedUid(null); onInspect?.(null); }
  }, [selectedUid, onInspect]);

  const ids = useMemo(() => blocks.map((b) => b.uid), [blocks]);
  const stages = useMemo(() => stagesOf(blocks), [blocks]);
  const indexOf = useMemo(() => {
    const m = new Map<string, number>();
    blocks.forEach((b, i) => m.set(b.uid, i));
    return m;
  }, [blocks]);

  // Outputs of steps in stages BEFORE this block — what its inputs can be wired to.
  const upstreamFor = (uid: string): UpstreamOutput[] => {
    const st = stagesOf(blocks);
    const stageIdx = st.findIndex((stage) => stage.some((b) => b.uid === uid));
    if (stageIdx <= 0) return [];
    const out: UpstreamOutput[] = [];
    for (let i = 0; i < stageIdx; i++) {
      for (const b of st[i]) {
        if (b.approval) continue;
        const def = blockDef(b, catalog);
        const nm = b.name || def?.name;
        if (!nm) continue;
        const dw = (def?.with ?? {}) as Record<string, unknown>;
        const oe = Array.isArray(dw.output_env) ? (dw.output_env as unknown[]).filter((x): x is string => typeof x === 'string') : [];
        out.push({ name: nm, outputEnv: oe });
      }
    }
    return out;
  };

  // Names of workspace volumes created by forge/create-volume steps in stages BEFORE
  // this block — the options its volume selector offers (a step attaches a volume an
  // earlier step made).
  const upstreamVolumesFor = (uid: string): string[] => {
    const st = stagesOf(blocks);
    const stageIdx = st.findIndex((stage) => stage.some((b) => b.uid === uid));
    if (stageIdx <= 0) return [];
    const names: string[] = [];
    for (let i = 0; i < stageIdx; i++) {
      for (const b of st[i]) {
        if (b.approval) continue;
        const def = blockDef(b, catalog);
        if (!def || !actionCreatesVolume(def.action)) continue;
        const eff = { ...((def.with ?? {}) as Record<string, unknown>), ...(b.with ?? {}) };
        const nm = createdVolumeName(eff);
        if (nm && !names.includes(nm)) names.push(nm);
      }
    }
    return names;
  };

  const card = (b: Block, inParallel: boolean) => {
    const isGate = !!b.approval;
    const def = blockDef(b, catalog);
    // An inline step's name IS its own name; a reference falls back to the def name.
    const label = isGate ? 'approval gate' : (b.name || def?.name || (b.stepId ? b.stepId.slice(0, 8) + '…' : 'inline step'));
    const action = isGate ? 'manual approval' : (def?.action ?? '');
    // The def config the per-occurrence override merges over: an inline step's def
    // lives on the block (inline.with); a reference's on the shared catalog step.
    const defWith = (def?.with ?? {}) as Record<string, unknown>;
    return (
      <BlockCard key={b.uid} block={b} label={label} action={action} editable={editable}
        canLink={(indexOf.get(b.uid) ?? 0) > 0} inParallel={inParallel} isGate={isGate}
        selected={selectedUid === b.uid} defWith={defWith}
        upstream={selectedUid === b.uid ? upstreamFor(b.uid) : []}
        upstreamVolumes={selectedUid === b.uid ? upstreamVolumesFor(b.uid) : []} repos={repos} token={token}
        onSelect={() => select(b)} onToggle={() => toggle(b)} onRename={setName} onSetWith={setWith}
        onToggleParallel={toggleParallel} onSetMatrix={setMatrix} onSetScatter={setScatter} onSetApproval={setApproval} onRemove={remove} />
    );
  };

  // Flat rows: cards are direct children (so dnd's vertical sort works), with a
  // sequential Gap before each stage + at the end, and a JoinZone under each
  // parallel container.
  const rows: ReactNode[] = [];
  let bi = 0;
  stages.forEach((stage, si) => {
    const isParallel = stage.length > 1;
    if (editable) rows.push(<Gap key={`gap-${bi}`} id={`gap-${bi}`} dragging={dragging} showArrow={si > 0} />);
    if (isParallel) {
      rows.push(
        <div key={`hdr-${stage[0].uid}`} style={{ fontFamily: T.mono, fontSize: 9, color: T.green, letterSpacing: 1, textTransform: 'uppercase', padding: '2px 0 4px' }}>
          ∥ parallel · runs concurrently
        </div>,
      );
      // Members side by side, so the simultaneous run reads at a glance.
      rows.push(
        <div key={`row-${stage[0].uid}`} style={{ display: 'flex', gap: 6, flexWrap: 'wrap', alignItems: 'stretch', marginBottom: 4 }}>
          {stage.map((b) => card(b, true))}
        </div>,
      );
    } else {
      rows.push(card(stage[0], false));
    }
    bi += stage.length;
  });
  if (editable) rows.push(<Gap key={`gap-${bi}`} id={`gap-${bi}`} dragging={dragging} showArrow={stages.length > 0} />);

  const activeEmptyParallel = editable && parallelMode && !parallelOpen ? (
    <div style={{ border: `1px dashed ${T.green}`, borderLeft: `3px solid ${T.green}`, background: T.bgAlt }}>
      <div style={{ fontFamily: T.mono, fontSize: 9, color: T.green, letterSpacing: 1, textTransform: 'uppercase', padding: '5px 10px', borderBottom: `1px dashed ${T.border}` }}>
        ∥ parallel · active
      </div>
      <div style={{ fontFamily: T.mono, fontSize: 11, color: T.faint, padding: 12 }}>→ click steps on the left to add them here</div>
    </div>
  ) : null;

  // Matrix mode has no canvas container (a matrix wraps a single step), so without
  // this the amber palette highlight was the only cue — clicking "matrix" felt inert.
  // Show the same active affordance parallel has: the next clicked step fans out, and
  // any existing block can be turned into a matrix with its own ⊞ toggle.
  const activeMatrixHint = editable && matrixMode ? (
    <div style={{ border: `1px dashed ${T.amber}`, borderLeft: `3px solid ${T.amber}`, background: T.bgAlt }}>
      <div style={{ fontFamily: T.mono, fontSize: 9, color: T.amber, letterSpacing: 1, textTransform: 'uppercase', padding: '5px 10px', borderBottom: `1px dashed ${T.border}` }}>
        ⊞ matrix · active
      </div>
      <div style={{ fontFamily: T.mono, fontSize: 11, color: T.faint, padding: 12 }}>→ click a step on the left to fan it out — or press ⊞ on any step below</div>
    </div>
  ) : null;

  const list = (
    // Clicking the empty canvas (not a card) deselects the open step.
    <div onClick={(e) => { if (editable && e.target === e.currentTarget) deselect(); }}
      style={{ display: 'flex', flexDirection: 'column', padding: 14, overflow: 'auto', flex: 1 }}>
      {blocks.length === 0 && !activeEmptyParallel && !activeMatrixHint ? (
        <div style={{ fontFamily: T.mono, fontSize: 12, color: T.faint }}>
          → empty pipeline{editable ? ' — add steps from the left' : ''}
        </div>
      ) : (
        <>{rows}{activeEmptyParallel}{activeMatrixHint}</>
      )}
    </div>
  );

  return (
    <div ref={splitRef} style={{ display: 'flex', height: height ?? '100%', minHeight: 200, border: `1px solid ${T.border}`, background: T.bg }}>
      {editable && (
        <div style={{ width: paletteW, flexShrink: 0, background: T.bgAlt, overflow: 'auto' }}>
          <div style={{ padding: '12px 16px', fontFamily: T.mono, fontSize: 11, color: T.faint, letterSpacing: 1, textTransform: 'uppercase', borderBottom: `1px solid ${T.border}` }}>
            blocks · click to add
          </div>
          <button onClick={toggleParallelMode}
            title="start a parallel container, then click steps to fill it; click again to finish"
            style={{ width: '100%', textAlign: 'left', padding: '12px 16px', background: parallelMode ? T.greenSoft : 'transparent', border: 'none', borderBottom: `1px solid ${T.border}`, borderLeft: `3px solid ${T.green}`, fontFamily: T.mono, cursor: 'pointer', color: parallelMode ? T.green : T.text }}>
            <div style={{ fontSize: 14, fontWeight: 700 }}>∥ parallel block</div>
            <div style={{ fontSize: 11, color: parallelMode ? T.green : T.faint, marginTop: 2 }}>
              {parallelMode ? 'active — add steps, click to finish' : 'run the next steps together'}
            </div>
          </button>
          <button onClick={toggleMatrixMode}
            title="fan a step out over a list of values — click this, then click a step to wrap"
            style={{ width: '100%', textAlign: 'left', padding: '12px 16px', background: matrixMode ? T.amberSoft : 'transparent', border: 'none', borderBottom: `1px solid ${T.border}`, borderLeft: `3px solid ${T.amber}`, fontFamily: T.mono, cursor: 'pointer', color: matrixMode ? T.amber : T.text }}>
            <div style={{ fontSize: 14, fontWeight: 700 }}>⊞ matrix block</div>
            <div style={{ fontSize: 11, color: matrixMode ? T.amber : T.faint, marginTop: 2 }}>
              {matrixMode ? 'active — click a step to fan out' : 'run one step per value in a list'}
            </div>
          </button>
          <button onClick={addGate}
            title="add a manual-approval gate — the run pauses here until approved (no step needed)"
            style={{ width: '100%', textAlign: 'left', padding: '12px 16px', background: 'transparent', border: 'none', borderBottom: `1px solid ${T.border}`, borderLeft: `3px solid ${T.blue}`, fontFamily: T.mono, cursor: 'pointer', color: T.text }}>
            <div style={{ fontSize: 14, fontWeight: 700 }}>⏸ approval gate</div>
            <div style={{ fontSize: 11, color: T.faint, marginTop: 2 }}>pause for manual approval</div>
          </button>
          {/* Actions — click to create & configure a new INLINE step from an action
              (private to this pipeline; can be converted to a reusable step later). */}
          {onPickAction && actions.length > 0 && (
            <>
              <div style={paletteSection}>actions · create an inline step</div>
              {actions.map((a) => (
                <button key={a.name} onClick={() => onPickAction(a)} title="create & configure a new inline step from this action (private to this pipeline)"
                  style={{ width: '100%', textAlign: 'left', padding: '10px 16px', background: 'transparent', border: 'none', borderBottom: `1px solid ${T.border}`, fontFamily: T.mono, cursor: 'pointer', color: T.text }}
                  onMouseEnter={(e) => { (e.currentTarget as HTMLButtonElement).style.background = T.cardHi; }}
                  onMouseLeave={(e) => { (e.currentTarget as HTMLButtonElement).style.background = 'transparent'; }}>
                  <div style={{ fontSize: 13, fontWeight: 600 }}>{a.name}</div>
                  {a.summary && <div style={{ fontSize: 10.5, color: T.faint, marginTop: 2, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{a.summary}</div>}
                </button>
              ))}
            </>
          )}
          {/* Reusable steps — add a pre-configured (saved) step. */}
          <div style={paletteSection}>steps · reuse a saved step</div>
          {palette.length === 0 ? (
            <div style={{ padding: '12px 16px', fontFamily: T.mono, fontSize: 11, color: T.faint }}>→ no saved steps — create one from an action above or in the steps tab</div>
          ) : palette.map((s) => (
            <button key={s.step_id} onClick={() => addStep(s)} title="add to pipeline (can be added more than once)"
              style={{ width: '100%', textAlign: 'left', padding: '12px 16px', background: 'transparent', border: 'none', borderBottom: `1px solid ${T.border}`, fontFamily: T.mono, cursor: 'pointer', color: T.text }}
              onMouseEnter={(e) => { (e.currentTarget as HTMLButtonElement).style.background = T.cardHi; }}
              onMouseLeave={(e) => { (e.currentTarget as HTMLButtonElement).style.background = 'transparent'; }}>
              <div style={{ fontSize: 14, fontWeight: 600 }}>{s.name}</div>
              <div style={{ fontSize: 11, color: T.faint, marginTop: 2 }}>{s.action}</div>
            </button>
          ))}
        </div>
      )}
      {/* Drag to rebalance the palette against the canvas. */}
      {editable && paletteHandle}
      <DndContext
        sensors={sensors}
        collisionDetection={collisionDetection}
        measuring={{ droppable: { strategy: MeasuringStrategy.Always } }}
        onDragStart={onDragStart}
        onDragEnd={onDragEnd}
        onDragCancel={() => setDragging(false)}
      >
        <SortableContext items={ids} strategy={verticalListSortingStrategy}>
          {list}
        </SortableContext>
      </DndContext>
    </div>
  );
});

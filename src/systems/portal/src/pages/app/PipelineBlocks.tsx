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
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import type { ReactNode } from 'react';
import {
  DndContext, PointerSensor, pointerWithin, closestCenter, useSensor, useSensors, useDroppable, MeasuringStrategy,
} from '@dnd-kit/core';
import type { DragEndEvent, DragStartEvent, CollisionDetection } from '@dnd-kit/core';
import { SortableContext, verticalListSortingStrategy, useSortable } from '@dnd-kit/sortable';
import { CSS } from '@dnd-kit/utilities';
import { T } from '../../theme';
import { useViewport, clamp } from '../../hooks/useViewport';
import type { Step, GitRepo, WorkflowAction } from '../../api/bff';
import { Block, StepRef, MatrixConfig, ApprovalGate, blocksFromSteps, stepsFromBlocks, stagesOf } from './pipelineGraph';
import { StepInputsEditor, UpstreamOutput } from './StepInputsEditor';
import { gitRepoFromWith, stepConfigIssues } from './stepSchema';

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
  // Reports the step_id and effective (occurrence) name of the selected block (null
  // step for a gate or no selection), so the builder can show that step's
  // inputs/output — referenced by the occurrence name — in an inspector panel.
  onInspect?: (stepId: string | null, name?: string) => void;
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

function labelFor(catalog: Record<string, Step>, stepId: string): { label: string; action: string } {
  const s = catalog[stepId];
  return { label: s?.name ?? stepId.slice(0, 8) + '…', action: s?.action ?? '' };
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
  repos: GitRepo[];
  token?: string;
  onSelect: () => void;
  onRename: (uid: string, name: string | undefined) => void;
  onSetWith: (uid: string, override: Record<string, unknown>) => void;
  onToggleParallel: (uid: string) => void;
  onSetMatrix: (uid: string, matrix: MatrixConfig | null) => void;
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
    </div>
  );
}

function BlockCard({ block, label, action, editable, canLink, inParallel, isGate, selected, defWith, upstream, repos, token, onSelect, onRename, onSetWith, onToggleParallel, onSetMatrix, onSetApproval, onRemove }: BlockCardProps) {
  const { attributes, listeners, setNodeRef, transform, transition, isDragging } = useSortable({ id: block.uid, disabled: !editable });
  const leftBar = isGate ? T.blue : block.matrix ? T.amber : inParallel ? T.green : T.dim;
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
  const prefix = isGate ? '⏸ ' : isMatrix ? '⊞ ' : '';
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
          <div onClick={onSelect} style={{ fontSize: 10, color: T.faint, cursor: 'pointer', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{action}{isMatrix ? ' · matrix fan-out' : ''}</div>
          {incomplete && (
            <div onClick={onSelect} title={`incomplete — ${issues.join('; ')}`}
              style={{ fontSize: 9.5, color: T.red, cursor: 'pointer', marginTop: 2, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
              ⚠ incomplete · {issues.join(' · ')}
            </div>
          )}
        </div>
        {editable && canLink && !isGate && !isMatrix && (
          <button onPointerDown={stop} onClick={() => onToggleParallel(block.uid)}
            title={inParallel ? 'make sequential (run after the stage above)' : 'run in parallel with the stage above'}
            style={{ background: inParallel ? T.greenSoft : 'transparent', border: `1px solid ${inParallel ? T.green : T.border}`, color: inParallel ? T.green : T.dim, fontFamily: T.mono, fontSize: 11, lineHeight: 1, padding: '4px 7px', cursor: 'pointer' }}>∥</button>
        )}
        {editable && (
          <button onPointerDown={stop} onClick={() => onRemove(block.uid)} title={isGate ? 'remove gate' : 'remove step'}
            style={{ background: 'transparent', border: 'none', color: T.faint, cursor: 'pointer', fontFamily: T.mono, fontSize: 12, lineHeight: 1, padding: 0 }}
            onMouseEnter={(e) => { (e.currentTarget as HTMLButtonElement).style.color = T.red; }}
            onMouseLeave={(e) => { (e.currentTarget as HTMLButtonElement).style.color = T.faint; }}>✕</button>
        )}
      </div>
      {showMatrix && block.matrix && <MatrixEditor uid={block.uid} matrix={block.matrix} onSetMatrix={onSetMatrix} />}
      {editable && isGate && block.approval && <GateEditor uid={block.uid} gate={block.approval} onSetApproval={onSetApproval} />}
      {editable && !isGate && selected && (
        <StepInputsEditor action={action} defWith={defWith} override={block.with ?? {}} upstream={upstream} repos={repos} token={token}
          onChange={(o) => onSetWith(block.uid, o)} />
      )}
    </div>
  );
}

export function PipelineBlocks({ initialSteps, catalog, editable = false, palette = [], actions = [], repos = [], token, onChange, onInspect, onPickAction, pendingAdd, onPendingConsumed, height }: PipelineBlocksProps) {
  const [blocks, setBlocks] = useState<Block[]>(() => blocksFromSteps(initialSteps));
  const [parallelMode, setParallelMode] = useState(false);
  const [parallelOpen, setParallelOpen] = useState(false);
  // matrixMode: the next step clicked from the palette becomes a matrix fan-out.
  const [matrixMode, setMatrixMode] = useState(false);
  const [selectedUid, setSelectedUid] = useState<string | null>(null);
  const [dragging, setDragging] = useState(false);
  const seq = useRef(0);
  const sensors = useSensors(useSensor(PointerSensor, { activationConstraint: { distance: 4 } }));
  const { width } = useViewport();
  const paletteW = clamp(Math.round(width * 0.2), 200, 300); // step palette scales with the screen

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

  // Select a block and report its step + occurrence name so the inspector updates.
  const select = useCallback((b: Block) => {
    setSelectedUid(b.uid);
    onInspect?.(b.approval ? null : b.stepId, b.name);
  }, [onInspect]);

  const addStep = useCallback((s: Step) => {
    // In matrix mode the clicked step becomes a solo matrix fan-out (one block,
    // then the mode ends — a matrix wraps a single step).
    const uid = `add${seq.current++}`;
    if (matrixMode) {
      setBlocks((bs) => [...bs, { uid, stepId: s.step_id, parallelWithPrev: false, matrix: { var: '', values: [] } }]);
      setMatrixMode(false);
    } else {
      setBlocks((bs) => [...bs, { uid, stepId: s.step_id, parallelWithPrev: parallelMode && parallelOpen }]);
      if (parallelMode) setParallelOpen(true);
    }
    setSelectedUid(uid);
    onInspect?.(s.step_id); // show the freshly-added step's inputs/output
  }, [parallelMode, parallelOpen, matrixMode, onInspect]);

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
    setBlocks((bs) => bs.map((b) => (b.uid === uid ? { ...b, parallelWithPrev: !b.parallelWithPrev, matrix: b.parallelWithPrev ? b.matrix : null } : b)));
  }, []);
  const setMatrix = useCallback((uid: string, matrix: MatrixConfig | null) => {
    setBlocks((bs) => bs.map((b) => (b.uid === uid ? { ...b, matrix } : b)));
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
      onInspect?.(b?.approval ? null : (b?.stepId ?? null), name);
    }
  }, [selectedUid, blocks, onInspect]);
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
        const def = catalog[b.stepId];
        const nm = b.name || def?.name;
        if (!nm) continue;
        const dw = (def?.with ?? {}) as Record<string, unknown>;
        const oe = Array.isArray(dw.output_env) ? (dw.output_env as unknown[]).filter((x): x is string => typeof x === 'string') : [];
        out.push({ name: nm, outputEnv: oe });
      }
    }
    return out;
  };

  const card = (b: Block, inParallel: boolean) => {
    const isGate = !!b.approval;
    const { label, action } = isGate ? { label: 'approval gate', action: 'manual approval' } : labelFor(catalog, b.stepId);
    return (
      <BlockCard key={b.uid} block={b} label={label} action={action} editable={editable}
        canLink={(indexOf.get(b.uid) ?? 0) > 0} inParallel={inParallel} isGate={isGate}
        selected={selectedUid === b.uid} defWith={(catalog[b.stepId]?.with ?? {}) as Record<string, unknown>}
        upstream={selectedUid === b.uid ? upstreamFor(b.uid) : []} repos={repos} token={token}
        onSelect={() => select(b)} onRename={setName} onSetWith={setWith}
        onToggleParallel={toggleParallel} onSetMatrix={setMatrix} onSetApproval={setApproval} onRemove={remove} />
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

  const list = (
    <div style={{ display: 'flex', flexDirection: 'column', padding: 14, overflow: 'auto', flex: 1 }}>
      {blocks.length === 0 && !activeEmptyParallel ? (
        <div style={{ fontFamily: T.mono, fontSize: 12, color: T.faint }}>
          → empty pipeline{editable ? ' — add steps from the left' : ''}
        </div>
      ) : (
        <>{rows}{activeEmptyParallel}</>
      )}
    </div>
  );

  return (
    <div style={{ display: 'flex', height: height ?? '100%', minHeight: 200, border: `1px solid ${T.border}`, background: T.bg }}>
      {editable && (
        <div style={{ width: paletteW, flexShrink: 0, borderRight: `1px solid ${T.border}`, background: T.bgAlt, overflow: 'auto' }}>
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
          {/* Actions — click to create & configure a new step inline from an action. */}
          {onPickAction && actions.length > 0 && (
            <>
              <div style={paletteSection}>actions · create a step</div>
              {actions.map((a) => (
                <button key={a.name} onClick={() => onPickAction(a)} title="create & configure a new step from this action"
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
}

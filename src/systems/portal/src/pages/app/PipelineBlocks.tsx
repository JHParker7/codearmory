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
import type { Step } from '../../api/bff';
import { Block, StepRef, blocksFromSteps, stepsFromBlocks, stagesOf } from './pipelineGraph';

export interface PipelineBlocksProps {
  initialSteps: StepRef[];
  catalog: Record<string, Step>;
  editable?: boolean;
  palette?: Step[];
  onChange?: (steps: StepRef[]) => void;
  height?: number | string;
}

// Prefer a droppable the pointer is inside (the gaps, join zones, and blocks under
// the cursor); fall back to nearest centre over empty space.
const collisionDetection: CollisionDetection = (args) => {
  const within = pointerWithin(args);
  return within.length ? within : closestCenter(args);
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
  onToggleParallel: (uid: string) => void;
  onRemove: (uid: string) => void;
}

function BlockCard({ block, label, action, editable, canLink, inParallel, onToggleParallel, onRemove }: BlockCardProps) {
  const { attributes, listeners, setNodeRef, transform, transition, isDragging } = useSortable({ id: block.uid, disabled: !editable });
  const style: React.CSSProperties = {
    transform: CSS.Transform.toString(transform),
    transition,
    opacity: isDragging ? 0.6 : 1,
    zIndex: isDragging ? 5 : undefined,
    display: 'flex', alignItems: 'center', gap: 10,
    // In a parallel stage the cards sit side by side in a flex row; otherwise each
    // is a full-width row in the vertical sequence.
    marginBottom: inParallel ? 0 : 4,
    flex: inParallel ? '1 1 160px' : undefined,
    minWidth: inParallel ? 0 : undefined,
    background: inParallel ? T.bgAlt : T.card, border: `1px solid ${isDragging ? T.green : T.border}`,
    borderLeft: `3px solid ${inParallel ? T.green : T.dim}`,
    padding: '8px 10px', fontFamily: T.mono,
    cursor: editable ? 'grab' : 'default', touchAction: editable ? 'none' : undefined, userSelect: 'none',
  };
  const stop = (e: React.PointerEvent) => e.stopPropagation();
  return (
    <div ref={setNodeRef} style={style} {...(editable ? attributes : {})} {...(editable ? listeners : {})}>
      {editable && <span style={{ color: T.faint, fontSize: 13, lineHeight: 1 }}>⠿</span>}
      <div style={{ flex: 1, minWidth: 0 }}>
        <div style={{ fontSize: 12.5, fontWeight: 700, color: T.textHi, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{label}</div>
        <div style={{ fontSize: 10, color: T.faint, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{action}</div>
      </div>
      {editable && canLink && (
        <button onPointerDown={stop} onClick={() => onToggleParallel(block.uid)}
          title={inParallel ? 'make sequential (run after the stage above)' : 'run in parallel with the stage above'}
          style={{ background: inParallel ? T.greenSoft : 'transparent', border: `1px solid ${inParallel ? T.green : T.border}`, color: inParallel ? T.green : T.dim, fontFamily: T.mono, fontSize: 11, lineHeight: 1, padding: '4px 7px', cursor: 'pointer' }}>∥</button>
      )}
      {editable && (
        <button onPointerDown={stop} onClick={() => onRemove(block.uid)} title="remove step"
          style={{ background: 'transparent', border: 'none', color: T.faint, cursor: 'pointer', fontFamily: T.mono, fontSize: 12, lineHeight: 1, padding: 0 }}
          onMouseEnter={(e) => { (e.currentTarget as HTMLButtonElement).style.color = T.red; }}
          onMouseLeave={(e) => { (e.currentTarget as HTMLButtonElement).style.color = T.faint; }}>✕</button>
      )}
    </div>
  );
}

export function PipelineBlocks({ initialSteps, catalog, editable = false, palette = [], onChange, height }: PipelineBlocksProps) {
  const [blocks, setBlocks] = useState<Block[]>(() => blocksFromSteps(initialSteps));
  const [parallelMode, setParallelMode] = useState(false);
  const [parallelOpen, setParallelOpen] = useState(false);
  const [dragging, setDragging] = useState(false);
  const seq = useRef(0);
  const sensors = useSensors(useSensor(PointerSensor, { activationConstraint: { distance: 4 } }));

  useEffect(() => { setBlocks(blocksFromSteps(initialSteps)); setParallelMode(false); setParallelOpen(false); }, [initialSteps]);
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
      if (overStage && overStage.length > 1) {
        const lastIdx = rest.findIndex((b) => b.uid === overStage[overStage.length - 1].uid);
        item.parallelWithPrev = true;
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

  const addStep = useCallback((s: Step) => {
    setBlocks((bs) => [...bs, { uid: `add${seq.current++}`, stepId: s.step_id, parallelWithPrev: parallelMode && parallelOpen }]);
    if (parallelMode) setParallelOpen(true);
  }, [parallelMode, parallelOpen]);

  const toggleParallelMode = useCallback(() => { setParallelMode((m) => !m); setParallelOpen(false); }, []);
  const toggleParallel = useCallback((uid: string) => {
    setBlocks((bs) => bs.map((b) => (b.uid === uid ? { ...b, parallelWithPrev: !b.parallelWithPrev } : b)));
  }, []);
  const remove = useCallback((uid: string) => { setBlocks((bs) => bs.filter((b) => b.uid !== uid)); }, []);

  const ids = useMemo(() => blocks.map((b) => b.uid), [blocks]);
  const stages = useMemo(() => stagesOf(blocks), [blocks]);
  const indexOf = useMemo(() => {
    const m = new Map<string, number>();
    blocks.forEach((b, i) => m.set(b.uid, i));
    return m;
  }, [blocks]);

  const card = (b: Block, inParallel: boolean) => {
    const { label, action } = labelFor(catalog, b.stepId);
    return (
      <BlockCard key={b.uid} block={b} label={label} action={action} editable={editable}
        canLink={(indexOf.get(b.uid) ?? 0) > 0} inParallel={inParallel}
        onToggleParallel={toggleParallel} onRemove={remove} />
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
        <div style={{ width: 280, flexShrink: 0, borderRight: `1px solid ${T.border}`, background: T.bgAlt, overflow: 'auto' }}>
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
          {palette.length === 0 ? (
            <div style={{ padding: '14px 16px', fontFamily: T.mono, fontSize: 12, color: T.faint }}>→ no steps yet — create one in the Steps tab</div>
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

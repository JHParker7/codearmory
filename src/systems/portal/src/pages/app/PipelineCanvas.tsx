/**
 * react-flow pipeline canvas — the portal's visual pipeline builder/viewer.
 *
 * Each workflow step is a node; wiring a node into another sequences them, and
 * nodes left on the same topological layer become a parallel stage. The graph is
 * translated to/from the backend's ordered-steps-with-parallel_group model by the
 * pure helpers in ./pipelineGraph (graphFromSteps / stepsFromGraph), so this file
 * only owns rendering and interaction.
 *
 * Two modes:
 *   - read-only (detail view): static, fit-to-view diagram of a saved pipeline.
 *   - editable (builder): a step palette adds nodes, dragging between handles
 *     wires them, and onChange reports the derived StepRef[] (or a cycle error).
 */
import { useCallback, useEffect, useMemo, useRef } from 'react';
import {
  ReactFlow, ReactFlowProvider, Background, Controls, Handle, Position,
  useNodesState, useEdgesState, addEdge,
} from '@xyflow/react';
import type { Node, Edge, Connection, NodeProps } from '@xyflow/react';
import '@xyflow/react/dist/style.css';
import { T } from '../../theme';
import type { Step } from '../../api/bff';
import {
  StepRef, graphFromSteps, stepsFromGraph, NODE_W, NODE_H,
} from './pipelineGraph';

/** Per-node data carried on the react-flow node. */
interface StepNodeData {
  stepId: string;
  label: string;
  action: string;
  editable: boolean;
  onDelete: (id: string) => void;
  [key: string]: unknown;
}
type StepNode = Node<StepNodeData, 'step'>;

/** A single step block. Target handle (top) accepts upstream wires; source
 * handle (bottom) starts downstream ones. */
function StepNodeView({ id, data, selected }: NodeProps<StepNode>) {
  return (
    <div style={{
      width: NODE_W, minHeight: NODE_H, boxSizing: 'border-box',
      background: T.card, border: `1px solid ${selected ? T.green : T.border}`,
      borderLeft: `3px solid ${T.green}`, padding: '7px 9px', fontFamily: T.mono,
      display: 'flex', flexDirection: 'column', justifyContent: 'center', gap: 2,
    }}>
      <Handle type="target" position={Position.Top} style={{ background: T.dim, width: 7, height: 7 }} />
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 6 }}>
        <span style={{ fontSize: 11.5, fontWeight: 700, color: T.textHi, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
          {data.label}
        </span>
        {data.editable && (
          <button
            onClick={(e) => { e.stopPropagation(); data.onDelete(id); }}
            title="remove step"
            style={{ background: 'transparent', border: 'none', color: T.faint, cursor: 'pointer', fontFamily: T.mono, fontSize: 11, lineHeight: 1, padding: 0 }}
            onMouseEnter={(e) => { (e.currentTarget as HTMLButtonElement).style.color = T.red; }}
            onMouseLeave={(e) => { (e.currentTarget as HTMLButtonElement).style.color = T.faint; }}
          >✕</button>
        )}
      </div>
      <span style={{ fontSize: 9.5, color: T.faint, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
        {data.action}
      </span>
      <Handle type="source" position={Position.Bottom} style={{ background: T.green, width: 7, height: 7 }} />
    </div>
  );
}

const nodeTypes = { step: StepNodeView };

export interface PipelineCanvasProps {
  /** Initial pipeline as stored: ordered steps with optional parallel_group. */
  initialSteps: StepRef[];
  /** step_id -> step metadata, for node labels. */
  catalog: Record<string, Step>;
  editable?: boolean;
  /** Steps available to add as nodes (editable mode). */
  palette?: Step[];
  /** Reports the derived steps (or a cycle error) whenever the graph changes. */
  onChange?: (result: { steps: StepRef[] | null; error: string | null }) => void;
  /** Canvas height; defaults to filling the parent. */
  height?: number | string;
}

/** Builds the display label for a step_id from the catalog (falls back to the id). */
function labelFor(catalog: Record<string, Step>, stepId: string): { label: string; action: string } {
  const s = catalog[stepId];
  return { label: s?.name ?? stepId.slice(0, 8) + '…', action: s?.action ?? '' };
}

function Canvas({ initialSteps, catalog, editable = false, palette = [], onChange, height }: PipelineCanvasProps) {
  const initial = useMemo(() => graphFromSteps(initialSteps), [initialSteps]);

  const onDelete = useCallback((id: string) => {
    setNodes((ns) => ns.filter((n) => n.id !== id));
    setEdges((es) => es.filter((e) => e.source !== id && e.target !== id));
  // setNodes/setEdges are stable from the hooks below.
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const toRfNode = useCallback((g: { id: string; position: { x: number; y: number }; data: { stepId: string } }): StepNode => {
    const { label, action } = labelFor(catalog, g.data.stepId);
    return { id: g.id, type: 'step', position: g.position, data: { stepId: g.data.stepId, label, action, editable, onDelete } };
  }, [catalog, editable, onDelete]);

  const [nodes, setNodes, onNodesChange] = useNodesState<StepNode>(initial.nodes.map(toRfNode));
  const [edges, setEdges, onEdgesChange] = useEdgesState<Edge>(initial.edges.map((e) => ({ id: e.id, source: e.source, target: e.target })));

  // Re-seed when the initial pipeline changes (e.g. opening the editor on a
  // different workflow). Editable graphs are user-owned after first mount.
  useEffect(() => {
    setNodes(initial.nodes.map(toRfNode));
    setEdges(initial.edges.map((e) => ({ id: e.id, source: e.source, target: e.target })));
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [initial]);

  // Derive StepRef[] from the live graph and report up (editable only).
  useEffect(() => {
    if (!editable || !onChange) return;
    try {
      const steps = stepsFromGraph(
        nodes.map((n) => ({ id: n.id, position: n.position, data: { stepId: n.data.stepId } })),
        edges.map((e) => ({ id: e.id, source: e.source, target: e.target })),
      );
      onChange({ steps, error: null });
    } catch (e) {
      onChange({ steps: null, error: (e as Error).message });
    }
  }, [nodes, edges, editable, onChange]);

  // Connecting two nodes can create duplicate wires; suffix the edge id so each
  // connection is distinct (a fan-out/fan-in graph has many edges).
  const edgeSeq = useRef(0);
  const onConnect = useCallback((c: Connection) => setEdges((es) => addEdge({ ...c, id: `e${edgeSeq.current++}:${c.source}->${c.target}` }, es)), [setEdges]);

  // Each add mints a fresh instance id so the same catalog step can be dropped in
  // multiple times; node.data.stepId keeps the link back to the step definition.
  const instanceSeq = useRef(0);
  const addStep = useCallback((step: Step) => {
    setNodes((ns) => {
      const { label, action } = labelFor(catalog, step.step_id);
      const id = `add${instanceSeq.current++}`;
      const y = ns.length * (NODE_H + 28);
      return [...ns, { id, type: 'step', position: { x: 40, y }, data: { stepId: step.step_id, label, action, editable, onDelete } }];
    });
  }, [catalog, editable, onDelete, setNodes]);

  return (
    <div style={{ display: 'flex', height: height ?? '100%', minHeight: 280, border: `1px solid ${T.border}`, background: T.bg }}>
      {editable && (
        <div style={{ width: 280, flexShrink: 0, borderRight: `1px solid ${T.border}`, background: T.bgAlt, overflow: 'auto' }}>
          <div style={{ padding: '12px 16px', fontFamily: T.mono, fontSize: 11, color: T.faint, letterSpacing: 1, textTransform: 'uppercase', borderBottom: `1px solid ${T.border}` }}>
            steps · click to add
          </div>
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
      <div style={{ flex: 1, minWidth: 0 }}>
        <ReactFlow
          nodes={nodes}
          edges={edges}
          nodeTypes={nodeTypes}
          onNodesChange={onNodesChange}
          onEdgesChange={onEdgesChange}
          onConnect={editable ? onConnect : undefined}
          nodesDraggable={editable}
          nodesConnectable={editable}
          elementsSelectable={editable}
          nodesFocusable={editable}
          edgesFocusable={editable}
          // Read-only graphs are a static diagram: no pan/zoom/scroll capture, so
          // they never hijack the page (wheel scrolls the panel, clicks pass through).
          panOnDrag={editable}
          panOnScroll={false}
          zoomOnScroll={editable}
          zoomOnPinch={editable}
          zoomOnDoubleClick={editable}
          preventScrolling={editable}
          fitView
          proOptions={{ hideAttribution: true }}
          style={{ background: T.bg }}
        >
          <Background color={T.border} gap={18} />
          {editable && <Controls showInteractive={false} />}
        </ReactFlow>
      </div>
    </div>
  );
}

/** Public wrapper — ReactFlow hooks require a provider in scope. */
export function PipelineCanvas(props: PipelineCanvasProps) {
  return (
    <ReactFlowProvider>
      <Canvas {...props} />
    </ReactFlowProvider>
  );
}

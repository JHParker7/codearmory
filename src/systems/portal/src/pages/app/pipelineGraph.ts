/**
 * Pure mapping between the react-flow pipeline canvas and the workflows backend
 * model — the portal twin of the CLI's tuiPipelineStages.
 *
 * The backend stores a pipeline as an *ordered* list of steps, each with an
 * optional parallel_group: consecutive steps sharing the same non-nil
 * parallel_group run concurrently (one "stage"); every other step is its own
 * sequential stage. The canvas, by contrast, is a DAG of step nodes wired
 * together. This module is the (pure, unit-tested) bridge:
 *
 *   stagesFromSteps  ordered steps      -> stages (consecutive-group collapse)
 *   graphFromSteps   ordered steps      -> laid-out nodes + edges to render
 *   stepsFromGraph   nodes + edges      -> ordered steps with parallel_group,
 *                                          by topological layering (one layer =
 *                                          one parallel stage)
 *
 * Keeping react-flow's types out of here lets the mapping be tested under mocha
 * without the library; the canvas component adapts these shapes to RF nodes.
 */

/** A pipeline step reference as the workflows API stores/accepts it. */
export interface StepRef {
  step_id: string;
  parallel_group?: number | null;
}

/** Minimal node shape — structurally compatible with a react-flow Node. */
export interface GraphNode {
  id: string;
  position: { x: number; y: number };
  data: { stepId: string };
}

/** Minimal edge shape — structurally compatible with a react-flow Edge. */
export interface GraphEdge {
  id: string;
  source: string;
  target: string;
}

// Layout constants (vertical flow: stages stack top→bottom, parallel steps
// spread left→right within a stage).
export const NODE_W = 176;
export const NODE_H = 50;
const X_GAP = 34; // horizontal gap between parallel siblings
const Y_GAP = 96; // vertical gap between stages (stage pitch)

/**
 * Collapse an ordered step list into sequential stages. Mirrors the CLI's
 * tuiPipelineStages: only *consecutive* steps with the same non-nil
 * parallel_group merge into one stage; a nil group (or a different group value,
 * or a gap) starts a new stage. Returns stages of step_ids in order.
 */
export function stagesFromSteps(steps: StepRef[]): string[][] {
  const stages: string[][] = [];
  let prevGroup: number | null | undefined = undefined;
  for (const s of steps) {
    const g = s.parallel_group ?? null;
    if (g !== null && g === prevGroup) {
      stages[stages.length - 1].push(s.step_id);
    } else {
      stages.push([s.step_id]);
    }
    prevGroup = g;
  }
  return stages;
}

/** x offset that centres `count` nodes of width NODE_W around x=0. */
function rowXs(count: number): number[] {
  const pitch = NODE_W + X_GAP;
  const total = count * pitch - X_GAP;
  const start = -total / 2 + NODE_W / 2;
  return Array.from({ length: count }, (_, i) => start + i * pitch);
}

/**
 * Lay an ordered step list out as a graph: each stage is a row, parallel steps
 * share a row, and every node in stage i fans out to every node in stage i+1.
 *
 * Node *id* is a per-occurrence instance id (so the same step can appear more
 * than once in a pipeline); the underlying step is carried in node.data.stepId,
 * which the canvas resolves to a label. Stages are grouped on step *index* (not
 * step_id) precisely so repeated steps stay distinct.
 */
export function graphFromSteps(steps: StepRef[]): { nodes: GraphNode[]; edges: GraphEdge[] } {
  const stages: number[][] = [];
  let prevGroup: number | null | undefined = undefined;
  steps.forEach((s, i) => {
    const g = s.parallel_group ?? null;
    if (g !== null && g === prevGroup) stages[stages.length - 1].push(i);
    else stages.push([i]);
    prevGroup = g;
  });

  const nodeId = (i: number) => `n${i}`; // unique per step occurrence
  const nodes: GraphNode[] = [];
  const edges: GraphEdge[] = [];
  stages.forEach((stage, row) => {
    const xs = rowXs(stage.length);
    stage.forEach((stepIdx, col) => {
      nodes.push({ id: nodeId(stepIdx), position: { x: xs[col], y: row * Y_GAP }, data: { stepId: steps[stepIdx].step_id } });
    });
    if (row > 0) {
      for (const from of stages[row - 1]) {
        for (const to of stage) {
          edges.push({ id: `${nodeId(from)}->${nodeId(to)}`, source: nodeId(from), target: nodeId(to) });
        }
      }
    }
  });
  return { nodes, edges };
}

/**
 * Convert a canvas graph back into an ordered step list with parallel_group set.
 *
 * Nodes are layered by longest path from a root (no incoming edge): every node
 * sits one level below its deepest parent. Each layer becomes one stage —
 * multi-node layers get a shared parallel_group; single-node layers get null.
 * Within a layer, nodes are ordered by x position (then id) so the on-canvas
 * left→right arrangement is preserved. Throws on a cycle (not a valid pipeline).
 */
export function stepsFromGraph(nodes: GraphNode[], edges: GraphEdge[]): StepRef[] {
  const ids = nodes.map((n) => n.id);
  const idSet = new Set(ids);
  const parents = new Map<string, string[]>();
  const children = new Map<string, string[]>();
  const indeg = new Map<string, number>();
  ids.forEach((id) => {
    parents.set(id, []);
    children.set(id, []);
    indeg.set(id, 0);
  });
  for (const e of edges) {
    if (!idSet.has(e.source) || !idSet.has(e.target) || e.source === e.target) continue;
    children.get(e.source)!.push(e.target);
    parents.get(e.target)!.push(e.source);
    indeg.set(e.target, indeg.get(e.target)! + 1);
  }

  // Kahn topological order; assign each node a layer = max(parent layer)+1.
  const layer = new Map<string, number>();
  const queue = ids.filter((id) => indeg.get(id) === 0);
  const work = new Map(ids.map((id) => [id, indeg.get(id)!]));
  let processed = 0;
  while (queue.length) {
    const id = queue.shift()!;
    processed++;
    const lp = Math.max(-1, ...parents.get(id)!.map((p) => layer.get(p) ?? 0));
    layer.set(id, lp + 1);
    for (const c of children.get(id)!) {
      work.set(c, work.get(c)! - 1);
      if (work.get(c) === 0) queue.push(c);
    }
  }
  if (processed !== ids.length) {
    throw new Error('pipeline has a cycle — steps must form a directed acyclic graph');
  }

  const xOf = new Map(nodes.map((n) => [n.id, n.position.x]));
  const stepIdOf = new Map(nodes.map((n) => [n.id, n.data.stepId]));
  const byLayer = new Map<number, string[]>();
  for (const id of ids) {
    const l = layer.get(id)!;
    if (!byLayer.has(l)) byLayer.set(l, []);
    byLayer.get(l)!.push(id);
  }

  const out: StepRef[] = [];
  let group = 0;
  for (const l of [...byLayer.keys()].sort((a, b) => a - b)) {
    const stage = byLayer.get(l)!.sort((a, b) => (xOf.get(a)! - xOf.get(b)!) || (a < b ? -1 : 1));
    if (stage.length > 1) {
      const g = group++;
      for (const id of stage) out.push({ step_id: stepIdOf.get(id)!, parallel_group: g });
    } else {
      out.push({ step_id: stepIdOf.get(stage[0])!, parallel_group: null });
    }
  }
  return out;
}

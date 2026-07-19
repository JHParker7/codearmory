/**
 * State-machine pipeline document ⇄ the builder model — the TypeScript twin of the
 * workflows service's statemachine.go, kept in lock-step so the portal shows exactly
 * what the backend stores.
 *
 * The service stores a pipeline as (steps, routes, maps): a DAG of named nodes with
 * explicit edges. That is precise but noisy to read. A *state machine* (Amazon-
 * States-Language style) names each state and gives it a transition, which reads far
 * better. This module converts both ways so the config panel can present the state
 * machine — as YAML or JSON — while the visual canvas keeps editing the model.
 *
 * The document is a plain object; YAML vs JSON is only how it is serialised (see
 * serializeDoc / parseDoc). The state key IS the node name that routes and
 * ${steps.<name>.output} already use, so the two models share one identity space.
 */
import { parse as yamlParse, stringify as yamlStringify } from 'yaml';
import { effectiveStepName } from './pipelineGraph';
import type {
  StepRef, Route, MapDef, MatrixConfig, ScatterConfig, ApprovalGate,
  WorkflowInputDef, WorkflowOutputDef,
} from './pipelineGraph';

/** A transition target: one state name, or a list for a parallel fan-out. */
export type SMNext = string | string[];

/** One conditional branch out of a `choice` state: `next` when `when` holds, or
 * `default` (the else — an unconditional edge taken on completion). */
export interface SMChoice { when?: string; next?: string; default?: string }

/** A map region embedded in a state: `over` is MapDef.values_from; `states` is the
 * region body (its own sub-graph), repeated once per value. */
export interface SMMap {
  var: string;
  values?: string[];
  over?: string;
  max_concurrent?: number;
  sequential?: boolean;
  volume?: string;
  mount_path?: string;
  size_mb?: number;
  medium?: string;
  outputs?: string[];
  states: Record<string, SMState>;
}

/** One state. Exactly one kind (run | use | approval | map) and at most one
 * transition (next | choice | end). */
export interface SMState {
  run?: string;   // inline step: the action it runs
  use?: string;   // reference: a stored step_id
  approval?: ApprovalGate;
  map?: SMMap;
  with?: Record<string, unknown>;
  timeout?: number;
  matrix?: MatrixConfig;
  scatter?: ScatterConfig;
  next?: SMNext;
  choice?: SMChoice[];
  end?: boolean;
}

export interface SMDoc {
  name?: string;
  description?: string;
  inputs?: WorkflowInputDef[];
  outputs?: WorkflowOutputDef[];
  states: Record<string, SMState>;
}

export type DefName = (stepId: string) => string | undefined;

// ── model -> document ────────────────────────────────────────────────────────────

/** Build the state-machine document for the current builder model, so the panel can
 * render it. Inverse of docToModel: map members nest under their container, boundary
 * edges become the container's inbound target / outbound transition, and each node's
 * outbound routes become a `next` (one or many) or a `choice`. */
export function modelToDoc(
  name: string, description: string, steps: StepRef[], routes: Route[], maps: MapDef[],
  inputs: WorkflowInputDef[], outputs: WorkflowOutputDef[], defName: DefName,
): SMDoc {
  const nameOf = (s: StepRef) => effectiveStepName(s, defName);
  const mapOf = new Map<string, string>(); // node name -> map id
  steps.forEach((s) => { if (s.map_id) mapOf.set(nameOf(s), s.map_id); });
  const display = (n: string) => mapOf.get(n) ?? n;

  type Edge = { to: string; when?: string };
  const outer = new Map<string, Edge[]>();
  const internal = new Map<string, Edge[]>();
  routes.forEach((r) => {
    const fm = mapOf.get(r.from), tm = mapOf.get(r.to);
    if (fm && fm === tm) {
      (internal.get(r.from) ?? internal.set(r.from, []).get(r.from)!).push({ to: r.to, when: r.when });
      return;
    }
    const key = display(r.from);
    (outer.get(key) ?? outer.set(key, []).get(key)!).push({ to: display(r.to), when: r.when });
  });

  /** Turn a node's outbound edges into a transition. All-unconditional → `next`
   * (a bare name, or a list for a fork); any condition present → `choice`, with the
   * when-less edges rendered as `default`. No edges → `end`. */
  const transition = (edges: Edge[] | undefined): Pick<SMState, 'next' | 'choice' | 'end'> => {
    if (!edges || edges.length === 0) return { end: true };
    if (edges.every((e) => !e.when)) {
      const tos = edges.map((e) => e.to).sort();
      return { next: tos.length === 1 ? tos[0] : tos };
    }
    const choice: SMChoice[] = [];
    edges.filter((e) => e.when).forEach((e) => choice.push({ when: e.when, next: e.to }));
    edges.filter((e) => !e.when).forEach((e) => choice.push({ default: e.to }));
    return { choice };
  };

  const taskState = (s: StepRef): SMState => {
    const st: SMState = {};
    if (s.approval) st.approval = s.approval;
    else if (s.step_id && !s.action) st.use = s.step_id;
    else { st.run = s.action ?? ''; if (s.timeout) st.timeout = s.timeout; }
    if (s.with && Object.keys(s.with).length > 0) st.with = s.with as Record<string, unknown>;
    if (s.matrix) st.matrix = s.matrix;
    if (s.scatter) st.scatter = s.scatter;
    return st;
  };

  const states: Record<string, SMState> = {};
  // Map containers first, with their nested states.
  maps.forEach((m) => {
    states[m.id] = {
      map: {
        var: m.var, values: m.values, over: m.values_from,
        max_concurrent: m.max_concurrent, sequential: m.sequential,
        volume: m.volume, mount_path: m.mount_path, size_mb: m.size_mb, medium: m.medium, outputs: m.outputs,
        states: {},
      },
    };
  });
  steps.forEach((s) => {
    const nm = nameOf(s);
    const st = taskState(s);
    if (s.map_id) {
      Object.assign(st, transition(internal.get(nm)));
      const container = states[s.map_id];
      if (container?.map) container.map.states[nm] = st;
      return;
    }
    Object.assign(st, transition(outer.get(nm)));
    states[nm] = st;
  });
  // Containers' own transitions come from their outer edges.
  maps.forEach((m) => { if (states[m.id]) Object.assign(states[m.id], transition(outer.get(m.id))); });

  const doc: SMDoc = { states };
  if (name) doc.name = name;
  if (description) doc.description = description;
  if (inputs.length) doc.inputs = inputs;
  if (outputs.length) doc.outputs = outputs;
  return doc;
}

// ── document -> model ────────────────────────────────────────────────────────────

export interface Model {
  name: string; description: string;
  steps: StepRef[]; routes: Route[]; maps: MapDef[];
  inputs: WorkflowInputDef[]; outputs: WorkflowOutputDef[];
}

/** Expand a state-machine document into the builder model. Mirrors smToModel in the
 * Go service: map containers resolve to their member sub-states and the region
 * boundary is rewired (an edge INTO a container → edges into its entry sub-states; a
 * container's transition → edges out of its exit sub-states). Throws a user-facing
 * Error on a malformed document; deeper checks (cycles, unknown steps) are left to
 * the builder's own validation, exactly as the backend leaves them to validateGraph. */
export function docToModel(doc: SMDoc): Model {
  if (!doc || !doc.states || Object.keys(doc.states).length === 0) {
    throw new Error('state machine has no states');
  }
  const steps: StepRef[] = [];
  const routes: Route[] = [];
  const maps: MapDef[] = [];
  const seen = new Set<string>();
  const claim = (name: string) => {
    if (!name || !name.trim()) throw new Error('a state name cannot be empty');
    if (seen.has(name)) throw new Error(`duplicate state name "${name}"`);
    seen.add(name);
  };

  const taskRef = (name: string, st: SMState, mapId?: string): StepRef => {
    const ref: StepRef = { name };
    if (mapId) ref.map_id = mapId;
    if (st.with && Object.keys(st.with).length) ref.with = st.with;
    if (st.approval) ref.approval = st.approval;
    else if (st.use) ref.step_id = st.use;
    else if (st.run) { ref.action = st.run; if (st.timeout) ref.timeout = st.timeout; }
    else throw new Error(`state "${name}" must set one of run, use, approval or map`);
    if (st.matrix) ref.matrix = st.matrix;
    if (st.scatter) ref.scatter = st.scatter;
    return ref;
  };

  type Edge = { to: string; when?: string };
  const edgesOf = (name: string, st: SMState): Edge[] => {
    const hasNext = st.next != null && (Array.isArray(st.next) ? st.next.length > 0 : true);
    const hasChoice = !!st.choice && st.choice.length > 0;
    if (st.end && (hasNext || hasChoice)) throw new Error(`state "${name}" sets end together with a transition`);
    if (hasNext && hasChoice) throw new Error(`state "${name}" sets both next and choice`);
    const out: Edge[] = [];
    if (hasNext) (Array.isArray(st.next) ? st.next : [st.next as string]).forEach((t) => out.push({ to: t }));
    (st.choice ?? []).forEach((c) => {
      if (c.default) out.push({ to: c.default });
      else if (c.next) out.push({ to: c.next, when: c.when });
      else throw new Error(`state "${name}": a choice branch needs next or default`);
    });
    return out;
  };

  // Pass 1 — register nodes; process map regions, recording entry/exit sub-states.
  const regions = new Map<string, { entries: string[]; exits: string[] }>();
  const internalRoutes: Route[] = [];
  for (const [name, st] of Object.entries(doc.states)) {
    if (!st.map) continue;
    const m = st.map;
    if (!m.states || Object.keys(m.states).length === 0) throw new Error(`map "${name}" has no states`);
    claim(name);
    maps.push({
      id: name, var: m.var, values: m.values, values_from: m.over,
      max_concurrent: m.max_concurrent, sequential: m.sequential,
      volume: m.volume, mount_path: m.mount_path, size_mb: m.size_mb, medium: m.medium, outputs: m.outputs,
    });
    const hasInbound = new Set<string>();
    const exits: string[] = [];
    for (const [subName, sub] of Object.entries(m.states)) {
      if (sub.map) throw new Error(`map "${name}": nested maps are not supported`);
      claim(subName);
      steps.push(taskRef(subName, sub, name));
      const edges = edgesOf(subName, sub);
      if (edges.length === 0) exits.push(subName);
      edges.forEach((e) => {
        if (!(e.to in m.states)) throw new Error(`map "${name}": state "${subName}" routes to "${e.to}" which is not in the region`);
        internalRoutes.push({ from: subName, to: e.to, when: e.when });
        hasInbound.add(e.to);
      });
    }
    const entries = Object.keys(m.states).filter((n) => !hasInbound.has(n)).sort();
    regions.set(name, { entries, exits: exits.sort() });
  }
  for (const [name, st] of Object.entries(doc.states)) {
    if (st.map) continue;
    claim(name);
    steps.push(taskRef(name, st));
  }

  const resolveTargets = (name: string): string[] => {
    const reg = regions.get(name);
    if (reg) {
      if (reg.entries.length === 0) throw new Error(`map "${name}" has no entry state (its states form a cycle)`);
      return reg.entries;
    }
    if (!seen.has(name)) throw new Error(`transition to unknown state "${name}"`);
    return [name];
  };

  // Pass 2 — outer transitions.
  const seenRoute = new Set<string>();
  const key = (r: Route) => `${r.from} ${r.to} ${r.when ?? ''}`;
  const addRoute = (from: string, to: string, when?: string) => {
    const r: Route = when ? { from, to, when } : { from, to };
    if (!seenRoute.has(key(r))) { seenRoute.add(key(r)); routes.push(r); }
  };
  internalRoutes.forEach((r) => addRoute(r.from, r.to, r.when));
  for (const [name, st] of Object.entries(doc.states)) {
    const edges = edgesOf(name, st);
    const reg = regions.get(name);
    const sources = reg ? reg.exits : [name];
    for (const e of edges) {
      for (const tgt of resolveTargets(e.to)) {
        for (const src of sources) addRoute(src, tgt, e.when);
      }
    }
  }

  return {
    name: doc.name ?? '', description: doc.description ?? '',
    steps, routes, maps, inputs: doc.inputs ?? [], outputs: doc.outputs ?? [],
  };
}

// ── serialisation ────────────────────────────────────────────────────────────────

export type DocFormat = 'yaml' | 'json';

/** Render a document as YAML or JSON — the two panel views. Empty keys are dropped
 * so the text stays clean (the structs already omit undefined). */
export function serializeDoc(doc: SMDoc, format: DocFormat): string {
  const clean = JSON.parse(JSON.stringify(doc)); // strip undefined
  return format === 'json' ? JSON.stringify(clean, null, 2) : yamlStringify(clean);
}

/** Parse panel text (YAML is a JSON superset, so `yaml.parse` handles both) into a
 * document. Throws a user-facing Error on malformed input or a non-object. */
export function parseDoc(text: string): SMDoc {
  let obj: unknown;
  try { obj = yamlParse(text); }
  catch (e: unknown) { throw new Error((e as Error).message); }
  if (!obj || typeof obj !== 'object' || Array.isArray(obj)) throw new Error('config must be a mapping with a "states" key');
  return obj as SMDoc;
}

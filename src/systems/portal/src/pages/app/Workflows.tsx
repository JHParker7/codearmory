/** Workflows page — the CI/CD control surface, a tabbed view over pipelines and the action catalog. pipelines tab creates/triggers/cancels/deletes workflows, lists their runs, and opens the visual builder (which now also hosts the reusable-step library); actions tab browses the read-only action catalog. all data goes through the typed BFF client (listWorkflows/createWorkflow/etc), never conductor directly. */
import { useState, useEffect, useCallback, useMemo, useRef } from 'react';
import type { ReactNode } from 'react';
import { useNavigate } from 'react-router-dom';
import { T } from '../../theme';
import { Pill } from '../../components/Pill';
import { useConfirm } from '../../components/ConfirmDialog';
import { useAppSelector } from '../../store/hooks';
import {
  listWorkflows, getWorkflow, deleteWorkflow, listWorkflowRuns, triggerWorkflow, cancelRun,
  createWorkflow, updateWorkflow, listSteps, listActions, listGitRepos,
} from '../../api/bff';
import type { Workflow, WorkflowRun, Step, WorkflowAction, GitRepo } from '../../api/bff';
import { ResizeHandle, useResizableWidth } from '../../components/ResizeHandle';
import { PipelineBlocks } from './PipelineBlocks';
import { StepDefForm } from './StepDefForm';
import { StepsTab } from './StepLibrary';
import type { StepRef } from './pipelineGraph';
import { stepsToPayload, configToJson, parseConfig, duplicateStepNames } from './pipelineGraph';
import { timeAgo, statusTone, isRunActive, fmtDuration } from '../../utils';

type MainTab = 'pipelines' | 'steps' | 'actions';

// Run status helpers (statusTone / isRunActive / fmtDuration) live in ../../utils
// so the run page (RunView) shares them. Clicking a run navigates to that page.

// ── Pipeline builder overlay ──────────────────────────────────────────────────

/** Full-surface pipeline builder: name/description plus the Scratch-style block
 * builder, beside a live, editable JSON config of the pipeline. Used for both
 * create (initial=null) and edit. The visual builder and the JSON panel are
 * two-way synced; save sends the same config to the API. */
function PipelineBuilderOverlay({
  token, initial, catalog, palette, onClose, onSaved, onStepsChanged,
}: {
  token: string;
  initial: Workflow | null;
  catalog: Record<string, Step>;
  palette: Step[];
  onClose: () => void;
  onSaved: (wf: Workflow) => void;
  // Reload the reusable-step catalog after an inline step create/edit, so the
  // palette/graph pick the change up without leaving the builder.
  onStepsChanged: () => void;
}) {
  const initialStepRefs = useMemo<StepRef[]>(
    () => initial ? initial.steps.map(s => {
      // The GET returns the effective (merged) name/with; recover the raw overrides
      // by keeping only what differs from the step definition, so unchanged steps
      // still track definition edits and the JSON/payload stay minimal.
      const def = (s.step_id ? (catalog[s.step_id]?.with ?? {}) : {}) as Record<string, unknown>;
      const merged = (s.with ?? {}) as Record<string, unknown>;
      const wo: Record<string, unknown> = {};
      for (const [k, v] of Object.entries(merged)) {
        if (JSON.stringify(v) !== JSON.stringify(def[k])) wo[k] = v;
      }
      return {
        step_id: s.step_id,
        name: (!s.approval && s.name && s.step_id && s.name !== catalog[s.step_id]?.name) ? s.name : undefined,
        // An approval gate has no step definition to override, so it never carries a
        // per-occurrence `with`; without this the synthesised gate's message/approvers
        // (returned in `with` by the GET) would leak in as a spurious override.
        with: (!s.approval && Object.keys(wo).length) ? wo : undefined,
        parallel_group: s.parallel_group ?? null,
        matrix: s.matrix ?? null,
        approval: s.approval ?? null,
      };
    }) : [],
    [initial, catalog],
  );

  const [name, setName] = useState(initial?.name ?? '');
  const [desc, setDesc] = useState(initial?.description ?? '');
  // `steps` is the live source of truth (mirrored from the visual builder via
  // onChange and from applying JSON edits); `builderSeed` is what re-seeds the
  // block builder — it changes only on a JSON apply, never on the builder's own
  // edits, so dragging blocks doesn't reset them.
  const [steps, setSteps] = useState<StepRef[]>(initialStepRefs);
  const [builderSeed, setBuilderSeed] = useState<StepRef[]>(initialStepRefs);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);
  // Tag a brand-new pipeline with the active project (workspace) so it isn't
  // immediately hidden by that filter. Only on create — an edit must not re-tag.
  const project = useAppSelector(s => s.project.current);

  // Live, editable JSON mirror. The textarea drives `jsonDraft`; while the user is
  // typing in it (jsonFocused) builder-side updates don't overwrite their text.
  const [jsonDraft, setJsonDraft] = useState(() => configToJson(initial?.name ?? '', initial?.description ?? '', initialStepRefs));
  const [jsonError, setJsonError] = useState<string | null>(null);
  const jsonFocused = useRef(false);

  // Right-panel tabs: the step editor (create a step inline from a chosen action, or
  // edit the step behind the selected block) and the live JSON config. Selecting a
  // block — or picking an action to create — switches to the step editor.
  const [rightTab, setRightTab] = useState<'step' | 'json'>('json');
  // Draggable split between the visual builder and the right (inspector/JSON) pane.
  // Width is in px, persisted so the layout survives reopening the builder; the
  // divider clamps it so neither pane collapses below a usable minimum.
  const splitRow = useRef<HTMLDivElement>(null);
  const [rightW, setRightW] = useState(() => {
    const v = Number(localStorage.getItem('ci.builder.rightW'));
    return v >= 280 ? v : 480;
  });
  useEffect(() => { localStorage.setItem('ci.builder.rightW', String(rightW)); }, [rightW]);
  const onSplitResize = useCallback((dx: number) => setRightW(w => {
    const total = splitRow.current?.offsetWidth ?? window.innerWidth;
    return Math.max(280, Math.min(w - dx, total - 360));
  }), []);
  // The step block currently selected in the builder (null = none / an approval
  // gate, whose config lives on the card). Its step is edited in the right panel.
  const [inspectId, setInspectId] = useState<string | null>(null);
  // An action the user picked from the palette to create a step from: the right
  // panel shows an inline create form for it until saved or cancelled.
  const [creatingAction, setCreatingAction] = useState<WorkflowAction | null>(null);
  // A freshly-created step handed to the block builder to add as a new block.
  const [pendingAdd, setPendingAdd] = useState<Step | null>(null);
  const [actions, setActions] = useState<WorkflowAction[]>([]);
  useEffect(() => { listActions(token).then(setActions).catch(() => {}); }, [token]);
  // Git repo catalog for the builder's per-step repo picker (forge blocks).
  const [repos, setRepos] = useState<GitRepo[]>([]);
  useEffect(() => { listGitRepos(token).then(setRepos).catch(() => {}); }, [token]);
  // Selecting a block shows that step in the editor and cancels any in-progress
  // create (picking a block wins over a half-started new step).
  const onInspect = useCallback((stepId: string | null) => {
    setInspectId(stepId);
    setCreatingAction(null);
    setRightTab('step');
  }, []);
  // Picking an action from the palette opens the inline create form for it.
  const onPickAction = useCallback((a: WorkflowAction) => {
    setCreatingAction(a);
    setRightTab('step');
  }, []);
  // A sensible, collision-free default name for a step created from an action:
  // the action slug (e.g. forge/run → forge-run), suffixed if already taken.
  const defaultStepName = useCallback((action: string) => {
    const base = action.replace(/[^A-Za-z0-9._-]+/g, '-').replace(/^-+|-+$/g, '') || 'step';
    const taken = new Set(palette.map(s => s.name));
    if (!taken.has(base)) return base;
    for (let n = 2; ; n++) if (!taken.has(`${base}-${n}`)) return `${base}-${n}`;
  }, [palette]);

  // Drop a per-occurrence name that just equals the step definition's name, so only
  // real overrides are saved/shown (and definition renames keep propagating).
  const cleanedSteps = useMemo(
    () => steps.map(s => (s.name && s.step_id && s.name === catalog[s.step_id]?.name) ? { ...s, name: undefined } : s),
    [steps, catalog],
  );
  const canonicalJson = useMemo(() => configToJson(name, desc, cleanedSteps), [name, desc, cleanedSteps]);

  // Reflect builder/name/description changes into the JSON panel, unless the user
  // is actively editing the JSON (their text is authoritative then).
  useEffect(() => {
    if (jsonFocused.current) return;
    setJsonDraft(canonicalJson);
    setJsonError(null);
  }, [canonicalJson]);

  // Apply a JSON edit back into the builder. On valid parse, name/description sync
  // immediately and the builder is re-seeded only when the steps actually changed
  // (so editing the name in JSON doesn't reset block state). Invalid JSON surfaces
  // an inline error and leaves the builder untouched.
  const applyJson = useCallback((raw: string) => {
    setJsonDraft(raw);
    let parsed: { name: string; description: string; steps: StepRef[] };
    try { parsed = parseConfig(raw); }
    catch (e: unknown) { setJsonError((e as Error).message); return; }
    setJsonError(null);
    setName(parsed.name);
    setDesc(parsed.description);
    if (configToJson('', '', parsed.steps) !== configToJson('', '', builderSeed)) {
      setBuilderSeed(parsed.steps);
      setSteps(parsed.steps);
    }
  }, [builderSeed]);

  // Two blocks sharing one effective name collide in the run output map, so
  // ${steps.<name>.output} wiring silently resolves against the wrong step. The
  // backend rejects it; block save here too, pointing at the offending name(s).
  const dupNames = useMemo(
    () => duplicateStepNames(cleanedSteps, (id) => catalog[id]?.name),
    [cleanedSteps, catalog],
  );
  const canSave = !!name.trim() && !saving && !jsonError && dupNames.length === 0;

  const handleSave = async () => {
    if (!name.trim() || dupNames.length > 0) return;
    setSaving(true); setSaveError(null);
    try {
      const payload = {
        name: name.trim(),
        description: desc.trim() || undefined,
        steps: stepsToPayload(cleanedSteps),
      };
      const wf = initial
        ? await updateWorkflow(token, initial.workflow_id, payload)
        : await createWorkflow(token, { ...payload, project: project ?? undefined });
      onSaved(wf);
    } catch (e: unknown) { setSaveError((e as Error).message); }
    finally { setSaving(false); }
  };

  return (
    <div style={{ position: 'absolute', inset: 0, zIndex: 30, background: T.bg, display: 'flex', flexDirection: 'column' }}>
      <div style={{ padding: '12px 20px', borderBottom: `1px solid ${T.border}`, background: T.bgAlt, display: 'flex', alignItems: 'center', gap: 10 }}>
        <span style={{ fontFamily: T.mono, fontSize: 11, color: T.faint, whiteSpace: 'nowrap' }}>
          <span style={{ color: T.green }}>$</span> armory ci {initial ? 'edit' : 'new'}
        </span>
        <input value={name} onChange={e => setName(e.target.value)} placeholder="pipeline name" autoFocus
          style={{ background: T.cardHi, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 12, padding: '5px 8px', outline: 'none', width: 200 }} />
        <input value={desc} onChange={e => setDesc(e.target.value)} placeholder="description (optional)"
          style={{ flex: 1, background: T.cardHi, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 12, padding: '5px 8px', outline: 'none' }} />
        <button onClick={onClose} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 10px', cursor: 'pointer' }}>✕ close</button>
      </div>
      <div ref={splitRow} style={{ flex: 1, minHeight: 0, display: 'flex' }}>
        <div style={{ flex: 1, minWidth: 0, padding: '14px 3px 14px 14px' }}>
          <PipelineBlocks editable initialSteps={builderSeed} catalog={catalog} palette={palette} actions={actions} repos={repos}
            onChange={setSteps} onInspect={onInspect} onPickAction={onPickAction}
            pendingAdd={pendingAdd} onPendingConsumed={() => setPendingAdd(null)} />
        </div>
        {/* Drag to rebalance the builder vs. step-editor/JSON panes. */}
        <ResizeHandle onResize={onSplitResize} />
        {/* Right panel: the step editor (create a step inline from a chosen action or
            edit the selected block's step) and the live, editable JSON config, as tabs. */}
        <div style={{ width: rightW, minWidth: 280, flexShrink: 0, padding: '14px 14px 14px 3px', display: 'flex', flexDirection: 'column', minHeight: 0 }}>
          <div style={{ display: 'flex', alignItems: 'stretch', border: `1px solid ${T.border}`, borderBottom: 'none', background: T.bgAlt }}>
            {(['step', 'json'] as const).map(tab => (
              <button key={tab} onClick={() => setRightTab(tab)}
                style={{ background: rightTab === tab ? T.bg : 'transparent', border: 'none', borderRight: `1px solid ${T.border}`, color: rightTab === tab ? T.textHi : T.faint, fontFamily: T.mono, fontSize: 10, letterSpacing: 1, textTransform: 'uppercase', padding: '8px 14px', cursor: 'pointer' }}>
                {tab === 'step' ? 'step' : 'pipeline.json'}
              </button>
            ))}
            <div style={{ flex: 1 }} />
            {rightTab === 'json' && <span style={{ alignSelf: 'center', padding: '0 12px', fontFamily: T.mono, fontSize: 9, color: jsonError ? T.red : T.green }}>{jsonError ? '✗ invalid' : '✓ in sync'}</span>}
          </div>
          <div style={{ flex: 1, minHeight: 0, border: `1px solid ${T.border}`, background: T.bg, display: 'flex', flexDirection: 'column' }}>
            {rightTab === 'step' ? (
              creatingAction ? (
                // Create a new step from the picked action; on save, drop it into the
                // pipeline as a new block (pendingAdd) and refresh the catalog.
                <div style={{ flex: 1, minHeight: 0, overflow: 'auto', padding: 14 }}>
                  <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, textTransform: 'uppercase', marginBottom: 10 }}>new step · {creatingAction.name}</div>
                  <StepDefForm token={token} initial={null} lockAction={creatingAction.name} autoFocus
                    defaultName={defaultStepName(creatingAction.name)} createLabel="[ add step ]"
                    onSaved={(saved) => { setPendingAdd(saved); setCreatingAction(null); onStepsChanged(); }}
                    onCancel={() => setCreatingAction(null)} />
                </div>
              ) : inspectId && catalog[inspectId] ? (
                // Edit the step behind the selected block, inline.
                <div style={{ flex: 1, minHeight: 0, overflow: 'auto', padding: 14 }}>
                  <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, textTransform: 'uppercase', marginBottom: 10 }}>edit step</div>
                  <StepDefForm key={inspectId} token={token} initial={catalog[inspectId]} lockAction={catalog[inspectId].action}
                    onSaved={() => onStepsChanged()} />
                </div>
              ) : (
                <div style={{ flex: 1, overflow: 'auto', padding: 14, fontFamily: T.mono, fontSize: 12, color: T.faint }}>
                  → pick an action on the left to create a step, or select a step block to edit it
                </div>
              )
            ) : (
              <>
                <textarea value={jsonDraft} spellCheck={false}
                  onChange={e => applyJson(e.target.value)}
                  onFocus={() => { jsonFocused.current = true; }}
                  onBlur={() => { jsonFocused.current = false; if (!jsonError) setJsonDraft(canonicalJson); }}
                  style={{ flex: 1, minHeight: 0, resize: 'none', background: T.bg, border: 'none', color: T.text, fontFamily: T.mono, fontSize: 12, lineHeight: 1.5, padding: 12, outline: 'none', whiteSpace: 'pre', overflow: 'auto', tabSize: 2 }} />
                <div style={{ minHeight: 16, padding: '4px 10px', borderTop: `1px solid ${T.border}`, fontFamily: T.mono, fontSize: 10, color: jsonError ? T.red : T.faint, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                  {jsonError ? `✗ ${jsonError}` : 'edit here or drag blocks — both stay in sync'}
                </div>
              </>
            )}
          </div>
        </div>
      </div>
      <div style={{ padding: '10px 20px', borderTop: `1px solid ${T.border}`, background: T.bgAlt, display: 'flex', alignItems: 'center', gap: 12 }}>
        <span style={{ fontFamily: T.mono, fontSize: 10, color: T.faint }}>
          palette: ∥ parallel · ⊞ matrix · ⏸ approval gate · click an action to create a step · click a block to edit it · drag to reorder
        </span>
        <div style={{ flex: 1 }} />
        {dupNames.length > 0 && (
          <span style={{ fontFamily: T.mono, fontSize: 10, color: T.red }}>
            duplicate step name{dupNames.length > 1 ? 's' : ''}: {dupNames.join(', ')} — each block needs a unique name
          </span>
        )}
        {saveError && <span style={{ fontFamily: T.mono, fontSize: 10, color: T.red }}>{saveError}</span>}
        <button onClick={handleSave} disabled={!canSave}
          style={{ background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 11, fontWeight: 600, padding: '6px 16px', cursor: canSave ? 'pointer' : 'not-allowed', opacity: canSave ? 1 : 0.5 }}>
          {saving ? '[ · · · ]' : initial ? '[ save ]' : '[ create ]'}
        </button>
      </div>
    </div>
  );
}

// ── Pipeline runs detail ──────────────────────────────────────────────────────

/** Pipelines tab — left rail lists workflows; "+" opens the visual builder. Right
 * panel shows the selected workflow's stage graph, stats, and recent runs, with
 * trigger/edit/delete controls. */
function PipelinesTab() {
  const token = useAppSelector(s => s.auth.token)!;
  const navigate = useNavigate();
  const [workflows, setWorkflows] = useState<Workflow[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const [runs, setRuns] = useState<WorkflowRun[]>([]);
  const [runsLoading, setRunsLoading] = useState(false);
  const [triggering, setTriggering] = useState(false);
  // builder === null: closed; { wf: null }: create; { wf }: edit that workflow.
  const [builder, setBuilder] = useState<{ wf: Workflow | null } | null>(null);
  const [catalog, setCatalog] = useState<Step[]>([]);
  const [railW, railHandle] = useResizableWidth('rail.workflows.pipelines', 260, { min: 200, max: 480 });
  // Current-project view filter — refetch whenever it changes so the list tracks
  // the sidebar switcher.
  const project = useAppSelector(s => s.project.current);

  const fetchWorkflows = useCallback(async () => {
    setLoading(true); setError(null);
    try { setWorkflows(await listWorkflows(token, project ?? undefined)); }
    catch (e: unknown) { setError((e as Error).message); }
    finally { setLoading(false); }
  }, [token, project]);

  useEffect(() => { fetchWorkflows(); }, [fetchWorkflows]);

  // Step catalog backs the builder palette and resolves step_id -> name/action
  // labels on the graph. Best effort; the canvas falls back to truncated ids.
  // reloadSteps is also handed to the builder's embedded step library so the
  // palette refreshes when a definition is created/edited/deleted in place.
  const reloadSteps = useCallback(() => { listSteps(token).then(setCatalog).catch(() => {}); }, [token]);
  useEffect(() => { reloadSteps(); }, [reloadSteps]);
  const catalogMap = useMemo(() => Object.fromEntries(catalog.map(s => [s.step_id, s])) as Record<string, Step>, [catalog]);

  // Live-refresh the runs list while any run is in flight, so statuses/durations
  // update without a manual refresh. The full per-run detail lives on the run page.
  const liveRun = runs.some(r => isRunActive(r.status));
  useEffect(() => {
    if (!selected || !liveRun) return;
    const id = setInterval(async () => {
      try { setRuns(await listWorkflowRuns(token, selected)); } catch { /* keep last good */ }
    }, 2500);
    return () => clearInterval(id);
  }, [selected, liveRun, token]);

  /** selects a workflow and loads its runs. */
  const selectWorkflow = useCallback(async (id: string) => {
    setSelected(id); setRunsLoading(true);
    // List responses omit step details (steps: []), so the detail graph and the
    // edit builder would be blank off the list entry. Fetch the full workflow and
    // merge it into the list so both see the real steps.
    getWorkflow(token, id)
      .then(full => setWorkflows(prev => prev.map(w => w.workflow_id === id ? full : w)))
      .catch(() => { /* keep the list entry on a transient error */ });
    try { setRuns(await listWorkflowRuns(token, id)); }
    catch { setRuns([]); }
    finally { setRunsLoading(false); }
  }, [token]);

  const handleTrigger = async () => {
    if (!selected) return;
    setTriggering(true);
    try {
      const run = await triggerWorkflow(token, selected);
      setRuns(prev => [run, ...prev]);
      navigate(`/app/workflows/runs/${run.run_id}`); // straight to the new run's live page
    } catch (e: unknown) { setError((e as Error).message); }
    finally { setTriggering(false); }
  };

  const handleCancel = async (runId: string) => {
    try {
      await cancelRun(token, runId);
      setRuns(prev => prev.map(r => r.run_id === runId ? { ...r, status: 'cancelled' } : r));
    } catch (e: unknown) { setError((e as Error).message); }
  };

  const [confirm, confirmEl] = useConfirm();

  const handleDeleteWorkflow = async (id: string) => {
    const name = workflows.find(w => w.workflow_id === id)?.name;
    if (!(await confirm({ message: `Delete pipeline ${name ?? id}? Its steps and run history will be removed.` }))) return;
    try {
      await deleteWorkflow(token, id);
      setWorkflows(prev => prev.filter(w => w.workflow_id !== id));
      if (selected === id) { setSelected(null); setRuns([]); }
    } catch (e: unknown) { setError((e as Error).message); }
  };

  const selectedWorkflow = workflows.find(w => w.workflow_id === selected);
  // Stable per selected workflow so the read-only graph isn't re-seeded on every
  // render (runs polling, trigger, etc. re-render this component frequently).
  const detailSteps = useMemo<StepRef[]>(
    () => selectedWorkflow ? selectedWorkflow.steps.map(s => ({ step_id: s.step_id, parallel_group: s.parallel_group ?? null, matrix: s.matrix ?? null, approval: s.approval ?? null })) : [],
    [selectedWorkflow],
  );

  /** Reflects a created/updated workflow into the list and selects it. */
  const handleSaved = (wf: Workflow) => {
    setWorkflows(prev => prev.some(w => w.workflow_id === wf.workflow_id)
      ? prev.map(w => w.workflow_id === wf.workflow_id ? wf : w)
      : [wf, ...prev]);
    setBuilder(null);
    selectWorkflow(wf.workflow_id);
  };

  return (
    <div style={{ display: 'flex', flex: 1, overflow: 'hidden', position: 'relative' }}>
      {confirmEl}
      {builder && (
        <PipelineBuilderOverlay
          token={token}
          initial={builder.wf}
          catalog={catalogMap}
          palette={catalog}
          onClose={() => setBuilder(null)}
          onSaved={handleSaved}
          onStepsChanged={reloadSteps}
        />
      )}
      {/* Left panel */}
      <div style={{ width: railW, flexShrink: 0, borderRight: `1px solid ${T.border}`, display: 'flex', flexDirection: 'column', background: T.bgAlt }}>
        <div style={{ padding: '14px 14px 10px', borderBottom: `1px solid ${T.border}` }}>
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 4 }}>
            <span style={{ fontFamily: T.mono, fontSize: 12, fontWeight: 700, color: T.textHi }}>pipelines</span>
            <div style={{ display: 'flex', gap: 6 }}>
              <button onClick={() => setBuilder({ wf: null })} title="new pipeline"
                style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 7px', cursor: 'pointer' }}>+</button>
              <button onClick={fetchWorkflows} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 7px', cursor: 'pointer' }}>↻</button>
            </div>
          </div>
        </div>

        <div style={{ flex: 1, overflow: 'auto' }}>
          {loading ? (
            <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint, animation: 'pulse 1s ease-in-out infinite' }}>→ loading · · ·</div>
          ) : error ? (
            <div style={{ padding: '14px', fontFamily: T.mono, fontSize: 11, color: T.red }}>{error}</div>
          ) : workflows.length === 0 ? (
            <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint }}>→ no workflows</div>
          ) : workflows.map(wf => {
            const isActive = selected === wf.workflow_id;
            return (
              <button key={wf.workflow_id} onClick={() => selectWorkflow(wf.workflow_id)}
                style={{ width: '100%', textAlign: 'left', padding: '10px 14px', background: isActive ? T.greenSoft : 'transparent', border: 0, borderLeft: `2px solid ${isActive ? T.green : 'transparent'}`, fontFamily: T.mono, cursor: 'pointer', color: T.text, display: 'block', transition: 'background .12s' }}>
                <div style={{ fontSize: 13, fontWeight: 600, color: isActive ? T.textHi : T.text }}>{wf.name}</div>
                <div style={{ fontSize: 11, color: T.faint, marginTop: 3 }}>
                  {wf.steps.length} step{wf.steps.length !== 1 ? 's' : ''} ·{' '}
                  {wf.active ? <span style={{ color: T.green }}>active</span> : <span style={{ color: T.dim }}>inactive</span>}
                </div>
                {/* Show the project tag only when unfiltered — under a filter every row shares it. */}
                {!project && wf.project && <div style={{ fontSize: 10, color: T.green, marginTop: 3 }}>◆ {wf.project}</div>}
              </button>
            );
          })}
        </div>
      </div>

      {railHandle}
      {/* Right panel */}
      <div style={{ flex: 1, display: 'flex', flexDirection: 'column', overflow: 'hidden' }}>
        <div style={{ padding: '12px 20px', borderBottom: `1px solid ${T.border}`, background: T.bgAlt, display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
          <div style={{ fontFamily: T.mono, fontSize: 11, color: T.faint }}>
            <span style={{ color: T.green }}>$</span> armory ci{selectedWorkflow ? ` · ${selectedWorkflow.name}` : ''}
          </div>
          {selectedWorkflow && (
            <div style={{ display: 'flex', gap: 8 }}>
              <button onClick={handleTrigger} disabled={triggering}
                style={{ background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: triggering ? 'not-allowed' : 'pointer', opacity: triggering ? 0.7 : 1 }}>
                {triggering ? '[ · · · ]' : '[ ▶ trigger ]'}
              </button>
              <button onClick={async () => {
                // Open the builder with the workflow's steps even if the select-time
                // fetch hasn't landed yet (list entries carry steps: []).
                const wf = selectedWorkflow.steps?.length
                  ? selectedWorkflow
                  : await getWorkflow(token, selectedWorkflow.workflow_id).catch(() => selectedWorkflow);
                setBuilder({ wf });
              }}
                style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}>
                [ edit ]
              </button>
              <button onClick={() => handleDeleteWorkflow(selectedWorkflow.workflow_id)}
                style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}
                onMouseEnter={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.red; (e.currentTarget as HTMLButtonElement).style.color = T.red; }}
                onMouseLeave={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.border; (e.currentTarget as HTMLButtonElement).style.color = T.dim; }}>
                [ delete ]
              </button>
            </div>
          )}
        </div>

        <div style={{ flex: 1, overflow: 'auto' }}>
          {!selectedWorkflow ? (
            <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', height: '100%' }}>
              <div style={{ fontFamily: T.mono, fontSize: 12, color: T.faint }}>→ select a workflow</div>
            </div>
          ) : (
            <div style={{ padding: '20px 24px' }}>
              <div style={{ display: 'grid', gridTemplateColumns: 'repeat(2, 1fr)', gap: 12, marginBottom: 20 }}>
                {([['steps', selectedWorkflow.steps.length], ['updated', timeAgo(selectedWorkflow.updated_at) + ' ago']] as [string, string | number][]).map(([k, v]) => (
                  <div key={k} style={{ background: T.card, border: `1px solid ${T.border}`, padding: '10px 14px' }}>
                    <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 4, textTransform: 'uppercase' }}>{k}</div>
                    <div style={{ fontFamily: T.mono, fontSize: 15, color: T.textHi, fontWeight: 700 }}>{v}</div>
                  </div>
                ))}
              </div>
              {selectedWorkflow.description && (
                <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '10px 14px', fontFamily: T.mono, fontSize: 12, color: T.dim, marginBottom: 20 }}>
                  {selectedWorkflow.description}
                </div>
              )}
              {selectedWorkflow.steps.length > 0 && (
                <>
                  <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 8 }}>PIPELINE</div>
                  <div style={{ height: 300, marginBottom: 20 }}>
                    <PipelineBlocks initialSteps={detailSteps} catalog={catalogMap} height={300} />
                  </div>
                </>
              )}
              <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 8 }}>RECENT RUNS</div>
              {runsLoading ? (
                <div style={{ fontFamily: T.mono, fontSize: 11, color: T.faint, animation: 'pulse 1s ease-in-out infinite' }}>→ loading · · ·</div>
              ) : runs.length === 0 ? (
                <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '14px', fontFamily: T.mono, fontSize: 12, color: T.faint, textAlign: 'center' }}>
                  → no runs yet — click [ ▶ trigger ] to start
                </div>
              ) : (
                <div style={{ background: T.card, border: `1px solid ${T.border}`, overflow: 'hidden' }}>
                  {runs.slice(0, 20).map((run, i) => {
                    const isRunning = isRunActive(run.status);
                    const dur = run.started_at ? fmtDuration(run.started_at, run.ended_at) : '';
                    return (
                      <div key={run.run_id} onClick={() => navigate(`/app/workflows/runs/${run.run_id}`)} title="open run (live pipeline + logs)"
                        style={{ display: 'flex', alignItems: 'center', padding: '9px 12px', gap: 12, cursor: 'pointer', borderBottom: i < Math.min(runs.length, 20) - 1 ? `1px solid ${T.border}` : 'none' }}
                        onMouseEnter={e => { (e.currentTarget as HTMLDivElement).style.background = T.cardHi; }}
                        onMouseLeave={e => { (e.currentTarget as HTMLDivElement).style.background = 'transparent'; }}>
                        <Pill tone={statusTone(run.status)}>{run.status}</Pill>
                        <span style={{ display: 'flex', flexDirection: 'column', flex: 1, minWidth: 0 }}>
                          <span style={{ fontFamily: T.mono, fontSize: 11.5, color: T.text, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{selectedWorkflow.name}</span>
                          <span style={{ fontFamily: T.mono, fontSize: 10, color: T.faint }}>{run.run_id.slice(0, 8)}…</span>
                        </span>
                        {dur && <span style={{ fontFamily: T.mono, fontSize: 10.5, color: isRunning ? T.amber : T.faint }}>{isRunning ? '⟳ ' : ''}{dur}</span>}
                        <span style={{ fontFamily: T.mono, fontSize: 11, color: T.faint }}>{timeAgo(run.created_at)} ago</span>
                        {isRunning && (
                          <button onClick={e => { e.stopPropagation(); handleCancel(run.run_id); }}
                            style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 7px', cursor: 'pointer' }}>
                            [ cancel ]
                          </button>
                        )}
                        <span style={{ fontFamily: T.mono, fontSize: 11, color: T.faint }}>→</span>
                      </div>
                    );
                  })}
                </div>
              )}
            </div>
          )}
        </div>
      </div>
    </div>
  );
}

// ── Actions catalog tab ───────────────────────────────────────────────────────

/** one label/value row inside a config card; value falls back to a dim placeholder when empty. */
function ConfigRow({ label, value, last }: { label: string; value: ReactNode; last?: boolean }) {
  return (
    <div style={{ display: 'flex', padding: '9px 14px', borderBottom: last ? 'none' : `1px solid ${T.border}`, fontFamily: T.mono, fontSize: 12, gap: 12 }}>
      <span style={{ color: T.faint, width: 130, flexShrink: 0, textTransform: 'uppercase', letterSpacing: 0.5, fontSize: 10, paddingTop: 1 }}>{label}</span>
      <span style={{ color: T.text, wordBreak: 'break-word' }}>{value}</span>
    </div>
  );
}

/** small uppercase section heading used above each config block. */
function SectionLabel({ children }: { children: ReactNode }) {
  return <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 8, marginTop: 20 }}>{children}</div>;
}

/** Actions tab — read-only browser of the action catalog; left rail lists action names + summaries, right panel shows the selected action's summary, description, and full config (route, service, required permission, body transforms, async polling). */
function ActionsTab() {
  const token = useAppSelector(s => s.auth.token)!;
  const [actions, setActions] = useState<WorkflowAction[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const [railW, railHandle] = useResizableWidth('rail.workflows.actions', 260, { min: 200, max: 480 });

  useEffect(() => {
    listActions(token).then(setActions).catch(e => setError((e as Error).message)).finally(() => setLoading(false));
  }, [token]);

  const a = actions.find(x => x.name === selected);
  const perm = a?.required_permission;
  const transforms = a?.body_transforms ?? [];
  const asyncCfg = a?.async;

  return (
    <div style={{ display: 'flex', flex: 1, overflow: 'hidden' }}>
      <div style={{ width: railW, flexShrink: 0, borderRight: `1px solid ${T.border}`, background: T.bgAlt, overflow: 'auto' }}>
        <div style={{ padding: '14px 14px 10px', borderBottom: `1px solid ${T.border}` }}>
          <span style={{ fontFamily: T.mono, fontSize: 12, fontWeight: 700, color: T.textHi }}>action catalog</span>
        </div>
        {loading ? <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint, animation: 'pulse 1s ease-in-out infinite' }}>→ loading · · ·</div>
          : error ? <div style={{ padding: '14px', fontFamily: T.mono, fontSize: 11, color: T.red }}>{error}</div>
          : actions.length === 0 ? <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint }}>→ no actions</div>
          : actions.map(act => {
            const isActive = selected === act.name;
            return (
              <button key={act.name} onClick={() => setSelected(act.name)}
                style={{ width: '100%', textAlign: 'left', padding: '10px 14px', background: isActive ? T.greenSoft : 'transparent', border: 0, borderLeft: `2px solid ${isActive ? T.green : 'transparent'}`, fontFamily: T.mono, cursor: 'pointer', color: T.text, display: 'block', transition: 'background .12s' }}>
                <div style={{ fontSize: 12, fontWeight: 600, color: isActive ? T.textHi : T.text }}>{act.name}</div>
                {act.summary && <div style={{ fontSize: 11, color: T.faint, marginTop: 2 }}>{act.summary}</div>}
              </button>
            );
          })}
      </div>
      {railHandle}
      <div style={{ flex: 1, overflow: 'auto' }}>
        {!a ? (
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', height: '100%' }}>
            <div style={{ fontFamily: T.mono, fontSize: 12, color: T.faint }}>→ select an action</div>
          </div>
        ) : (
          <div style={{ padding: '20px 24px' }}>
            <div style={{ fontFamily: T.mono, fontSize: 18, fontWeight: 700, color: T.blue, marginBottom: a.summary ? 4 : 0 }}>{a.name}</div>
            {a.summary && <div style={{ fontFamily: T.mono, fontSize: 13, color: T.dim }}>{a.summary}</div>}
            {a.description && (
              <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '12px 14px', fontFamily: T.mono, fontSize: 12.5, color: T.text, lineHeight: 1.5, marginTop: 16, whiteSpace: 'pre-wrap' }}>
                {a.description}
              </div>
            )}

            <SectionLabel>CONFIG</SectionLabel>
            <div style={{ background: T.card, border: `1px solid ${T.border}`, overflow: 'hidden' }}>
              {(a.method || a.path) && (
                <ConfigRow label="route" value={
                  <span><span style={{ color: T.green, fontWeight: 600 }}>{a.method}</span>{a.method && a.path ? ' ' : ''}<span style={{ color: T.textHi }}>{a.path}</span></span>
                } />
              )}
              {a.service_name && <ConfigRow label="service" value={a.service_name} />}
              <ConfigRow label="required perm" last value={
                perm ? <span>{perm.service} · <span style={{ color: T.textHi }}>{perm.action}</span> · {perm.resource}</span>
                     : <span style={{ color: T.faint }}>none</span>
              } />
            </div>

            {transforms.length > 0 && (
              <>
                <SectionLabel>BODY TRANSFORMS</SectionLabel>
                <div style={{ background: T.card, border: `1px solid ${T.border}`, overflow: 'hidden' }}>
                  {transforms.map((t, i) => (
                    <div key={i} style={{ padding: '9px 14px', borderBottom: i < transforms.length - 1 ? `1px solid ${T.border}` : 'none', fontFamily: T.mono, fontSize: 12, color: T.text }}>
                      <span style={{ color: T.faint }}>{t.from_key}</span>
                      <span style={{ color: T.dim, margin: '0 8px' }}>→</span>
                      <span style={{ color: T.textHi }}>{t.to_key}</span>
                      {t.wrap && t.wrap.length > 0 && <span style={{ color: T.dim, marginLeft: 10 }}>wrap: [{t.wrap.join(', ')}]</span>}
                    </div>
                  ))}
                </div>
              </>
            )}

            {asyncCfg && (
              <>
                <SectionLabel>ASYNC POLLING</SectionLabel>
                <div style={{ background: T.card, border: `1px solid ${T.border}`, overflow: 'hidden' }}>
                  <ConfigRow label="poll" value={<span><span style={{ color: T.textHi }}>{asyncCfg.poll_path}</span> every {asyncCfg.poll_interval_secs}s</span>} />
                  <ConfigRow label="id field" value={asyncCfg.id_field} />
                  <ConfigRow label="status field" value={asyncCfg.status_field} />
                  {asyncCfg.success_states && asyncCfg.success_states.length > 0 && <ConfigRow label="success" value={<span style={{ color: T.green }}>{asyncCfg.success_states.join(', ')}</span>} />}
                  {asyncCfg.failure_states && asyncCfg.failure_states.length > 0 && <ConfigRow label="failure" value={<span style={{ color: T.red }}>{asyncCfg.failure_states.join(', ')}</span>} />}
                  {asyncCfg.cancel_states && asyncCfg.cancel_states.length > 0 && <ConfigRow label="cancel" value={asyncCfg.cancel_states.join(', ')} />}
                  {asyncCfg.output_map_field && <ConfigRow label="output" value={asyncCfg.output_map_field} />}
                  {asyncCfg.output_field && <ConfigRow label={asyncCfg.output_map_field ? 'failure output' : 'output field'} value={asyncCfg.output_field} />}
                  {asyncCfg.error_fields && asyncCfg.error_fields.length > 0 && <ConfigRow label="error fields" last value={asyncCfg.error_fields.join(', ')} />}
                </div>
              </>
            )}
          </div>
        )}
      </div>
    </div>
  );
}

// ── Workflows page ────────────────────────────────────────────────────────────

/** Workflows route — top tab bar switching between the pipelines, steps (pre-configured reusable steps), and actions tabs. Steps can also be created and configured inline while building a pipeline. */
export function Workflows() {
  const [tab, setTab] = useState<MainTab>('pipelines');

  return (
    <div style={{ display: 'flex', height: '100%', overflow: 'hidden', flexDirection: 'column' }}>
      <div style={{ display: 'flex', borderBottom: `1px solid ${T.border}`, background: T.bgAlt, flexShrink: 0 }}>
        {(['pipelines', 'steps', 'actions'] as MainTab[]).map(t => (
          <button key={t} onClick={() => setTab(t)}
            style={{ background: tab === t ? T.card : 'transparent', border: 'none', borderBottom: `2px solid ${tab === t ? T.green : 'transparent'}`, color: tab === t ? T.textHi : T.dim, fontFamily: T.mono, fontSize: 12, padding: '11px 20px', cursor: 'pointer', letterSpacing: 0.3 }}>
            {t}
          </button>
        ))}
        <div style={{ flex: 1, display: 'flex', alignItems: 'center', paddingRight: 16, justifyContent: 'flex-end' }}>
          <span style={{ fontFamily: T.mono, fontSize: 11, color: T.faint }}>
            <span style={{ color: T.green }}>$</span> armory ci
          </span>
        </div>
      </div>
      <div style={{ flex: 1, display: 'flex', overflow: 'hidden' }}>
        {tab === 'pipelines' && <PipelinesTab />}
        {tab === 'steps' && <StepsTab />}
        {tab === 'actions' && <ActionsTab />}
      </div>
    </div>
  );
}

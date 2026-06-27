/** Workflows page — the CI/CD control surface, a tabbed view over pipelines, steps, and the action catalog. pipelines tab creates/triggers/cancels/deletes workflows and lists their runs; steps tab CRUDs reusable steps; actions tab browses the read-only action catalog. all data goes through the typed BFF client (listWorkflows/createStep/etc), never conductor directly. */
import { useState, useEffect, useCallback, useMemo, useRef } from 'react';
import type { ReactNode, CSSProperties } from 'react';
import { useNavigate } from 'react-router-dom';
import { T } from '../../theme';
import { Pill } from '../../components/Pill';
import { useConfirm } from '../../components/ConfirmDialog';
import { useAppSelector } from '../../store/hooks';
import {
  listWorkflows, getWorkflow, deleteWorkflow, listWorkflowRuns, triggerWorkflow, cancelRun,
  createWorkflow, updateWorkflow, listSteps, createStep, updateStep, deleteStep, listActions, listForgeImages,
} from '../../api/bff';
import type { Workflow, WorkflowRun, Step, WorkflowAction } from '../../api/bff';
import { ImageSelect } from '../../components/ImageSelect';
import { PipelineBlocks } from './PipelineBlocks';
import { StepInspector } from './StepInspector';
import type { StepRef } from './pipelineGraph';
import { stepsToPayload, configToJson, parseConfig } from './pipelineGraph';
import { schemaForAction, buildStepWith, formValsFromWith, WITH_KEY_PREFIX } from './stepSchema';
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
  token, initial, catalog, palette, onClose, onSaved,
}: {
  token: string;
  initial: Workflow | null;
  catalog: Record<string, Step>;
  palette: Step[];
  onClose: () => void;
  onSaved: (wf: Workflow) => void;
}) {
  const initialStepRefs = useMemo<StepRef[]>(
    () => initial ? initial.steps.map(s => ({
      step_id: s.step_id,
      // The GET returns the effective name; keep it as an override only when it
      // actually differs from the step definition's name (so unchanged steps still
      // track definition renames, and the JSON/payload stay clean).
      name: (!s.approval && s.name && s.step_id && s.name !== catalog[s.step_id]?.name) ? s.name : undefined,
      parallel_group: s.parallel_group ?? null,
      matrix: s.matrix ?? null,
      approval: s.approval ?? null,
    })) : [],
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

  // Live, editable JSON mirror. The textarea drives `jsonDraft`; while the user is
  // typing in it (jsonFocused) builder-side updates don't overwrite their text.
  const [jsonDraft, setJsonDraft] = useState(() => configToJson(initial?.name ?? '', initial?.description ?? '', initialStepRefs));
  const [jsonError, setJsonError] = useState<string | null>(null);
  const jsonFocused = useRef(false);

  // Right-panel tabs: a step inspector (inputs/output of the selected step) and the
  // live JSON config. Selecting a step in the builder switches to the inspector.
  const [rightTab, setRightTab] = useState<'inspector' | 'json'>('json');
  const [inspectId, setInspectId] = useState<string | null>(null);
  const [inspectName, setInspectName] = useState<string | undefined>(undefined);
  const [actions, setActions] = useState<WorkflowAction[]>([]);
  useEffect(() => { listActions(token).then(setActions).catch(() => {}); }, [token]);
  const actionsByName = useMemo(() => Object.fromEntries(actions.map(a => [a.name, a])), [actions]);
  const onInspect = useCallback((stepId: string | null, name?: string) => {
    setInspectId(stepId);
    setInspectName(name);
    setRightTab('inspector');
  }, []);

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

  const canSave = !!name.trim() && !saving && !jsonError;

  const handleSave = async () => {
    if (!name.trim()) return;
    setSaving(true); setSaveError(null);
    try {
      const payload = {
        name: name.trim(),
        description: desc.trim() || undefined,
        steps: stepsToPayload(cleanedSteps),
      };
      const wf = initial
        ? await updateWorkflow(token, initial.workflow_id, payload)
        : await createWorkflow(token, payload);
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
      <div style={{ flex: 1, minHeight: 0, display: 'flex' }}>
        <div style={{ flex: 1, minWidth: 0, padding: '14px 7px 14px 14px' }}>
          <PipelineBlocks editable initialSteps={builderSeed} catalog={catalog} palette={palette} onChange={setSteps} onInspect={onInspect} />
        </div>
        {/* Right panel: step inspector (inputs/output of the selected step) +
            live, editable JSON config, as tabs. */}
        <div style={{ width: 'min(42%, 560px)', minWidth: 300, flexShrink: 0, padding: '14px 14px 14px 7px', display: 'flex', flexDirection: 'column', minHeight: 0 }}>
          <div style={{ display: 'flex', alignItems: 'stretch', border: `1px solid ${T.border}`, borderBottom: 'none', background: T.bgAlt }}>
            {(['inspector', 'json'] as const).map(tab => (
              <button key={tab} onClick={() => setRightTab(tab)}
                style={{ background: rightTab === tab ? T.bg : 'transparent', border: 'none', borderRight: `1px solid ${T.border}`, color: rightTab === tab ? T.textHi : T.faint, fontFamily: T.mono, fontSize: 10, letterSpacing: 1, textTransform: 'uppercase', padding: '8px 14px', cursor: 'pointer' }}>
                {tab === 'inspector' ? 'step' : 'pipeline.json'}
              </button>
            ))}
            <div style={{ flex: 1 }} />
            {rightTab === 'json' && <span style={{ alignSelf: 'center', padding: '0 12px', fontFamily: T.mono, fontSize: 9, color: jsonError ? T.red : T.green }}>{jsonError ? '✗ invalid' : '✓ in sync'}</span>}
          </div>
          <div style={{ flex: 1, minHeight: 0, border: `1px solid ${T.border}`, background: T.bg, display: 'flex', flexDirection: 'column' }}>
            {rightTab === 'inspector' ? (
              <StepInspector step={inspectId ? (catalog[inspectId] ?? null) : null} name={inspectName} action={inspectId && catalog[inspectId] ? actionsByName[catalog[inspectId].action] : undefined} />
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
          palette: ∥ parallel · ⊞ matrix · ⏸ approval gate · click a step to inspect its inputs &amp; output · drag to reorder
        </span>
        <div style={{ flex: 1 }} />
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

  const fetchWorkflows = useCallback(async () => {
    setLoading(true); setError(null);
    try { setWorkflows(await listWorkflows(token)); }
    catch (e: unknown) { setError((e as Error).message); }
    finally { setLoading(false); }
  }, [token]);

  useEffect(() => { fetchWorkflows(); }, [fetchWorkflows]);

  // Step catalog backs the builder palette and resolves step_id -> name/action
  // labels on the graph. Best effort; the canvas falls back to truncated ids.
  useEffect(() => { listSteps(token).then(setCatalog).catch(() => {}); }, [token]);
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
        />
      )}
      {/* Left panel */}
      <div style={{ width: 260, flexShrink: 0, borderRight: `1px solid ${T.border}`, display: 'flex', flexDirection: 'column', background: T.bgAlt }}>
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
              </button>
            );
          })}
        </div>
      </div>

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
              <div style={{ display: 'grid', gridTemplateColumns: 'repeat(3, 1fr)', gap: 12, marginBottom: 20 }}>
                {([['id', selectedWorkflow.workflow_id.slice(0, 8) + '…'], ['steps', selectedWorkflow.steps.length], ['updated', timeAgo(selectedWorkflow.updated_at) + ' ago']] as [string, string | number][]).map(([k, v]) => (
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
                        <span style={{ fontFamily: T.mono, fontSize: 11.5, color: T.dim, flex: 1 }}>{run.run_id.slice(0, 8)}…</span>
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

// ── Steps tab ─────────────────────────────────────────────────────────────────

/** shared input/select style for the create-step form. */
const stepInputStyle: CSSProperties = { width: '100%', background: T.cardHi, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 11, padding: '6px 8px', outline: 'none', boxSizing: 'border-box', marginBottom: 6 };
/** small uppercase label shown above each tailored `with` field. */
const stepLabelStyle: CSSProperties = { display: 'block', fontFamily: T.mono, fontSize: 9, color: T.faint, textTransform: 'uppercase', letterSpacing: 0.5, marginBottom: 3 };

/** Steps tab — left rail lists reusable steps with an inline create form; right panel shows the selected step's action, timeout, and `with` inputs, with delete. */
function StepsTab() {
  const token = useAppSelector(s => s.auth.token)!;
  const [steps, setSteps] = useState<Step[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const [actions, setActions] = useState<WorkflowAction[]>([]);
  const [images, setImages] = useState<string[]>([]);
  const [showCreate, setShowCreate] = useState(false);
  const [name, setName] = useState('');
  const [description, setDescription] = useState('');
  const [timeoutSecs, setTimeoutSecs] = useState('');
  const [action, setAction] = useState('');
  const [withVals, setWithVals] = useState<Record<string, string>>({});
  const [creating, setCreating] = useState(false);
  const [createError, setCreateError] = useState<string | null>(null);
  // null = the form (when open) creates a new step; a step_id = it edits that step.
  const [editId, setEditId] = useState<string | null>(null);

  const fetchSteps = useCallback(async () => {
    setLoading(true); setError(null);
    try { setSteps(await listSteps(token)); }
    catch (e: unknown) { setError((e as Error).message); }
    finally { setLoading(false); }
  }, [token]);

  useEffect(() => { fetchSteps(); }, [fetchSteps]);

  // Load the action catalog once so the create form's Action field becomes a
  // selector whose tailored `with` inputs change with the chosen action — mirroring
  // the CLI TUI. Degrades to a free-text Action input if the catalog is unavailable.
  useEffect(() => { listActions(token).then(setActions).catch(() => {}); }, [token]);

  // Load the forge image allowlist so the forge/run image field is a picker (like
  // the Forge run form). Best effort — degrades to free text if unavailable.
  useEffect(() => { listForgeImages(token).then(setImages).catch(() => {}); }, [token]);

  // The Action selector offers every catalog action plus the built-in `http`
  // escape hatch. Manual approval is added directly in the pipeline builder as an
  // inline gate, so it is intentionally not a step action here.
  const actionOptions = useMemo(() => {
    const names = actions.map(a => a.name);
    return names.includes('http') ? names : [...names, 'http'];
  }, [actions]);

  // Default the Action to forge/run when the form opens. forge is core so the
  // catalog always offers it; the selector renders the current value even if the
  // catalog hasn't landed yet, so there's no broken intermediate state.
  useEffect(() => {
    if (showCreate && action === '') setAction('forge/run');
  }, [showCreate, action]);

  const selectedStep = steps.find(s => s.step_id === selected);

  const setWith = (key: string, val: string) => setWithVals(v => ({ ...v, [key]: val }));
  const resetCreate = () => { setName(''); setDescription(''); setTimeoutSecs(''); setAction(''); setWithVals({}); setCreateError(null); setEditId(null); };

  // Toggle the form open in create mode (clearing any in-progress edit), or close
  // it if it's already open for a create.
  const openCreate = () => {
    if (showCreate && editId === null) { setShowCreate(false); resetCreate(); return; }
    resetCreate();
    setShowCreate(true);
  };

  // Load an existing step into the form and open it in edit mode. formValsFromWith
  // is the inverse of buildStepWith, so the tailored `with` inputs prefill.
  const startEdit = (s: Step) => {
    setEditId(s.step_id);
    setName(s.name);
    setDescription(s.description ?? '');
    setTimeoutSecs(s.timeout != null ? String(s.timeout) : '');
    setAction(s.action);
    setWithVals(formValsFromWith(s.action, s.with ?? {}));
    setCreateError(null);
    setShowCreate(true);
  };

  /** Creates or (when editId is set) updates a step, assembling the `with` map from
   *  the action's tailored schema fields. The backend update is a full replace, so
   *  the payload is identical for both — only the endpoint differs. */
  const handleSubmit = async () => {
    if (!name.trim() || !action.trim()) return;
    setCreating(true); setCreateError(null);
    try {
      const withMap = buildStepWith(action, k => withVals[k] ?? '');
      const payload = {
        name: name.trim(),
        description: description.trim() || undefined,
        action: action.trim(),
        with: Object.keys(withMap).length > 0 ? withMap : undefined,
        timeout: timeoutSecs ? parseInt(timeoutSecs, 10) : undefined,
      };
      if (editId) {
        const updated = await updateStep(token, editId, payload);
        setSteps(prev => prev.map(s => s.step_id === editId ? updated : s));
      } else {
        const s = await createStep(token, payload);
        setSteps(prev => [s, ...prev]);
      }
      resetCreate();
      setShowCreate(false);
    } catch (e: unknown) { setCreateError((e as Error).message); }
    finally { setCreating(false); }
  };

  const [confirm, confirmEl] = useConfirm();

  const handleDelete = async (id: string) => {
    const name = steps.find(s => s.step_id === id)?.name;
    if (!(await confirm({ message: `Delete step ${name ?? id}? Pipelines referencing it may break.` }))) return;
    try {
      await deleteStep(token, id);
      setSteps(prev => prev.filter(s => s.step_id !== id));
      if (selected === id) setSelected(null);
    } catch (e: unknown) { setError((e as Error).message); }
  };

  return (
    <div style={{ display: 'flex', flex: 1, overflow: 'hidden' }}>
      {confirmEl}
      <div style={{ width: 260, flexShrink: 0, borderRight: `1px solid ${T.border}`, display: 'flex', flexDirection: 'column', background: T.bgAlt }}>
        <div style={{ padding: '14px 14px 10px', borderBottom: `1px solid ${T.border}` }}>
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 4 }}>
            <span style={{ fontFamily: T.mono, fontSize: 12, fontWeight: 700, color: T.textHi }}>steps</span>
            <div style={{ display: 'flex', gap: 6 }}>
              <button onClick={openCreate} title="new step"
                style={{ background: showCreate ? T.greenSoft : 'transparent', border: `1px solid ${showCreate ? T.green : T.border}`, color: showCreate ? T.green : T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 7px', cursor: 'pointer' }}>+</button>
              <button onClick={fetchSteps} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 7px', cursor: 'pointer' }}>↻</button>
            </div>
          </div>
          <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint }}>
            {steps.length > 0 && `${steps.length} step${steps.length !== 1 ? 's' : ''}`}
          </div>
        </div>

        {showCreate && (
          <div style={{ padding: '10px 14px', borderBottom: `1px solid ${T.border}`, background: T.card, overflow: 'auto', maxHeight: '62vh' }}>
            <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, textTransform: 'uppercase', marginBottom: 8 }}>{editId ? 'edit step' : 'new step'}</div>
            {createError && <div style={{ color: T.red, fontFamily: T.mono, fontSize: 10, marginBottom: 6 }}>{createError}</div>}

            <label style={stepLabelStyle}>name *</label>
            <input value={name} onChange={e => setName(e.target.value)} placeholder="unit_tests" autoFocus style={stepInputStyle} />

            <label style={stepLabelStyle}>action *</label>
            {actionOptions.length > 0 ? (
              <select value={action} onChange={e => { setAction(e.target.value); setCreateError(null); }} style={stepInputStyle}>
                {action && !actionOptions.includes(action) && <option value={action}>{action}</option>}
                {actionOptions.map(a => <option key={a} value={a}>{a}</option>)}
              </select>
            ) : (
              <input value={action} onChange={e => setAction(e.target.value)} placeholder="forge/run" style={stepInputStyle} />
            )}

            {schemaForAction(action).map(f => {
              const key = WITH_KEY_PREFIX + f.key;
              const val = withVals[key] ?? '';
              return (
                <div key={key}>
                  <label style={stepLabelStyle}>{f.label}{f.required ? ' *' : ''}</label>
                  {f.catalog === 'image' && images.length > 0 ? (
                    <div style={{ marginBottom: 6 }}>
                      <ImageSelect value={val} onChange={v => setWith(key, v)} options={images} placeholder={f.placeholder} fontSize={11} />
                    </div>
                  ) : f.multiline ? (
                    <textarea value={val} onChange={e => setWith(key, e.target.value)} placeholder={f.placeholder}
                      rows={f.key === 'run' ? 3 : 2} style={{ ...stepInputStyle, resize: 'vertical' }} />
                  ) : (
                    <input value={val} onChange={e => setWith(key, e.target.value)} placeholder={f.placeholder} style={stepInputStyle} />
                  )}
                </div>
              );
            })}

            <label style={stepLabelStyle}>timeout</label>
            <input value={timeoutSecs} onChange={e => setTimeoutSecs(e.target.value)} placeholder="seconds (default 30)" style={stepInputStyle} />

            <label style={stepLabelStyle}>description</label>
            <input value={description} onChange={e => setDescription(e.target.value)} placeholder="optional" style={stepInputStyle} />

            <div style={{ display: 'flex', gap: 6 }}>
              <button onClick={handleSubmit} disabled={!name.trim() || !action.trim() || creating}
                style={{ flex: 1, background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 10, fontWeight: 600, padding: '5px 0', cursor: 'pointer', opacity: (!name.trim() || !action.trim() || creating) ? 0.6 : 1 }}>
                {creating ? '[ · · · ]' : editId ? '[ save ]' : '[ create ]'}
              </button>
              <button onClick={() => { setShowCreate(false); resetCreate(); }} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '5px 8px', cursor: 'pointer' }}>✕</button>
            </div>
          </div>
        )}

        <div style={{ flex: 1, overflow: 'auto' }}>
          {loading ? (
            <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint, animation: 'pulse 1s ease-in-out infinite' }}>→ loading · · ·</div>
          ) : error ? (
            <div style={{ padding: '14px', fontFamily: T.mono, fontSize: 11, color: T.red }}>{error}</div>
          ) : steps.length === 0 ? (
            <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint }}>→ no steps</div>
          ) : steps.map(s => {
            const isActive = selected === s.step_id;
            return (
              <button key={s.step_id} onClick={() => setSelected(s.step_id)}
                style={{ width: '100%', textAlign: 'left', padding: '10px 14px', background: isActive ? T.greenSoft : 'transparent', border: 0, borderLeft: `2px solid ${isActive ? T.green : 'transparent'}`, fontFamily: T.mono, cursor: 'pointer', color: T.text, display: 'block', transition: 'background .12s' }}>
                <div style={{ fontSize: 13, fontWeight: 600, color: isActive ? T.textHi : T.text }}>{s.name}</div>
                <div style={{ fontSize: 11, color: T.faint, marginTop: 2 }}>{s.action}</div>
              </button>
            );
          })}
        </div>
      </div>

      <div style={{ flex: 1, overflow: 'auto' }}>
        {!selectedStep ? (
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', height: '100%' }}>
            <div style={{ fontFamily: T.mono, fontSize: 12, color: T.faint }}>→ select a step</div>
          </div>
        ) : (
          <div style={{ padding: '20px 24px' }}>
            <div style={{ display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between', marginBottom: 20 }}>
              <div>
                <div style={{ fontFamily: T.mono, fontSize: 18, fontWeight: 700, color: T.textHi, marginBottom: 4 }}>{selectedStep.name}</div>
                <div style={{ fontFamily: T.mono, fontSize: 12, color: T.blue }}>{selectedStep.action}</div>
                {selectedStep.description && <div style={{ fontFamily: T.mono, fontSize: 12, color: T.dim, marginTop: 4 }}>{selectedStep.description}</div>}
                <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, marginTop: 4 }}>updated {timeAgo(selectedStep.updated_at)} ago</div>
              </div>
              <div style={{ display: 'flex', gap: 8, flexShrink: 0 }}>
                <button onClick={() => startEdit(selectedStep)}
                  style={{ background: editId === selectedStep.step_id ? T.greenSoft : 'transparent', border: `1px solid ${editId === selectedStep.step_id ? T.green : T.border}`, color: editId === selectedStep.step_id ? T.green : T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}>
                  [ edit ]
                </button>
                <button onClick={() => handleDelete(selectedStep.step_id)}
                  style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}
                  onMouseEnter={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.red; (e.currentTarget as HTMLButtonElement).style.color = T.red; }}
                  onMouseLeave={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.border; (e.currentTarget as HTMLButtonElement).style.color = T.dim; }}>
                  [ delete ]
                </button>
              </div>
            </div>

            <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 12, marginBottom: 20 }}>
              <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '10px 14px' }}>
                <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 4, textTransform: 'uppercase' }}>id</div>
                <div style={{ fontFamily: T.mono, fontSize: 12, color: T.textHi }}>{selectedStep.step_id.slice(0, 8)}…</div>
              </div>
              {selectedStep.timeout != null && (
                <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '10px 14px' }}>
                  <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 4, textTransform: 'uppercase' }}>timeout</div>
                  <div style={{ fontFamily: T.mono, fontSize: 12, color: T.textHi }}>{selectedStep.timeout}s</div>
                </div>
              )}
            </div>

            {selectedStep.with && Object.keys(selectedStep.with).length > 0 && (
              <>
                <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 8 }}>WITH</div>
                <div style={{ background: T.card, border: `1px solid ${T.border}`, overflow: 'hidden' }}>
                  {Object.entries(selectedStep.with).map(([k, v], i, arr) => (
                    <div key={k} style={{ display: 'flex', padding: '8px 14px', borderBottom: i < arr.length - 1 ? `1px solid ${T.border}` : 'none', fontFamily: T.mono, fontSize: 12, gap: 12 }}>
                      <span style={{ color: T.faint, width: 120, flexShrink: 0 }}>{k}</span>
                      <span style={{ color: T.text, wordBreak: 'break-word' }}>{typeof v === 'string' ? v : JSON.stringify(v)}</span>
                    </div>
                  ))}
                </div>
              </>
            )}
          </div>
        )}
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

  useEffect(() => {
    listActions(token).then(setActions).catch(e => setError((e as Error).message)).finally(() => setLoading(false));
  }, [token]);

  const a = actions.find(x => x.name === selected);
  const perm = a?.required_permission;
  const transforms = a?.body_transforms ?? [];
  const asyncCfg = a?.async;

  return (
    <div style={{ display: 'flex', flex: 1, overflow: 'hidden' }}>
      <div style={{ width: 260, flexShrink: 0, borderRight: `1px solid ${T.border}`, background: T.bgAlt, overflow: 'auto' }}>
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
                  {asyncCfg.output_field && <ConfigRow label="output field" value={asyncCfg.output_field} />}
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

/** Workflows route — top tab bar switching between the pipelines, steps, and actions tabs. */
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

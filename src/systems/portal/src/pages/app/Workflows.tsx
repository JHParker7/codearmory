/** Workflows page — the CI/CD control surface, a tabbed view over pipelines, steps, and the action catalog. pipelines tab creates/triggers/cancels/deletes workflows and lists their runs; steps tab CRUDs reusable steps; actions tab browses the read-only action catalog. all data goes through the typed BFF client (listWorkflows/createStep/etc), never conductor directly. */
import { useState, useEffect, useCallback, useMemo } from 'react';
import type { ReactNode, CSSProperties } from 'react';
import { T } from '../../theme';
import { Pill } from '../../components/Pill';
import { useAppSelector } from '../../store/hooks';
import {
  listWorkflows, deleteWorkflow, listWorkflowRuns, triggerWorkflow, cancelRun,
  createWorkflow, listSteps, createStep, deleteStep, listActions, listForgeImages,
} from '../../api/bff';
import type { Workflow, WorkflowRun, Step, WorkflowAction } from '../../api/bff';
import { ImageSelect } from '../../components/ImageSelect';
import { schemaForAction, buildStepWith, WITH_KEY_PREFIX } from './stepSchema';
import { timeAgo } from '../../utils';

type MainTab = 'pipelines' | 'steps' | 'actions';

/** maps a run status to a Pill tone — green=completed/success, amber=in-flight, red=failed, dim=otherwise. */
function statusTone(status: string): 'green' | 'amber' | 'red' | 'dim' {
  if (['completed', 'success'].includes(status)) return 'green';
  if (['running', 'in_progress', 'pending', 'queued'].includes(status)) return 'amber';
  if (['failed', 'error'].includes(status)) return 'red';
  return 'dim';
}

// ── Pipeline runs detail ──────────────────────────────────────────────────────

/** Pipelines tab — left rail lists workflows with an inline create form (name/description/comma-separated step IDs); right panel shows the selected workflow's stats and recent runs, with trigger/cancel/delete controls. */
function PipelinesTab() {
  const token = useAppSelector(s => s.auth.token)!;
  const [workflows, setWorkflows] = useState<Workflow[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const [runs, setRuns] = useState<WorkflowRun[]>([]);
  const [runsLoading, setRunsLoading] = useState(false);
  const [triggering, setTriggering] = useState(false);
  const [showCreate, setShowCreate] = useState(false);
  const [newName, setNewName] = useState('');
  const [newDesc, setNewDesc] = useState('');
  const [newStepIds, setNewStepIds] = useState('');
  const [creating, setCreating] = useState(false);
  const [createError, setCreateError] = useState<string | null>(null);

  const fetchWorkflows = useCallback(async () => {
    setLoading(true); setError(null);
    try { setWorkflows(await listWorkflows(token)); }
    catch (e: unknown) { setError((e as Error).message); }
    finally { setLoading(false); }
  }, [token]);

  useEffect(() => { fetchWorkflows(); }, [fetchWorkflows]);

  /** selects a workflow and loads its runs. */
  const selectWorkflow = useCallback(async (id: string) => {
    setSelected(id); setRunsLoading(true);
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
    } catch (e: unknown) { setError((e as Error).message); }
    finally { setTriggering(false); }
  };

  const handleCancel = async (runId: string) => {
    try {
      await cancelRun(token, runId);
      setRuns(prev => prev.map(r => r.run_id === runId ? { ...r, status: 'cancelled' } : r));
    } catch (e: unknown) { setError((e as Error).message); }
  };

  const handleDeleteWorkflow = async (id: string) => {
    try {
      await deleteWorkflow(token, id);
      setWorkflows(prev => prev.filter(w => w.workflow_id !== id));
      if (selected === id) { setSelected(null); setRuns([]); }
    } catch (e: unknown) { setError((e as Error).message); }
  };

  /** creates a workflow from the form, parsing comma-separated step IDs into step refs. */
  const handleCreate = async () => {
    if (!newName.trim()) return;
    setCreating(true); setCreateError(null);
    try {
      const steps = newStepIds.split(',').map(s => s.trim()).filter(Boolean).map(step_id => ({ step_id }));
      const wf = await createWorkflow(token, { name: newName.trim(), description: newDesc.trim() || undefined, steps });
      setWorkflows(prev => [wf, ...prev]);
      setNewName(''); setNewDesc(''); setNewStepIds(''); setShowCreate(false);
    } catch (e: unknown) { setCreateError((e as Error).message); }
    finally { setCreating(false); }
  };

  const selectedWorkflow = workflows.find(w => w.workflow_id === selected);

  return (
    <div style={{ display: 'flex', flex: 1, overflow: 'hidden' }}>
      {/* Left panel */}
      <div style={{ width: 260, flexShrink: 0, borderRight: `1px solid ${T.border}`, display: 'flex', flexDirection: 'column', background: T.bgAlt }}>
        <div style={{ padding: '14px 14px 10px', borderBottom: `1px solid ${T.border}` }}>
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 4 }}>
            <span style={{ fontFamily: T.mono, fontSize: 12, fontWeight: 700, color: T.textHi }}>pipelines</span>
            <div style={{ display: 'flex', gap: 6 }}>
              <button onClick={() => setShowCreate(v => !v)}
                style={{ background: showCreate ? T.greenSoft : 'transparent', border: `1px solid ${showCreate ? T.green : T.border}`, color: showCreate ? T.green : T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 7px', cursor: 'pointer' }}>+</button>
              <button onClick={fetchWorkflows} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 7px', cursor: 'pointer' }}>↻</button>
            </div>
          </div>
        </div>

        {showCreate && (
          <div style={{ padding: '10px 14px', borderBottom: `1px solid ${T.border}`, background: T.card }}>
            {createError && <div style={{ color: T.red, fontFamily: T.mono, fontSize: 10, marginBottom: 6 }}>{createError}</div>}
            <input value={newName} onChange={e => setNewName(e.target.value)} placeholder="pipeline name" autoFocus
              style={{ width: '100%', background: T.cardHi, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 11, padding: '6px 8px', outline: 'none', boxSizing: 'border-box', marginBottom: 6 }} />
            <input value={newDesc} onChange={e => setNewDesc(e.target.value)} placeholder="description (optional)"
              style={{ width: '100%', background: T.cardHi, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 11, padding: '6px 8px', outline: 'none', boxSizing: 'border-box', marginBottom: 6 }} />
            <input value={newStepIds} onChange={e => setNewStepIds(e.target.value)} placeholder="step IDs (comma-separated)"
              style={{ width: '100%', background: T.cardHi, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 11, padding: '6px 8px', outline: 'none', boxSizing: 'border-box', marginBottom: 6 }} />
            <div style={{ display: 'flex', gap: 6 }}>
              <button onClick={handleCreate} disabled={!newName.trim() || creating}
                style={{ flex: 1, background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 10, fontWeight: 600, padding: '5px 0', cursor: 'pointer', opacity: (!newName.trim() || creating) ? 0.6 : 1 }}>
                {creating ? '[ · · · ]' : '[ create ]'}
              </button>
              <button onClick={() => setShowCreate(false)} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '5px 8px', cursor: 'pointer' }}>✕</button>
            </div>
          </div>
        )}

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
                    const isRunning = ['running', 'in_progress', 'pending', 'queued'].includes(run.status);
                    return (
                      <div key={run.run_id} style={{ display: 'flex', alignItems: 'center', padding: '9px 12px', borderBottom: i < runs.length - 1 ? `1px solid ${T.border}` : 'none', gap: 12 }}>
                        <Pill tone={statusTone(run.status)}>{run.status}</Pill>
                        <span style={{ fontFamily: T.mono, fontSize: 11.5, color: T.dim, flex: 1 }}>{run.run_id.slice(0, 8)}…</span>
                        <span style={{ fontFamily: T.mono, fontSize: 11, color: T.faint }}>{timeAgo(run.created_at)} ago</span>
                        {isRunning && (
                          <button onClick={() => handleCancel(run.run_id)}
                            style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 7px', cursor: 'pointer' }}>
                            [ cancel ]
                          </button>
                        )}
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

  // The Action selector offers every catalog action plus the `http` escape hatch.
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
  const resetCreate = () => { setName(''); setDescription(''); setTimeoutSecs(''); setAction(''); setWithVals({}); setCreateError(null); };

  /** creates a step, assembling the `with` map from the action's tailored schema fields. */
  const handleCreate = async () => {
    if (!name.trim() || !action.trim()) return;
    setCreating(true); setCreateError(null);
    try {
      const withMap = buildStepWith(action, k => withVals[k] ?? '');
      const s = await createStep(token, {
        name: name.trim(),
        description: description.trim() || undefined,
        action: action.trim(),
        with: Object.keys(withMap).length > 0 ? withMap : undefined,
        timeout: timeoutSecs ? parseInt(timeoutSecs, 10) : undefined,
      });
      setSteps(prev => [s, ...prev]);
      resetCreate();
      setShowCreate(false);
    } catch (e: unknown) { setCreateError((e as Error).message); }
    finally { setCreating(false); }
  };

  const handleDelete = async (id: string) => {
    try {
      await deleteStep(token, id);
      setSteps(prev => prev.filter(s => s.step_id !== id));
      if (selected === id) setSelected(null);
    } catch (e: unknown) { setError((e as Error).message); }
  };

  return (
    <div style={{ display: 'flex', flex: 1, overflow: 'hidden' }}>
      <div style={{ width: 260, flexShrink: 0, borderRight: `1px solid ${T.border}`, display: 'flex', flexDirection: 'column', background: T.bgAlt }}>
        <div style={{ padding: '14px 14px 10px', borderBottom: `1px solid ${T.border}` }}>
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 4 }}>
            <span style={{ fontFamily: T.mono, fontSize: 12, fontWeight: 700, color: T.textHi }}>steps</span>
            <div style={{ display: 'flex', gap: 6 }}>
              <button onClick={() => setShowCreate(v => !v)}
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
              <button onClick={handleCreate} disabled={!name.trim() || !action.trim() || creating}
                style={{ flex: 1, background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 10, fontWeight: 600, padding: '5px 0', cursor: 'pointer', opacity: (!name.trim() || !action.trim() || creating) ? 0.6 : 1 }}>
                {creating ? '[ · · · ]' : '[ create ]'}
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
              <button onClick={() => handleDelete(selectedStep.step_id)}
                style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}
                onMouseEnter={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.red; (e.currentTarget as HTMLButtonElement).style.color = T.red; }}
                onMouseLeave={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.border; (e.currentTarget as HTMLButtonElement).style.color = T.dim; }}>
                [ delete ]
              </button>
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

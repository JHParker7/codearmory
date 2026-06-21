import { useState, useEffect, useCallback } from 'react';
import { T } from '../../theme';
import { Pill } from '../../components/Pill';
import { useAppSelector } from '../../store/hooks';
import {
  listWorkflows, deleteWorkflow, listWorkflowRuns, triggerWorkflow, cancelRun,
  createWorkflow, listSteps, createStep, deleteStep, listActions,
} from '../../api/bff';
import type { Workflow, WorkflowRun, Step, WorkflowAction } from '../../api/bff';
import { timeAgo } from '../../utils';

type MainTab = 'pipelines' | 'steps' | 'actions';

function statusTone(status: string): 'green' | 'amber' | 'red' | 'dim' {
  if (['completed', 'success'].includes(status)) return 'green';
  if (['running', 'in_progress', 'pending', 'queued'].includes(status)) return 'amber';
  if (['failed', 'error'].includes(status)) return 'red';
  return 'dim';
}

// ── Pipeline runs detail ──────────────────────────────────────────────────────

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

function StepsTab() {
  const token = useAppSelector(s => s.auth.token)!;
  const [steps, setSteps] = useState<Step[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const [showCreate, setShowCreate] = useState(false);
  const [form, setForm] = useState({ name: '', description: '', action: '', with: '', timeout: '' });
  const [creating, setCreating] = useState(false);
  const [createError, setCreateError] = useState<string | null>(null);

  const fetchSteps = useCallback(async () => {
    setLoading(true); setError(null);
    try { setSteps(await listSteps(token)); }
    catch (e: unknown) { setError((e as Error).message); }
    finally { setLoading(false); }
  }, [token]);

  useEffect(() => { fetchSteps(); }, [fetchSteps]);

  const selectedStep = steps.find(s => s.step_id === selected);

  const handleCreate = async () => {
    if (!form.name.trim() || !form.action.trim()) return;
    setCreating(true); setCreateError(null);
    try {
      const withPairs: Record<string, string> = {};
      form.with.split(',').forEach(pair => {
        const [k, ...rest] = pair.split('=');
        if (k?.trim()) withPairs[k.trim()] = rest.join('=').trim();
      });
      const s = await createStep(token, {
        name: form.name.trim(),
        description: form.description.trim() || undefined,
        action: form.action.trim(),
        with: Object.keys(withPairs).length > 0 ? withPairs : undefined,
        timeout: form.timeout ? parseInt(form.timeout, 10) : undefined,
      });
      setSteps(prev => [s, ...prev]);
      setForm({ name: '', description: '', action: '', with: '', timeout: '' });
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
          <div style={{ padding: '10px 14px', borderBottom: `1px solid ${T.border}`, background: T.card, overflow: 'auto' }}>
            {createError && <div style={{ color: T.red, fontFamily: T.mono, fontSize: 10, marginBottom: 6 }}>{createError}</div>}
            {(['name', 'action', 'description'] as const).map(field => (
              <input key={field} value={form[field]} onChange={e => setForm(f => ({ ...f, [field]: e.target.value }))}
                placeholder={field} autoFocus={field === 'name'}
                style={{ width: '100%', background: T.cardHi, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 11, padding: '6px 8px', outline: 'none', boxSizing: 'border-box', marginBottom: 6 }} />
            ))}
            <input value={form.with} onChange={e => setForm(f => ({ ...f, with: e.target.value }))} placeholder="with (key=val,key2=val2)"
              style={{ width: '100%', background: T.cardHi, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 11, padding: '6px 8px', outline: 'none', boxSizing: 'border-box', marginBottom: 6 }} />
            <input value={form.timeout} onChange={e => setForm(f => ({ ...f, timeout: e.target.value }))} placeholder="timeout (seconds)"
              style={{ width: '100%', background: T.cardHi, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 11, padding: '6px 8px', outline: 'none', boxSizing: 'border-box', marginBottom: 6 }} />
            <div style={{ display: 'flex', gap: 6 }}>
              <button onClick={handleCreate} disabled={!form.name.trim() || !form.action.trim() || creating}
                style={{ flex: 1, background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 10, fontWeight: 600, padding: '5px 0', cursor: 'pointer', opacity: (!form.name.trim() || !form.action.trim() || creating) ? 0.6 : 1 }}>
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
                      <span style={{ color: T.text }}>{v}</span>
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

function ActionsTab() {
  const token = useAppSelector(s => s.auth.token)!;
  const [actions, setActions] = useState<WorkflowAction[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<string | null>(null);

  useEffect(() => {
    listActions(token).then(setActions).catch(e => setError((e as Error).message)).finally(() => setLoading(false));
  }, [token]);

  const selectedAction = actions.find(a => a.name === selected);

  return (
    <div style={{ display: 'flex', flex: 1, overflow: 'hidden' }}>
      <div style={{ width: 260, flexShrink: 0, borderRight: `1px solid ${T.border}`, background: T.bgAlt, overflow: 'auto' }}>
        <div style={{ padding: '14px 14px 10px', borderBottom: `1px solid ${T.border}` }}>
          <span style={{ fontFamily: T.mono, fontSize: 12, fontWeight: 700, color: T.textHi }}>action catalog</span>
        </div>
        {loading ? <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint, animation: 'pulse 1s ease-in-out infinite' }}>→ loading · · ·</div>
          : error ? <div style={{ padding: '14px', fontFamily: T.mono, fontSize: 11, color: T.red }}>{error}</div>
          : actions.length === 0 ? <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint }}>→ no actions</div>
          : actions.map(a => {
            const isActive = selected === a.name;
            return (
              <button key={a.name} onClick={() => setSelected(a.name)}
                style={{ width: '100%', textAlign: 'left', padding: '10px 14px', background: isActive ? T.greenSoft : 'transparent', border: 0, borderLeft: `2px solid ${isActive ? T.green : 'transparent'}`, fontFamily: T.mono, cursor: 'pointer', color: T.text, display: 'block', transition: 'background .12s' }}>
                <div style={{ fontSize: 12, fontWeight: 600, color: isActive ? T.textHi : T.text }}>{a.name}</div>
              </button>
            );
          })}
      </div>
      <div style={{ flex: 1, overflow: 'auto' }}>
        {!selectedAction ? (
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', height: '100%' }}>
            <div style={{ fontFamily: T.mono, fontSize: 12, color: T.faint }}>→ select an action</div>
          </div>
        ) : (
          <div style={{ padding: '20px 24px' }}>
            <div style={{ fontFamily: T.mono, fontSize: 18, fontWeight: 700, color: T.blue, marginBottom: 8 }}>{selectedAction.name}</div>
            {selectedAction.description && <div style={{ fontFamily: T.mono, fontSize: 13, color: T.dim, marginBottom: 20 }}>{selectedAction.description}</div>}
            {selectedAction.inputs && Object.keys(selectedAction.inputs).length > 0 && (
              <>
                <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 8 }}>INPUTS</div>
                <div style={{ background: T.card, border: `1px solid ${T.border}`, overflow: 'hidden' }}>
                  {Object.entries(selectedAction.inputs).map(([k, v], i, arr) => (
                    <div key={k} style={{ padding: '9px 14px', borderBottom: i < arr.length - 1 ? `1px solid ${T.border}` : 'none', fontFamily: T.mono, fontSize: 12 }}>
                      <span style={{ color: T.textHi, fontWeight: 600 }}>{k}</span>
                      {v !== null && v !== undefined && <span style={{ color: T.dim, marginLeft: 12 }}>{JSON.stringify(v)}</span>}
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

// ── Workflows page ────────────────────────────────────────────────────────────

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

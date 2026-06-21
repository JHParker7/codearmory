import { useState, useEffect, useCallback } from 'react';
import { T } from '../../theme';
import { useAppSelector } from '../../store/hooks';
import {
  listExecutions, getExecution, cancelExecution, createExecution,
  listRunnerClasses, createRunnerClass, updateRunnerClass, deleteRunnerClass,
} from '../../api/bff';
import type { Execution, RunnerClass } from '../../api/bff';
import { timeAgo } from '../../utils';

type ForgeTab = 'executions' | 'runner-classes';

function statusTone(status: string): 'green' | 'amber' | 'red' | 'dim' {
  if (['completed', 'success'].includes(status)) return 'green';
  if (['running', 'in_progress', 'pending', 'queued'].includes(status)) return 'amber';
  if (['failed', 'error'].includes(status)) return 'red';
  return 'dim';
}

// ── Executions tab ────────────────────────────────────────────────────────────

function ExecutionsTab() {
  const token = useAppSelector(s => s.auth.token)!;
  const [executions, setExecutions] = useState<Execution[]>([]);
  const [runnerClasses, setRunnerClasses] = useState<RunnerClass[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const [outputTab, setOutputTab] = useState<'stdout' | 'stderr'>('stdout');

  const [showCreate, setShowCreate] = useState(false);
  const [image, setImage] = useState('');
  const [cmd, setCmd] = useState('');
  const [envStr, setEnvStr] = useState('');
  const [timeout, setTimeout_] = useState('');
  const [runnerClass, setRunnerClass] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [createError, setCreateError] = useState<string | null>(null);

  const fetchExecutions = useCallback(async () => {
    setLoading(true); setError(null);
    try { setExecutions(await listExecutions(token)); }
    catch (e: unknown) { setError((e as Error).message); }
    finally { setLoading(false); }
  }, [token]);

  useEffect(() => {
    fetchExecutions();
    listRunnerClasses(token).then(setRunnerClasses).catch(() => {});
  }, [fetchExecutions, token]);

  const handleCancel = async (id: string) => {
    try {
      await cancelExecution(token, id);
      setExecutions(prev => prev.map(e => e.execution_id === id ? { ...e, status: 'cancelled' } : e));
    } catch (e: unknown) { setError((e as Error).message); }
  };

  const handleCreate = async () => {
    if (!image.trim() || !cmd.trim()) return;
    setSubmitting(true); setCreateError(null);
    try {
      const envPairs: Record<string, string> = {};
      envStr.split('\n').forEach(line => {
        const [k, ...rest] = line.split('=');
        if (k?.trim()) envPairs[k.trim()] = rest.join('=').trim();
      });
      const exec = await createExecution(token, {
        image: image.trim(),
        command: cmd.trim().split(/\s+/),
        env: Object.keys(envPairs).length > 0 ? envPairs : undefined,
        timeout: timeout ? parseInt(timeout, 10) : undefined,
        runner_class: runnerClass.trim() || undefined,
      });
      setExecutions(prev => [exec, ...prev]);
      setImage(''); setCmd(''); setEnvStr(''); setTimeout_(''); setRunnerClass('');
      setShowCreate(false);
      setSelected(exec.execution_id);
    } catch (e: unknown) { setCreateError((e as Error).message); }
    finally { setSubmitting(false); }
  };

  const isRunning = (s: string) => ['running', 'in_progress', 'pending', 'queued'].includes(s);

  // The list endpoint omits stdout/stderr to stay lightweight, so the full
  // execution detail must be fetched when one is selected — and re-polled while
  // it is still running so the output streams in (matching the TUI).
  const [selectedExec, setSelectedExec] = useState<Execution | null>(null);
  useEffect(() => {
    if (!selected) { setSelectedExec(null); return; }
    let cancelled = false;
    // Seed metadata instantly from the list row, then load the full record.
    setSelectedExec(executions.find(e => e.execution_id === selected) ?? null);
    const tick = async () => {
      try {
        const detail = await getExecution(token, selected);
        if (cancelled) return;
        setSelectedExec(detail);
        if (!isRunning(detail.status)) clearInterval(timer);
      } catch { /* keep last-known view on transient errors */ }
    };
    const timer = setInterval(tick, 2000);
    tick();
    return () => { cancelled = true; clearInterval(timer); };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [selected, token]);

  return (
    <div style={{ display: 'flex', flex: 1, overflow: 'hidden' }}>
      {/* Left panel */}
      <div style={{ width: 260, flexShrink: 0, borderRight: `1px solid ${T.border}`, display: 'flex', flexDirection: 'column', background: T.bgAlt }}>
        <div style={{ padding: '14px 14px 10px', borderBottom: `1px solid ${T.border}` }}>
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 4 }}>
            <span style={{ fontFamily: T.mono, fontSize: 12, fontWeight: 700, color: T.textHi }}>executions</span>
            <div style={{ display: 'flex', gap: 6 }}>
              <button onClick={() => setShowCreate(v => !v)}
                style={{ background: showCreate ? T.greenSoft : 'transparent', border: `1px solid ${showCreate ? T.green : T.border}`, color: showCreate ? T.green : T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 7px', cursor: 'pointer' }}>+</button>
              <button onClick={fetchExecutions} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 7px', cursor: 'pointer' }}>↻</button>
            </div>
          </div>
          <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint }}>
            {executions.length > 0 && `${executions.length} execution${executions.length !== 1 ? 's' : ''}`}
          </div>
        </div>

        {showCreate && (
          <div style={{ padding: '10px 14px', borderBottom: `1px solid ${T.border}`, background: T.card, overflow: 'auto', maxHeight: 320 }}>
            {createError && <div style={{ color: T.red, fontFamily: T.mono, fontSize: 10, marginBottom: 6 }}>{createError}</div>}
            <div style={{ fontFamily: T.mono, fontSize: 9, color: T.faint, marginBottom: 3, letterSpacing: 0.5 }}>IMAGE</div>
            <input value={image} onChange={e => setImage(e.target.value)} placeholder="ubuntu:22.04" autoFocus
              style={{ width: '100%', background: T.cardHi, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 11, padding: '6px 8px', outline: 'none', boxSizing: 'border-box', marginBottom: 6 }} />
            <div style={{ fontFamily: T.mono, fontSize: 9, color: T.faint, marginBottom: 3, letterSpacing: 0.5 }}>COMMAND</div>
            <input value={cmd} onChange={e => setCmd(e.target.value)} placeholder="echo hello world"
              style={{ width: '100%', background: T.cardHi, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 11, padding: '6px 8px', outline: 'none', boxSizing: 'border-box', marginBottom: 6 }} />
            <div style={{ fontFamily: T.mono, fontSize: 9, color: T.faint, marginBottom: 3, letterSpacing: 0.5 }}>ENV (KEY=VALUE, one per line)</div>
            <textarea value={envStr} onChange={e => setEnvStr(e.target.value)} rows={2} placeholder="FOO=bar"
              style={{ width: '100%', background: T.cardHi, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 11, padding: '6px 8px', outline: 'none', resize: 'none', boxSizing: 'border-box', marginBottom: 6 }} />
            <div style={{ display: 'flex', gap: 6, marginBottom: 6 }}>
              <div style={{ flex: 1 }}>
                <div style={{ fontFamily: T.mono, fontSize: 9, color: T.faint, marginBottom: 3, letterSpacing: 0.5 }}>TIMEOUT (s)</div>
                <input value={timeout} onChange={e => setTimeout_(e.target.value)} placeholder="60"
                  style={{ width: '100%', background: T.cardHi, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 11, padding: '6px 8px', outline: 'none', boxSizing: 'border-box' }} />
              </div>
              <div style={{ flex: 1 }}>
                <div style={{ fontFamily: T.mono, fontSize: 9, color: T.faint, marginBottom: 3, letterSpacing: 0.5 }}>RUNNER CLASS</div>
                <select value={runnerClass} onChange={e => setRunnerClass(e.target.value)}
                  style={{ width: '100%', background: T.cardHi, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 11, padding: '6px 8px', outline: 'none' }}>
                  <option value="">default</option>
                  {runnerClasses.filter(r => r.enabled).map(r => <option key={r.name} value={r.name}>{r.name}</option>)}
                </select>
              </div>
            </div>
            <div style={{ display: 'flex', gap: 6 }}>
              <button onClick={handleCreate} disabled={!image.trim() || !cmd.trim() || submitting}
                style={{ flex: 1, background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 10, fontWeight: 600, padding: '5px 0', cursor: 'pointer', opacity: (!image.trim() || !cmd.trim() || submitting) ? 0.6 : 1 }}>
                {submitting ? '[ · · · ]' : '[ run ]'}
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
          ) : executions.length === 0 ? (
            <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint }}>→ no executions</div>
          ) : executions.map(exec => {
            const isActive = selected === exec.execution_id;
            const tone = statusTone(exec.status);
            const dotColor = tone === 'green' ? T.green : tone === 'amber' ? T.amber : tone === 'red' ? T.red : T.dim;
            return (
              <button key={exec.execution_id} onClick={() => { setSelected(exec.execution_id); setOutputTab('stdout'); }}
                style={{ width: '100%', textAlign: 'left', padding: '10px 14px', background: isActive ? T.greenSoft : 'transparent', border: 0, borderLeft: `2px solid ${isActive ? T.green : 'transparent'}`, fontFamily: T.mono, cursor: 'pointer', color: T.text, display: 'block', transition: 'background .12s' }}>
                <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                  <span style={{ width: 6, height: 6, borderRadius: 3, background: dotColor, flexShrink: 0, animation: tone === 'amber' ? 'pulse 1.5s ease-in-out infinite' : 'none' }} />
                  <span style={{ fontSize: 12, fontWeight: 600, color: isActive ? T.textHi : T.text, flex: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                    {exec.image.split('/').pop() ?? exec.image}
                  </span>
                </div>
                <div style={{ fontSize: 11, color: T.faint, marginTop: 3, paddingLeft: 14 }}>{timeAgo(exec.created_at)} ago</div>
              </button>
            );
          })}
        </div>
      </div>

      {/* Right panel */}
      <div style={{ flex: 1, display: 'flex', flexDirection: 'column', overflow: 'hidden' }}>
        <div style={{ padding: '12px 20px', borderBottom: `1px solid ${T.border}`, background: T.bgAlt, display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
          <div style={{ fontFamily: T.mono, fontSize: 11, color: T.faint }}>
            <span style={{ color: T.green }}>$</span> armory forge{selectedExec ? ` · ${selectedExec.execution_id.slice(0, 8)}…` : ''}
          </div>
          {selectedExec && isRunning(selectedExec.status) && (
            <button onClick={() => handleCancel(selectedExec.execution_id)}
              style={{ background: 'transparent', border: `1px solid ${T.red}`, color: T.red, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}>
              [ ✕ cancel ]
            </button>
          )}
        </div>

        <div style={{ flex: 1, overflow: 'auto' }}>
          {!selectedExec ? (
            <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', height: '100%' }}>
              <div style={{ fontFamily: T.mono, fontSize: 12, color: T.faint }}>→ select an execution</div>
            </div>
          ) : (
            <div style={{ padding: '20px 24px' }}>
              <div style={{ display: 'grid', gridTemplateColumns: 'repeat(4, 1fr)', gap: 12, marginBottom: 20 }}>
                {([
                  ['status', selectedExec.status],
                  ['image', selectedExec.image.split('/').pop() ?? selectedExec.image],
                  ['exit code', selectedExec.exit_code ?? '—'],
                  ['duration', selectedExec.started_at && selectedExec.ended_at
                    ? `${Math.round((new Date(selectedExec.ended_at).getTime() - new Date(selectedExec.started_at).getTime()) / 1000)}s`
                    : isRunning(selectedExec.status) ? 'running…' : '—'],
                ] as [string, string | number][]).map(([k, v]) => (
                  <div key={k} style={{ background: T.card, border: `1px solid ${T.border}`, padding: '10px 14px' }}>
                    <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 4, textTransform: 'uppercase' }}>{k}</div>
                    <div style={{ fontFamily: T.mono, fontSize: 14, color: T.textHi, fontWeight: 700 }}>{v}</div>
                  </div>
                ))}
              </div>

              <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '10px 14px', marginBottom: 20, fontFamily: T.mono, fontSize: 12, color: T.text }}>
                <span style={{ color: T.faint }}>$ </span>{selectedExec.command.join(' ')}
              </div>

              {selectedExec.runner_class && (
                <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '8px 14px', marginBottom: 20, fontFamily: T.mono, fontSize: 11, color: T.dim }}>
                  <span style={{ color: T.faint }}>runner  </span>{selectedExec.runner_class}
                </div>
              )}

              {(selectedExec.stdout !== null || selectedExec.stderr !== null) && (
                <>
                  <div style={{ display: 'flex', borderBottom: `1px solid ${T.border}` }}>
                    {(['stdout', 'stderr'] as const).map(tab => (
                      <button key={tab} onClick={() => setOutputTab(tab)}
                        style={{ background: outputTab === tab ? T.card : 'transparent', border: 'none', borderBottom: `2px solid ${outputTab === tab ? T.green : 'transparent'}`, color: outputTab === tab ? T.textHi : T.dim, fontFamily: T.mono, fontSize: 11, padding: '7px 16px', cursor: 'pointer' }}>
                        {tab}
                      </button>
                    ))}
                  </div>
                  <div style={{ background: T.bg, border: `1px solid ${T.border}`, borderTop: 'none', padding: '12px 14px', maxHeight: 320, overflow: 'auto' }}>
                    <pre style={{ margin: 0, fontFamily: T.mono, fontSize: 12, color: outputTab === 'stderr' ? T.red : T.text, lineHeight: 1.6, whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>
                      {(outputTab === 'stdout' ? selectedExec.stdout : selectedExec.stderr) || <span style={{ color: T.faint }}>— empty —</span>}
                    </pre>
                  </div>
                </>
              )}
            </div>
          )}
        </div>
      </div>
    </div>
  );
}

// ── Runner Classes tab ────────────────────────────────────────────────────────

function RunnerClassesTab() {
  const token = useAppSelector(s => s.auth.token)!;
  const canWrite = useAppSelector(s => s.auth.permissions?.['forge:createRunnerClass'] === true);
  const [classes, setClasses] = useState<RunnerClass[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const [showCreate, setShowCreate] = useState(false);
  const [form, setForm] = useState({ name: '', memory_mb: '', cpu_millicores: '', pids_limit: '', tmpfs_mb: '', enabled: true });
  const [creating, setCreating] = useState(false);
  const [createError, setCreateError] = useState<string | null>(null);
  const [editingEnabled, setEditingEnabled] = useState<{ name: string; enabled: boolean } | null>(null);

  const fetchClasses = useCallback(async () => {
    setLoading(true); setError(null);
    try { setClasses(await listRunnerClasses(token)); }
    catch (e: unknown) { setError((e as Error).message); }
    finally { setLoading(false); }
  }, [token]);

  useEffect(() => { fetchClasses(); }, [fetchClasses]);

  const selectedClass = classes.find(c => c.name === selected);

  const handleCreate = async () => {
    if (!form.name.trim() || !form.memory_mb || !form.cpu_millicores) return;
    setCreating(true); setCreateError(null);
    try {
      const rc = await createRunnerClass(token, {
        name: form.name.trim(),
        memory_mb: parseInt(form.memory_mb, 10),
        cpu_millicores: parseInt(form.cpu_millicores, 10),
        pids_limit: form.pids_limit ? parseInt(form.pids_limit, 10) : undefined,
        tmpfs_mb: form.tmpfs_mb ? parseInt(form.tmpfs_mb, 10) : undefined,
        enabled: form.enabled,
      });
      setClasses(prev => [rc, ...prev]);
      setForm({ name: '', memory_mb: '', cpu_millicores: '', pids_limit: '', tmpfs_mb: '', enabled: true });
      setShowCreate(false);
    } catch (e: unknown) { setCreateError((e as Error).message); }
    finally { setCreating(false); }
  };

  const handleToggleEnabled = async (name: string, enabled: boolean) => {
    setEditingEnabled({ name, enabled: !enabled });
    try {
      await updateRunnerClass(token, name, { enabled: !enabled });
      setClasses(prev => prev.map(c => c.name === name ? { ...c, enabled: !enabled } : c));
    } catch (e: unknown) { setError((e as Error).message); }
    finally { setEditingEnabled(null); }
  };

  const handleDelete = async (name: string) => {
    try {
      await deleteRunnerClass(token, name);
      setClasses(prev => prev.filter(c => c.name !== name));
      if (selected === name) setSelected(null);
    } catch (e: unknown) { setError((e as Error).message); }
  };

  return (
    <div style={{ display: 'flex', flex: 1, overflow: 'hidden' }}>
      <div style={{ width: 260, flexShrink: 0, borderRight: `1px solid ${T.border}`, display: 'flex', flexDirection: 'column', background: T.bgAlt }}>
        <div style={{ padding: '14px 14px 10px', borderBottom: `1px solid ${T.border}` }}>
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 4 }}>
            <span style={{ fontFamily: T.mono, fontSize: 12, fontWeight: 700, color: T.textHi }}>runner classes</span>
            <div style={{ display: 'flex', gap: 6 }}>
              {canWrite && <button onClick={() => setShowCreate(v => !v)}
                style={{ background: showCreate ? T.greenSoft : 'transparent', border: `1px solid ${showCreate ? T.green : T.border}`, color: showCreate ? T.green : T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 7px', cursor: 'pointer' }}>+</button>}
              <button onClick={fetchClasses} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 7px', cursor: 'pointer' }}>↻</button>
            </div>
          </div>
        </div>

        {canWrite && showCreate && (
          <div style={{ padding: '10px 14px', borderBottom: `1px solid ${T.border}`, background: T.card }}>
            {createError && <div style={{ color: T.red, fontFamily: T.mono, fontSize: 10, marginBottom: 6 }}>{createError}</div>}
            {[{ f: 'name', ph: 'name' }, { f: 'memory_mb', ph: 'memory (MB)' }, { f: 'cpu_millicores', ph: 'CPU (millicores)' }, { f: 'pids_limit', ph: 'pids limit (opt)' }, { f: 'tmpfs_mb', ph: 'tmpfs (MB, opt)' }].map(({ f, ph }) => (
              <input key={f} value={(form as Record<string, unknown>)[f] as string} onChange={e => setForm(prev => ({ ...prev, [f]: e.target.value }))} placeholder={ph}
                autoFocus={f === 'name'}
                style={{ width: '100%', background: T.cardHi, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 11, padding: '5px 8px', outline: 'none', boxSizing: 'border-box', marginBottom: 5 }} />
            ))}
            <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 6 }}>
              <button onClick={() => setForm(f => ({ ...f, enabled: !f.enabled }))}
                style={{ background: form.enabled ? T.greenSoft : 'transparent', border: `1px solid ${form.enabled ? T.green : T.border}`, color: form.enabled ? T.green : T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 8px', cursor: 'pointer' }}>
                {form.enabled ? '● enabled' : '○ disabled'}
              </button>
            </div>
            <div style={{ display: 'flex', gap: 6 }}>
              <button onClick={handleCreate} disabled={!form.name.trim() || !form.memory_mb || !form.cpu_millicores || creating}
                style={{ flex: 1, background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 10, fontWeight: 600, padding: '5px 0', cursor: 'pointer', opacity: (!form.name.trim() || !form.memory_mb || !form.cpu_millicores || creating) ? 0.6 : 1 }}>
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
          ) : classes.length === 0 ? (
            <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint }}>→ no runner classes</div>
          ) : classes.map(rc => {
            const isActive = selected === rc.name;
            return (
              <button key={rc.name} onClick={() => setSelected(rc.name)}
                style={{ width: '100%', textAlign: 'left', padding: '10px 14px', background: isActive ? T.greenSoft : 'transparent', border: 0, borderLeft: `2px solid ${isActive ? T.green : 'transparent'}`, fontFamily: T.mono, cursor: 'pointer', color: T.text, display: 'block', transition: 'background .12s' }}>
                <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
                  <span style={{ fontSize: 13, fontWeight: 600, color: isActive ? T.textHi : T.text }}>{rc.name}</span>
                  {!rc.enabled && <span style={{ fontSize: 9, color: T.faint, border: `1px solid ${T.faint}`, padding: '0 4px' }}>disabled</span>}
                </div>
                <div style={{ fontSize: 10, color: T.faint, marginTop: 2 }}>{rc.memory_mb}MB · {rc.cpu_millicores}m</div>
              </button>
            );
          })}
        </div>
      </div>

      <div style={{ flex: 1, overflow: 'auto' }}>
        {!selectedClass ? (
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', height: '100%' }}>
            <div style={{ fontFamily: T.mono, fontSize: 12, color: T.faint }}>→ select a runner class</div>
          </div>
        ) : (
          <div style={{ padding: '20px 24px' }}>
            <div style={{ display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between', marginBottom: 20 }}>
              <div>
                <div style={{ fontFamily: T.mono, fontSize: 20, fontWeight: 700, color: T.textHi, marginBottom: 8 }}>{selectedClass.name}</div>
                {canWrite && (
                  <button onClick={() => handleToggleEnabled(selectedClass.name, selectedClass.enabled)}
                    disabled={editingEnabled?.name === selectedClass.name}
                    style={{ background: selectedClass.enabled ? T.greenSoft : T.card, border: `1px solid ${selectedClass.enabled ? T.green : T.border}`, color: selectedClass.enabled ? T.green : T.dim, fontFamily: T.mono, fontSize: 11, padding: '4px 12px', cursor: 'pointer' }}>
                    {editingEnabled?.name === selectedClass.name ? '[ · · · ]' : selectedClass.enabled ? '● enabled' : '○ disabled'}
                  </button>
                )}
              </div>
              {canWrite && (
                <button onClick={() => handleDelete(selectedClass.name)}
                  style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}
                  onMouseEnter={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.red; (e.currentTarget as HTMLButtonElement).style.color = T.red; }}
                  onMouseLeave={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.border; (e.currentTarget as HTMLButtonElement).style.color = T.dim; }}>
                  [ delete ]
                </button>
              )}
            </div>

            <div style={{ display: 'grid', gridTemplateColumns: 'repeat(2, 1fr)', gap: 12 }}>
              {([
                ['memory', `${selectedClass.memory_mb} MB`],
                ['cpu', `${selectedClass.cpu_millicores}m`],
                ...(selectedClass.pids_limit != null ? [['pids limit', String(selectedClass.pids_limit)] as [string, string]] : []),
                ...(selectedClass.tmpfs_mb != null ? [['tmpfs', `${selectedClass.tmpfs_mb} MB`] as [string, string]] : []),
              ] as [string, string][]).map(([k, v]) => (
                <div key={k} style={{ background: T.card, border: `1px solid ${T.border}`, padding: '10px 14px' }}>
                  <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 4, textTransform: 'uppercase' }}>{k}</div>
                  <div style={{ fontFamily: T.mono, fontSize: 15, color: T.textHi, fontWeight: 700 }}>{v}</div>
                </div>
              ))}
            </div>
          </div>
        )}
      </div>
    </div>
  );
}

// ── Forge page ────────────────────────────────────────────────────────────────

export function Forge() {
  const [tab, setTab] = useState<ForgeTab>('executions');

  return (
    <div style={{ display: 'flex', height: '100%', overflow: 'hidden', flexDirection: 'column' }}>
      <div style={{ display: 'flex', borderBottom: `1px solid ${T.border}`, background: T.bgAlt, flexShrink: 0 }}>
        {(['executions', 'runner-classes'] as ForgeTab[]).map(t => (
          <button key={t} onClick={() => setTab(t)}
            style={{ background: tab === t ? T.card : 'transparent', border: 'none', borderBottom: `2px solid ${tab === t ? T.green : 'transparent'}`, color: tab === t ? T.textHi : T.dim, fontFamily: T.mono, fontSize: 12, padding: '11px 20px', cursor: 'pointer', letterSpacing: 0.3 }}>
            {t}
          </button>
        ))}
        <div style={{ flex: 1, display: 'flex', alignItems: 'center', paddingRight: 16, justifyContent: 'flex-end' }}>
          <span style={{ fontFamily: T.mono, fontSize: 11, color: T.faint }}>
            <span style={{ color: T.green }}>$</span> armory forge
          </span>
        </div>
      </div>
      <div style={{ flex: 1, display: 'flex', overflow: 'hidden' }}>
        {tab === 'executions' && <ExecutionsTab />}
        {tab === 'runner-classes' && <RunnerClassesTab />}
      </div>
    </div>
  );
}

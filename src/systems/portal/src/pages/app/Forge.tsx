/**
 * Forge page: sandboxed execution + runner-class admin. Two tabs — executions
 * (run a one-off image+command and stream stdout/stderr) and runner classes (the
 * admin cpu/memory/limits resource classes). Talks to the BFF (listExecutions /
 * createExecution / listRunnerClasses / …); write actions on runner classes are
 * gated by the forge:createRunnerClass permission.
 */
import { useState, useEffect, useCallback, type CSSProperties } from 'react';
import { T } from '../../theme';
import { useConfirm } from '../../components/ConfirmDialog';
import { useAppSelector } from '../../store/hooks';
import {
  listExecutions, getExecution, cancelExecution, createExecution,
  listRunnerClasses, createRunnerClass, updateRunnerClass, deleteRunnerClass,
  listForgeImages, listRuntimeBackends, listGitRepos,
} from '../../api/bff';
import type { Execution, RunnerClass, RuntimeBackend, GitRepo } from '../../api/bff';
import { ImageSelect } from '../../components/ImageSelect';
import { RepoSelect } from '../../components/RepoSelect';
import { useResizableWidth } from '../../components/ResizeHandle';
import { timeAgo } from '../../utils';

type ForgeTab = 'executions' | 'runner-classes';

// VM-isolated backends (kata microVMs, proxmox VMs) put the isolation boundary at
// the VM, not the container — the only place `privileged` is meaningful. Used to
// tint the runtime badge so a kata/VM class is distinguishable at a glance.
const VM_ISOLATED = new Set(['kata', 'proxmox']);

/** Badge style for a runner class's runtime type — VM-isolated types stand out green. */
function runtimeBadge(type: string): CSSProperties {
  const c = VM_ISOLATED.has(type) ? T.green : T.dim;
  return { fontSize: 9, color: c, border: `1px solid ${c}`, padding: '0 4px', letterSpacing: 0.3, textTransform: 'uppercase' };
}

/** Badge for a privileged runner class — amber, since it relaxes the sandbox. */
const privBadge: CSSProperties = { fontSize: 9, color: T.amber, border: `1px solid ${T.amber}`, padding: '0 4px', letterSpacing: 0.3, textTransform: 'uppercase' };

/** maps an execution status to a status tone (green=done, amber=in-flight, red=failed, dim=other). */
function statusTone(status: string): 'green' | 'amber' | 'red' | 'dim' {
  if (['completed', 'success'].includes(status)) return 'green';
  if (['running', 'in_progress', 'pending', 'queued'].includes(status)) return 'amber';
  if (['failed', 'error'].includes(status)) return 'red';
  return 'dim';
}

/**
 * Renders a stored command argv back into the script the user typed. We submit
 * commands wrapped as ["sh","-c", <script>] (see handleCreate), so unwrap that
 * form to show the original — multi-line and without the sh -c scaffolding.
 * Anything not in that shape (legacy argv-style executions) joins with spaces.
 */
function displayCommand(command: string[]): string {
  if (command.length === 3 && (command[0] === 'sh' || command[0] === '/bin/sh') && command[1] === '-c') {
    return command[2];
  }
  return command.join(' ');
}

// ── Create-execution modal ────────────────────────────────────────────────────

/** The fields needed to launch an execution — shared by the create modal and rerun. */
type ExecutionInput = { image: string; command: string[]; env?: Record<string, string>; timeout?: number; runner_class?: string; secret_refs?: Record<string, string> };

/** Env var the repo picker injects the minted clone URL as (matches the workflow forge/run step). */
const GIT_CLONE_ENV = 'GIT_CLONE_URL';

/**
 * Centered modal for launching a one-off execution. Roomier than the old inline
 * sidebar form: a multi-line command textarea and an image picker that filters the
 * forge allowlist (ImageSelect). Forge only runs allowlisted images, so the picker
 * is the source of truth for what can be launched. Submitting (and the optimistic
 * list update) is delegated to onSubmit; it rejects on failure so the modal can
 * surface the error and stay open.
 */
function CreateExecutionModal({ token, runnerClasses, onClose, onSubmit }: {
  token: string;
  runnerClasses: RunnerClass[];
  onClose: () => void;
  onSubmit: (input: ExecutionInput) => Promise<void>;
}) {
  const [image, setImage] = useState('');
  const [cmd, setCmd] = useState('');
  const [envStr, setEnvStr] = useState('');
  const [repoUrl, setRepoUrl] = useState('');
  const [timeout, setTimeout_] = useState('');
  const [runnerClass, setRunnerClass] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [createError, setCreateError] = useState<string | null>(null);
  const [imageOptions, setImageOptions] = useState<string[]>([]);
  const [repos, setRepos] = useState<GitRepo[]>([]);

  // The forge image allowlist drives the image picker. Best effort — an empty or
  // failing response just leaves the picker with no suggestions to filter.
  useEffect(() => {
    listForgeImages(token).then(setImageOptions).catch(() => setImageOptions([]));
  }, [token]);

  // The git repo list drives the repo picker; selecting one injects clone creds.
  // Best effort — degrades to a free-text URL field when unavailable.
  useEffect(() => {
    listGitRepos(token).then(setRepos).catch(() => setRepos([]));
  }, [token]);

  const handleCreate = async () => {
    if (!image.trim() || !cmd.trim()) return;
    setSubmitting(true); setCreateError(null);
    try {
      const envPairs: Record<string, string> = {};
      envStr.split('\n').forEach(line => {
        const [k, ...rest] = line.split('=');
        if (k?.trim()) envPairs[k.trim()] = rest.join('=').trim();
      });
      const repo = repoUrl.trim();
      await onSubmit({
        image: image.trim(),
        // Wrap in `sh -c` so the textarea runs as a shell script — multi-line
        // scripts, pipes, and && work, matching the forge/run pipeline action
        // (registry-manifest wrap:["sh","-c"]) and the CLI's --run flag. Forge
        // executes command[] via execve with no shell, so without this a
        // multi-line command is tokenised on whitespace into a broken argv.
        command: ['sh', '-c', cmd.trim()],
        env: Object.keys(envPairs).length > 0 ? envPairs : undefined,
        timeout: timeout ? parseInt(timeout, 10) : undefined,
        runner_class: runnerClass.trim() || undefined,
        // Selecting a repo injects a short-lived clone URL as $GIT_CLONE_URL at
        // dispatch (resolved by the git broker, never persisted with the run).
        secret_refs: repo ? { [GIT_CLONE_ENV]: `git:${repo}` } : undefined,
      });
    } catch (e: unknown) { setCreateError((e as Error).message); }
    finally { setSubmitting(false); }
  };

  const canSubmit = !!image.trim() && !!cmd.trim() && !submitting;
  const label = { fontFamily: T.mono, fontSize: 10, color: T.faint, marginBottom: 4, letterSpacing: 0.5 } as const;
  const field = { width: '100%', background: T.cardHi, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 12, padding: '8px 10px', outline: 'none', boxSizing: 'border-box' } as const;

  return (
    <div onClick={onClose} style={{ position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.7)', zIndex: 50, display: 'flex', alignItems: 'center', justifyContent: 'center' }}>
      <div onClick={e => e.stopPropagation()} style={{ width: 640, maxWidth: '92vw', maxHeight: '90vh', overflow: 'auto', background: T.card, border: `1px solid ${T.borderHi}` }}>
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '12px 18px', borderBottom: `1px solid ${T.border}`, background: T.cardHi }}>
          <span style={{ fontFamily: T.mono, fontSize: 12, color: T.dim }}><span style={{ color: T.green }}>$</span> new execution</span>
          <button onClick={onClose} style={{ background: 'transparent', border: 0, color: T.faint, cursor: 'pointer', fontSize: 16 }}>×</button>
        </div>

        <div style={{ padding: '18px 20px 16px' }}>
          {createError && <div style={{ color: T.red, fontFamily: T.mono, fontSize: 11, marginBottom: 12 }}>{createError}</div>}

          <div style={label}>IMAGE</div>
          <div style={{ marginBottom: 14 }}>
            <ImageSelect value={image} onChange={setImage} options={imageOptions} autoFocus />
          </div>

          <div style={label}>COMMAND <span style={{ color: T.faint, opacity: 0.7 }}>(runs via sh -c · multi-line ok)</span></div>
          <textarea value={cmd} onChange={e => setCmd(e.target.value)} rows={5} placeholder={'echo hello\necho world'}
            style={{ ...field, resize: 'vertical', lineHeight: 1.5, marginBottom: 14 }} />

          <div style={label}>ENV <span style={{ color: T.faint, opacity: 0.7 }}>(KEY=VALUE, one per line)</span></div>
          <textarea value={envStr} onChange={e => setEnvStr(e.target.value)} rows={3} placeholder="FOO=bar"
            style={{ ...field, resize: 'vertical', lineHeight: 1.5, marginBottom: 14 }} />

          <div style={label}>GIT REPO <span style={{ color: T.faint, opacity: 0.7 }}>(optional · clone creds injected as ${GIT_CLONE_ENV})</span></div>
          <div style={{ marginBottom: 18 }}>
            <RepoSelect value={repoUrl} onChange={setRepoUrl} repos={repos} />
          </div>

          <div style={{ display: 'flex', gap: 12, marginBottom: 18 }}>
            <div style={{ flex: 1 }}>
              <div style={label}>TIMEOUT (s)</div>
              <input value={timeout} onChange={e => setTimeout_(e.target.value)} placeholder="60" style={field} />
            </div>
            <div style={{ flex: 1 }}>
              <div style={label}>RUNNER CLASS</div>
              <select value={runnerClass} onChange={e => setRunnerClass(e.target.value)} style={field}>
                <option value="">default</option>
                {runnerClasses.filter(r => r.enabled).map(r => <option key={r.name} value={r.name}>{r.name}</option>)}
              </select>
            </div>
          </div>

          <div style={{ display: 'flex', gap: 10 }}>
            <button onClick={handleCreate} disabled={!canSubmit}
              style={{ flex: 1, background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 13, fontWeight: 600, padding: '9px 0', cursor: canSubmit ? 'pointer' : 'default', letterSpacing: 0.4, opacity: canSubmit ? 1 : 0.6 }}>
              {submitting ? '[ · · · ]' : '[ run ]'}
            </button>
            <button onClick={onClose}
              style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 12, padding: '9px 16px', cursor: 'pointer' }}>
              cancel
            </button>
          </div>
        </div>
      </div>
    </div>
  );
}

// ── Executions tab ────────────────────────────────────────────────────────────

/** Executions tab: left list of executions + a create button (opens the create modal), right pane streams the selected run's detail (status/exit/duration, command, stdout/stderr). Running rows can be cancelled. */
function ExecutionsTab() {
  const token = useAppSelector(s => s.auth.token)!;
  // Current-project view filter from the sidebar switcher: filters the list and
  // tags new executions so they stay visible under the active filter.
  const project = useAppSelector(s => s.project.current);
  const [executions, setExecutions] = useState<Execution[]>([]);
  const [runnerClasses, setRunnerClasses] = useState<RunnerClass[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const [outputTab, setOutputTab] = useState<'stdout' | 'stderr'>('stdout');

  const [showCreate, setShowCreate] = useState(false);

  const fetchExecutions = useCallback(async () => {
    setLoading(true); setError(null);
    try { setExecutions(await listExecutions(token, project ?? undefined)); }
    catch (e: unknown) { setError((e as Error).message); }
    finally { setLoading(false); }
  }, [token, project]);

  useEffect(() => {
    fetchExecutions();
    listRunnerClasses(token).then(setRunnerClasses).catch(() => {});
  }, [fetchExecutions, token]);

  // Auto-refresh the run list every 5s so each row's status (pending → running →
  // succeeded/failed) updates without a manual reload. Silent on purpose: it
  // refreshes the list in place rather than calling fetchExecutions, so it never
  // toggles the loading spinner or clobbers an error with a transient blip.
  useEffect(() => {
    const timer = setInterval(() => {
      listExecutions(token, project ?? undefined).then(setExecutions).catch(() => { /* keep last-known list */ });
    }, 5000);
    return () => clearInterval(timer);
  }, [token, project]);

  const [rerunning, setRerunning] = useState(false);
  const [rerunError, setRerunError] = useState<string | null>(null);

  const handleCancel = async (id: string) => {
    try {
      await cancelExecution(token, id);
      setExecutions(prev => prev.map(e => e.execution_id === id ? { ...e, status: 'cancelled' } : e));
    } catch (e: unknown) { setError((e as Error).message); }
  };

  // Launch an execution and optimistically prepend it. POST only returns the id, so
  // synthesise a full row from the input; the 2s detail poll backfills the real
  // record. Rejects on failure so the modal/rerun caller can show the error.
  const submitExecution = useCallback(async (input: ExecutionInput) => {
    const { execution_id } = await createExecution(token, { ...input, project: project ?? undefined });
    setExecutions(prev => [{
      execution_id, user_id: '', image: input.image, command: input.command,
      env: input.env ?? null, timeout: input.timeout ?? null,
      runner_class: input.runner_class ?? null, secret_refs: input.secret_refs ?? null,
      project: project ?? undefined, status: 'pending',
      exit_code: null, stdout: null, stderr: null,
      created_at: new Date().toISOString(), started_at: null, ended_at: null,
    }, ...prev]);
    setShowCreate(false);
    setSelected(execution_id);
  }, [token, project]);

  // Rerun reuses an existing execution's settings verbatim.
  const handleRerun = async (exec: Execution) => {
    setRerunning(true); setRerunError(null);
    try {
      await submitExecution({
        image: exec.image,
        command: exec.command,
        env: exec.env && Object.keys(exec.env).length > 0 ? exec.env : undefined,
        timeout: exec.timeout ?? undefined,
        runner_class: exec.runner_class ?? undefined,
        secret_refs: exec.secret_refs && Object.keys(exec.secret_refs).length > 0 ? exec.secret_refs : undefined,
      });
    } catch (e: unknown) { setRerunError((e as Error).message); }
    finally { setRerunning(false); }
  };

  const isRunning = (s: string) => ['running', 'in_progress', 'pending', 'queued'].includes(s);

  // The list endpoint omits stdout/stderr to stay lightweight, so the full
  // execution detail must be fetched when one is selected — and re-polled while
  // it is still running so the output streams in (matching the TUI).
  const [selectedExec, setSelectedExec] = useState<Execution | null>(null);
  useEffect(() => {
    setRerunError(null);
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

  const [railW, railHandle] = useResizableWidth('rail.forge.executions', 260, { min: 200, max: 480 });

  return (
    <div style={{ display: 'flex', flex: 1, overflow: 'hidden' }}>
      {/* Left panel */}
      <div style={{ width: railW, flexShrink: 0, borderRight: `1px solid ${T.border}`, display: 'flex', flexDirection: 'column', background: T.bgAlt }}>
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
          <CreateExecutionModal token={token} runnerClasses={runnerClasses}
            onClose={() => setShowCreate(false)} onSubmit={submitExecution} />
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
                {/* Project tag shown only when unfiltered. */}
                {!project && exec.project && <div style={{ fontSize: 10, color: T.green, marginTop: 2, paddingLeft: 14 }}>◆ {exec.project}</div>}
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
            <span style={{ color: T.green }}>$</span> armory forge{selectedExec ? ` · ${selectedExec.execution_id.slice(0, 8)}…` : ''}
          </div>
          {selectedExec && (
            <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
              {rerunError && <span style={{ fontFamily: T.mono, fontSize: 10, color: T.red }}>{rerunError}</span>}
              <button onClick={() => handleRerun(selectedExec)} disabled={rerunning}
                style={{ background: 'transparent', border: `1px solid ${T.green}`, color: T.green, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: rerunning ? 'default' : 'pointer', opacity: rerunning ? 0.6 : 1 }}>
                {rerunning ? '[ · · · ]' : '[ ↻ rerun ]'}
              </button>
              {isRunning(selectedExec.status) && (
                <button onClick={() => handleCancel(selectedExec.execution_id)}
                  style={{ background: 'transparent', border: `1px solid ${T.red}`, color: T.red, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}>
                  [ ✕ cancel ]
                </button>
              )}
            </div>
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
                {displayCommand(selectedExec.command).split('\n').map((line, i) => (
                  <div key={i} style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>
                    <span style={{ color: T.faint }}>$ </span>{line}
                  </div>
                ))}
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

/** Runner-classes admin tab: lists runner classes, with a create form + per-class enable toggle and delete (all write actions gated by forge:createRunnerClass). The form submits name/memory_mb/cpu_millicores/pids_limit/tmpfs_mb/enabled via createRunnerClass; toggle/delete go through updateRunnerClass/deleteRunnerClass. */
function RunnerClassesTab() {
  const token = useAppSelector(s => s.auth.token)!;
  const canWrite = useAppSelector(s => s.auth.permissions?.['forge:createRunnerClass'] === true);
  const [classes, setClasses] = useState<RunnerClass[]>([]);
  const [backends, setBackends] = useState<RuntimeBackend[]>([]);
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

  useEffect(() => {
    fetchClasses();
    // Best-effort: resolves each class's backend name to its runtime type (kata vs
    // not). A missing list (no permission / older forge) just hides the runtime tag.
    listRuntimeBackends(token).then(setBackends).catch(() => {});
  }, [fetchClasses, token]);

  const selectedClass = classes.find(c => c.name === selected);
  // A class's backend defaults to "default" (the backend seeded from RUNTIME).
  const runtimeOf = (rc: RunnerClass) => backends.find(b => b.name === (rc.backend || 'default'))?.type;
  const selectedRuntime = selectedClass ? runtimeOf(selectedClass) : undefined;

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

  const [confirm, confirmEl] = useConfirm();

  const handleDelete = async (name: string) => {
    if (!(await confirm({ message: `Delete runner class ${name}? Executions configured to use it will fall back to the default.` }))) return;
    try {
      await deleteRunnerClass(token, name);
      setClasses(prev => prev.filter(c => c.name !== name));
      if (selected === name) setSelected(null);
    } catch (e: unknown) { setError((e as Error).message); }
  };

  const [railW, railHandle] = useResizableWidth('rail.forge.runner-classes', 260, { min: 200, max: 480 });

  return (
    <div style={{ display: 'flex', flex: 1, overflow: 'hidden' }}>
      {confirmEl}
      <div style={{ width: railW, flexShrink: 0, borderRight: `1px solid ${T.border}`, display: 'flex', flexDirection: 'column', background: T.bgAlt }}>
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
            const rt = runtimeOf(rc);
            return (
              <button key={rc.name} onClick={() => setSelected(rc.name)}
                style={{ width: '100%', textAlign: 'left', padding: '10px 14px', background: isActive ? T.greenSoft : 'transparent', border: 0, borderLeft: `2px solid ${isActive ? T.green : 'transparent'}`, fontFamily: T.mono, cursor: 'pointer', color: T.text, display: 'block', transition: 'background .12s' }}>
                <div style={{ display: 'flex', alignItems: 'center', gap: 6, flexWrap: 'wrap' }}>
                  <span style={{ fontSize: 13, fontWeight: 600, color: isActive ? T.textHi : T.text }}>{rc.name}</span>
                  {rt && <span style={runtimeBadge(rt)}>{rt}</span>}
                  {rc.privileged && <span style={privBadge}>priv</span>}
                  {!rc.enabled && <span style={{ fontSize: 9, color: T.faint, border: `1px solid ${T.faint}`, padding: '0 4px' }}>disabled</span>}
                </div>
                <div style={{ fontSize: 10, color: T.faint, marginTop: 2 }}>{rc.memory_mb}MB · {rc.cpu_millicores}m</div>
              </button>
            );
          })}
        </div>
      </div>
      {railHandle}

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
                // Runtime (kata vs not) + privileged lead, since they define the isolation
                // model; VM-isolated runtimes show green, a privileged class shows amber.
                ['runtime', selectedRuntime ?? (selectedClass.backend || 'default'),
                  selectedRuntime && VM_ISOLATED.has(selectedRuntime) ? T.green : undefined],
                ['privileged', selectedClass.privileged ? 'yes' : 'no',
                  selectedClass.privileged ? T.amber : undefined],
                ['backend', selectedClass.backend || 'default'],
                ['memory', `${selectedClass.memory_mb} MB`],
                ['cpu', `${selectedClass.cpu_millicores}m`],
                ...(selectedClass.pids_limit != null ? [['pids limit', String(selectedClass.pids_limit)] as [string, string]] : []),
                ...(selectedClass.tmpfs_mb != null ? [['tmpfs', `${selectedClass.tmpfs_mb} MB`] as [string, string]] : []),
                ...(selectedClass.disk_gb != null ? [['disk', `${selectedClass.disk_gb} GB`] as [string, string]] : []),
              ] as [string, string, (string | undefined)?][]).map(([k, v, color]) => (
                <div key={k} style={{ background: T.card, border: `1px solid ${T.border}`, padding: '10px 14px' }}>
                  <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 4, textTransform: 'uppercase' }}>{k}</div>
                  <div style={{ fontFamily: T.mono, fontSize: 15, color: color ?? T.textHi, fontWeight: 700 }}>{v}</div>
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

/** Forge route: tabbed shell switching between the executions and runner-classes tabs. */
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

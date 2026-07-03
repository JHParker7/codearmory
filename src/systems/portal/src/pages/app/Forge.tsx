/**
 * Forge page: sandboxed execution + runner admin. Three tabs — executions (run a
 * one-off image+command and stream stdout/stderr), runner classes (the admin
 * cpu/memory/limits resource classes), and runtime backends (the admin runtime
 * targets: docker / kubernetes / kata / gvisor). Talks to the BFF (listExecutions /
 * createExecution / listRunnerClasses / listRuntimeBackends / …); write actions are
 * gated by the forge:createRunnerClass and forge:createRuntimeBackend permissions.
 */
import { useState, useEffect, useCallback, type CSSProperties } from 'react';
import { T } from '../../theme';
import { useConfirm } from '../../components/ConfirmDialog';
import { useAppSelector } from '../../store/hooks';
import {
  listExecutions, getExecution, cancelExecution, createExecution,
  listRunnerClasses, createRunnerClass, updateRunnerClass, deleteRunnerClass,
  listForgeImages, listRuntimeBackends, createRuntimeBackend, updateRuntimeBackend, deleteRuntimeBackend,
  listGitRepos,
} from '../../api/bff';
import type { Execution, RunnerClass, RuntimeBackend, RuntimeBackendInput, GitRepo, CheckoutSpec } from '../../api/bff';
import { RUNTIME_TYPES, requiresRuntimeClass, parseKVLines, formatKVLines } from './runtimeBackend';
import { ImageSelect } from '../../components/ImageSelect';
import { RepoSelect } from '../../components/RepoSelect';
import { BranchSelect } from '../../components/BranchSelect';
import { useResizableWidth } from '../../components/ResizeHandle';
import { timeAgo } from '../../utils';

type ForgeTab = 'executions' | 'runner-classes' | 'runtime-backends';

// Kernel-isolated backends (kata microVMs, gVisor's userspace kernel) put the
// isolation boundary outside the container — the only place `privileged` is
// meaningful. Used to tint the runtime badge so such a class stands out at a glance.
const KERNEL_ISOLATED = new Set(['kata', 'gvisor']);

/** Badge style for a runner class's runtime type — kernel-isolated types stand out green. */
function runtimeBadge(type: string): CSSProperties {
  const c = KERNEL_ISOLATED.has(type) ? T.green : T.dim;
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
type ExecutionInput = { image: string; command: string[]; env?: Record<string, string>; timeout?: number; runner_class?: string; secret_refs?: Record<string, string>; checkout?: CheckoutSpec };

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
  // Checkout: when on, forge clones the selected repo into the working dir and cd's
  // into it before the command runs (actions/checkout-style). Only meaningful with a
  // repo selected. checkoutPath overrides the clone dir (blank = derived repo name);
  // checkoutRef picks the branch/tag to clone (blank = the remote's default branch).
  const [checkout, setCheckout] = useState(false);
  const [checkoutPath, setCheckoutPath] = useState('');
  const [checkoutRef, setCheckoutRef] = useState('');
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
        // With checkout on, forge clones $GIT_CLONE_URL into the working dir and
        // cd's in before running the command — the command starts inside the repo.
        // A branch/tag ref, when set, becomes `git clone --branch`.
        checkout: repo && checkout ? {
          ...(checkoutPath.trim() ? { path: checkoutPath.trim() } : {}),
          ...(checkoutRef.trim() ? { ref: checkoutRef.trim() } : {}),
        } : undefined,
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
          <div style={{ marginBottom: repoUrl.trim() ? 10 : 18 }}>
            <RepoSelect value={repoUrl} onChange={setRepoUrl} repos={repos} />
          </div>

          {/* Auto-checkout is only meaningful once a repo is selected. */}
          {repoUrl.trim() && (
            <div style={{ marginBottom: 18 }}>
              <label style={{ display: 'flex', alignItems: 'center', gap: 8, cursor: 'pointer', fontFamily: T.mono, fontSize: 12, color: T.dim }}>
                <input type="checkbox" checked={checkout} onChange={e => setCheckout(e.target.checked)} />
                check out into working dir <span style={{ color: T.faint, opacity: 0.7 }}>(git clone + cd before the command, like actions/checkout)</span>
              </label>
              {checkout && (
                <>
                  <div style={{ marginTop: 8 }}>
                    <div style={label}>BRANCH <span style={{ color: T.faint, opacity: 0.7 }}>(optional · defaults to the remote's default branch)</span></div>
                    <BranchSelect token={token} repoUrl={repoUrl} value={checkoutRef} onChange={setCheckoutRef} />
                  </div>
                  <div style={{ marginTop: 8 }}>
                    <div style={label}>CLONE DIR <span style={{ color: T.faint, opacity: 0.7 }}>(optional · defaults to the repo name)</span></div>
                    <input value={checkoutPath} onChange={e => setCheckoutPath(e.target.value)} placeholder="repo" style={field} />
                  </div>
                </>
              )}
            </div>
          )}

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
                  selectedRuntime && KERNEL_ISOLATED.has(selectedRuntime) ? T.green : undefined],
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

// ── Runtime Backends tab ──────────────────────────────────────────────────────

/**
 * Create/edit form for a runtime backend, reused for both (keyed on the selected
 * name so it remounts fresh per selection). Name is immutable on edit (it's the
 * primary key / PUT path). type/enabled/config/secret_refs are always sent whole —
 * forge re-validates on every write. config/secret_refs are `key=value`-per-line;
 * kata/gvisor require a `runtime_class` config key (checked here for a fast error,
 * authoritatively enforced server-side). onSave rejects on failure so we stay open.
 */
function BackendForm({ existing, onSave, onCancel, onDelete }: {
  existing?: RuntimeBackend;
  onSave: (input: RuntimeBackendInput) => Promise<void>;
  onCancel: () => void;
  onDelete?: () => void;
}) {
  const isEdit = !!existing;
  const [name, setName] = useState(existing?.name ?? '');
  const [type, setType] = useState<string>(existing?.type ?? 'docker');
  const [enabled, setEnabled] = useState(existing?.enabled ?? true);
  const [configStr, setConfigStr] = useState(formatKVLines(existing?.config));
  const [secretsStr, setSecretsStr] = useState(formatKVLines(existing?.secret_refs));
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const handleSave = async () => {
    if (!name.trim()) { setError('name is required'); return; }
    let config: Record<string, string>;
    let secret_refs: Record<string, string>;
    try { config = parseKVLines(configStr); } catch (e) { setError(`config: ${(e as Error).message}`); return; }
    try { secret_refs = parseKVLines(secretsStr); } catch (e) { setError(`secret_refs: ${(e as Error).message}`); return; }
    if (requiresRuntimeClass(type) && !config['runtime_class']) {
      setError(`${type} requires a config runtime_class (the Kubernetes RuntimeClass, e.g. kata-qemu)`);
      return;
    }
    setSaving(true); setError(null);
    try { await onSave({ name: name.trim(), type, enabled, config, secret_refs }); }
    catch (e) { setError((e as Error).message); }
    finally { setSaving(false); }
  };

  const label = { fontFamily: T.mono, fontSize: 10, color: T.faint, marginBottom: 4, letterSpacing: 0.5, textTransform: 'uppercase' } as const;
  const field = { width: '100%', background: T.cardHi, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 12, padding: '7px 10px', outline: 'none', boxSizing: 'border-box' } as const;

  return (
    <div style={{ padding: '20px 24px', maxWidth: 620 }}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 18 }}>
        <div style={{ fontFamily: T.mono, fontSize: 16, fontWeight: 700, color: T.textHi }}>
          {isEdit ? existing!.name : 'new runtime backend'}
        </div>
        {isEdit && onDelete && (
          <button onClick={onDelete}
            style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}
            onMouseEnter={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.red; (e.currentTarget as HTMLButtonElement).style.color = T.red; }}
            onMouseLeave={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.border; (e.currentTarget as HTMLButtonElement).style.color = T.dim; }}>
            [ delete ]
          </button>
        )}
      </div>

      {error && <div style={{ color: T.red, fontFamily: T.mono, fontSize: 11, marginBottom: 12 }}>{error}</div>}

      <div style={label}>name</div>
      <input value={name} onChange={e => setName(e.target.value)} disabled={isEdit} autoFocus={!isEdit}
        placeholder="e.g. kata-prod"
        style={{ ...field, marginBottom: 14, opacity: isEdit ? 0.6 : 1 }} />

      <div style={{ display: 'flex', gap: 12, marginBottom: 14 }}>
        <div style={{ flex: 1 }}>
          <div style={label}>type</div>
          <select value={type} onChange={e => setType(e.target.value)} style={field}>
            {RUNTIME_TYPES.map(t => <option key={t} value={t}>{t}</option>)}
          </select>
        </div>
        <div style={{ flex: 1 }}>
          <div style={label}>enabled</div>
          <button onClick={() => setEnabled(v => !v)}
            style={{ ...field, textAlign: 'left', cursor: 'pointer', background: enabled ? T.greenSoft : T.cardHi, color: enabled ? T.green : T.dim, borderColor: enabled ? T.green : T.border }}>
            {enabled ? '● enabled' : '○ disabled'}
          </button>
        </div>
      </div>

      <div style={label}>config <span style={{ color: T.faint, opacity: 0.7, textTransform: 'none' }}>(key=value per line{requiresRuntimeClass(type) ? ` · ${type} needs runtime_class=…` : ''})</span></div>
      <textarea value={configStr} onChange={e => setConfigStr(e.target.value)} rows={3}
        placeholder={requiresRuntimeClass(type) ? 'runtime_class=kata-qemu' : 'docker/kubernetes read their settings from env'}
        style={{ ...field, resize: 'vertical', lineHeight: 1.5, marginBottom: 14 }} />

      <div style={label}>secret refs <span style={{ color: T.faint, opacity: 0.7, textTransform: 'none' }}>(logical=ENV_VAR_NAME per line · never a secret value)</span></div>
      <textarea value={secretsStr} onChange={e => setSecretsStr(e.target.value)} rows={2}
        placeholder="token=SOME_ENV_VAR_NAME"
        style={{ ...field, resize: 'vertical', lineHeight: 1.5, marginBottom: 18 }} />

      <div style={{ display: 'flex', gap: 10 }}>
        <button onClick={handleSave} disabled={saving || !name.trim()}
          style={{ background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 12, fontWeight: 600, padding: '8px 20px', cursor: (saving || !name.trim()) ? 'default' : 'pointer', letterSpacing: 0.4, opacity: (saving || !name.trim()) ? 0.6 : 1 }}>
          {saving ? '[ · · · ]' : isEdit ? '[ save ]' : '[ create ]'}
        </button>
        <button onClick={onCancel}
          style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 12, padding: '8px 16px', cursor: 'pointer' }}>
          cancel
        </button>
      </div>
    </div>
  );
}

/**
 * Runtime-backends admin tab: lists the admin-managed runtime targets (docker /
 * kubernetes / kata / gvisor) and, for holders of forge:createRuntimeBackend,
 * a create/edit form + delete. The "default" backend cannot be deleted (forge
 * 409s; surfaced inline). Read (list) is granted to everyone by default, so the
 * tab always renders; only write affordances are gated.
 */
function RuntimeBackendsTab() {
  const token = useAppSelector(s => s.auth.token)!;
  const canWrite = useAppSelector(s => s.auth.permissions?.['forge:createRuntimeBackend'] === true);
  const [backends, setBackends] = useState<RuntimeBackend[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);
  const [confirm, confirmEl] = useConfirm();

  const fetchBackends = useCallback(async () => {
    setLoading(true); setError(null);
    try { setBackends(await listRuntimeBackends(token)); }
    catch (e: unknown) { setError((e as Error).message); }
    finally { setLoading(false); }
  }, [token]);

  useEffect(() => { fetchBackends(); }, [fetchBackends]);

  const selectedBackend = backends.find(b => b.name === selected);

  const handleCreate = async (input: RuntimeBackendInput) => {
    const created = await createRuntimeBackend(token, input);
    setBackends(prev => [created, ...prev.filter(b => b.name !== created.name)]);
    setCreating(false);
    setSelected(created.name);
  };

  const handleEdit = async (input: RuntimeBackendInput) => {
    const { name, ...rest } = input;
    const updated = await updateRuntimeBackend(token, name, rest);
    setBackends(prev => prev.map(b => b.name === name ? updated : b));
  };

  const handleDelete = async (name: string) => {
    if (!(await confirm({ message: `Delete runtime backend ${name}? Runner classes targeting it will fail to launch until repointed to another backend.` }))) return;
    try {
      await deleteRuntimeBackend(token, name);
      setBackends(prev => prev.filter(b => b.name !== name));
      if (selected === name) setSelected(null);
    } catch (e: unknown) { setError((e as Error).message); }
  };

  const [railW, railHandle] = useResizableWidth('rail.forge.runtime-backends', 260, { min: 200, max: 480 });

  return (
    <div style={{ display: 'flex', flex: 1, overflow: 'hidden' }}>
      {confirmEl}
      <div style={{ width: railW, flexShrink: 0, borderRight: `1px solid ${T.border}`, display: 'flex', flexDirection: 'column', background: T.bgAlt }}>
        <div style={{ padding: '14px 14px 10px', borderBottom: `1px solid ${T.border}` }}>
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 4 }}>
            <span style={{ fontFamily: T.mono, fontSize: 12, fontWeight: 700, color: T.textHi }}>runtime backends</span>
            <div style={{ display: 'flex', gap: 6 }}>
              {canWrite && <button onClick={() => { setCreating(true); setSelected(null); }}
                style={{ background: creating ? T.greenSoft : 'transparent', border: `1px solid ${creating ? T.green : T.border}`, color: creating ? T.green : T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 7px', cursor: 'pointer' }}>+</button>}
              <button onClick={fetchBackends} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 7px', cursor: 'pointer' }}>↻</button>
            </div>
          </div>
        </div>

        <div style={{ flex: 1, overflow: 'auto' }}>
          {loading ? (
            <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint, animation: 'pulse 1s ease-in-out infinite' }}>→ loading · · ·</div>
          ) : error && backends.length === 0 ? (
            <div style={{ padding: '14px', fontFamily: T.mono, fontSize: 11, color: T.red }}>{error}</div>
          ) : backends.length === 0 ? (
            <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint }}>→ no runtime backends</div>
          ) : backends.map(b => {
            const isActive = selected === b.name && !creating;
            return (
              <button key={b.name} onClick={() => { setSelected(b.name); setCreating(false); }}
                style={{ width: '100%', textAlign: 'left', padding: '10px 14px', background: isActive ? T.greenSoft : 'transparent', border: 0, borderLeft: `2px solid ${isActive ? T.green : 'transparent'}`, fontFamily: T.mono, cursor: 'pointer', color: T.text, display: 'block', transition: 'background .12s' }}>
                <div style={{ display: 'flex', alignItems: 'center', gap: 6, flexWrap: 'wrap' }}>
                  <span style={{ fontSize: 13, fontWeight: 600, color: isActive ? T.textHi : T.text }}>{b.name}</span>
                  <span style={runtimeBadge(b.type)}>{b.type}</span>
                  {b.name === 'default' && <span style={{ fontSize: 9, color: T.faint, border: `1px solid ${T.faint}`, padding: '0 4px' }}>default</span>}
                  {!b.enabled && <span style={{ fontSize: 9, color: T.faint, border: `1px solid ${T.faint}`, padding: '0 4px' }}>disabled</span>}
                </div>
              </button>
            );
          })}
        </div>
      </div>
      {railHandle}

      <div style={{ flex: 1, overflow: 'auto' }}>
        {error && backends.length > 0 && <div style={{ padding: '10px 24px 0', fontFamily: T.mono, fontSize: 11, color: T.red }}>{error}</div>}
        {creating && canWrite ? (
          <BackendForm key="__new__" onSave={handleCreate} onCancel={() => setCreating(false)} />
        ) : !selectedBackend ? (
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', height: '100%' }}>
            <div style={{ fontFamily: T.mono, fontSize: 12, color: T.faint }}>→ select a runtime backend</div>
          </div>
        ) : canWrite ? (
          <BackendForm key={selectedBackend.name} existing={selectedBackend}
            onSave={handleEdit} onCancel={() => setSelected(null)} onDelete={() => handleDelete(selectedBackend.name)} />
        ) : (
          <div style={{ padding: '20px 24px' }}>
            <div style={{ fontFamily: T.mono, fontSize: 20, fontWeight: 700, color: T.textHi, marginBottom: 20 }}>{selectedBackend.name}</div>
            <div style={{ display: 'grid', gridTemplateColumns: 'repeat(2, 1fr)', gap: 12 }}>
              {([
                ['type', selectedBackend.type, KERNEL_ISOLATED.has(selectedBackend.type) ? T.green : undefined],
                ['enabled', selectedBackend.enabled ? 'yes' : 'no', selectedBackend.enabled ? undefined : T.faint],
                ['config', formatKVLines(selectedBackend.config) || '—'],
                ['secret refs', formatKVLines(selectedBackend.secret_refs) || '—'],
              ] as [string, string, (string | undefined)?][]).map(([k, v, color]) => (
                <div key={k} style={{ background: T.card, border: `1px solid ${T.border}`, padding: '10px 14px' }}>
                  <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 4, textTransform: 'uppercase' }}>{k}</div>
                  <div style={{ fontFamily: T.mono, fontSize: 14, color: color ?? T.textHi, fontWeight: 700, whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>{v}</div>
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

/** Forge route: tabbed shell switching between the executions, runner-classes and runtime-backends tabs. */
export function Forge() {
  const [tab, setTab] = useState<ForgeTab>('executions');

  return (
    <div style={{ display: 'flex', height: '100%', overflow: 'hidden', flexDirection: 'column' }}>
      <div style={{ display: 'flex', borderBottom: `1px solid ${T.border}`, background: T.bgAlt, flexShrink: 0 }}>
        {(['executions', 'runner-classes', 'runtime-backends'] as ForgeTab[]).map(t => (
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
        {tab === 'runtime-backends' && <RuntimeBackendsTab />}
      </div>
    </div>
  );
}

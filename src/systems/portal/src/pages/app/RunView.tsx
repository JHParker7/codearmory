/**
 * Run page — a live view of a single pipeline run: the pipeline laid out as
 * stages with each step's status (the running step highlighted), beside a logs
 * panel showing the selected step's captured output. While the run is in flight
 * it re-polls so statuses, durations, and logs update on their own.
 *
 * Reached at /app/workflows/runs/:runId (clicking a run in the Workflows page).
 */
import { useCallback, useEffect, useMemo, useState } from 'react';
import { useParams, useNavigate } from 'react-router-dom';
import { T } from '../../theme';
import { Pill } from '../../components/Pill';
import { useAppSelector } from '../../store/hooks';
import { getRun, getWorkflow, listSteps, cancelRun, approveRun, rejectRun } from '../../api/bff';
import type { WorkflowRun, Workflow, WorkflowStepRun, Step } from '../../api/bff';
import { statusTone, isRunActive, fmtDuration, timeAgo } from '../../utils';
import { useViewport, clamp } from '../../hooks/useViewport';

/** Solid accent colour for a status (used for a step's left bar). */
function statusColor(status: string): string {
  const t = statusTone(status);
  return t === 'green' ? T.green : t === 'amber' ? T.amber : t === 'red' ? T.red : T.dim;
}

/** One step in the run, resolved from the workflow structure + its step run. */
interface RunStep {
  index: number;
  label: string;
  sr?: WorkflowStepRun;
}

/** Group the run's steps into sequential stages of parallel steps. Uses the
 * workflow's parallel_group structure when available, else the flat step-run
 * order (a deleted workflow still shows its run as a sequence). */
function buildStages(workflow: Workflow | null, stepRuns: WorkflowStepRun[], catalog: Record<string, Step>): RunStep[][] {
  const byIndex = new Map<number, WorkflowStepRun>();
  stepRuns.forEach((sr) => byIndex.set(sr.step_index, sr));

  if (workflow && workflow.steps.length > 0) {
    const stages: RunStep[][] = [];
    let prev: number | null | undefined = undefined;
    workflow.steps.forEach((s, i) => {
      const g = s.parallel_group ?? null;
      const sr = byIndex.get(i);
      const label = s.approval
        ? (s.name || 'approval gate')
        : (s.name ?? catalog[s.step_id ?? '']?.name ?? sr?.step_name ?? (s.step_id ?? '').slice(0, 8) + '…');
      const step: RunStep = { index: i, label, sr };
      if (g !== null && g === prev) stages[stages.length - 1].push(step);
      else stages.push([step]);
      prev = g;
    });
    return stages;
  }

  // Fallback: one stage per recorded step run, in index order.
  return stepRuns
    .slice()
    .sort((a, b) => a.step_index - b.step_index)
    .map((sr) => [{ index: sr.step_index, label: sr.step_name, sr }]);
}

export function RunView() {
  const { runId = '' } = useParams();
  const navigate = useNavigate();
  const token = useAppSelector((s) => s.auth.token)!;
  const { width } = useViewport();
  // Below ~1000px the page splits vertically (pipeline over logs); above it, the
  // pipeline panel scales with the viewport rather than a fixed width.
  const narrow = width < 1000;
  const pipelineW = clamp(Math.round(width * 0.30), 320, 560);

  const [run, setRun] = useState<WorkflowRun | null>(null);
  const [workflow, setWorkflow] = useState<Workflow | null>(null);
  const [catalog, setCatalog] = useState<Record<string, Step>>({});
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<number | null>(null);
  const [deciding, setDeciding] = useState(false);
  // Approve/reject failures show inline beside the gate controls rather than
  // replacing the whole run view (which `error` does for a failed load).
  const [decideErr, setDecideErr] = useState<string | null>(null);

  // Initial load: the run, its workflow (best-effort — may be deleted), and the
  // step catalog for labels.
  useEffect(() => {
    let cancelled = false;
    (async () => {
      setLoading(true); setError(null);
      try {
        const r = await getRun(token, runId);
        if (cancelled) return;
        setRun(r);
        getWorkflow(token, r.workflow_id).then((w) => { if (!cancelled) setWorkflow(w); }).catch(() => {});
        listSteps(token).then((steps) => {
          if (!cancelled) setCatalog(Object.fromEntries(steps.map((s) => [s.step_id, s])));
        }).catch(() => {});
      } catch (e: unknown) {
        if (!cancelled) setError((e as Error).message);
      } finally {
        if (!cancelled) setLoading(false);
      }
    })();
    return () => { cancelled = true; };
  }, [token, runId]);

  // Live-refresh while the run is in flight.
  const live = !!run && isRunActive(run.status);
  useEffect(() => {
    if (!live) return;
    const id = setInterval(() => {
      getRun(token, runId).then(setRun).catch(() => {});
    }, 2500);
    return () => clearInterval(id);
  }, [live, token, runId]);

  const stepRuns = useMemo(() => run?.step_runs ?? [], [run]);
  const stages = useMemo(() => buildStages(workflow, stepRuns, catalog), [workflow, stepRuns, catalog]);

  // Default the logs panel to the running step (or the last step with output).
  useEffect(() => {
    if (selected !== null) return;
    const active = stepRuns.find((sr) => isRunActive(sr.status));
    const withOut = [...stepRuns].reverse().find((sr) => sr.output);
    const pick = active ?? withOut ?? stepRuns[stepRuns.length - 1];
    if (pick) setSelected(pick.step_index);
  }, [stepRuns, selected]);

  const selectedSr = useMemo(() => stepRuns.find((sr) => sr.step_index === selected) ?? null, [stepRuns, selected]);

  const handleCancel = useCallback(async () => {
    if (!run) return;
    try { await cancelRun(token, run.run_id); setRun({ ...run, status: 'cancelled' }); }
    catch (e: unknown) { setError((e as Error).message); }
  }, [run, token]);

  // Resume a run paused on a manual-approval gate. The response carries the new
  // status; live-refresh then continues to stream the resumed steps.
  const handleApprove = useCallback(async () => {
    if (!run) return;
    setDeciding(true); setDecideErr(null);
    try { setRun(await approveRun(token, run.run_id)); }
    catch (e: unknown) { setDecideErr((e as Error).message); }
    finally { setDeciding(false); }
  }, [run, token]);

  // Reject fails the run, so prompt for an optional reason (recorded in the gate's
  // audit line). A cancelled prompt aborts the decision.
  const handleReject = useCallback(async () => {
    if (!run) return;
    const comment = window.prompt('Reason for rejecting this run? (optional)');
    if (comment === null) return;
    setDeciding(true); setDecideErr(null);
    try { setRun(await rejectRun(token, run.run_id, comment || undefined)); }
    catch (e: unknown) { setDecideErr((e as Error).message); }
    finally { setDeciding(false); }
  }, [run, token]);

  const backBtn = (
    <button onClick={() => navigate('/app/workflows')}
      style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 10px', cursor: 'pointer' }}>
      ← runs
    </button>
  );

  if (loading) return <div style={{ padding: 24, fontFamily: T.mono, fontSize: 12, color: T.faint, animation: 'pulse 1s ease-in-out infinite' }}>→ loading run · · ·</div>;
  if (error || !run) return (
    <div style={{ padding: 24 }}>
      <div style={{ marginBottom: 12 }}>{backBtn}</div>
      <div style={{ fontFamily: T.mono, fontSize: 12, color: T.red }}>{error ?? 'run not found'}</div>
    </div>
  );

  return (
    <div style={{ display: 'flex', flexDirection: 'column', flex: 1, overflow: 'hidden' }}>
      {/* Header */}
      <div style={{ padding: '12px 20px', borderBottom: `1px solid ${T.border}`, background: T.bgAlt, display: 'flex', alignItems: 'center', gap: 12, flexWrap: 'wrap' }}>
        {backBtn}
        <span style={{ fontFamily: T.mono, fontSize: 13, fontWeight: 700, color: T.textHi }}>{workflow?.name ?? 'pipeline run'}</span>
        <Pill tone={statusTone(run.status)}>{run.status}</Pill>
        <span style={{ fontFamily: T.mono, fontSize: 10, color: T.faint }}>
          {run.started_at ? `${run.ended_at ? 'took' : 'elapsed'} ${fmtDuration(run.started_at, run.ended_at)} · ` : ''}
          by user {run.triggered_by ? run.triggered_by.slice(0, 8) + '…' : '—'}
          {run.started_at ? ` · started ${timeAgo(run.started_at)} ago` : ''}
        </span>
        <div style={{ flex: 1 }} />
        {isRunActive(run.status) && (
          <button onClick={handleCancel}
            style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}>
            [ cancel ]
          </button>
        )}
      </div>

      <div style={{ display: 'flex', flexDirection: narrow ? 'column' : 'row', flex: 1, overflow: 'hidden' }}>
        {/* Pipeline with live status */}
        <div style={{
          flexShrink: 0, overflow: 'auto', padding: 16, background: T.bg,
          ...(narrow
            ? { width: '100%', maxHeight: '46vh', borderBottom: `1px solid ${T.border}` }
            : { width: pipelineW, borderRight: `1px solid ${T.border}` }),
        }}>
          <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, textTransform: 'uppercase', marginBottom: 10 }}>pipeline</div>
          {stages.length === 0 ? (
            <div style={{ fontFamily: T.mono, fontSize: 12, color: T.faint }}>→ {isRunActive(run.status) ? 'waiting for the first step…' : 'no steps recorded'}</div>
          ) : stages.map((stage, si) => {
            const parallel = stage.length > 1;
            return (
              <div key={stage[0].index}>
                {si > 0 && (
                  <div style={{ display: 'flex', alignItems: 'center', gap: 6, paddingLeft: 16, height: 16 }}>
                    <span style={{ width: 2, height: '100%', background: parallel ? T.green : T.border }} />
                    <span style={{ fontFamily: T.mono, fontSize: 9, color: parallel ? T.green : T.faint }}>{parallel ? '∥' : '↓'}</span>
                  </div>
                )}
                {parallel && <div style={{ fontFamily: T.mono, fontSize: 9, color: T.green, letterSpacing: 1, textTransform: 'uppercase', padding: '2px 0 4px' }}>∥ parallel</div>}
                <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap', marginBottom: 4 }}>
                  {stage.map((st) => {
                    const status = st.sr?.status ?? 'pending';
                    const active = isRunActive(status);
                    const isSel = st.index === selected;
                    const dur = st.sr?.ended_at ? fmtDuration(st.sr.started_at, st.sr.ended_at) : (st.sr?.started_at && active ? fmtDuration(st.sr.started_at) : '');
                    return (
                      <button key={st.index} onClick={() => setSelected(st.index)}
                        style={{ flex: parallel ? '1 1 150px' : undefined, width: parallel ? undefined : '100%', textAlign: 'left', minWidth: 0,
                          display: 'flex', alignItems: 'center', gap: 8, padding: '8px 10px', cursor: 'pointer',
                          background: isSel ? T.cardHi : T.card, border: `1px solid ${isSel ? T.green : active ? T.amber : T.border}`,
                          borderLeft: `3px solid ${statusColor(status)}`, fontFamily: T.mono,
                          animation: active ? 'pulse 1.4s ease-in-out infinite' : undefined }}>
                        <Pill tone={statusTone(status)}>{status}</Pill>
                        <span style={{ flex: 1, minWidth: 0, fontSize: 12, fontWeight: 600, color: T.textHi, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{st.label}</span>
                        {dur && <span style={{ fontSize: 10, color: active ? T.amber : T.faint }}>{active ? '⟳ ' : ''}{dur}</span>}
                      </button>
                    );
                  })}
                </div>
              </div>
            );
          })}
        </div>

        {/* Logs for the selected step */}
        <div style={{ flex: 1, minWidth: 0, minHeight: 0, display: 'flex', flexDirection: 'column', overflow: 'hidden', background: T.bg }}>
          <div style={{ padding: '10px 16px', borderBottom: `1px solid ${T.border}`, background: T.bgAlt, display: 'flex', alignItems: 'center', gap: 10 }}>
            <span style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, textTransform: 'uppercase' }}>logs</span>
            {selectedSr && <Pill tone={statusTone(selectedSr.status)}>{selectedSr.status}</Pill>}
            <span style={{ fontFamily: T.mono, fontSize: 12, color: T.textHi, fontWeight: 600 }}>{selectedSr?.step_name ?? '—'}</span>
            {selectedSr?.started_at && <span style={{ fontFamily: T.mono, fontSize: 10, color: T.faint }}>{fmtDuration(selectedSr.started_at, selectedSr.ended_at)}</span>}
          </div>
          <div style={{ flex: 1, overflow: 'auto', padding: 16 }}>
            {!selectedSr ? (
              <div style={{ fontFamily: T.mono, fontSize: 12, color: T.faint }}>→ select a step to see its logs</div>
            ) : selectedSr.status === 'awaiting_approval' ? (
              // Approval gate: the decision lives here, in place of step output. The
              // gate's optional prompt (its captured output) sits above the controls.
              <div style={{ display: 'flex', flexDirection: 'column', gap: 14, maxWidth: 640 }}>
                {selectedSr.output && (
                  <pre style={{ margin: 0, fontFamily: T.mono, fontSize: 12, color: T.text, whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>{selectedSr.output}</pre>
                )}
                <div style={{ border: `1px solid ${T.amber}`, background: T.card, padding: 16, display: 'flex', flexDirection: 'column', gap: 12 }}>
                  <div style={{ fontFamily: T.mono, fontSize: 12, fontWeight: 700, color: T.amber }}>⏸ paused for manual approval</div>
                  <div style={{ fontFamily: T.mono, fontSize: 11, color: T.dim, lineHeight: 1.5 }}>
                    Approve to resume the pipeline, or reject to fail this run.
                  </div>
                  <div style={{ display: 'flex', gap: 10 }}>
                    <button onClick={handleApprove} disabled={deciding}
                      style={{ background: 'transparent', border: `1px solid ${T.green}`, color: T.green, fontFamily: T.mono, fontSize: 11, fontWeight: 700, padding: '6px 14px', cursor: deciding ? 'default' : 'pointer', opacity: deciding ? 0.5 : 1 }}>
                      [ ✓ approve ]
                    </button>
                    <button onClick={handleReject} disabled={deciding}
                      style={{ background: 'transparent', border: `1px solid ${T.red}`, color: T.red, fontFamily: T.mono, fontSize: 11, fontWeight: 700, padding: '6px 14px', cursor: deciding ? 'default' : 'pointer', opacity: deciding ? 0.5 : 1 }}>
                      [ ✗ reject ]
                    </button>
                  </div>
                  {decideErr && <div style={{ fontFamily: T.mono, fontSize: 11, color: T.red }}>{decideErr}</div>}
                </div>
              </div>
            ) : selectedSr.output ? (
              <pre style={{ margin: 0, fontFamily: T.mono, fontSize: 12, color: T.text, whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>{selectedSr.output}</pre>
            ) : (
              <div style={{ fontFamily: T.mono, fontSize: 12, color: T.faint, animation: isRunActive(selectedSr.status) ? 'pulse 1.2s ease-in-out infinite' : undefined }}>
                → {isRunActive(selectedSr.status) ? 'running — no output yet · · ·' : 'no output captured'}
              </div>
            )}
          </div>
        </div>
      </div>
    </div>
  );
}

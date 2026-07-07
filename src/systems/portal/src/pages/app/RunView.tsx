/**
 * Run page — a live view of a single pipeline run: the pipeline laid out as
 * stages with each step's status (the running step highlighted), beside a logs
 * panel showing the selected step's captured output. While the run is in flight
 * it re-polls so statuses, durations, and logs update on their own.
 *
 * Reached at /app/workflows/runs/:runId (clicking a run in the Workflows page).
 */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useParams, useNavigate } from 'react-router-dom';
import { T } from '../../theme';
import { Pill } from '../../components/Pill';
import { useResizablePane } from '../../components/ResizeHandle';
import { useAppSelector } from '../../store/hooks';
import { getRun, getWorkflow, listSteps, cancelRun, approveRun, rejectRun } from '../../api/bff';
import type { WorkflowRun, Workflow, WorkflowStepRun, Step, ApprovalGate } from '../../api/bff';
import { statusTone, isRunActive, fmtDuration, timeAgo, shortId } from '../../utils';
import { useUserNames } from '../../hooks/useNames';
import { useViewport } from '../../hooks/useViewport';

/** Solid accent colour for a status (used for a step's left bar). */
function statusColor(status: string): string {
  const t = statusTone(status);
  return t === 'green' ? T.green : t === 'amber' ? T.amber : t === 'red' ? T.red : t === 'blue' ? T.blue : T.dim;
}

/** The decision controls for a run paused on a manual-approval gate, rendered
 * right on the gate's pipeline block so the call to action sits where the eye
 * already is (rather than only in the logs panel). The optional gate message
 * tells the approver what they are signing off on. */
function ApprovalPanel({ gate, deciding, decideErr, onApprove, onReject }: {
  gate?: ApprovalGate | null;
  deciding: boolean;
  decideErr: string | null;
  onApprove: () => void;
  onReject: () => void;
}) {
  return (
    <div style={{ border: `1px solid ${T.blue}`, background: T.blueSoft, padding: '10px 12px', display: 'flex', flexDirection: 'column', gap: 8 }}>
      <div style={{ fontFamily: T.mono, fontSize: 11, fontWeight: 700, color: T.blue, letterSpacing: 0.5 }}>⏸ NEEDS YOUR APPROVAL</div>
      {gate?.message && (
        <div style={{ fontFamily: T.mono, fontSize: 11, color: T.text, lineHeight: 1.5 }}>{gate.message}</div>
      )}
      <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
        <button onClick={onApprove} disabled={deciding}
          style={{ flex: '1 1 90px', background: 'transparent', border: `1px solid ${T.green}`, color: T.green, fontFamily: T.mono, fontSize: 11, fontWeight: 700, padding: '6px 10px', cursor: deciding ? 'default' : 'pointer', opacity: deciding ? 0.5 : 1 }}>
          [ ✓ approve ]
        </button>
        <button onClick={onReject} disabled={deciding}
          style={{ flex: '1 1 90px', background: 'transparent', border: `1px solid ${T.red}`, color: T.red, fontFamily: T.mono, fontSize: 11, fontWeight: 700, padding: '6px 10px', cursor: deciding ? 'default' : 'pointer', opacity: deciding ? 0.5 : 1 }}>
          [ ✗ reject ]
        </button>
      </div>
      {decideErr && <div style={{ fontFamily: T.mono, fontSize: 11, color: T.red }}>{decideErr}</div>}
    </div>
  );
}

/** One block in the run, resolved from the workflow structure + its step run. A
 * matrix step contributes several of these (one per combination). */
interface RunStep {
  /** Unique selection/React key: the step run's id once it has run, else a
   * synthetic per-index key for a step still pending (no step run yet). */
  key: string;
  index: number;
  label: string;
  /** The manual-approval gate config, when this step is a gate (so the block can
   * show what the approver is signing off on). */
  gate?: ApprovalGate | null;
  sr?: WorkflowStepRun;
}

/** A sequential stage of the run. `parallel` marks a real parallel_group of more
 * than one workflow step — not a single step's matrix fan-out, which also yields
 * several blocks (`steps`) but occupies one stage. */
interface RunStage {
  parallel: boolean;
  steps: RunStep[];
}

/** Group the run's steps into sequential stages. Uses the workflow's
 * parallel_group structure when available, else the flat step-run order (a deleted
 * workflow still shows its run as a sequence). A matrix step fans out into several
 * step runs that all share one step_index; each becomes its own block so every
 * combination's status and output is shown — not just one. */
function buildStages(workflow: Workflow | null, stepRuns: WorkflowStepRun[], catalog: Record<string, Step>): RunStage[] {
  const byIndex = new Map<number, WorkflowStepRun[]>();
  stepRuns.forEach((sr) => {
    const arr = byIndex.get(sr.step_index);
    if (arr) arr.push(sr);
    else byIndex.set(sr.step_index, [sr]);
  });

  if (workflow && workflow.steps.length > 0) {
    const stages: RunStage[] = [];
    let prev: number | null | undefined = undefined;
    workflow.steps.forEach((s, i) => {
      const g = s.parallel_group ?? null;
      const runs = byIndex.get(i) ?? [];
      const base = s.approval
        ? (s.name || 'approval gate')
        : (s.name ?? catalog[s.step_id ?? '']?.name ?? runs[0]?.step_name ?? (s.step_id ?? '').slice(0, 8) + '…');
      // One block per matrix combination (labelled by its run's step_name, which
      // carries the `[var=val]` suffix); otherwise a single block for the step. The
      // backend returns fan-out runs in no fixed order within an index, so sort by
      // name for a stable layout across live-poll refreshes.
      const blocks: RunStep[] = runs.length > 1
        ? [...runs]
            .sort((a, b) => a.step_name.localeCompare(b.step_name))
            .map((sr) => ({ key: sr.step_run_id, index: i, gate: s.approval, sr, label: sr.step_name || base }))
        : [{ key: runs[0]?.step_run_id ?? `idx:${i}`, index: i, gate: s.approval, sr: runs[0], label: base }];
      if (g !== null && g === prev) {
        const stage = stages[stages.length - 1];
        stage.steps.push(...blocks);
        stage.parallel = true; // more than one workflow step shares this parallel_group
      } else {
        stages.push({ parallel: false, steps: blocks });
      }
      prev = g;
    });
    return stages;
  }

  // Fallback: one stage per recorded step run, in index order.
  return stepRuns
    .slice()
    .sort((a, b) => a.step_index - b.step_index)
    .map((sr) => ({ parallel: false, steps: [{ key: sr.step_run_id, index: sr.step_index, label: sr.step_name, sr }] }));
}

/** A matrix step's fan-out collapsed into a single block: one status card plus a
 * dropdown to pick which combination's logs to view. Replaces the previous
 * one-card-per-combination layout, which overwhelmed the pipeline column once a
 * matrix fanned out over more than a handful of values. Picking a combination in
 * the dropdown drives the shared logs panel (`setSelected`); the card mirrors the
 * chosen combination's status/duration and, if it is a gate, its approval panel. */
function MatrixBlock({ steps, selected, setSelected, deciding, decideErr, onApprove, onReject }: {
  steps: RunStep[];
  selected: string | null;
  setSelected: (key: string) => void;
  deciding: boolean;
  decideErr: string | null;
  onApprove: () => void;
  onReject: () => void;
}) {
  // The combination the block reflects: the globally-selected one when it belongs
  // to this matrix, otherwise a sensible preview — an awaiting gate first (so it
  // surfaces without a click), then a running one, else the first combination.
  const preferred = steps.find((s) => s.sr?.status === 'awaiting_approval')
    ?? steps.find((s) => isRunActive(s.sr?.status ?? '')) ?? steps[0];
  const shown = steps.find((s) => s.key === selected) ?? preferred;
  const status = shown.sr?.status ?? 'pending';
  const active = isRunActive(status);
  const awaiting = status === 'awaiting_approval';
  const isSel = shown.key === selected;
  const dur = shown.sr?.ended_at
    ? fmtDuration(shown.sr.started_at, shown.sr.ended_at)
    : (shown.sr?.started_at && active ? fmtDuration(shown.sr.started_at) : '');
  const borderColor = isSel ? T.green : awaiting ? T.blue : active ? T.amber : T.border;
  // Failing/running counts so the collapsed matrix still flags trouble at a glance.
  const failed = steps.filter((s) => statusTone(s.sr?.status ?? 'pending') === 'red').length;
  const running = steps.filter((s) => isRunActive(s.sr?.status ?? '') && s.sr?.status !== 'awaiting_approval').length;
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
      <div style={{
        display: 'flex', flexDirection: 'column', gap: 8, padding: '8px 10px',
        background: isSel ? T.cardHi : T.card, border: `1px solid ${borderColor}`,
        borderLeft: `3px solid ${statusColor(status)}`,
      }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap' }}>
          <Pill tone={statusTone(status)}>{status}</Pill>
          {dur && <span style={{ fontFamily: T.mono, fontSize: 10, color: active ? T.amber : T.faint }}>{active ? '⟳ ' : ''}{dur}</span>}
          <span style={{ flex: 1 }} />
          <span style={{ fontFamily: T.mono, fontSize: 10, color: T.faint }}>{steps.length} combinations</span>
          {running > 0 && <span style={{ fontFamily: T.mono, fontSize: 10, color: T.amber }}>{running} running</span>}
          {failed > 0 && <span style={{ fontFamily: T.mono, fontSize: 10, color: T.red }}>{failed} failed</span>}
        </div>
        <select
          title="pick a matrix combination to view its logs"
          value={shown.key}
          onChange={(e) => setSelected(e.target.value)}
          style={{
            width: '100%', minWidth: 0, background: T.bg, color: T.textHi,
            border: `1px solid ${T.border}`, fontFamily: T.mono, fontSize: 12,
            padding: '6px 8px', cursor: 'pointer',
          }}>
          {steps.map((st) => (
            <option key={st.key} value={st.key}>
              {st.label} · {st.sr?.status ?? 'pending'}
            </option>
          ))}
        </select>
      </div>
      {awaiting && (
        <ApprovalPanel gate={shown.gate} deciding={deciding} decideErr={decideErr} onApprove={onApprove} onReject={onReject} />
      )}
    </div>
  );
}

export function RunView() {
  const { runId = '' } = useParams();
  const navigate = useNavigate();
  const token = useAppSelector((s) => s.auth.token)!;
  const userNames = useUserNames(token);
  const { width } = useViewport();
  // Below ~1000px the page splits vertically (pipeline over logs); above it, the
  // panels sit side by side. Either way the split is user-draggable (and persisted)
  // so the pipeline side can be grown or shrunk against the logs.
  const narrow = width < 1000;
  const splitRef = useRef<HTMLDivElement>(null);
  const [pipelineW, widthHandle] = useResizablePane('split.runview.pipeline.w', 400, {
    min: 280, max: 900, side: 'left', direction: 'horizontal', containerRef: splitRef, otherMin: 320,
  });
  const [pipelineH, heightHandle] = useResizablePane('split.runview.pipeline.h', 320, {
    min: 140, max: 1200, side: 'left', direction: 'vertical', containerRef: splitRef, otherMin: 200,
  });

  const [run, setRun] = useState<WorkflowRun | null>(null);
  const [workflow, setWorkflow] = useState<Workflow | null>(null);
  const [catalog, setCatalog] = useState<Record<string, Step>>({});
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  // The selected block's key — a step_run_id (unique per matrix combination), or a
  // synthetic `idx:N` for a still-pending step. Keyed per run, not per step_index,
  // so matrix combinations (which share an index) are individually selectable.
  const [selected, setSelected] = useState<string | null>(null);
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
    const withOut = [...stepRuns].reverse().find((sr) => sr.logs || sr.output);
    const pick = active ?? withOut ?? stepRuns[stepRuns.length - 1];
    if (pick) setSelected(pick.step_run_id);
  }, [stepRuns, selected]);

  const selectedSr = useMemo(() => stepRuns.find((sr) => sr.step_run_id === selected) ?? null, [stepRuns, selected]);

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
          by {run.triggered_by ? (userNames[run.triggered_by] ?? shortId(run.triggered_by)) : '—'}
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

      {/* Captured outputs: the pipeline-level outputs resolved at completion. Shown
          above the panels so a finished run's published values read at a glance. */}
      {run.outputs && Object.keys(run.outputs).length > 0 && (
        <div style={{ padding: '10px 20px', borderBottom: `1px solid ${T.border}`, background: T.bg }}>
          <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, textTransform: 'uppercase', marginBottom: 6 }}>captured outputs</div>
          <div style={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
            {Object.entries(run.outputs).map(([k, v]) => (
              <div key={k} style={{ display: 'flex', gap: 8, alignItems: 'baseline', flexWrap: 'wrap' }}>
                <span style={{ fontFamily: T.mono, fontSize: 11, color: T.green, fontWeight: 600 }}>{k}</span>
                <span style={{ fontFamily: T.mono, fontSize: 11, color: T.text, wordBreak: 'break-word' }}>{v}</span>
              </div>
            ))}
          </div>
        </div>
      )}

      <div ref={splitRef} style={{ display: 'flex', flexDirection: narrow ? 'column' : 'row', flex: 1, overflow: 'hidden' }}>
        {/* Pipeline with live status — its size (width when side-by-side, height when
            stacked) is the draggable side of the split; the divider follows. */}
        <div style={{
          flexShrink: 0, overflow: 'auto', padding: 16, background: T.bg,
          ...(narrow ? { width: '100%', height: pipelineH } : { width: pipelineW }),
        }}>
          <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, textTransform: 'uppercase', marginBottom: 10 }}>pipeline</div>
          {stages.length === 0 ? (
            <div style={{ fontFamily: T.mono, fontSize: 12, color: T.faint }}>→ {isRunActive(run.status) ? 'waiting for the first step…' : 'no steps recorded'}</div>
          ) : stages.map((stage, si) => {
            const parallel = stage.parallel;
            // A non-parallel stage with several blocks is a single step's matrix
            // fan-out (each combination its own block).
            const matrix = !parallel && stage.steps.length > 1;
            return (
              <div key={stage.steps[0].key}>
                {si > 0 && (
                  <div style={{ display: 'flex', alignItems: 'center', gap: 6, paddingLeft: 16, height: 16 }}>
                    <span style={{ width: 2, height: '100%', background: parallel ? T.green : T.border }} />
                    <span style={{ fontFamily: T.mono, fontSize: 9, color: parallel ? T.green : T.faint }}>{parallel ? '∥' : '↓'}</span>
                  </div>
                )}
                {parallel && <div style={{ fontFamily: T.mono, fontSize: 9, color: T.green, letterSpacing: 1, textTransform: 'uppercase', padding: '2px 0 4px' }}>∥ parallel</div>}
                {matrix && <div style={{ fontFamily: T.mono, fontSize: 9, color: T.amber, letterSpacing: 1, textTransform: 'uppercase', padding: '2px 0 4px' }}>⊞ matrix</div>}
                {matrix ? (
                  <div style={{ marginBottom: 4 }}>
                    <MatrixBlock steps={stage.steps} selected={selected} setSelected={setSelected}
                      deciding={deciding} decideErr={decideErr} onApprove={handleApprove} onReject={handleReject} />
                  </div>
                ) : (
                <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap', marginBottom: 4 }}>
                  {stage.steps.map((st) => {
                    const status = st.sr?.status ?? 'pending';
                    const active = isRunActive(status);
                    const awaiting = status === 'awaiting_approval';
                    const isSel = st.key === selected;
                    const dur = st.sr?.ended_at ? fmtDuration(st.sr.started_at, st.sr.ended_at) : (st.sr?.started_at && active ? fmtDuration(st.sr.started_at) : '');
                    // A gate waiting on a human gets a blue border so it reads apart
                    // from the amber of a step that is merely running.
                    const borderColor = isSel ? T.green : awaiting ? T.blue : active ? T.amber : T.border;
                    // Parallel siblings wrap as cards; a lone sequential step spans
                    // the full width. (Matrix fan-outs take the dropdown branch above.)
                    const wrap = parallel;
                    return (
                      <div key={st.key} style={{ flex: wrap ? '1 1 150px' : undefined, width: wrap ? undefined : '100%', minWidth: 0, display: 'flex', flexDirection: 'column', gap: 6 }}>
                        <button onClick={() => setSelected(st.key)}
                          style={{ width: '100%', textAlign: 'left', minWidth: 0,
                            display: 'flex', alignItems: 'center', gap: 8, padding: '8px 10px', cursor: 'pointer',
                            background: isSel ? T.cardHi : T.card, border: `1px solid ${borderColor}`,
                            borderLeft: `3px solid ${statusColor(status)}`, fontFamily: T.mono,
                            // The gate draws the eye with colour + its action panel, so
                            // skip the pulse there; keep it for steps that are running.
                            animation: active && !awaiting ? 'pulse 1.4s ease-in-out infinite' : undefined }}>
                          <Pill tone={statusTone(status)}>{status}</Pill>
                          <span style={{ flex: 1, minWidth: 0, fontSize: 12, fontWeight: 600, color: T.textHi, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{st.label}</span>
                          {dur && <span style={{ fontSize: 10, color: active ? T.amber : T.faint }}>{active ? '⟳ ' : ''}{dur}</span>}
                        </button>
                        {awaiting && (
                          <ApprovalPanel gate={st.gate} deciding={deciding} decideErr={decideErr} onApprove={handleApprove} onReject={handleReject} />
                        )}
                      </div>
                    );
                  })}
                </div>
                )}
              </div>
            );
          })}
        </div>

        {/* Drag to rebalance the pipeline side against the logs side. */}
        {narrow ? heightHandle : widthHandle}

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
              // Approval gate: the decision controls now sit on the gate's pipeline
              // block (left), so here we just surface its prompt/output for context
              // and point the approver at the block.
              <div style={{ display: 'flex', flexDirection: 'column', gap: 14, maxWidth: 640 }}>
                <div style={{ border: `1px solid ${T.blue}`, background: T.blueSoft, padding: 16, display: 'flex', flexDirection: 'column', gap: 6 }}>
                  <div style={{ fontFamily: T.mono, fontSize: 12, fontWeight: 700, color: T.blue }}>⏸ paused for manual approval</div>
                  <div style={{ fontFamily: T.mono, fontSize: 11, color: T.dim, lineHeight: 1.5 }}>
                    Approve or reject on the highlighted gate step in the pipeline at left.
                  </div>
                </div>
                {selectedSr.output && (
                  <pre style={{ margin: 0, fontFamily: T.mono, fontSize: 12, color: T.text, whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>{selectedSr.output}</pre>
                )}
              </div>
            ) : (selectedSr.logs || selectedSr.output) ? (
              // Two distinct things: `logs` is the command's stdout (captured on
              // success for viewing), `output` is the consumable captured outputs
              // (output_env map) — or, on a failed step, the failure detail. Show
              // both, skipping `output` when it just repeats the logs.
              <div style={{ display: 'flex', flexDirection: 'column', gap: 16 }}>
                {selectedSr.logs && (
                  <pre style={{ margin: 0, fontFamily: T.mono, fontSize: 12, color: T.text, whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>{selectedSr.logs}</pre>
                )}
                {selectedSr.output && selectedSr.output !== selectedSr.logs && (
                  <div style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
                    <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, textTransform: 'uppercase' }}>
                      {selectedSr.status === 'completed' ? 'captured outputs' : 'failure detail'}
                    </div>
                    <pre style={{ margin: 0, fontFamily: T.mono, fontSize: 12, color: T.text, whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>{selectedSr.output}</pre>
                  </div>
                )}
              </div>
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

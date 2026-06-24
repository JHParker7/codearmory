/**
 * Chaos page — chaos-experiment runner: lists experiments with their status and
 * verdict (live-polling while any are in flight), and launches new ones against
 * a chaos-enabled outpost's target workload. experiments run via the outpost;
 * data via the bff.
 */
import { useState, useEffect, useCallback } from 'react';
import { T } from '../../theme';
import { useAppSelector } from '../../store/hooks';
import {
  listExperiments, createExperiment, deleteExperiment, listExperimentTypes,
  listOutposts,
  type Experiment, type ExperimentType, type Outpost,
} from '../../api/bff';
import { timeAgo } from '../../utils';

/** Map an experiment status to a UI tone (Pass→green, pending/running→amber, Fail/Error→red, else dim). */
function statusTone(status: string): 'green' | 'amber' | 'red' | 'dim' {
  if (status === 'Pass') return 'green';
  if (status === 'pending' || status === 'running') return 'amber';
  if (status === 'Fail' || status === 'Error') return 'red';
  return 'dim';
}
const toneColor: Record<string, string> = { green: T.green, amber: T.amber, red: T.red, dim: T.faint };

/** Small status dot colored by tone; pulses while in flight (amber). */
function Dot({ status }: { status: string }) {
  const tone = statusTone(status);
  return <span style={{ display: 'inline-block', width: 7, height: 7, borderRadius: '50%', marginRight: 8, background: toneColor[tone], animation: tone === 'amber' ? 'pulse 1.5s ease-in-out infinite' : undefined }} />;
}

/** Chaos experiment runner: lists experiments (live-polling in-flight ones), launches new ones, and stops/deletes them. */
export function Chaos() {
  const token = useAppSelector(s => s.auth.token)!;
  const [experiments, setExperiments] = useState<Experiment[]>([]);
  const [types, setTypes] = useState<ExperimentType[]>([]);
  const [outposts, setOutposts] = useState<Outpost[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [showCreate, setShowCreate] = useState(false);

  const fetchAll = useCallback(async () => {
    setError(null);
    try {
      const [exps, ts, ops] = await Promise.all([
        listExperiments(token),
        listExperimentTypes(token).catch(() => [] as ExperimentType[]),
        listOutposts(token).catch(() => [] as Outpost[]),
      ]);
      setExperiments(exps); setTypes(ts); setOutposts(ops);
    } catch (e: unknown) { setError((e as Error).message); }
    finally { setLoading(false); }
  }, [token]);

  useEffect(() => { fetchAll(); }, [fetchAll]);

  // Poll while any experiment is in flight so verdicts surface live.
  useEffect(() => {
    const inFlight = experiments.some(e => e.status === 'pending' || e.status === 'running');
    if (!inFlight) return;
    const id = setInterval(() => { listExperiments(token).then(setExperiments).catch(() => {}); }, 4000);
    return () => clearInterval(id);
  }, [experiments, token]);

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100%', overflow: 'hidden' }}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '12px 16px', borderBottom: `1px solid ${T.border}`, background: T.bgAlt }}>
        <div style={{ fontFamily: T.mono, color: T.textHi, fontSize: 13 }}>$ armory chaos</div>
        <div style={{ display: 'flex', gap: 8 }}>
          <button onClick={fetchAll} style={btnStyle}>↻</button>
          <button onClick={() => setShowCreate(s => !s)} style={btnStyle}>+ experiment</button>
        </div>
      </div>

      {showCreate && (
        <CreateForm token={token} types={types} outposts={outposts}
          onClose={() => setShowCreate(false)}
          onCreated={() => { setShowCreate(false); fetchAll(); }} />
      )}

      <div style={{ flex: 1, overflowY: 'auto', padding: 16 }}>
        {loading && <div style={{ color: T.faint, fontFamily: T.mono, fontSize: 12 }}>→ loading · · ·</div>}
        {error && <div style={{ color: T.red, fontFamily: T.mono, fontSize: 12 }}>error: {error}</div>}
        {!loading && experiments.length === 0 && !error && (
          <div style={{ color: T.dim, fontFamily: T.mono, fontSize: 12 }}>no experiments yet.</div>
        )}
        <div style={{ display: 'grid', gap: 8 }}>
          {experiments.map(e => (
            <div key={e.experiment_id} style={{ border: `1px solid ${T.border}`, background: T.card, padding: 12 }}>
              <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline' }}>
                <div style={{ fontFamily: T.mono, color: T.textHi, fontSize: 13 }}>
                  <Dot status={e.status} />{e.experiment_type} <span style={{ color: T.faint }}>→ {e.target_app_label}</span>
                </div>
                <div style={{ fontFamily: T.mono, fontSize: 12, color: toneColor[statusTone(e.status)] }}>{e.status}{e.verdict && e.verdict !== e.status ? ` (${e.verdict})` : ''}</div>
              </div>
              <div style={{ fontFamily: T.mono, color: T.faint, fontSize: 11, marginTop: 4 }}>
                ns: {e.target_app_ns} · {timeAgo(e.created_at)} ago{e.fail_step && e.fail_step !== 'N/A' ? ` · failStep: ${e.fail_step}` : ''}{e.probe_success ? ` · probes: ${e.probe_success}%` : ''}
              </div>
              <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginTop: 6 }}>
                <span style={{ fontFamily: T.mono, color: T.faint, fontSize: 10.5 }}>{e.experiment_id}</span>
                <button onClick={async () => { await deleteExperiment(token, e.experiment_id); fetchAll(); }} style={{ ...btnStyle, color: T.dim }}>
                  {e.status === 'pending' || e.status === 'running' ? 'stop' : 'delete'}
                </button>
              </div>
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}

/** Inline form for a new experiment; submits outpost + experiment type and target ns/label/kind via createExperiment. */
function CreateForm({ token, types, outposts, onClose, onCreated }: {
  token: string; types: ExperimentType[]; outposts: Outpost[];
  onClose: () => void; onCreated: () => void;
}) {
  const chaosOutposts = outposts.filter(o => (o.modules || '').split(',').includes('chaos'));
  const [outpostId, setOutpostId] = useState(chaosOutposts[0]?.outpost_id ?? '');
  const [experimentType, setExperimentType] = useState(types[0]?.name ?? 'pod-delete');
  const [ns, setNs] = useState('');
  const [label, setLabel] = useState('');
  const [kind, setKind] = useState('deployment');
  const [submitting, setSubmitting] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const submit = async () => {
    setSubmitting(true); setErr(null);
    try {
      await createExperiment(token, {
        outpost_id: outpostId, experiment_type: experimentType,
        target_app_ns: ns.trim(), target_app_label: label.trim(), target_app_kind: kind,
      });
      onCreated();
    } catch (e: unknown) { setErr((e as Error).message); }
    finally { setSubmitting(false); }
  };

  const ready = outpostId && ns.trim() && label.trim();
  return (
    <div style={{ padding: 16, borderBottom: `1px solid ${T.border}`, background: T.bgAlt, display: 'grid', gap: 10 }}>
      <div style={{ display: 'flex', gap: 10, flexWrap: 'wrap' }}>
        <select value={outpostId} onChange={e => setOutpostId(e.target.value)} style={inputStyle}>
          <option value="">{chaosOutposts.length ? 'select outpost' : 'no chaos-enabled outposts'}</option>
          {chaosOutposts.map(o => <option key={o.outpost_id} value={o.outpost_id}>{o.name} ({o.status})</option>)}
        </select>
        <select value={experimentType} onChange={e => setExperimentType(e.target.value)} style={inputStyle}>
          {(types.length ? types.map(t => t.name) : ['pod-delete', 'pod-network-latency']).map(n => <option key={n} value={n}>{n}</option>)}
        </select>
        <select value={kind} onChange={e => setKind(e.target.value)} style={inputStyle}>
          {['deployment', 'statefulset', 'daemonset'].map(k => <option key={k} value={k}>{k}</option>)}
        </select>
      </div>
      <div style={{ display: 'flex', gap: 10, flexWrap: 'wrap' }}>
        <input placeholder="target namespace" value={ns} onChange={e => setNs(e.target.value)} style={inputStyle} />
        <input placeholder="target label (app.kubernetes.io/component=conductor)" value={label} onChange={e => setLabel(e.target.value)} style={{ ...inputStyle, minWidth: 360 }} />
        <button disabled={!ready || submitting} onClick={submit} style={{ ...btnStyle, opacity: (!ready || submitting) ? 0.5 : 1 }}>{submitting ? '...' : 'run'}</button>
        <button onClick={onClose} style={{ ...btnStyle, color: T.dim }}>cancel</button>
      </div>
      {err && <div style={{ color: T.red, fontFamily: T.mono, fontSize: 12 }}>error: {err}</div>}
    </div>
  );
}

const btnStyle: React.CSSProperties = {
  background: 'transparent', border: `1px solid ${T.border}`, color: T.text,
  fontFamily: T.mono, fontSize: 12, padding: '5px 10px', cursor: 'pointer',
};
const inputStyle: React.CSSProperties = {
  background: T.bg, border: `1px solid ${T.border}`, color: T.textHi,
  fontFamily: T.mono, fontSize: 12, padding: '6px 10px', minWidth: 200,
};

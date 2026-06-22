import { useState, useEffect, useCallback } from 'react';
import { T } from '../../theme';
import { useAppSelector } from '../../store/hooks';
import {
  listOutposts, createOutpost, deleteOutpost,
  type Outpost, type CreateOutpostResponse,
} from '../../api/bff';
import { timeAgo } from '../../utils';

const ALL_MODULES = ['chaos', 'argo'];

function statusTone(status: string): 'green' | 'amber' | 'red' | 'dim' {
  if (status === 'connected') return 'green';
  if (status === 'pending') return 'amber';
  if (status === 'stale') return 'red';
  return 'dim';
}

const toneColor: Record<string, string> = { green: T.green, amber: T.amber, red: T.red, dim: T.faint };

function Dot({ status }: { status: string }) {
  const tone = statusTone(status);
  return (
    <span style={{
      display: 'inline-block', width: 7, height: 7, borderRadius: '50%', marginRight: 8,
      background: toneColor[tone],
      animation: tone === 'amber' ? 'pulse 1.5s ease-in-out infinite' : undefined,
    }} />
  );
}

export function Outposts() {
  const token = useAppSelector(s => s.auth.token)!;
  const [outposts, setOutposts] = useState<Outpost[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [showCreate, setShowCreate] = useState(false);
  const [created, setCreated] = useState<CreateOutpostResponse | null>(null);

  const fetchOutposts = useCallback(async () => {
    setLoading(true); setError(null);
    try { setOutposts(await listOutposts(token)); }
    catch (e: unknown) { setError((e as Error).message); }
    finally { setLoading(false); }
  }, [token]);

  useEffect(() => { fetchOutposts(); }, [fetchOutposts]);

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100%', overflow: 'hidden' }}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '12px 16px', borderBottom: `1px solid ${T.border}`, background: T.bgAlt }}>
        <div style={{ fontFamily: T.mono, color: T.textHi, fontSize: 13 }}>$ armory outposts</div>
        <div style={{ display: 'flex', gap: 8 }}>
          <button onClick={fetchOutposts} style={btnStyle}>↻</button>
          <button onClick={() => { setShowCreate(true); setCreated(null); }} style={btnStyle}>+ register</button>
        </div>
      </div>

      {showCreate && <CreateForm token={token} onClose={() => setShowCreate(false)} onCreated={(o) => { setCreated(o); setShowCreate(false); fetchOutposts(); }} />}
      {created && <EnrollmentPanel outpost={created} onDismiss={() => setCreated(null)} />}

      <div style={{ flex: 1, overflowY: 'auto', padding: 16 }}>
        {loading && <div style={{ color: T.faint, fontFamily: T.mono, fontSize: 12 }}>→ loading · · ·</div>}
        {error && <div style={{ color: T.red, fontFamily: T.mono, fontSize: 12 }}>error: {error}</div>}
        {!loading && !error && outposts.length === 0 && (
          <div style={{ color: T.dim, fontFamily: T.mono, fontSize: 12 }}>no outposts yet — register one to connect a cluster.</div>
        )}
        <div style={{ display: 'grid', gap: 8 }}>
          {outposts.map(o => (
            <div key={o.outpost_id} style={{ border: `1px solid ${T.border}`, background: T.card, padding: 12, display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
              <div>
                <div style={{ fontFamily: T.mono, color: T.textHi, fontSize: 13 }}><Dot status={o.status} />{o.name}</div>
                <div style={{ fontFamily: T.mono, color: T.faint, fontSize: 11, marginTop: 4 }}>
                  {o.status} · modules: {o.modules || '—'} · {o.last_seen_at ? `seen ${timeAgo(o.last_seen_at)} ago` : 'never seen'}
                </div>
                <div style={{ fontFamily: T.mono, color: T.faint, fontSize: 10.5, marginTop: 2 }}>{o.outpost_id}</div>
              </div>
              <button onClick={async () => { if (confirm(`Delete outpost ${o.name}?`)) { await deleteOutpost(token, o.outpost_id); fetchOutposts(); } }}
                style={{ ...btnStyle, color: T.dim }}>delete</button>
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}

function CreateForm({ token, onClose, onCreated }: { token: string; onClose: () => void; onCreated: (o: CreateOutpostResponse) => void }) {
  const [name, setName] = useState('');
  const [modules, setModules] = useState<string[]>(['chaos']);
  const [submitting, setSubmitting] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const toggle = (m: string) => setModules(prev => prev.includes(m) ? prev.filter(x => x !== m) : [...prev, m]);

  const submit = async () => {
    setSubmitting(true); setErr(null);
    try { onCreated(await createOutpost(token, { name: name.trim(), modules })); }
    catch (e: unknown) { setErr((e as Error).message); }
    finally { setSubmitting(false); }
  };

  return (
    <div style={{ padding: 16, borderBottom: `1px solid ${T.border}`, background: T.bgAlt }}>
      <div style={{ display: 'flex', gap: 10, alignItems: 'center', flexWrap: 'wrap' }}>
        <input autoFocus placeholder="outpost name (e.g. prod-eu)" value={name} onChange={e => setName(e.target.value)} style={inputStyle} />
        {ALL_MODULES.map(m => (
          <label key={m} style={{ fontFamily: T.mono, fontSize: 12, color: T.text, display: 'flex', alignItems: 'center', gap: 5, cursor: 'pointer' }}>
            <input type="checkbox" checked={modules.includes(m)} onChange={() => toggle(m)} /> {m}
          </label>
        ))}
        <button disabled={!name.trim() || modules.length === 0 || submitting} onClick={submit} style={{ ...btnStyle, opacity: (!name.trim() || submitting) ? 0.5 : 1 }}>
          {submitting ? '...' : 'create'}
        </button>
        <button onClick={onClose} style={{ ...btnStyle, color: T.dim }}>cancel</button>
      </div>
      {err && <div style={{ color: T.red, fontFamily: T.mono, fontSize: 12, marginTop: 8 }}>error: {err}</div>}
    </div>
  );
}

function EnrollmentPanel({ outpost, onDismiss }: { outpost: CreateOutpostResponse; onDismiss: () => void }) {
  const snippet = `helm install ${outpost.name} infra/helm/outpost \\
  --namespace codearmory-outpost --create-namespace \\
  --set controlPlaneURL=<your-gateway-url> \\
  --set modules="${outpost.modules.split(',').join('\\,')}" \\
  --set enrollmentToken=${outpost.enrollment_token}`;
  return (
    <div style={{ padding: 16, borderBottom: `1px solid ${T.green}`, background: T.cardHi }}>
      <div style={{ fontFamily: T.mono, color: T.green, fontSize: 12, marginBottom: 8 }}>
        ✓ outpost <b>{outpost.name}</b> registered. Copy the enrollment token now — it is shown only once.
      </div>
      <pre style={{ fontFamily: T.mono, fontSize: 11.5, color: T.textHi, background: T.bg, border: `1px solid ${T.border}`, padding: 12, overflowX: 'auto', whiteSpace: 'pre-wrap' }}>{snippet}</pre>
      <div style={{ display: 'flex', gap: 8, marginTop: 8 }}>
        <button onClick={() => navigator.clipboard?.writeText(outpost.enrollment_token)} style={btnStyle}>copy token</button>
        <button onClick={() => navigator.clipboard?.writeText(snippet)} style={btnStyle}>copy install</button>
        <button onClick={onDismiss} style={{ ...btnStyle, color: T.dim }}>dismiss</button>
      </div>
    </div>
  );
}

const btnStyle: React.CSSProperties = {
  background: 'transparent', border: `1px solid ${T.border}`, color: T.text,
  fontFamily: T.mono, fontSize: 12, padding: '5px 10px', cursor: 'pointer',
};
const inputStyle: React.CSSProperties = {
  background: T.bg, border: `1px solid ${T.border}`, color: T.textHi,
  fontFamily: T.mono, fontSize: 12, padding: '6px 10px', minWidth: 240,
};

import { useState, useEffect, useCallback } from 'react';
import { T } from '../../theme';
import { useAppSelector } from '../../store/hooks';
import {
  listArgoApps, syncArgoApp, getArgoSync, listOutposts,
  type ArgoApp, type ArgoSync, type Outpost,
} from '../../api/bff';
import { timeAgo } from '../../utils';

function healthTone(h?: string): 'green' | 'amber' | 'red' | 'dim' {
  if (h === 'Healthy') return 'green';
  if (h === 'Progressing' || h === 'Suspended') return 'amber';
  if (h === 'Degraded' || h === 'Missing') return 'red';
  return 'dim';
}
function syncTone(s?: string): 'green' | 'amber' | 'red' | 'dim' {
  if (s === 'Synced') return 'green';
  if (s === 'OutOfSync') return 'amber';
  return 'dim';
}
const toneColor: Record<string, string> = { green: T.green, amber: T.amber, red: T.red, dim: T.faint };

export function Argo() {
  const token = useAppSelector(s => s.auth.token)!;
  const [apps, setApps] = useState<ArgoApp[]>([]);
  const [outposts, setOutposts] = useState<Outpost[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [syncs, setSyncs] = useState<Record<string, ArgoSync>>({});
  const [manualOutpost, setManualOutpost] = useState('');

  const fetchAll = useCallback(async () => {
    setError(null);
    try {
      const [a, ops] = await Promise.all([
        listArgoApps(token),
        listOutposts(token).catch(() => [] as Outpost[]),
      ]);
      setApps(a); setOutposts(ops);
    } catch (e: unknown) { setError((e as Error).message); }
    finally { setLoading(false); }
  }, [token]);

  useEffect(() => { fetchAll(); }, [fetchAll]);

  const argoOutposts = outposts.filter(o => (o.modules || '').split(',').includes('argo'));

  const triggerSync = async (app: ArgoApp) => {
    try {
      const s = await syncArgoApp(token, app.name, app.outpost_id ? undefined : { outpost_id: manualOutpost });
      setSyncs(prev => ({ ...prev, [app.name]: s }));
      pollSync(app.name, s.sync_id);
    } catch (e: unknown) { setError((e as Error).message); }
  };

  const pollSync = (appName: string, syncId: string) => {
    const id = setInterval(async () => {
      try {
        const s = await getArgoSync(token, syncId);
        setSyncs(prev => ({ ...prev, [appName]: s }));
        if (s.status === 'Synced' || s.status === 'Failed') { clearInterval(id); fetchAll(); }
      } catch { clearInterval(id); }
    }, 4000);
  };

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100%', overflow: 'hidden' }}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '12px 16px', borderBottom: `1px solid ${T.border}`, background: T.bgAlt }}>
        <div style={{ fontFamily: T.mono, color: T.textHi, fontSize: 13 }}>$ armory argo</div>
        <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
          {argoOutposts.length > 0 && (
            <select value={manualOutpost} onChange={e => setManualOutpost(e.target.value)} style={inputStyle} title="outpost to use when an app hasn't reported state yet">
              <option value="">default outpost</option>
              {argoOutposts.map(o => <option key={o.outpost_id} value={o.outpost_id}>{o.name}</option>)}
            </select>
          )}
          <button onClick={fetchAll} style={btnStyle}>↻</button>
        </div>
      </div>

      <div style={{ flex: 1, overflowY: 'auto', padding: 16 }}>
        {loading && <div style={{ color: T.faint, fontFamily: T.mono, fontSize: 12 }}>→ loading · · ·</div>}
        {error && <div style={{ color: T.red, fontFamily: T.mono, fontSize: 12 }}>error: {error}</div>}
        {!loading && apps.length === 0 && !error && (
          <div style={{ color: T.dim, fontFamily: T.mono, fontSize: 12 }}>
            no apps reported yet — once an argo-enabled outpost connects, its Argo CD applications appear here.
          </div>
        )}
        <div style={{ display: 'grid', gap: 8 }}>
          {apps.map(a => {
            const s = syncs[a.name];
            return (
              <div key={a.app_id} style={{ border: `1px solid ${T.border}`, background: T.card, padding: 12 }}>
                <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline' }}>
                  <div style={{ fontFamily: T.mono, color: T.textHi, fontSize: 13 }}>{a.name}</div>
                  <div style={{ display: 'flex', gap: 12, fontFamily: T.mono, fontSize: 12 }}>
                    <span style={{ color: toneColor[syncTone(a.sync_status)] }}>{a.sync_status || 'unknown'}</span>
                    <span style={{ color: toneColor[healthTone(a.health_status)] }}>{a.health_status || 'unknown'}</span>
                  </div>
                </div>
                <div style={{ fontFamily: T.mono, color: T.faint, fontSize: 11, marginTop: 4 }}>
                  rev: {a.revision ? a.revision.slice(0, 10) : '—'} · {a.updated_at ? `updated ${timeAgo(a.updated_at)} ago` : ''}
                  {s && <span style={{ color: toneColor[s.status === 'Synced' ? 'green' : s.status === 'Failed' ? 'red' : 'amber'] }}> · sync: {s.status}{s.message ? ` (${s.message})` : ''}</span>}
                </div>
                <div style={{ display: 'flex', justifyContent: 'flex-end', marginTop: 6 }}>
                  <button onClick={() => triggerSync(a)} style={btnStyle}
                    disabled={!a.outpost_id && !manualOutpost}>sync</button>
                </div>
              </div>
            );
          })}
        </div>
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
  fontFamily: T.mono, fontSize: 12, padding: '5px 10px',
};

import { useState, useEffect, useCallback } from 'react';
import { T } from '../../theme';
import { useAppSelector } from '../../store/hooks';
import { listAuditLogs } from '../../api/bff';
import type { AuditLog } from '../../api/bff';
import { timeAgo } from '../../utils';

export function Audit() {
  const token = useAppSelector(s => s.auth.token)!;
  const [logs, setLogs] = useState<AuditLog[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<string | null>(null);

  const [filterActor, setFilterActor] = useState('');
  const [filterAction, setFilterAction] = useState('');
  const [filterResource, setFilterResource] = useState('');

  const fetchLogs = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const filters: { actor_id?: string; action?: string; resource_id?: string; limit: number } = { limit: 200 };
      if (filterActor.trim()) filters.actor_id = filterActor.trim();
      if (filterAction.trim()) filters.action = filterAction.trim();
      if (filterResource.trim()) filters.resource_id = filterResource.trim();
      setLogs(await listAuditLogs(token, filters));
    } catch (e: unknown) {
      setError((e as Error).message);
    } finally {
      setLoading(false);
    }
  }, [token, filterActor, filterAction, filterResource]);

  useEffect(() => { fetchLogs(); }, []); // eslint-disable-line react-hooks/exhaustive-deps

  const selectedLog = logs.find(l => l.audit_id === selected);

  const inputStyle = {
    background: 'transparent',
    border: `1px solid ${T.border}`,
    color: T.text,
    fontFamily: T.mono,
    fontSize: 11,
    padding: '5px 8px',
    outline: 'none',
    width: '100%',
    boxSizing: 'border-box' as const,
  };

  return (
    <div style={{ display: 'flex', height: '100%', overflow: 'hidden', flexDirection: 'column' }}>
      {/* Header / filters */}
      <div style={{ padding: '10px 16px', borderBottom: `1px solid ${T.border}`, background: T.bgAlt, flexShrink: 0 }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 12, flexWrap: 'wrap' }}>
          <span style={{ fontFamily: T.mono, fontSize: 11, color: T.faint }}>
            <span style={{ color: T.green }}>$</span> armory audit list
          </span>
          <input value={filterActor} onChange={e => setFilterActor(e.target.value)} placeholder="actor id" style={{ ...inputStyle, width: 160 }} />
          <input value={filterAction} onChange={e => setFilterAction(e.target.value)} placeholder="action (e.g. role.create)" style={{ ...inputStyle, width: 200 }} />
          <input value={filterResource} onChange={e => setFilterResource(e.target.value)} placeholder="resource id" style={{ ...inputStyle, width: 200 }} />
          <button onClick={fetchLogs}
            style={{ background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 11, padding: '5px 14px', cursor: 'pointer', fontWeight: 600 }}>
            [ search ]
          </button>
          <button onClick={() => { setFilterActor(''); setFilterAction(''); setFilterResource(''); }}
            style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 10px', cursor: 'pointer' }}>
            clear
          </button>
          {!loading && <span style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, marginLeft: 'auto' }}>{logs.length} entries</span>}
        </div>
      </div>

      <div style={{ flex: 1, display: 'flex', overflow: 'hidden' }}>
        {/* Log list */}
        <div style={{ width: 320, flexShrink: 0, borderRight: `1px solid ${T.border}`, background: T.bgAlt, overflow: 'auto' }}>
          {loading ? (
            <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint, animation: 'pulse 1s ease-in-out infinite' }}>→ loading · · ·</div>
          ) : error ? (
            <div style={{ padding: '14px', fontFamily: T.mono, fontSize: 11, color: T.red }}>{error}</div>
          ) : logs.length === 0 ? (
            <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint }}>→ no audit logs</div>
          ) : logs.map(log => {
            const isActive = selected === log.audit_id;
            return (
              <button key={log.audit_id} onClick={() => setSelected(log.audit_id)}
                style={{ width: '100%', textAlign: 'left', padding: '9px 14px', background: isActive ? T.greenSoft : 'transparent', border: 0, borderLeft: `2px solid ${isActive ? T.green : 'transparent'}`, fontFamily: T.mono, cursor: 'pointer', color: T.text, display: 'block', transition: 'background .12s' }}>
                <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 8, marginBottom: 2 }}>
                  <span style={{ fontSize: 12, fontWeight: 600, color: isActive ? T.textHi : T.blue }}>{log.action}</span>
                  <span style={{ fontSize: 10, color: T.faint, flexShrink: 0 }}>{timeAgo(log.created_at)} ago</span>
                </div>
                <div style={{ fontSize: 10, color: T.dim, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                  actor: {log.actor_id.slice(0, 8)}…
                </div>
              </button>
            );
          })}
        </div>

        {/* Detail */}
        <div style={{ flex: 1, overflow: 'auto' }}>
          {!selectedLog ? (
            <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', height: '100%' }}>
              <div style={{ fontFamily: T.mono, fontSize: 12, color: T.faint }}>→ select an entry</div>
            </div>
          ) : (
            <div style={{ padding: '20px 24px' }}>
              <div style={{ fontFamily: T.mono, fontSize: 18, fontWeight: 700, color: T.blue, marginBottom: 4 }}>{selectedLog.action}</div>
              <div style={{ fontFamily: T.mono, fontSize: 11, color: T.faint, marginBottom: 20 }}>{new Date(selectedLog.created_at).toLocaleString()}</div>

              <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 12, marginBottom: 20 }}>
                {([
                  ['actor', selectedLog.actor_id],
                  ['resource', selectedLog.resource_id],
                  ...(selectedLog.resource_type ? [['resource type', selectedLog.resource_type] as [string, string]] : []),
                  ...(selectedLog.org_id ? [['org', selectedLog.org_id] as [string, string]] : []),
                ] as [string, string][]).map(([k, v]) => (
                  <div key={k} style={{ background: T.card, border: `1px solid ${T.border}`, padding: '10px 14px' }}>
                    <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 4, textTransform: 'uppercase' }}>{k}</div>
                    <div style={{ fontFamily: T.mono, fontSize: 12, color: T.textHi, wordBreak: 'break-all' }}>{v}</div>
                  </div>
                ))}
              </div>

              {selectedLog.meta && Object.keys(selectedLog.meta).length > 0 && (
                <>
                  <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 8 }}>META</div>
                  <div style={{ background: T.bg, border: `1px solid ${T.border}`, padding: '10px 12px' }}>
                    <pre style={{ margin: 0, fontFamily: T.mono, fontSize: 11, color: T.text, lineHeight: 1.6 }}>
                      {JSON.stringify(selectedLog.meta, null, 2)}
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

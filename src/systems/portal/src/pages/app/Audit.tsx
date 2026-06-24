/**
 * Audit page — two admin views behind a tab bar:
 *  - Mutations: the gatekeeper change log (who created/updated/deleted what), with
 *    actor/action/resource filters and a detail panel; ids are resolved to names.
 *  - Access checks: the permission-evaluation log (one row per access decision,
 *    granted or denied, across every service) for spotting suspicious usage. Gated
 *    on the listPermissionCheck permission.
 * data via the bff.
 */
import { useState, useEffect, useCallback, useMemo } from 'react';
import { T } from '../../theme';
import { useAppSelector } from '../../store/hooks';
import { listAuditLogs, listPermissionChecks } from '../../api/bff';
import type { AuditLog, PermissionCheck } from '../../api/bff';
import { timeAgo, shortId } from '../../utils';
import { useUserNames, useOrgNames, usePermissionNames, useSecretNames } from '../../hooks/useNames';

// The kind of entity an audit entry's resource_id points at, keyed by the action's
// domain prefix (e.g. "role.update" → "role"). Only kinds with a resolvable name
// are listed; anything else (role, invite, session, run_token, …) has no name.
const RESOURCE_KIND: Record<string, 'user' | 'org' | 'permission' | 'secret'> = {
  user: 'user',
  mfa: 'user',
  org: 'org',
  secret_provider: 'org',
  permission: 'permission',
  secret: 'secret',
};

/**
 * The service an audit entry originated from. Entries are recorded by gatekeeper;
 * a service-actor entry names the calling service (builder/blueprints/forge/…),
 * while a user-initiated entry is a gatekeeper operation.
 */
function auditService(log: AuditLog): string {
  return log.actor_type === 'service' ? log.actor_id : 'gatekeeper';
}

const inputStyle: React.CSSProperties = {
  background: 'transparent',
  border: `1px solid ${T.border}`,
  color: T.text,
  fontFamily: T.mono,
  fontSize: 11,
  padding: '5px 8px',
  outline: 'none',
  boxSizing: 'border-box',
};

/** A 2-column grid of labelled value cells, shared by both detail panels. */
function DetailGrid({ rows }: { rows: [string, string][] }) {
  return (
    <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 12, marginBottom: 20 }}>
      {rows.map(([k, v]) => (
        <div key={k} style={{ background: T.card, border: `1px solid ${T.border}`, padding: '10px 14px' }}>
          <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 4, textTransform: 'uppercase' }}>{k}</div>
          <div style={{ fontFamily: T.mono, fontSize: 12, color: T.textHi, wordBreak: 'break-all' }}>{v}</div>
        </div>
      ))}
    </div>
  );
}

/** Audit page shell: a tab bar over the mutation log and (when permitted) the access-check log. */
export function Audit() {
  const permissions = useAppSelector(s => s.auth.permissions);
  const canAccessChecks = !!permissions?.['gatekeeper:listPermissionCheck'];
  const [tab, setTab] = useState<'mutations' | 'access'>('mutations');

  const tabBtn = (key: 'mutations' | 'access', label: string) => (
    <button onClick={() => setTab(key)}
      style={{
        background: 'transparent', border: 0, borderBottom: `2px solid ${tab === key ? T.green : 'transparent'}`,
        color: tab === key ? T.textHi : T.dim, fontFamily: T.mono, fontSize: 12, padding: '8px 14px', cursor: 'pointer',
      }}>
      {label}
    </button>
  );

  return (
    <div style={{ display: 'flex', height: '100%', overflow: 'hidden', flexDirection: 'column' }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 4, padding: '0 12px', borderBottom: `1px solid ${T.border}`, background: T.bgAlt, flexShrink: 0 }}>
        {tabBtn('mutations', 'mutations')}
        {canAccessChecks && tabBtn('access', 'access checks')}
      </div>
      {tab === 'mutations' || !canAccessChecks ? <MutationsView /> : <AccessChecksView />}
    </div>
  );
}

/** Mutation log: gatekeeper change entries with actor/action/resource filters, a list, and a detail panel. */
function MutationsView() {
  const token = useAppSelector(s => s.auth.token)!;
  const [logs, setLogs] = useState<AuditLog[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const userNames = useUserNames(token);
  const orgNames = useOrgNames(token);
  const permissionNames = usePermissionNames(token);
  const secretNames = useSecretNames(token);

  const [filterActor, setFilterActor] = useState('');
  const [filterAction, setFilterAction] = useState('');
  const [filterResource, setFilterResource] = useState('');

  // Reverse of userNames (username → user-id), for translating the actor filter.
  const idByActor = useMemo(
    () => Object.fromEntries(Object.entries(userNames).map(([id, name]) => [name, id])),
    [userNames],
  );
  // Sorted, de-duplicated usernames offered as datalist suggestions on the actor filter.
  const actorOptions = useMemo(() => [...new Set(Object.values(userNames))].sort(), [userNames]);

  const fetchLogs = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const filters: { actor_id?: string; action?: string; resource_id?: string; limit: number } = { limit: 200 };
      // The actor filter accepts a username for convenience; translate a known
      // username back to its user-id (the backend filters on actor_id), but pass
      // an unrecognised value through as-is so raw ids / service names still work.
      const actor = filterActor.trim();
      if (actor) filters.actor_id = idByActor[actor] ?? actor;
      if (filterAction.trim()) filters.action = filterAction.trim();
      if (filterResource.trim()) filters.resource_id = filterResource.trim();
      setLogs(await listAuditLogs(token, filters));
    } catch (e: unknown) {
      setError((e as Error).message);
    } finally {
      setLoading(false);
    }
  }, [token, filterActor, filterAction, filterResource, idByActor]);

  useEffect(() => { fetchLogs(); }, []); // eslint-disable-line react-hooks/exhaustive-deps

  // username for an actor id when known (service actors / unknown ids fall back to the raw id).
  const actorLabel = (id: string) => userNames[id] ?? id;

  // Resolve an entry's resource_id to a name based on the action's domain, when one
  // is available; undefined for entities with no name (roles, invites, sessions, …).
  const resourceName = (log: AuditLog): string | undefined => {
    switch (RESOURCE_KIND[log.action.split('.')[0]]) {
      case 'user': return userNames[log.resource_id];
      case 'org': return orgNames[log.resource_id];
      case 'permission': return permissionNames[log.resource_id];
      case 'secret': return secretNames[log.resource_id];
      default: return undefined;
    }
  };

  const selectedLog = logs.find(l => l.audit_log_id === selected);
  const selectedResourceName = selectedLog ? resourceName(selectedLog) : undefined;

  return (
    <div style={{ display: 'flex', height: '100%', overflow: 'hidden', flexDirection: 'column' }}>
      {/* Filters */}
      <div style={{ padding: '10px 16px', borderBottom: `1px solid ${T.border}`, background: T.bgAlt, flexShrink: 0 }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 12, flexWrap: 'wrap' }}>
          <span style={{ fontFamily: T.mono, fontSize: 11, color: T.faint }}>
            <span style={{ color: T.green }}>$</span> armory audit list
          </span>
          <input value={filterActor} onChange={e => setFilterActor(e.target.value)} placeholder="actor (username or id)" list="audit-actors" style={{ ...inputStyle, width: 200 }} />
          <datalist id="audit-actors">
            {actorOptions.map(name => <option key={name} value={name} />)}
          </datalist>
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
            const isActive = selected === log.audit_log_id;
            return (
              <button key={log.audit_log_id} onClick={() => setSelected(log.audit_log_id)}
                style={{ width: '100%', textAlign: 'left', padding: '9px 14px', background: isActive ? T.greenSoft : 'transparent', border: 0, borderLeft: `2px solid ${isActive ? T.green : 'transparent'}`, fontFamily: T.mono, cursor: 'pointer', color: T.text, display: 'block', transition: 'background .12s' }}>
                <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 8, marginBottom: 2 }}>
                  <span style={{ display: 'flex', alignItems: 'center', gap: 6, minWidth: 0 }}>
                    <span style={{ fontSize: 12, fontWeight: 600, color: isActive ? T.textHi : T.blue, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{log.action}</span>
                    <span style={{ fontSize: 9, color: T.faint, border: `1px solid ${T.border}`, padding: '0 4px', flexShrink: 0, textTransform: 'uppercase', letterSpacing: 0.5 }}>{auditService(log)}</span>
                  </span>
                  <span style={{ fontSize: 10, color: T.faint, flexShrink: 0 }}>{timeAgo(log.created_at)} ago</span>
                </div>
                <div style={{ fontSize: 10, color: T.dim, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                  actor: {userNames[log.actor_id] ?? shortId(log.actor_id)}
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

              <DetailGrid rows={[
                ['service', auditService(selectedLog)],
                ['actor', actorLabel(selectedLog.actor_id)],
                ...(userNames[selectedLog.actor_id] ? [['actor id', selectedLog.actor_id] as [string, string]] : []),
                ...(selectedLog.actor_type ? [['actor type', selectedLog.actor_type] as [string, string]] : []),
                ...(selectedLog.org_id ? [['org', orgNames[selectedLog.org_id] ?? shortId(selectedLog.org_id)] as [string, string]] : []),
                ['resource', selectedResourceName ?? selectedLog.resource_id],
                ...(selectedResourceName ? [['resource id', selectedLog.resource_id] as [string, string]] : []),
              ]} />

              {selectedLog.detail && (
                <>
                  <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 8 }}>DETAIL</div>
                  <div style={{ background: T.bg, border: `1px solid ${T.border}`, padding: '10px 12px' }}>
                    <pre style={{ margin: 0, fontFamily: T.mono, fontSize: 11, color: T.text, lineHeight: 1.6, whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>
                      {selectedLog.detail}
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

/**
 * Access-check log: one row per permission evaluation (granted or denied) across
 * every service, for spotting suspicious usage. Filter by user/service/action,
 * resource substring, and decision; denials are emphasized in red.
 */
function AccessChecksView() {
  const token = useAppSelector(s => s.auth.token)!;
  const [checks, setChecks] = useState<PermissionCheck[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const userNames = useUserNames(token);
  const orgNames = useOrgNames(token);

  const [filterUser, setFilterUser] = useState('');
  const [filterService, setFilterService] = useState('');
  const [filterAction, setFilterAction] = useState('');
  const [filterResource, setFilterResource] = useState('');
  const [filterGranted, setFilterGranted] = useState(''); // '' | 'true' | 'false'

  const idByUser = useMemo(
    () => Object.fromEntries(Object.entries(userNames).map(([id, name]) => [name, id])),
    [userNames],
  );
  const userOptions = useMemo(() => [...new Set(Object.values(userNames))].sort(), [userNames]);

  const fetchChecks = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const filters: { user_id?: string; service?: string; action?: string; resource?: string; granted?: boolean; limit: number } = { limit: 200 };
      const user = filterUser.trim();
      if (user) filters.user_id = idByUser[user] ?? user;
      if (filterService.trim()) filters.service = filterService.trim();
      if (filterAction.trim()) filters.action = filterAction.trim();
      if (filterResource.trim()) filters.resource = filterResource.trim();
      if (filterGranted) filters.granted = filterGranted === 'true';
      setChecks(await listPermissionChecks(token, filters));
    } catch (e: unknown) {
      setError((e as Error).message);
    } finally {
      setLoading(false);
    }
  }, [token, filterUser, filterService, filterAction, filterResource, filterGranted, idByUser]);

  useEffect(() => { fetchChecks(); }, []); // eslint-disable-line react-hooks/exhaustive-deps

  const userLabel = (id: string) => userNames[id] ?? id;
  const selectedCheck = checks.find(c => c.permissions_check_id === selected);

  return (
    <div style={{ display: 'flex', height: '100%', overflow: 'hidden', flexDirection: 'column' }}>
      {/* Filters */}
      <div style={{ padding: '10px 16px', borderBottom: `1px solid ${T.border}`, background: T.bgAlt, flexShrink: 0 }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 10, flexWrap: 'wrap' }}>
          <span style={{ fontFamily: T.mono, fontSize: 11, color: T.faint }}>
            <span style={{ color: T.green }}>$</span> armory access checks
          </span>
          <input value={filterUser} onChange={e => setFilterUser(e.target.value)} placeholder="user (username or id)" list="access-users" style={{ ...inputStyle, width: 180 }} />
          <datalist id="access-users">
            {userOptions.map(name => <option key={name} value={name} />)}
          </datalist>
          <input value={filterService} onChange={e => setFilterService(e.target.value)} placeholder="service" style={{ ...inputStyle, width: 120 }} />
          <input value={filterAction} onChange={e => setFilterAction(e.target.value)} placeholder="action" style={{ ...inputStyle, width: 150 }} />
          <input value={filterResource} onChange={e => setFilterResource(e.target.value)} placeholder="resource contains…" style={{ ...inputStyle, width: 180 }} />
          <select value={filterGranted} onChange={e => setFilterGranted(e.target.value)} style={{ ...inputStyle, width: 110 }}>
            <option value="">all</option>
            <option value="false">denied</option>
            <option value="true">granted</option>
          </select>
          <button onClick={fetchChecks}
            style={{ background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 11, padding: '5px 14px', cursor: 'pointer', fontWeight: 600 }}>
            [ search ]
          </button>
          <button onClick={() => { setFilterUser(''); setFilterService(''); setFilterAction(''); setFilterResource(''); setFilterGranted(''); }}
            style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 10px', cursor: 'pointer' }}>
            clear
          </button>
          {!loading && <span style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, marginLeft: 'auto' }}>{checks.length} checks</span>}
        </div>
      </div>

      <div style={{ flex: 1, display: 'flex', overflow: 'hidden' }}>
        {/* Check list */}
        <div style={{ width: 340, flexShrink: 0, borderRight: `1px solid ${T.border}`, background: T.bgAlt, overflow: 'auto' }}>
          {loading ? (
            <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint, animation: 'pulse 1s ease-in-out infinite' }}>→ loading · · ·</div>
          ) : error ? (
            <div style={{ padding: '14px', fontFamily: T.mono, fontSize: 11, color: T.red }}>{error}</div>
          ) : checks.length === 0 ? (
            <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint }}>→ no permission checks</div>
          ) : checks.map(c => {
            const isActive = selected === c.permissions_check_id;
            const denied = !c.granted;
            return (
              <button key={c.permissions_check_id} onClick={() => setSelected(c.permissions_check_id)}
                style={{ width: '100%', textAlign: 'left', padding: '9px 14px', background: isActive ? T.greenSoft : 'transparent', border: 0, borderLeft: `2px solid ${isActive ? T.green : denied ? T.red : 'transparent'}`, fontFamily: T.mono, cursor: 'pointer', color: T.text, display: 'block', transition: 'background .12s' }}>
                <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 8, marginBottom: 2 }}>
                  <span style={{ display: 'flex', alignItems: 'center', gap: 6, minWidth: 0 }}>
                    <span style={{ fontSize: 11, fontWeight: 700, color: denied ? T.red : T.green, flexShrink: 0 }}>{denied ? '✗' : '✓'}</span>
                    <span style={{ fontSize: 12, fontWeight: 600, color: isActive ? T.textHi : T.blue, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{c.action}</span>
                    <span style={{ fontSize: 9, color: T.faint, border: `1px solid ${T.border}`, padding: '0 4px', flexShrink: 0, textTransform: 'uppercase', letterSpacing: 0.5 }}>{c.service}</span>
                  </span>
                  <span style={{ fontSize: 10, color: T.faint, flexShrink: 0 }}>{timeAgo(c.created_at)} ago</span>
                </div>
                <div style={{ fontSize: 10, color: T.dim, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                  {userNames[c.user_id] ?? shortId(c.user_id)} · {c.resource}
                </div>
              </button>
            );
          })}
        </div>

        {/* Detail */}
        <div style={{ flex: 1, overflow: 'auto' }}>
          {!selectedCheck ? (
            <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', height: '100%' }}>
              <div style={{ fontFamily: T.mono, fontSize: 12, color: T.faint }}>→ select a check</div>
            </div>
          ) : (
            <div style={{ padding: '20px 24px' }}>
              <div style={{ display: 'flex', alignItems: 'center', gap: 10, marginBottom: 4 }}>
                <span style={{ fontFamily: T.mono, fontSize: 18, fontWeight: 700, color: T.blue }}>{selectedCheck.action}</span>
                <span style={{
                  fontFamily: T.mono, fontSize: 11, fontWeight: 700, letterSpacing: 0.5, textTransform: 'uppercase',
                  color: selectedCheck.granted ? T.green : T.red,
                  background: selectedCheck.granted ? T.greenSoft : T.redSoft,
                  border: `1px solid ${selectedCheck.granted ? T.green : T.red}`, padding: '1px 8px',
                }}>{selectedCheck.granted ? 'granted' : 'denied'}</span>
              </div>
              <div style={{ fontFamily: T.mono, fontSize: 11, color: T.faint, marginBottom: 20 }}>{new Date(selectedCheck.created_at).toLocaleString()}</div>

              <DetailGrid rows={[
                ['service', selectedCheck.service],
                ['user', userLabel(selectedCheck.user_id)],
                ...(userNames[selectedCheck.user_id] ? [['user id', selectedCheck.user_id] as [string, string]] : []),
                ...(selectedCheck.org_id ? [['org', orgNames[selectedCheck.org_id] ?? shortId(selectedCheck.org_id)] as [string, string]] : []),
                ...(selectedCheck.team_id ? [['team', shortId(selectedCheck.team_id)] as [string, string]] : []),
              ]} />

              <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 8 }}>RESOURCE</div>
              <div style={{ background: T.bg, border: `1px solid ${T.border}`, padding: '10px 12px' }}>
                <pre style={{ margin: 0, fontFamily: T.mono, fontSize: 11, color: T.text, lineHeight: 1.6, whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>
                  {selectedCheck.resource}
                </pre>
              </div>
            </div>
          )}
        </div>
      </div>
    </div>
  );
}

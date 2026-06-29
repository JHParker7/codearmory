/**
 * gatekeeper admin page — the auth/RBAC control surface. tabbed view over the
 * gatekeeper entities: users, roles, permissions, secrets (+ per-org secret
 * backend), teams, orgs, invites, and service-permission-requests. all data
 * goes through the typed bff client (src/api/bff.ts), which proxies /api/* to
 * conductor; tabs are gated on the caller's redux permissions.
 */
import { useState, useEffect, useCallback, useMemo } from 'react';
import { T } from '../../theme';
import { Pill } from '../../components/Pill';
import { useConfirm } from '../../components/ConfirmDialog';
import { useAppSelector } from '../../store/hooks';
import {
  listUsers, updateUser, deleteUser,
  listRoles, createRole, updateRole, deleteRole,
  listPermissions, createPermission, updatePermission, deletePermission,
  listSecrets, createSecret, updateSecret, deleteSecret,
  getSecretProvider, setSecretProvider, deleteSecretProvider,
  listTeams, createTeam, updateTeam, deleteTeam, inviteToTeam,
  listInvites, acceptInvite, declineInvite, deleteInvite,
  listServiceRequests, approveServiceRequest, declineServiceRequest,
  listOrgs, createOrg, updateOrg, deleteOrg, inviteToOrg,
  getOrg, getTeam,
} from '../../api/bff';
import type { User, Role, Permission, Secret, Org, Team, Invite, ServiceRequest, SecretProvider, SecretProviderName } from '../../api/bff';
import { timeAgo, shortId } from '../../utils';
import { useUserNames } from '../../hooks/useNames';
import { useResizableWidth } from '../../components/ResizeHandle';

type Tab = 'users' | 'roles' | 'permissions' | 'secrets' | 'teams' | 'orgs' | 'invites' | 'service-requests';

// ── Shared input style ─────────────────────────────────────────────────────────

const inputStyle = {
  background: 'transparent' as const,
  border: `1px solid ${T.border}`,
  color: T.text,
  fontFamily: T.mono,
  fontSize: 12,
  padding: '7px 10px',
  outline: 'none',
  width: '100%',
  boxSizing: 'border-box' as const,
};

// roleLabel is the human-facing name for a role — its name when set, otherwise a
// short slice of the UUID. A raw role_id (a UUID) means nothing to an admin, so
// every place a role is shown or chosen renders this instead.
function roleLabel(role: Role): string {
  return role.name?.trim() || `${shortId(role.role_id)} (unnamed)`;
}

// ── Users tab ─────────────────────────────────────────────────────────────────

/** users tab: master/detail list of all users; the detail pane edits a user (email/username/name/password via updateUser) and deletes via deleteUser, and resolves the user's org/team names through getOrg/getTeam. */
function UsersTab() {
  const token = useAppSelector(s => s.auth.token)!;
  const currentUserId = useAppSelector(s => s.auth.user?.user_id);
  const [users, setUsers] = useState<User[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const [org, setOrg] = useState<Org | null>(null);
  const [team, setTeam] = useState<Team | null>(null);
  const [roles, setRoles] = useState<Role[]>([]);
  const [editing, setEditing] = useState(false);
  const [form, setForm] = useState({ email: '', username: '', firstname: '', lastname: '', password: '' });
  const [saving, setSaving] = useState(false);

  const fetchUsers = useCallback(async () => {
    setLoading(true); setError(null);
    try { setUsers(await listUsers(token)); }
    catch (e: unknown) { setError((e as Error).message); }
    finally { setLoading(false); }
  }, [token]);

  useEffect(() => { fetchUsers(); }, [fetchUsers]);
  // Resolve a user's role_id to a name in the detail pane. Best effort.
  useEffect(() => { listRoles(token).then(setRoles).catch(() => {}); }, [token]);

  const selectedUser = users.find(u => u.user_id === selected);
  const roleById = useMemo(() => new Map(roles.map(r => [r.role_id, r])), [roles]);

  useEffect(() => {
    setEditing(false);
    if (!selectedUser) return;
    setOrg(null); setTeam(null);
    if (selectedUser.org_id) getOrg(token, selectedUser.org_id).then(setOrg).catch(() => {});
    if (selectedUser.team_id) getTeam(token, selectedUser.team_id).then(setTeam).catch(() => {});
  }, [selected, selectedUser, token]);

  const startEdit = (u: User) => {
    setForm({ email: u.email, username: u.username, firstname: u.firstname ?? '', lastname: u.lastname ?? '', password: '' });
    setError(null); setEditing(true);
  };

  const handleSave = async (id: string) => {
    if (!form.email.trim() || !form.username.trim()) return;
    setSaving(true); setError(null);
    try {
      const updated = await updateUser(token, id, {
        email: form.email.trim(),
        username: form.username.trim(),
        ...(form.firstname.trim() && { firstname: form.firstname.trim() }),
        ...(form.lastname.trim() && { lastname: form.lastname.trim() }),
        ...(form.password && { password: form.password }),
      });
      setUsers(prev => prev.map(u => u.user_id === id ? updated : u));
      setEditing(false);
    } catch (e: unknown) { setError((e as Error).message); }
    finally { setSaving(false); }
  };

  const [confirm, confirmEl] = useConfirm();
  const [railW, railHandle] = useResizableWidth('rail.gatekeeper.users', 260, { min: 200, max: 480 });

  const handleDelete = async (id: string) => {
    const name = users.find(u => u.user_id === id)?.username;
    if (!(await confirm({ message: `Delete user @${name ?? id}? This permanently removes the account and cannot be undone.` }))) return;
    try {
      await deleteUser(token, id);
      setUsers(prev => prev.filter(u => u.user_id !== id));
      if (selected === id) setSelected(null);
    } catch (e: unknown) { setError((e as Error).message); }
  };

  return (
    <div style={{ display: 'flex', flex: 1, overflow: 'hidden' }}>
      {confirmEl}
      <div style={{ width: railW, flexShrink: 0, borderRight: `1px solid ${T.border}`, display: 'flex', flexDirection: 'column', background: T.bgAlt, overflow: 'auto' }}>
        <div style={{ padding: '12px 14px', borderBottom: `1px solid ${T.border}`, display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
          <span style={{ fontFamily: T.mono, fontSize: 10, color: T.faint }}>{users.length > 0 ? `${users.length} user${users.length !== 1 ? 's' : ''}` : ''}</span>
          <button onClick={fetchUsers} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '2px 6px', cursor: 'pointer' }}>↻</button>
        </div>
        {loading ? <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint, animation: 'pulse 1s ease-in-out infinite' }}>→ loading · · ·</div>
          : error ? <div style={{ padding: '14px', fontFamily: T.mono, fontSize: 11, color: T.red }}>{error}</div>
          : users.map(u => {
            const isActive = selected === u.user_id;
            return (
              <button key={u.user_id} onClick={() => setSelected(u.user_id)}
                style={{ width: '100%', textAlign: 'left', padding: '10px 14px', background: isActive ? T.greenSoft : 'transparent', border: 0, borderLeft: `2px solid ${isActive ? T.green : 'transparent'}`, fontFamily: T.mono, cursor: 'pointer', color: T.text, display: 'block', transition: 'background .12s' }}>
                <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
                  <span style={{ fontSize: 13, fontWeight: 600, color: isActive ? T.textHi : T.text }}>@{u.username}</span>
                  {u.user_id === currentUserId && <span style={{ fontSize: 9, color: T.green, border: `1px solid ${T.green}`, padding: '0 4px' }}>you</span>}
                  {!u.active && <span style={{ fontSize: 9, color: T.red, border: `1px solid ${T.red}`, padding: '0 4px' }}>inactive</span>}
                </div>
                <div style={{ fontSize: 11, color: T.faint, marginTop: 2 }}>{u.email}</div>
              </button>
            );
          })}
      </div>
      {railHandle}
      <div style={{ flex: 1, overflow: 'auto' }}>
        {!selectedUser ? (
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', height: '100%' }}>
            <div style={{ fontFamily: T.mono, fontSize: 12, color: T.faint }}>→ select a user</div>
          </div>
        ) : (
          <div style={{ padding: '20px 24px' }}>
            <div style={{ display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between', marginBottom: 20 }}>
              <div>
                <div style={{ fontFamily: T.mono, fontSize: 20, fontWeight: 700, color: T.textHi, marginBottom: 4 }}>@{selectedUser.username}</div>
                <div style={{ fontFamily: T.mono, fontSize: 13, color: T.dim }}>{selectedUser.email}</div>
                {(selectedUser.firstname || selectedUser.lastname) && (
                  <div style={{ fontFamily: T.mono, fontSize: 12, color: T.faint, marginTop: 2 }}>
                    {[selectedUser.firstname, selectedUser.lastname].filter(Boolean).join(' ')}
                  </div>
                )}
              </div>
              <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
                <Pill tone={selectedUser.active ? 'green' : 'red'}>{selectedUser.active ? 'active' : 'inactive'}</Pill>
                <button onClick={() => editing ? setEditing(false) : startEdit(selectedUser)}
                  style={{ background: editing ? T.greenSoft : 'transparent', border: `1px solid ${editing ? T.green : T.border}`, color: editing ? T.green : T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}>
                  {editing ? '[ cancel ]' : '[ edit ]'}
                </button>
                {selectedUser.user_id !== currentUserId && (
                  <button onClick={() => handleDelete(selectedUser.user_id)}
                    style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}
                    onMouseEnter={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.red; (e.currentTarget as HTMLButtonElement).style.color = T.red; }}
                    onMouseLeave={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.border; (e.currentTarget as HTMLButtonElement).style.color = T.dim; }}>
                    [ delete ]
                  </button>
                )}
              </div>
            </div>
            {editing && (
              <div style={{ background: T.card, border: `1px solid ${T.borderHi}`, padding: '16px', marginBottom: 20 }}>
                {error && <div style={{ background: T.redSoft, border: `1px solid ${T.red}`, padding: '8px 12px', fontFamily: T.mono, fontSize: 11, color: T.red, marginBottom: 12 }}>{error}</div>}
                <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 12, marginBottom: 12 }}>
                  {([['email', 'email'], ['username', 'username'], ['firstname', 'first name'], ['lastname', 'last name'], ['password', 'new password (blank = keep)']] as [keyof typeof form, string][]).map(([field, label]) => (
                    <div key={field} style={{ gridColumn: field === 'password' ? '1 / -1' : undefined }}>
                      <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, marginBottom: 6, letterSpacing: 0.5, textTransform: 'uppercase' }}>{label}</div>
                      <input value={form[field]} type={field === 'password' ? 'password' : 'text'}
                        onChange={e => setForm(f => ({ ...f, [field]: e.target.value }))}
                        style={{ ...inputStyle, background: T.cardHi }} />
                    </div>
                  ))}
                </div>
                <div style={{ display: 'flex', gap: 8 }}>
                  <button onClick={() => handleSave(selectedUser.user_id)} disabled={!form.email.trim() || !form.username.trim() || saving}
                    style={{ background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 12, fontWeight: 600, padding: '7px 16px', cursor: 'pointer', opacity: (!form.email.trim() || !form.username.trim() || saving) ? 0.6 : 1 }}>
                    {saving ? '[ · · · ]' : '[ save ]'}
                  </button>
                  <button onClick={() => setEditing(false)} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '7px 12px', cursor: 'pointer' }}>cancel</button>
                </div>
              </div>
            )}
            <div style={{ display: 'grid', gridTemplateColumns: 'repeat(3, 1fr)', gap: 12, marginBottom: 20 }}>
              {([['user id', selectedUser.user_id.slice(0, 8) + '…'], ['joined', timeAgo(selectedUser.created_at) + ' ago'], ['updated', timeAgo(selectedUser.updated_at) + ' ago']] as [string, string][]).map(([k, v]) => (
                <div key={k} style={{ background: T.card, border: `1px solid ${T.border}`, padding: '10px 14px' }}>
                  <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 4, textTransform: 'uppercase' }}>{k}</div>
                  <div style={{ fontFamily: T.mono, fontSize: 13, color: T.textHi }}>{v}</div>
                </div>
              ))}
            </div>
            {(org || team || selectedUser.role_id) && (
              <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '12px 16px', fontFamily: T.mono, fontSize: 12 }}>
                {org && <div style={{ marginBottom: 4 }}><span style={{ color: T.faint }}>org   </span><span style={{ color: T.text }}>{org.org_name}</span></div>}
                {team && <div style={{ marginBottom: 4 }}><span style={{ color: T.faint }}>team  </span><span style={{ color: T.text }}>{team.team_name}</span></div>}
                {selectedUser.role_id && <div><span style={{ color: T.faint }}>role  </span><span style={{ color: T.dim }}>{(() => { const r = roleById.get(selectedUser.role_id); return r ? roleLabel(r) : shortId(selectedUser.role_id); })()}</span></div>}
              </div>
            )}
          </div>
        )}
      </div>
    </div>
  );
}

// ── Roles tab ─────────────────────────────────────────────────────────────────

/** roles tab: master/detail list of roles; create + inline-edit a role by supplying a comma-separated permission-id set (createRole/updateRole), deletes via deleteRole, and resolves the attached permissions against listPermissions for the detail view. */
function RolesTab() {
  const token = useAppSelector(s => s.auth.token)!;
  const [roles, setRoles] = useState<Role[]>([]);
  const [permissions, setPermissions] = useState<Permission[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const [showCreate, setShowCreate] = useState(false);
  const [newName, setNewName] = useState('');
  const [newPermIds, setNewPermIds] = useState('');
  const [creating, setCreating] = useState(false);
  const [editing, setEditing] = useState(false);
  const [editName, setEditName] = useState('');
  const [editPermIds, setEditPermIds] = useState('');
  const [savingEdit, setSavingEdit] = useState(false);

  const fetchAll = useCallback(async () => {
    setLoading(true); setError(null);
    try {
      const [r, p] = await Promise.all([listRoles(token), listPermissions(token)]);
      setRoles(r); setPermissions(p);
    } catch (e: unknown) { setError((e as Error).message); }
    finally { setLoading(false); }
  }, [token]);

  useEffect(() => { fetchAll(); }, [fetchAll]);
  useEffect(() => { setEditing(false); }, [selected]);

  const selectedRole = roles.find(r => r.role_id === selected);

  const startEdit = (r: Role) => { setEditName(r.name ?? ''); setEditPermIds(r.permissions_ids.join(', ')); setError(null); setEditing(true); };

  const handleUpdate = async (id: string) => {
    const ids = editPermIds.split(',').map(s => s.trim()).filter(Boolean);
    setSavingEdit(true); setError(null);
    try {
      const updated = await updateRole(token, id, { name: editName.trim(), permissions_ids: ids });
      setRoles(prev => prev.map(r => r.role_id === id ? updated : r));
      setEditing(false);
    } catch (e: unknown) { setError((e as Error).message); }
    finally { setSavingEdit(false); }
  };
  const rolePermissions = selectedRole
    ? permissions.filter(p => selectedRole.permissions_ids.includes(p.permissions_id))
    : [];

  const handleCreate = async () => {
    const ids = newPermIds.split(',').map(s => s.trim()).filter(Boolean);
    setCreating(true); setError(null);
    try {
      const role = await createRole(token, { name: newName.trim() || undefined, permissions_ids: ids });
      setRoles(prev => [role, ...prev]);
      setNewName(''); setNewPermIds(''); setShowCreate(false);
    } catch (e: unknown) { setError((e as Error).message); }
    finally { setCreating(false); }
  };

  const [confirm, confirmEl] = useConfirm();
  const [railW, railHandle] = useResizableWidth('rail.gatekeeper.roles', 260, { min: 200, max: 480 });

  const handleDelete = async (id: string) => {
    const r = roles.find(x => x.role_id === id);
    if (!(await confirm({ message: `Delete role ${r ? roleLabel(r) : id}? Users assigned this role will lose its permissions.` }))) return;
    try {
      await deleteRole(token, id);
      setRoles(prev => prev.filter(r => r.role_id !== id));
      if (selected === id) setSelected(null);
    } catch (e: unknown) { setError((e as Error).message); }
  };

  return (
    <div style={{ display: 'flex', flex: 1, overflow: 'hidden' }}>
      {confirmEl}
      <div style={{ width: railW, flexShrink: 0, borderRight: `1px solid ${T.border}`, display: 'flex', flexDirection: 'column', background: T.bgAlt, overflow: 'auto' }}>
        <div style={{ padding: '12px 14px', borderBottom: `1px solid ${T.border}`, display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
          <span style={{ fontFamily: T.mono, fontSize: 10, color: T.faint }}>{roles.length > 0 ? `${roles.length} role${roles.length !== 1 ? 's' : ''}` : ''}</span>
          <div style={{ display: 'flex', gap: 6 }}>
            <button onClick={() => setShowCreate(v => !v)}
              style={{ background: showCreate ? T.greenSoft : 'transparent', border: `1px solid ${showCreate ? T.green : T.border}`, color: showCreate ? T.green : T.dim, fontFamily: T.mono, fontSize: 10, padding: '2px 6px', cursor: 'pointer' }}>+</button>
            <button onClick={fetchAll} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '2px 6px', cursor: 'pointer' }}>↻</button>
          </div>
        </div>
        {showCreate && (
          <div style={{ padding: '10px 14px', borderBottom: `1px solid ${T.border}`, background: T.card }}>
            {error && <div style={{ color: T.red, fontFamily: T.mono, fontSize: 10, marginBottom: 6 }}>{error}</div>}
            <input value={newName} onChange={e => setNewName(e.target.value)} placeholder="role name (e.g. ci-deployer)" autoFocus
              style={{ ...inputStyle, fontSize: 11, marginBottom: 6 }} />
            <input value={newPermIds} onChange={e => setNewPermIds(e.target.value)} placeholder="permission IDs (comma-separated)"
              style={{ ...inputStyle, fontSize: 11, marginBottom: 6 }} />
            <div style={{ display: 'flex', gap: 6 }}>
              <button onClick={handleCreate} disabled={creating}
                style={{ flex: 1, background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 10, fontWeight: 600, padding: '5px 0', cursor: 'pointer', opacity: creating ? 0.6 : 1 }}>
                {creating ? '[ · · · ]' : '[ create ]'}
              </button>
              <button onClick={() => setShowCreate(false)} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '5px 8px', cursor: 'pointer' }}>✕</button>
            </div>
          </div>
        )}
        {loading ? <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint, animation: 'pulse 1s ease-in-out infinite' }}>→ loading · · ·</div>
          : roles.length === 0 ? <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint }}>→ no roles</div>
          : roles.map(role => {
            const isActive = selected === role.role_id;
            return (
              <button key={role.role_id} onClick={() => setSelected(role.role_id)}
                style={{ width: '100%', textAlign: 'left', padding: '10px 14px', background: isActive ? T.greenSoft : 'transparent', border: 0, borderLeft: `2px solid ${isActive ? T.green : 'transparent'}`, fontFamily: T.mono, cursor: 'pointer', color: T.text, display: 'block', transition: 'background .12s' }}>
                <div style={{ fontSize: 12, fontWeight: 600, color: isActive ? T.textHi : T.text }}>{roleLabel(role)}</div>
                <div style={{ fontSize: 11, color: T.faint, marginTop: 2 }}>{role.permissions_ids.length} permission{role.permissions_ids.length !== 1 ? 's' : ''}</div>
              </button>
            );
          })}
      </div>
      {railHandle}
      <div style={{ flex: 1, overflow: 'auto' }}>
        {!selectedRole ? (
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', height: '100%' }}>
            <div style={{ fontFamily: T.mono, fontSize: 12, color: T.faint }}>→ select a role</div>
          </div>
        ) : (
          <div style={{ padding: '20px 24px' }}>
            <div style={{ display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between', marginBottom: 16 }}>
              <div>
                <div style={{ fontFamily: T.mono, fontSize: 16, fontWeight: 700, color: T.textHi }}>{roleLabel(selectedRole)}</div>
                <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, marginTop: 2 }}>{selectedRole.role_id}</div>
                <div style={{ fontFamily: T.mono, fontSize: 11, color: T.faint, marginTop: 2 }}>
                  updated {timeAgo(selectedRole.updated_at)} ago ·{' '}
                  {selectedRole.active ? <span style={{ color: T.green }}>active</span> : <span style={{ color: T.red }}>inactive</span>}
                </div>
              </div>
              <div style={{ display: 'flex', gap: 8 }}>
                <button onClick={() => editing ? setEditing(false) : startEdit(selectedRole)}
                  style={{ background: editing ? T.greenSoft : 'transparent', border: `1px solid ${editing ? T.green : T.border}`, color: editing ? T.green : T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}>
                  {editing ? '[ cancel ]' : '[ edit ]'}
                </button>
                <button onClick={() => handleDelete(selectedRole.role_id)}
                  style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}
                  onMouseEnter={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.red; (e.currentTarget as HTMLButtonElement).style.color = T.red; }}
                  onMouseLeave={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.border; (e.currentTarget as HTMLButtonElement).style.color = T.dim; }}>
                  [ delete ]
                </button>
              </div>
            </div>
            {editing && (
              <div style={{ background: T.card, border: `1px solid ${T.borderHi}`, padding: '14px 16px', marginBottom: 16 }}>
                <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, marginBottom: 8, letterSpacing: 0.5 }}>NAME</div>
                <input value={editName} onChange={e => setEditName(e.target.value)} placeholder="role name (e.g. ci-deployer)"
                  style={{ ...inputStyle, background: T.cardHi, marginBottom: 12 }} />
                <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, marginBottom: 8, letterSpacing: 0.5 }}>PERMISSION IDS · comma-separated</div>
                <textarea value={editPermIds} onChange={e => setEditPermIds(e.target.value)} rows={3}
                  style={{ ...inputStyle, background: T.cardHi, resize: 'vertical', marginBottom: 10 }} />
                <div style={{ display: 'flex', gap: 8 }}>
                  <button onClick={() => handleUpdate(selectedRole.role_id)} disabled={savingEdit}
                    style={{ background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 12, fontWeight: 600, padding: '7px 16px', cursor: 'pointer', opacity: savingEdit ? 0.6 : 1 }}>
                    {savingEdit ? '[ · · · ]' : '[ save ]'}
                  </button>
                  <button onClick={() => setEditing(false)} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '7px 12px', cursor: 'pointer' }}>cancel</button>
                </div>
              </div>
            )}
            <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 8 }}>PERMISSIONS · {rolePermissions.length}</div>
            {rolePermissions.length === 0 ? (
              <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '12px 14px', fontFamily: T.mono, fontSize: 12, color: T.faint }}>→ no permissions attached</div>
            ) : (
              <div style={{ background: T.card, border: `1px solid ${T.border}`, overflow: 'hidden' }}>
                {rolePermissions.map((p, i) => (
                  <div key={p.permissions_id} style={{ padding: '10px 14px', borderBottom: i < rolePermissions.length - 1 ? `1px solid ${T.border}` : 'none' }}>
                    <div style={{ display: 'flex', alignItems: 'center', gap: 10, marginBottom: 4 }}>
                      <span style={{ fontFamily: T.mono, fontSize: 13, color: T.textHi, fontWeight: 600 }}>{p.name}</span>
                      <span style={{ fontFamily: T.mono, fontSize: 10, color: T.blue, border: `1px solid ${T.blue}`, padding: '1px 5px' }}>{p.service}</span>
                    </div>
                    <div style={{ fontFamily: T.mono, fontSize: 11, color: T.dim }}><span style={{ color: T.faint }}>actions  </span>{p.actions.join(', ')}</div>
                    <div style={{ fontFamily: T.mono, fontSize: 11, color: T.dim }}><span style={{ color: T.faint }}>resources </span>{p.resources.join(', ')}</div>
                  </div>
                ))}
              </div>
            )}
          </div>
        )}
      </div>
    </div>
  );
}

// ── Permissions tab ───────────────────────────────────────────────────────────

/** permissions tab: master/detail list of permissions; create + inline-edit a permission's name/service/actions/resources (createPermission/updatePermission) and delete via deletePermission. */
function PermissionsTab() {
  const token = useAppSelector(s => s.auth.token)!;
  const [perms, setPerms] = useState<Permission[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const [showCreate, setShowCreate] = useState(false);
  const [form, setForm] = useState({ name: '', service: '', actions: '', resources: '' });
  const [creating, setCreating] = useState(false);
  const [editing, setEditing] = useState(false);
  const [editForm, setEditForm] = useState({ name: '', service: '', actions: '', resources: '' });
  const [savingEdit, setSavingEdit] = useState(false);

  const fetchPerms = useCallback(async () => {
    setLoading(true); setError(null);
    try { setPerms(await listPermissions(token)); }
    catch (e: unknown) { setError((e as Error).message); }
    finally { setLoading(false); }
  }, [token]);

  useEffect(() => { fetchPerms(); }, [fetchPerms]);
  useEffect(() => { setEditing(false); }, [selected]);

  const selectedPerm = perms.find(p => p.permissions_id === selected);

  const startEdit = (p: Permission) => {
    setEditForm({ name: p.name, service: p.service, actions: p.actions.join(', '), resources: p.resources.join(', ') });
    setError(null); setEditing(true);
  };

  const handleUpdate = async (id: string) => {
    if (!editForm.name.trim() || !editForm.service.trim()) return;
    setSavingEdit(true); setError(null);
    try {
      const updated = await updatePermission(token, id, {
        name: editForm.name.trim(),
        service: editForm.service.trim(),
        actions: editForm.actions.split(',').map(s => s.trim()).filter(Boolean),
        resources: editForm.resources.split(',').map(s => s.trim()).filter(Boolean),
      });
      setPerms(prev => prev.map(p => p.permissions_id === id ? updated : p));
      setEditing(false);
    } catch (e: unknown) { setError((e as Error).message); }
    finally { setSavingEdit(false); }
  };

  const handleCreate = async () => {
    if (!form.name.trim() || !form.service.trim()) return;
    setCreating(true); setError(null);
    try {
      const p = await createPermission(token, {
        name: form.name.trim(),
        service: form.service.trim(),
        actions: form.actions.split(',').map(s => s.trim()).filter(Boolean),
        resources: form.resources.split(',').map(s => s.trim()).filter(Boolean),
      });
      setPerms(prev => [p, ...prev]);
      setForm({ name: '', service: '', actions: '', resources: '' });
      setShowCreate(false);
    } catch (e: unknown) { setError((e as Error).message); }
    finally { setCreating(false); }
  };

  const [confirm, confirmEl] = useConfirm();
  const [railW, railHandle] = useResizableWidth('rail.gatekeeper.permissions', 260, { min: 200, max: 480 });

  const handleDelete = async (id: string) => {
    const name = perms.find(p => p.permissions_id === id)?.name;
    if (!(await confirm({ message: `Delete permission ${name ?? id}? Roles referencing it will lose these grants.` }))) return;
    try {
      await deletePermission(token, id);
      setPerms(prev => prev.filter(p => p.permissions_id !== id));
      if (selected === id) setSelected(null);
    } catch (e: unknown) { setError((e as Error).message); }
  };

  return (
    <div style={{ display: 'flex', flex: 1, overflow: 'hidden' }}>
      {confirmEl}
      <div style={{ width: railW, flexShrink: 0, borderRight: `1px solid ${T.border}`, display: 'flex', flexDirection: 'column', background: T.bgAlt, overflow: 'auto' }}>
        <div style={{ padding: '12px 14px', borderBottom: `1px solid ${T.border}`, display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
          <span style={{ fontFamily: T.mono, fontSize: 10, color: T.faint }}>{perms.length > 0 ? `${perms.length} permission${perms.length !== 1 ? 's' : ''}` : ''}</span>
          <div style={{ display: 'flex', gap: 6 }}>
            <button onClick={() => setShowCreate(v => !v)}
              style={{ background: showCreate ? T.greenSoft : 'transparent', border: `1px solid ${showCreate ? T.green : T.border}`, color: showCreate ? T.green : T.dim, fontFamily: T.mono, fontSize: 10, padding: '2px 6px', cursor: 'pointer' }}>+</button>
            <button onClick={fetchPerms} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '2px 6px', cursor: 'pointer' }}>↻</button>
          </div>
        </div>
        {showCreate && (
          <div style={{ padding: '10px 14px', borderBottom: `1px solid ${T.border}`, background: T.card }}>
            {error && <div style={{ color: T.red, fontFamily: T.mono, fontSize: 10, marginBottom: 6 }}>{error}</div>}
            {(['name', 'service', 'actions', 'resources'] as const).map(field => (
              <input key={field} value={form[field]} onChange={e => setForm(f => ({ ...f, [field]: e.target.value }))}
                placeholder={field === 'actions' || field === 'resources' ? `${field} (comma-separated)` : field}
                style={{ ...inputStyle, fontSize: 11, marginBottom: 6 }} />
            ))}
            <div style={{ display: 'flex', gap: 6 }}>
              <button onClick={handleCreate} disabled={!form.name.trim() || !form.service.trim() || creating}
                style={{ flex: 1, background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 10, fontWeight: 600, padding: '5px 0', cursor: 'pointer', opacity: (!form.name.trim() || !form.service.trim() || creating) ? 0.6 : 1 }}>
                {creating ? '[ · · · ]' : '[ create ]'}
              </button>
              <button onClick={() => setShowCreate(false)} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '5px 8px', cursor: 'pointer' }}>✕</button>
            </div>
          </div>
        )}
        {loading ? <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint, animation: 'pulse 1s ease-in-out infinite' }}>→ loading · · ·</div>
          : perms.length === 0 ? <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint }}>→ no permissions</div>
          : perms.map(p => {
            const isActive = selected === p.permissions_id;
            return (
              <button key={p.permissions_id} onClick={() => setSelected(p.permissions_id)}
                style={{ width: '100%', textAlign: 'left', padding: '10px 14px', background: isActive ? T.greenSoft : 'transparent', border: 0, borderLeft: `2px solid ${isActive ? T.green : 'transparent'}`, fontFamily: T.mono, cursor: 'pointer', color: T.text, display: 'block', transition: 'background .12s' }}>
                <div style={{ fontSize: 12, fontWeight: 600, color: isActive ? T.textHi : T.text }}>{p.name}</div>
                <div style={{ fontSize: 10, color: T.blue, marginTop: 2 }}>{p.service}</div>
              </button>
            );
          })}
      </div>
      {railHandle}
      <div style={{ flex: 1, overflow: 'auto' }}>
        {!selectedPerm ? (
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', height: '100%' }}>
            <div style={{ fontFamily: T.mono, fontSize: 12, color: T.faint }}>→ select a permission</div>
          </div>
        ) : (
          <div style={{ padding: '20px 24px' }}>
            <div style={{ display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between', marginBottom: 20 }}>
              <div>
                <div style={{ fontFamily: T.mono, fontSize: 18, fontWeight: 700, color: T.textHi, marginBottom: 4 }}>{selectedPerm.name}</div>
                <div style={{ fontFamily: T.mono, fontSize: 11, color: T.blue }}>{selectedPerm.service}</div>
                <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, marginTop: 4 }}>updated {timeAgo(selectedPerm.updated_at)} ago</div>
              </div>
              <div style={{ display: 'flex', gap: 8 }}>
                <button onClick={() => editing ? setEditing(false) : startEdit(selectedPerm)}
                  style={{ background: editing ? T.greenSoft : 'transparent', border: `1px solid ${editing ? T.green : T.border}`, color: editing ? T.green : T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}>
                  {editing ? '[ cancel ]' : '[ edit ]'}
                </button>
                <button onClick={() => handleDelete(selectedPerm.permissions_id)}
                  style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}
                  onMouseEnter={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.red; (e.currentTarget as HTMLButtonElement).style.color = T.red; }}
                  onMouseLeave={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.border; (e.currentTarget as HTMLButtonElement).style.color = T.dim; }}>
                  [ delete ]
                </button>
              </div>
            </div>
            {editing ? (
              <div style={{ background: T.card, border: `1px solid ${T.borderHi}`, padding: '16px' }}>
                {(['name', 'service', 'actions', 'resources'] as const).map(field => (
                  <div key={field} style={{ marginBottom: 10 }}>
                    <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, marginBottom: 6, letterSpacing: 0.5, textTransform: 'uppercase' }}>{field === 'actions' || field === 'resources' ? `${field} · comma-separated` : field}</div>
                    <input value={editForm[field]} onChange={e => setEditForm(f => ({ ...f, [field]: e.target.value }))} style={{ ...inputStyle, background: T.cardHi }} />
                  </div>
                ))}
                <div style={{ display: 'flex', gap: 8, marginTop: 4 }}>
                  <button onClick={() => handleUpdate(selectedPerm.permissions_id)} disabled={!editForm.name.trim() || !editForm.service.trim() || savingEdit}
                    style={{ background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 12, fontWeight: 600, padding: '7px 16px', cursor: 'pointer', opacity: (!editForm.name.trim() || !editForm.service.trim() || savingEdit) ? 0.6 : 1 }}>
                    {savingEdit ? '[ · · · ]' : '[ save ]'}
                  </button>
                  <button onClick={() => setEditing(false)} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '7px 12px', cursor: 'pointer' }}>cancel</button>
                </div>
              </div>
            ) : (
            <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 12 }}>
              {([['actions', selectedPerm.actions.join(', ')], ['resources', selectedPerm.resources.join(', ')]] as [string, string][]).map(([k, v]) => (
                <div key={k} style={{ background: T.card, border: `1px solid ${T.border}`, padding: '10px 14px' }}>
                  <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 6, textTransform: 'uppercase' }}>{k}</div>
                  <div style={{ fontFamily: T.mono, fontSize: 12, color: T.text }}>{v || '—'}</div>
                </div>
              ))}
            </div>
            )}
          </div>
        )}
      </div>
    </div>
  );
}

// ── Secret backend (per-org provider) ──────────────────────────────────────────

const PROVIDERS: { id: SecretProviderName; label: string; help: string }[] = [
  { id: 'builtin', label: 'builtin', help: 'Encrypted in the platform database (default).' },
  { id: 'vault', label: 'vault', help: 'HashiCorp Vault. Config requires an address, e.g. {"address":"https://vault.example.com"}' },
  { id: 'doppler', label: 'doppler', help: 'Doppler secrets manager.' },
  { id: 'aws_sm', label: 'aws_sm', help: 'AWS Secrets Manager.' },
];

/** secret backend panel: shows/sets the caller's org's secret provider — picks builtin/vault/doppler/aws_sm and posts optional JSON config via setSecretProvider, or resets to builtin via deleteSecretProvider. */
function SecretProviderPanel() {
  const token = useAppSelector(s => s.auth.token)!;
  const orgId = useAppSelector(s => s.auth.user?.org_id);
  const [current, setCurrent] = useState<string>('builtin');
  const [choice, setChoice] = useState<SecretProviderName>('builtin');
  const [config, setConfig] = useState('');
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [confirm, confirmEl] = useConfirm();

  const load = useCallback(async () => {
    if (!orgId) { setLoading(false); return; }
    setLoading(true); setError(null);
    try {
      const p: SecretProvider | null = await getSecretProvider(token, orgId);
      const name = p?.provider ?? 'builtin';
      setCurrent(name); setChoice(name as SecretProviderName);
    } catch (e: unknown) { setError((e as Error).message); }
    finally { setLoading(false); }
  }, [token, orgId]);

  useEffect(() => { load(); }, [load]);

  const apply = async () => {
    if (!orgId) return;
    if (choice === 'builtin' && current !== 'builtin'
      && !(await confirm({ message: `Reset the secret backend from ${current} to builtin? The ${current} configuration is removed and secrets are served from the platform database.`, confirmLabel: 'reset to builtin' }))) return;
    setSaving(true); setError(null); setNotice(null);
    try {
      let cfg: Record<string, unknown> | undefined;
      if (choice !== 'builtin' && config.trim()) {
        try { cfg = JSON.parse(config); }
        catch { setError('config must be valid JSON'); setSaving(false); return; }
      }
      if (choice === 'builtin') await deleteSecretProvider(token, orgId);
      else await setSecretProvider(token, orgId, choice, cfg);
      setCurrent(choice); setConfig(''); setNotice('secret backend updated');
      setTimeout(() => setNotice(null), 3000);
    } catch (e: unknown) { setError((e as Error).message); }
    finally { setSaving(false); }
  };

  if (!orgId) return null;

  return (
    <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '16px', marginBottom: 20 }}>
      {confirmEl}
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 12 }}>
        <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1 }}>SECRET BACKEND</div>
        {loading ? <span style={{ fontFamily: T.mono, fontSize: 10, color: T.faint }}>· · ·</span>
          : <Pill tone={current === 'builtin' ? 'dim' : 'green'}>{current}</Pill>}
      </div>
      {error && <div style={{ background: T.redSoft, border: `1px solid ${T.red}`, padding: '8px 12px', fontFamily: T.mono, fontSize: 11, color: T.red, marginBottom: 12 }}>{error}</div>}
      {notice && <div style={{ background: T.greenSoft, border: `1px solid ${T.green}`, padding: '8px 12px', fontFamily: T.mono, fontSize: 11, color: T.green, marginBottom: 12 }}>→ {notice}</div>}
      <div style={{ display: 'flex', gap: 8, marginBottom: 10, flexWrap: 'wrap' }}>
        {PROVIDERS.map(p => (
          <button key={p.id} onClick={() => setChoice(p.id)}
            style={{ background: choice === p.id ? T.greenSoft : 'transparent', border: `1px solid ${choice === p.id ? T.green : T.border}`, color: choice === p.id ? T.green : T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}>
            {p.label}
          </button>
        ))}
      </div>
      <div style={{ fontFamily: T.mono, fontSize: 11, color: T.faint, marginBottom: 10 }}>{PROVIDERS.find(p => p.id === choice)?.help}</div>
      {choice !== 'builtin' && (
        <textarea value={config} onChange={e => setConfig(e.target.value)} rows={3}
          placeholder={choice === 'vault' ? '{"address":"https://vault.example.com"}' : '{ }  · provider config JSON (optional)'}
          style={{ ...inputStyle, background: T.cardHi, resize: 'vertical', marginBottom: 10 }} />
      )}
      <button onClick={apply} disabled={saving || (choice === current && !config.trim())}
        style={{ background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 12, fontWeight: 600, padding: '7px 16px', cursor: 'pointer', opacity: (saving || (choice === current && !config.trim())) ? 0.6 : 1 }}>
        {saving ? '[ · · · ]' : choice === 'builtin' ? '[ reset to builtin ]' : '[ apply ]'}
      </button>
    </div>
  );
}

// ── Secrets tab ───────────────────────────────────────────────────────────────

/** secrets tab: renders the secret-backend panel plus a flat list of secrets; create (createSecret), rotate the value in place (updateSecret), and delete (deleteSecret) — values are write-only, never displayed. */
function SecretsTab() {
  const token = useAppSelector(s => s.auth.token)!;
  const [secrets, setSecrets] = useState<Secret[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [showCreate, setShowCreate] = useState(false);
  const [newName, setNewName] = useState('');
  const [newValue, setNewValue] = useState('');
  const [creating, setCreating] = useState(false);
  const [deletingId, setDeletingId] = useState<string | null>(null);
  const [editingId, setEditingId] = useState<string | null>(null);
  const [editValue, setEditValue] = useState('');
  const [savingId, setSavingId] = useState<string | null>(null);

  const fetchSecrets = useCallback(async () => {
    setLoading(true); setError(null);
    try { setSecrets(await listSecrets(token)); }
    catch (e: unknown) { setError((e as Error).message); }
    finally { setLoading(false); }
  }, [token]);

  useEffect(() => { fetchSecrets(); }, [fetchSecrets]);

  const handleUpdate = async (id: string) => {
    if (!editValue.trim()) return;
    setSavingId(id); setError(null);
    try {
      const updated = await updateSecret(token, id, editValue.trim());
      setSecrets(prev => prev.map(s => s.secret_id === id ? updated : s));
      setEditingId(null); setEditValue('');
    } catch (e: unknown) { setError((e as Error).message); }
    finally { setSavingId(null); }
  };

  const handleCreate = async () => {
    if (!newName.trim() || !newValue.trim()) return;
    setCreating(true); setError(null);
    try {
      const s = await createSecret(token, { name: newName.trim(), value: newValue.trim() });
      setSecrets(prev => [s, ...prev]);
      setNewName(''); setNewValue(''); setShowCreate(false);
    } catch (e: unknown) { setError((e as Error).message); }
    finally { setCreating(false); }
  };

  const [confirm, confirmEl] = useConfirm();

  const handleDelete = async (id: string) => {
    const name = secrets.find(s => s.secret_id === id)?.name;
    if (!(await confirm({ message: `Delete secret ${name ?? id}? Services relying on it will lose access.` }))) return;
    setDeletingId(id);
    try {
      await deleteSecret(token, id);
      setSecrets(prev => prev.filter(s => s.secret_id !== id));
    } catch (e: unknown) { setError((e as Error).message); }
    finally { setDeletingId(null); }
  };

  return (
    <div style={{ flex: 1, overflow: 'auto', padding: '20px 24px' }}>
      {confirmEl}
      <SecretProviderPanel />
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 20 }}>
        <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1 }}>SECRETS · {secrets.length}</div>
        <div style={{ display: 'flex', gap: 8 }}>
          <button onClick={() => setShowCreate(v => !v)}
            style={{ background: showCreate ? T.greenSoft : 'transparent', border: `1px solid ${showCreate ? T.green : T.border}`, color: showCreate ? T.green : T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}>
            + new
          </button>
          <button onClick={fetchSecrets} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 10px', cursor: 'pointer' }}>↻</button>
        </div>
      </div>
      {showCreate && (
        <div style={{ background: T.card, border: `1px solid ${T.borderHi}`, padding: '16px', marginBottom: 20 }}>
          {error && <div style={{ background: T.redSoft, border: `1px solid ${T.red}`, padding: '8px 12px', fontFamily: T.mono, fontSize: 11, color: T.red, marginBottom: 12 }}>{error}</div>}
          <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 12, marginBottom: 12 }}>
            <div>
              <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, marginBottom: 6, letterSpacing: 0.5 }}>NAME</div>
              <input value={newName} onChange={e => setNewName(e.target.value)} autoFocus placeholder="MY_SECRET" style={{ ...inputStyle, background: T.cardHi }} />
            </div>
            <div>
              <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, marginBottom: 6, letterSpacing: 0.5 }}>VALUE</div>
              <input value={newValue} onChange={e => setNewValue(e.target.value)} type="password" placeholder="••••••••" onKeyDown={e => e.key === 'Enter' && handleCreate()} style={{ ...inputStyle, background: T.cardHi }} />
            </div>
          </div>
          <div style={{ display: 'flex', gap: 8 }}>
            <button onClick={handleCreate} disabled={!newName.trim() || !newValue.trim() || creating}
              style={{ background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 12, fontWeight: 600, padding: '7px 16px', cursor: 'pointer', opacity: (!newName.trim() || !newValue.trim() || creating) ? 0.6 : 1 }}>
              {creating ? '[ · · · ]' : '[ create ]'}
            </button>
            <button onClick={() => setShowCreate(false)} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '7px 12px', cursor: 'pointer' }}>cancel</button>
          </div>
        </div>
      )}
      {error && !showCreate && <div style={{ background: T.redSoft, border: `1px solid ${T.red}`, padding: '10px 14px', fontFamily: T.mono, fontSize: 11, color: T.red, marginBottom: 16 }}>{error}</div>}
      {loading ? <div style={{ fontFamily: T.mono, fontSize: 11, color: T.faint, animation: 'pulse 1s ease-in-out infinite' }}>→ loading · · ·</div>
        : secrets.length === 0 ? <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '20px', fontFamily: T.mono, fontSize: 12, color: T.faint, textAlign: 'center' }}>→ no secrets</div>
        : (
          <div style={{ background: T.card, border: `1px solid ${T.border}`, overflow: 'hidden' }}>
            {secrets.map((s, i) => (
              <div key={s.secret_id} style={{ display: 'flex', alignItems: 'center', padding: '10px 14px', borderBottom: i < secrets.length - 1 ? `1px solid ${T.border}` : 'none', gap: 12 }}>
                <span style={{ fontFamily: T.mono, fontSize: 13, color: T.textHi, fontWeight: 600, flex: editingId === s.secret_id ? 0 : 1, whiteSpace: 'nowrap' }}>{s.name}</span>
                {editingId === s.secret_id ? (
                  <>
                    <input value={editValue} onChange={e => setEditValue(e.target.value)} type="password" autoFocus placeholder="new value"
                      onKeyDown={e => e.key === 'Enter' && handleUpdate(s.secret_id)}
                      style={{ ...inputStyle, flex: 1, background: T.cardHi, padding: '4px 8px' }} />
                    <button onClick={() => handleUpdate(s.secret_id)} disabled={!editValue.trim() || savingId === s.secret_id}
                      style={{ background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 10, fontWeight: 600, padding: '4px 10px', cursor: 'pointer', opacity: (!editValue.trim() || savingId === s.secret_id) ? 0.6 : 1 }}>
                      {savingId === s.secret_id ? '· · ·' : 'save'}
                    </button>
                    <button onClick={() => { setEditingId(null); setEditValue(''); }}
                      style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '4px 8px', cursor: 'pointer' }}>✕</button>
                  </>
                ) : (
                  <>
                    <span style={{ fontFamily: T.mono, fontSize: 11, color: T.faint }}>updated {timeAgo(s.updated_at)} ago</span>
                    <button onClick={() => { setEditingId(s.secret_id); setEditValue(''); setError(null); }}
                      style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 8px', cursor: 'pointer' }}>
                      [ rotate ]
                    </button>
                    <button onClick={() => handleDelete(s.secret_id)} disabled={deletingId === s.secret_id}
                      style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 8px', cursor: 'pointer', opacity: deletingId === s.secret_id ? 0.5 : 1 }}
                      onMouseEnter={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.red; (e.currentTarget as HTMLButtonElement).style.color = T.red; }}
                      onMouseLeave={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.border; (e.currentTarget as HTMLButtonElement).style.color = T.dim; }}>
                      [ delete ]
                    </button>
                  </>
                )}
              </div>
            ))}
          </div>
        )}
    </div>
  );
}

// ── Teams tab ─────────────────────────────────────────────────────────────────

/** teams tab: master/detail list of teams; create (createTeam, optional role_id), rename (updateTeam), delete (deleteTeam), and invite a member by email (inviteToTeam) from the detail pane. */
function TeamsTab() {
  const token = useAppSelector(s => s.auth.token)!;
  const [teams, setTeams] = useState<Team[]>([]);
  const [roles, setRoles] = useState<Role[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const [showCreate, setShowCreate] = useState(false);
  const [newName, setNewName] = useState('');
  const [newRoleId, setNewRoleId] = useState('');
  const [creating, setCreating] = useState(false);
  const [inviteEmail, setInviteEmail] = useState('');
  const [inviting, setInviting] = useState(false);
  const [inviteSuccess, setInviteSuccess] = useState(false);
  const [editing, setEditing] = useState(false);
  const [editName, setEditName] = useState('');
  const [savingEdit, setSavingEdit] = useState(false);

  const fetchTeams = useCallback(async () => {
    setLoading(true); setError(null);
    try { setTeams(await listTeams(token)); }
    catch (e: unknown) { setError((e as Error).message); }
    finally { setLoading(false); }
  }, [token]);

  useEffect(() => { fetchTeams(); }, [fetchTeams]);
  // Roles back the create form's role picker and resolve a team's role_id to a
  // name in the detail pane. Best effort — the picker degrades to "no roles".
  useEffect(() => { listRoles(token).then(setRoles).catch(() => {}); }, [token]);
  useEffect(() => { setEditing(false); }, [selected]);

  const selectedTeam = teams.find(t => t.team_id === selected);
  const roleById = useMemo(() => new Map(roles.map(r => [r.role_id, r])), [roles]);

  const handleRename = async (id: string) => {
    if (!editName.trim()) return;
    setSavingEdit(true); setError(null);
    try {
      const updated = await updateTeam(token, id, { team_name: editName.trim() });
      setTeams(prev => prev.map(t => t.team_id === id ? updated : t));
      setEditing(false);
    } catch (e: unknown) { setError((e as Error).message); }
    finally { setSavingEdit(false); }
  };

  const handleCreate = async () => {
    if (!newName.trim()) return;
    setCreating(true); setError(null);
    try {
      const t = await createTeam(token, { team_name: newName.trim(), role_id: newRoleId.trim() || undefined });
      setTeams(prev => [t, ...prev]);
      setNewName(''); setNewRoleId(''); setShowCreate(false);
    } catch (e: unknown) { setError((e as Error).message); }
    finally { setCreating(false); }
  };

  const [confirm, confirmEl] = useConfirm();
  const [railW, railHandle] = useResizableWidth('rail.gatekeeper.teams', 260, { min: 200, max: 480 });

  const handleDelete = async (id: string) => {
    const name = teams.find(t => t.team_id === id)?.team_name;
    if (!(await confirm({ message: `Delete team ${name ?? id}? Its memberships will be removed.` }))) return;
    try {
      await deleteTeam(token, id);
      setTeams(prev => prev.filter(t => t.team_id !== id));
      if (selected === id) setSelected(null);
    } catch (e: unknown) { setError((e as Error).message); }
  };

  const handleInvite = async () => {
    if (!selected || !inviteEmail.trim()) return;
    setInviting(true); setError(null);
    try {
      await inviteToTeam(token, selected, inviteEmail.trim());
      setInviteEmail(''); setInviteSuccess(true);
      setTimeout(() => setInviteSuccess(false), 3000);
    } catch (e: unknown) { setError((e as Error).message); }
    finally { setInviting(false); }
  };

  return (
    <div style={{ display: 'flex', flex: 1, overflow: 'hidden' }}>
      {confirmEl}
      <div style={{ width: railW, flexShrink: 0, borderRight: `1px solid ${T.border}`, display: 'flex', flexDirection: 'column', background: T.bgAlt, overflow: 'auto' }}>
        <div style={{ padding: '12px 14px', borderBottom: `1px solid ${T.border}`, display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
          <span style={{ fontFamily: T.mono, fontSize: 10, color: T.faint }}>{teams.length > 0 ? `${teams.length} team${teams.length !== 1 ? 's' : ''}` : ''}</span>
          <div style={{ display: 'flex', gap: 6 }}>
            <button onClick={() => setShowCreate(v => !v)}
              style={{ background: showCreate ? T.greenSoft : 'transparent', border: `1px solid ${showCreate ? T.green : T.border}`, color: showCreate ? T.green : T.dim, fontFamily: T.mono, fontSize: 10, padding: '2px 6px', cursor: 'pointer' }}>+</button>
            <button onClick={fetchTeams} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '2px 6px', cursor: 'pointer' }}>↻</button>
          </div>
        </div>
        {showCreate && (
          <div style={{ padding: '10px 14px', borderBottom: `1px solid ${T.border}`, background: T.card }}>
            {error && <div style={{ color: T.red, fontFamily: T.mono, fontSize: 10, marginBottom: 6 }}>{error}</div>}
            <input value={newName} onChange={e => setNewName(e.target.value)} placeholder="team name" autoFocus style={{ ...inputStyle, fontSize: 11, marginBottom: 6 }} />
            <select value={newRoleId} onChange={e => setNewRoleId(e.target.value)} style={{ ...inputStyle, fontSize: 11, marginBottom: 6 }}>
              <option value="">no role</option>
              {roles.map(r => <option key={r.role_id} value={r.role_id}>{roleLabel(r)}</option>)}
            </select>
            <div style={{ display: 'flex', gap: 6 }}>
              <button onClick={handleCreate} disabled={!newName.trim() || creating}
                style={{ flex: 1, background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 10, fontWeight: 600, padding: '5px 0', cursor: 'pointer', opacity: (!newName.trim() || creating) ? 0.6 : 1 }}>
                {creating ? '[ · · · ]' : '[ create ]'}
              </button>
              <button onClick={() => setShowCreate(false)} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '5px 8px', cursor: 'pointer' }}>✕</button>
            </div>
          </div>
        )}
        {loading ? <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint, animation: 'pulse 1s ease-in-out infinite' }}>→ loading · · ·</div>
          : teams.length === 0 ? <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint }}>→ no teams</div>
          : teams.map(t => {
            const isActive = selected === t.team_id;
            return (
              <button key={t.team_id} onClick={() => setSelected(t.team_id)}
                style={{ width: '100%', textAlign: 'left', padding: '10px 14px', background: isActive ? T.greenSoft : 'transparent', border: 0, borderLeft: `2px solid ${isActive ? T.green : 'transparent'}`, fontFamily: T.mono, cursor: 'pointer', color: T.text, display: 'block', transition: 'background .12s' }}>
                <div style={{ fontSize: 13, fontWeight: 600, color: isActive ? T.textHi : T.text }}>{t.team_name}</div>
                <div style={{ fontSize: 10, color: T.faint, marginTop: 2 }}>{t.team_id.slice(0, 8)}…</div>
              </button>
            );
          })}
      </div>
      {railHandle}
      <div style={{ flex: 1, overflow: 'auto' }}>
        {!selectedTeam ? (
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', height: '100%' }}>
            <div style={{ fontFamily: T.mono, fontSize: 12, color: T.faint }}>→ select a team</div>
          </div>
        ) : (
          <div style={{ padding: '20px 24px' }}>
            <div style={{ display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between', marginBottom: 20 }}>
              <div>
                <div style={{ fontFamily: T.mono, fontSize: 20, fontWeight: 700, color: T.textHi, marginBottom: 4 }}>{selectedTeam.team_name}</div>
                <div style={{ fontFamily: T.mono, fontSize: 11, color: T.faint }}>updated {timeAgo(selectedTeam.updated_at)} ago</div>
                {selectedTeam.role_id && <div style={{ fontFamily: T.mono, fontSize: 11, color: T.dim, marginTop: 2 }}>role: {(() => { const r = roleById.get(selectedTeam.role_id); return r ? roleLabel(r) : shortId(selectedTeam.role_id); })()}</div>}
              </div>
              <div style={{ display: 'flex', gap: 8 }}>
                <button onClick={() => { if (editing) { setEditing(false); } else { setEditName(selectedTeam.team_name); setError(null); setEditing(true); } }}
                  style={{ background: editing ? T.greenSoft : 'transparent', border: `1px solid ${editing ? T.green : T.border}`, color: editing ? T.green : T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}>
                  {editing ? '[ cancel ]' : '[ rename ]'}
                </button>
                <button onClick={() => handleDelete(selectedTeam.team_id)}
                  style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}
                  onMouseEnter={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.red; (e.currentTarget as HTMLButtonElement).style.color = T.red; }}
                  onMouseLeave={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.border; (e.currentTarget as HTMLButtonElement).style.color = T.dim; }}>
                  [ delete ]
                </button>
              </div>
            </div>
            {editing && (
              <div style={{ display: 'flex', gap: 8, marginBottom: 16 }}>
                <input value={editName} onChange={e => setEditName(e.target.value)} autoFocus placeholder="new team name"
                  onKeyDown={e => e.key === 'Enter' && handleRename(selectedTeam.team_id)}
                  style={{ ...inputStyle, flex: 1, background: T.cardHi }} />
                <button onClick={() => handleRename(selectedTeam.team_id)} disabled={!editName.trim() || savingEdit}
                  style={{ background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 11, fontWeight: 600, padding: '7px 14px', cursor: 'pointer', opacity: (!editName.trim() || savingEdit) ? 0.6 : 1 }}>
                  {savingEdit ? '[ · · · ]' : '[ save ]'}
                </button>
              </div>
            )}
            <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 8 }}>INVITE MEMBER</div>
            <div style={{ display: 'flex', gap: 8, marginBottom: error ? 12 : 0 }}>
              <input value={inviteEmail} onChange={e => setInviteEmail(e.target.value)} placeholder="email address"
                onKeyDown={e => e.key === 'Enter' && handleInvite()}
                style={{ ...inputStyle, flex: 1, background: T.card }} />
              <button onClick={handleInvite} disabled={!inviteEmail.trim() || inviting}
                style={{ background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 11, fontWeight: 600, padding: '7px 14px', cursor: 'pointer', opacity: (!inviteEmail.trim() || inviting) ? 0.6 : 1 }}>
                {inviting ? '[ · · · ]' : inviteSuccess ? '[ sent! ]' : '[ invite ]'}
              </button>
            </div>
            {error && <div style={{ background: T.redSoft, border: `1px solid ${T.red}`, padding: '8px 12px', fontFamily: T.mono, fontSize: 11, color: T.red, marginTop: 8 }}>{error}</div>}
          </div>
        )}
      </div>
    </div>
  );
}

// ── Orgs tab ──────────────────────────────────────────────────────────────────

/** orgs tab: master/detail list of orgs; create (createOrg), rename (updateOrg), delete (deleteOrg), and invite a member by email (inviteToOrg) from the detail pane. */
function OrgsTab() {
  const token = useAppSelector(s => s.auth.token)!;
  const [orgs, setOrgs] = useState<Org[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const [inviteEmail, setInviteEmail] = useState('');
  const [inviting, setInviting] = useState(false);
  const [inviteSuccess, setInviteSuccess] = useState(false);
  const [showCreate, setShowCreate] = useState(false);
  const [newName, setNewName] = useState('');
  const [creating, setCreating] = useState(false);
  const [editing, setEditing] = useState(false);
  const [editName, setEditName] = useState('');
  const [savingEdit, setSavingEdit] = useState(false);

  const fetchOrgs = useCallback(async () => {
    setLoading(true); setError(null);
    try { setOrgs(await listOrgs(token)); }
    catch (e: unknown) { setError((e as Error).message); }
    finally { setLoading(false); }
  }, [token]);

  useEffect(() => { fetchOrgs(); }, [fetchOrgs]);
  useEffect(() => { setEditing(false); }, [selected]);

  const selectedOrg = orgs.find(o => o.org_id === selected);

  const handleCreate = async () => {
    if (!newName.trim()) return;
    setCreating(true); setError(null);
    try {
      const o = await createOrg(token, newName.trim());
      setOrgs(prev => [o, ...prev]);
      setNewName(''); setShowCreate(false);
    } catch (e: unknown) { setError((e as Error).message); }
    finally { setCreating(false); }
  };

  const handleRename = async (id: string) => {
    if (!editName.trim()) return;
    setSavingEdit(true); setError(null);
    try {
      const updated = await updateOrg(token, id, editName.trim());
      setOrgs(prev => prev.map(o => o.org_id === id ? updated : o));
      setEditing(false);
    } catch (e: unknown) { setError((e as Error).message); }
    finally { setSavingEdit(false); }
  };

  const [confirm, confirmEl] = useConfirm();
  const [railW, railHandle] = useResizableWidth('rail.gatekeeper.orgs', 260, { min: 200, max: 480 });

  const handleDelete = async (id: string) => {
    const name = orgs.find(o => o.org_id === id)?.org_name;
    if (!(await confirm({ message: `Delete org ${name ?? id}? This removes the organization and its scoped resources.` }))) return;
    try {
      await deleteOrg(token, id);
      setOrgs(prev => prev.filter(o => o.org_id !== id));
      if (selected === id) setSelected(null);
    } catch (e: unknown) { setError((e as Error).message); }
  };

  const handleInvite = async () => {
    if (!selected || !inviteEmail.trim()) return;
    setInviting(true); setError(null);
    try {
      await inviteToOrg(token, selected, inviteEmail.trim());
      setInviteEmail(''); setInviteSuccess(true);
      setTimeout(() => setInviteSuccess(false), 3000);
    } catch (e: unknown) { setError((e as Error).message); }
    finally { setInviting(false); }
  };

  return (
    <div style={{ display: 'flex', flex: 1, overflow: 'hidden' }}>
      {confirmEl}
      <div style={{ width: railW, flexShrink: 0, borderRight: `1px solid ${T.border}`, display: 'flex', flexDirection: 'column', background: T.bgAlt, overflow: 'auto' }}>
        <div style={{ padding: '12px 14px', borderBottom: `1px solid ${T.border}`, display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
          <span style={{ fontFamily: T.mono, fontSize: 10, color: T.faint }}>{orgs.length > 0 ? `${orgs.length} org${orgs.length !== 1 ? 's' : ''}` : ''}</span>
          <div style={{ display: 'flex', gap: 6 }}>
            <button onClick={() => setShowCreate(v => !v)}
              style={{ background: showCreate ? T.greenSoft : 'transparent', border: `1px solid ${showCreate ? T.green : T.border}`, color: showCreate ? T.green : T.dim, fontFamily: T.mono, fontSize: 10, padding: '2px 6px', cursor: 'pointer' }}>+</button>
            <button onClick={fetchOrgs} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '2px 6px', cursor: 'pointer' }}>↻</button>
          </div>
        </div>
        {showCreate && (
          <div style={{ padding: '10px 14px', borderBottom: `1px solid ${T.border}`, background: T.card }}>
            {error && <div style={{ color: T.red, fontFamily: T.mono, fontSize: 10, marginBottom: 6 }}>{error}</div>}
            <input value={newName} onChange={e => setNewName(e.target.value)} placeholder="org name" autoFocus
              onKeyDown={e => e.key === 'Enter' && handleCreate()}
              style={{ ...inputStyle, fontSize: 11, marginBottom: 6 }} />
            <div style={{ display: 'flex', gap: 6 }}>
              <button onClick={handleCreate} disabled={!newName.trim() || creating}
                style={{ flex: 1, background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 10, fontWeight: 600, padding: '5px 0', cursor: 'pointer', opacity: (!newName.trim() || creating) ? 0.6 : 1 }}>
                {creating ? '[ · · · ]' : '[ create ]'}
              </button>
              <button onClick={() => setShowCreate(false)} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '5px 8px', cursor: 'pointer' }}>✕</button>
            </div>
          </div>
        )}
        {loading ? <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint, animation: 'pulse 1s ease-in-out infinite' }}>→ loading · · ·</div>
          : orgs.length === 0 ? <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint }}>→ no orgs</div>
          : orgs.map(o => {
            const isActive = selected === o.org_id;
            return (
              <button key={o.org_id} onClick={() => setSelected(o.org_id)}
                style={{ width: '100%', textAlign: 'left', padding: '10px 14px', background: isActive ? T.greenSoft : 'transparent', border: 0, borderLeft: `2px solid ${isActive ? T.green : 'transparent'}`, fontFamily: T.mono, cursor: 'pointer', color: T.text, display: 'block', transition: 'background .12s' }}>
                <div style={{ fontSize: 13, fontWeight: 600, color: isActive ? T.textHi : T.text }}>{o.org_name}</div>
                <div style={{ fontSize: 10, color: T.faint, marginTop: 2 }}>{o.org_id.slice(0, 8)}…</div>
              </button>
            );
          })}
      </div>
      {railHandle}
      <div style={{ flex: 1, overflow: 'auto' }}>
        {!selectedOrg ? (
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', height: '100%' }}>
            <div style={{ fontFamily: T.mono, fontSize: 12, color: T.faint }}>→ select an org</div>
          </div>
        ) : (
          <div style={{ padding: '20px 24px' }}>
            <div style={{ display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between', marginBottom: 20 }}>
              <div>
                <div style={{ fontFamily: T.mono, fontSize: 20, fontWeight: 700, color: T.textHi, marginBottom: 4 }}>{selectedOrg.org_name}</div>
                <div style={{ fontFamily: T.mono, fontSize: 11, color: T.faint }}>created {timeAgo(selectedOrg.created_at)} ago</div>
                {selectedOrg.active
                  ? <Pill tone="green">active</Pill>
                  : <Pill tone="red">inactive</Pill>}
              </div>
              <div style={{ display: 'flex', gap: 8 }}>
                <button onClick={() => { if (editing) { setEditing(false); } else { setEditName(selectedOrg.org_name); setError(null); setEditing(true); } }}
                  style={{ background: editing ? T.greenSoft : 'transparent', border: `1px solid ${editing ? T.green : T.border}`, color: editing ? T.green : T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}>
                  {editing ? '[ cancel ]' : '[ rename ]'}
                </button>
                <button onClick={() => handleDelete(selectedOrg.org_id)}
                  style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}
                  onMouseEnter={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.red; (e.currentTarget as HTMLButtonElement).style.color = T.red; }}
                  onMouseLeave={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.border; (e.currentTarget as HTMLButtonElement).style.color = T.dim; }}>
                  [ delete ]
                </button>
              </div>
            </div>
            {editing && (
              <div style={{ display: 'flex', gap: 8, marginBottom: 16 }}>
                <input value={editName} onChange={e => setEditName(e.target.value)} autoFocus placeholder="new org name"
                  onKeyDown={e => e.key === 'Enter' && handleRename(selectedOrg.org_id)}
                  style={{ ...inputStyle, flex: 1, background: T.cardHi }} />
                <button onClick={() => handleRename(selectedOrg.org_id)} disabled={!editName.trim() || savingEdit}
                  style={{ background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 11, fontWeight: 600, padding: '7px 14px', cursor: 'pointer', opacity: (!editName.trim() || savingEdit) ? 0.6 : 1 }}>
                  {savingEdit ? '[ · · · ]' : '[ save ]'}
                </button>
              </div>
            )}
            <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 8 }}>INVITE MEMBER</div>
            <div style={{ display: 'flex', gap: 8 }}>
              <input value={inviteEmail} onChange={e => setInviteEmail(e.target.value)} placeholder="email address"
                onKeyDown={e => e.key === 'Enter' && handleInvite()}
                style={{ ...inputStyle, flex: 1, background: T.card }} />
              <button onClick={handleInvite} disabled={!inviteEmail.trim() || inviting}
                style={{ background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 11, fontWeight: 600, padding: '7px 14px', cursor: 'pointer', opacity: (!inviteEmail.trim() || inviting) ? 0.6 : 1 }}>
                {inviting ? '[ · · · ]' : inviteSuccess ? '[ sent! ]' : '[ invite ]'}
              </button>
            </div>
            {error && <div style={{ background: T.redSoft, border: `1px solid ${T.red}`, padding: '8px 12px', fontFamily: T.mono, fontSize: 11, color: T.red, marginTop: 8 }}>{error}</div>}
          </div>
        )}
      </div>
    </div>
  );
}

// ── Invites tab ───────────────────────────────────────────────────────────────

/** invites tab: flat list of invites with accept/decline (acceptInvite/declineInvite) on pending ones and delete (deleteInvite) on any. */
function InvitesTab() {
  const token = useAppSelector(s => s.auth.token)!;
  const [invites, setInvites] = useState<Invite[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [acting, setActing] = useState<string | null>(null);

  const fetchInvites = useCallback(async () => {
    setLoading(true); setError(null);
    try { setInvites(await listInvites(token)); }
    catch (e: unknown) { setError((e as Error).message); }
    finally { setLoading(false); }
  }, [token]);

  useEffect(() => { fetchInvites(); }, [fetchInvites]);

  const handleAccept = async (id: string) => {
    setActing(id);
    try {
      await acceptInvite(token, id);
      setInvites(prev => prev.map(i => i.invite_id === id ? { ...i, status: 'accepted' } : i));
    } catch (e: unknown) { setError((e as Error).message); }
    finally { setActing(null); }
  };

  const handleDecline = async (id: string) => {
    setActing(id);
    try {
      await declineInvite(token, id);
      setInvites(prev => prev.map(i => i.invite_id === id ? { ...i, status: 'declined' } : i));
    } catch (e: unknown) { setError((e as Error).message); }
    finally { setActing(null); }
  };

  const [confirm, confirmEl] = useConfirm();

  const handleDelete = async (id: string) => {
    const email = invites.find(i => i.invite_id === id)?.email;
    if (!(await confirm({ message: `Delete invite for ${email ?? id}? The invitation link will stop working.` }))) return;
    setActing(id);
    try {
      await deleteInvite(token, id);
      setInvites(prev => prev.filter(i => i.invite_id !== id));
    } catch (e: unknown) { setError((e as Error).message); }
    finally { setActing(null); }
  };

  /** maps an invite status to a pill tone (accepted=green, pending=amber, declined=red, else dim). */
  function inviteTone(status: string): 'green' | 'amber' | 'red' | 'dim' {
    if (status === 'accepted') return 'green';
    if (status === 'pending') return 'amber';
    if (status === 'declined') return 'red';
    return 'dim';
  }

  return (
    <div style={{ flex: 1, overflow: 'auto', padding: '20px 24px' }}>
      {confirmEl}
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 20 }}>
        <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1 }}>INVITES · {invites.length}</div>
        <button onClick={fetchInvites} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 10px', cursor: 'pointer' }}>↻</button>
      </div>
      {error && <div style={{ background: T.redSoft, border: `1px solid ${T.red}`, padding: '10px 14px', fontFamily: T.mono, fontSize: 11, color: T.red, marginBottom: 16 }}>{error}</div>}
      {loading ? <div style={{ fontFamily: T.mono, fontSize: 11, color: T.faint, animation: 'pulse 1s ease-in-out infinite' }}>→ loading · · ·</div>
        : invites.length === 0 ? <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '20px', fontFamily: T.mono, fontSize: 12, color: T.faint, textAlign: 'center' }}>→ no invites</div>
        : (
          <div style={{ background: T.card, border: `1px solid ${T.border}`, overflow: 'hidden' }}>
            {invites.map((inv, i) => (
              <div key={inv.invite_id} style={{ display: 'flex', alignItems: 'center', padding: '10px 14px', borderBottom: i < invites.length - 1 ? `1px solid ${T.border}` : 'none', gap: 12 }}>
                <Pill tone={inviteTone(inv.status)}>{inv.status}</Pill>
                <span style={{ fontFamily: T.mono, fontSize: 13, color: T.textHi, fontWeight: 600, flex: 1 }}>{inv.email}</span>
                <span style={{ fontFamily: T.mono, fontSize: 10, color: T.faint }}>{timeAgo(inv.created_at)} ago</span>
                {inv.status === 'pending' && (
                  <>
                    <button onClick={() => handleAccept(inv.invite_id)} disabled={acting === inv.invite_id}
                      style={{ background: T.greenSoft, border: `1px solid ${T.green}`, color: T.green, fontFamily: T.mono, fontSize: 10, padding: '3px 8px', cursor: 'pointer', opacity: acting === inv.invite_id ? 0.5 : 1 }}>
                      accept
                    </button>
                    <button onClick={() => handleDecline(inv.invite_id)} disabled={acting === inv.invite_id}
                      style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 8px', cursor: 'pointer', opacity: acting === inv.invite_id ? 0.5 : 1 }}>
                      decline
                    </button>
                  </>
                )}
                <button onClick={() => handleDelete(inv.invite_id)} disabled={acting === inv.invite_id}
                  style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 8px', cursor: 'pointer', opacity: acting === inv.invite_id ? 0.5 : 1 }}
                  onMouseEnter={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.red; (e.currentTarget as HTMLButtonElement).style.color = T.red; }}
                  onMouseLeave={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.border; (e.currentTarget as HTMLButtonElement).style.color = T.dim; }}>
                  [ delete ]
                </button>
              </div>
            ))}
          </div>
        )}
    </div>
  );
}

// ── Service requests tab ──────────────────────────────────────────────────────

/** service-requests tab: master/detail list of service-permission-requests (filterable by status); the detail pane shows the requested permissions and approves/declines pending ones via approveServiceRequest/declineServiceRequest. */
function ServiceRequestsTab() {
  const token = useAppSelector(s => s.auth.token)!;
  const userNames = useUserNames(token);
  const [requests, setRequests] = useState<ServiceRequest[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const [acting, setActing] = useState<string | null>(null);
  const [statusFilter, setStatusFilter] = useState('');

  const fetchRequests = useCallback(async () => {
    setLoading(true); setError(null);
    try { setRequests(await listServiceRequests(token, statusFilter ? { status: statusFilter } : undefined)); }
    catch (e: unknown) { setError((e as Error).message); }
    finally { setLoading(false); }
  }, [token, statusFilter]);

  useEffect(() => { fetchRequests(); }, [fetchRequests]);
  const [railW, railHandle] = useResizableWidth('rail.gatekeeper.service-requests', 260, { min: 200, max: 480 });

  const selectedReq = requests.find(r => r.request_id === selected);

  const handleApprove = async (id: string) => {
    setActing(id);
    try {
      await approveServiceRequest(token, id);
      setRequests(prev => prev.map(r => r.request_id === id ? { ...r, status: 'approved' } : r));
    } catch (e: unknown) { setError((e as Error).message); }
    finally { setActing(null); }
  };

  const handleDecline = async (id: string) => {
    setActing(id);
    try {
      await declineServiceRequest(token, id);
      setRequests(prev => prev.map(r => r.request_id === id ? { ...r, status: 'declined' } : r));
    } catch (e: unknown) { setError((e as Error).message); }
    finally { setActing(null); }
  };

  /** maps a service-request status to a pill tone (approved=green, pending=amber, declined=red, else dim). */
  function reqTone(status: string): 'green' | 'amber' | 'red' | 'dim' {
    if (status === 'approved') return 'green';
    if (status === 'pending') return 'amber';
    if (status === 'declined') return 'red';
    return 'dim';
  }

  return (
    <div style={{ display: 'flex', flex: 1, overflow: 'hidden' }}>
      <div style={{ width: railW, flexShrink: 0, borderRight: `1px solid ${T.border}`, display: 'flex', flexDirection: 'column', background: T.bgAlt, overflow: 'auto' }}>
        <div style={{ padding: '10px 14px', borderBottom: `1px solid ${T.border}` }}>
          <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 6 }}>
            <span style={{ fontFamily: T.mono, fontSize: 10, color: T.faint }}>{requests.length} request{requests.length !== 1 ? 's' : ''}</span>
            <button onClick={fetchRequests} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '2px 6px', cursor: 'pointer' }}>↻</button>
          </div>
          <select value={statusFilter} onChange={e => setStatusFilter(e.target.value)}
            style={{ width: '100%', background: T.bgAlt, border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '4px 6px', outline: 'none' }}>
            <option value="">all statuses</option>
            {['pending', 'approved', 'declined'].map(s => <option key={s} value={s}>{s}</option>)}
          </select>
        </div>
        {loading ? <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint, animation: 'pulse 1s ease-in-out infinite' }}>→ loading · · ·</div>
          : requests.length === 0 ? <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint }}>→ no requests</div>
          : requests.map(req => {
            const isActive = selected === req.request_id;
            return (
              <button key={req.request_id} onClick={() => setSelected(req.request_id)}
                style={{ width: '100%', textAlign: 'left', padding: '10px 14px', background: isActive ? T.greenSoft : 'transparent', border: 0, borderLeft: `2px solid ${isActive ? T.green : 'transparent'}`, fontFamily: T.mono, cursor: 'pointer', color: T.text, display: 'block', transition: 'background .12s' }}>
                <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                  <Pill tone={reqTone(req.status)}>{req.status}</Pill>
                  <span style={{ fontSize: 12, fontWeight: 600, color: isActive ? T.textHi : T.text }}>{req.service_name}</span>
                </div>
                <div style={{ fontSize: 10, color: T.faint, marginTop: 2 }}>{timeAgo(req.created_at)} ago</div>
              </button>
            );
          })}
      </div>
      {railHandle}
      <div style={{ flex: 1, overflow: 'auto' }}>
        {!selectedReq ? (
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', height: '100%' }}>
            <div style={{ fontFamily: T.mono, fontSize: 12, color: T.faint }}>→ select a request</div>
          </div>
        ) : (
          <div style={{ padding: '20px 24px' }}>
            <div style={{ display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between', marginBottom: 20 }}>
              <div>
                <div style={{ display: 'flex', alignItems: 'center', gap: 10, marginBottom: 4 }}>
                  <Pill tone={reqTone(selectedReq.status)}>{selectedReq.status}</Pill>
                  <span style={{ fontFamily: T.mono, fontSize: 18, fontWeight: 700, color: T.textHi }}>{selectedReq.service_name}</span>
                </div>
                <div style={{ fontFamily: T.mono, fontSize: 11, color: T.faint }}>
                  requested by {userNames[selectedReq.requested_by] ?? shortId(selectedReq.requested_by)} · {timeAgo(selectedReq.created_at)} ago
                </div>
              </div>
              {selectedReq.status === 'pending' && (
                <div style={{ display: 'flex', gap: 8 }}>
                  <button onClick={() => handleApprove(selectedReq.request_id)} disabled={acting === selectedReq.request_id}
                    style={{ background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 11, fontWeight: 600, padding: '6px 14px', cursor: 'pointer', opacity: acting === selectedReq.request_id ? 0.6 : 1 }}>
                    {acting === selectedReq.request_id ? '[ · · · ]' : '[ approve ]'}
                  </button>
                  <button onClick={() => handleDecline(selectedReq.request_id)} disabled={acting === selectedReq.request_id}
                    style={{ background: 'transparent', border: `1px solid ${T.red}`, color: T.red, fontFamily: T.mono, fontSize: 11, padding: '6px 12px', cursor: 'pointer', opacity: acting === selectedReq.request_id ? 0.6 : 1 }}>
                    [ decline ]
                  </button>
                </div>
              )}
            </div>
            {error && <div style={{ background: T.redSoft, border: `1px solid ${T.red}`, padding: '8px 12px', fontFamily: T.mono, fontSize: 11, color: T.red, marginBottom: 16 }}>{error}</div>}
            {selectedReq.permissions && selectedReq.permissions.length > 0 && (
              <>
                <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 8 }}>REQUESTED PERMISSIONS</div>
                <div style={{ background: T.card, border: `1px solid ${T.border}`, overflow: 'hidden' }}>
                  {selectedReq.permissions.map((p, i) => (
                    <div key={i} style={{ padding: '8px 14px', borderBottom: i < selectedReq.permissions!.length - 1 ? `1px solid ${T.border}` : 'none', fontFamily: T.mono, fontSize: 12, color: T.text }}>
                      {p}
                    </div>
                  ))}
                </div>
              </>
            )}
          </div>
        )}
      </div>
    </div>
  );
}

// ── Gatekeeper page ───────────────────────────────────────────────────────────

/** tab descriptors paired with the gatekeeper permission key each one requires to be shown. */
const ALL_TABS: { id: Tab; label: string; permission: string }[] = [
  { id: 'users',            label: 'users',        permission: 'gatekeeper:listUser' },
  { id: 'roles',            label: 'roles',        permission: 'gatekeeper:listRole' },
  { id: 'permissions',      label: 'permissions',  permission: 'gatekeeper:createPermission' },
  { id: 'secrets',          label: 'secrets',      permission: 'gatekeeper:listSecrets' },
  { id: 'teams',            label: 'teams',        permission: 'gatekeeper:listTeam' },
  { id: 'orgs',             label: 'orgs',         permission: 'gatekeeper:listOrg' },
  { id: 'invites',          label: 'invites',      permission: 'gatekeeper:listInvite' },
  { id: 'service-requests', label: 'svc requests', permission: 'gatekeeper:listSPR' },
];

/** gatekeeper admin page: renders the tab bar (filtered to the tabs the caller's redux permissions allow) and switches between the entity tabs, defaulting to the first visible one. */
export function Gatekeeper() {
  const permissions = useAppSelector(s => s.auth.permissions);
  const visibleTabs = permissions
    ? ALL_TABS.filter(t => permissions[t.permission])
    : [];
  const [tab, setTab] = useState<Tab>('users');

  useEffect(() => {
    if (visibleTabs.length > 0 && !visibleTabs.find(t => t.id === tab)) {
      setTab(visibleTabs[0].id);
    }
  }, [permissions]); // eslint-disable-line react-hooks/exhaustive-deps

  return (
    <div style={{ display: 'flex', height: '100%', overflow: 'hidden', flexDirection: 'column' }}>
      <div style={{ display: 'flex', borderBottom: `1px solid ${T.border}`, background: T.bgAlt, flexShrink: 0, overflowX: 'auto' }}>
        {visibleTabs.map(t => (
          <button key={t.id} onClick={() => setTab(t.id)}
            style={{ background: tab === t.id ? T.card : 'transparent', border: 'none', borderBottom: `2px solid ${tab === t.id ? T.green : 'transparent'}`, color: tab === t.id ? T.textHi : T.dim, fontFamily: T.mono, fontSize: 12, padding: '11px 18px', cursor: 'pointer', letterSpacing: 0.3, flexShrink: 0 }}>
            {t.label}
          </button>
        ))}
        <div style={{ flex: 1, display: 'flex', alignItems: 'center', paddingRight: 16, justifyContent: 'flex-end' }}>
          <span style={{ fontFamily: T.mono, fontSize: 11, color: T.faint }}>
            <span style={{ color: T.green }}>$</span> armory gatekeeper
          </span>
        </div>
      </div>
      <div style={{ flex: 1, display: 'flex', overflow: 'hidden' }}>
        {tab === 'users' && <UsersTab />}
        {tab === 'roles' && <RolesTab />}
        {tab === 'permissions' && <PermissionsTab />}
        {tab === 'secrets' && <SecretsTab />}
        {tab === 'teams' && <TeamsTab />}
        {tab === 'orgs' && <OrgsTab />}
        {tab === 'invites' && <InvitesTab />}
        {tab === 'service-requests' && <ServiceRequestsTab />}
      </div>
    </div>
  );
}

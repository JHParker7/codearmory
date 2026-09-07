/**
 * Projects — the portal's home/explore surface (GitHub-explore style). The centre
 * column lists the projects the caller can reach (owned + shared) as cards you
 * open to enter, or expand to manage members (owned only). A side column shows the
 * organisations, teams, and people the caller belongs to / can see. Opening a
 * project sets it as the active scope and enters the app; "+ new project" creates
 * a real gatekeeper Project (the caller becomes its admin).
 *
 * Data goes through the typed bff client (proxied to conductor); the auth token
 * comes from the redux store.
 */
import { useState, useEffect, useCallback, useMemo } from 'react';
import { useNavigate } from 'react-router-dom';
import { T } from '../../theme';
import { Pill } from '../../components/Pill';
import { useConfirm } from '../../components/ConfirmDialog';
import { useAppSelector, useAppDispatch } from '../../store/hooks';
import { setCurrentProject } from '../../store/projectSlice';
import {
  listAccessibleProjects, createProject, deleteProject,
  addProjectMember, removeProjectMember, listRoleMembers,
  listOrgs, listTeams,
} from '../../api/bff';
import type { Project, ProjectTier, ProjectMemberTier, Org, Team } from '../../api/bff';
import { shortId } from '../../utils';
import { useUserNames, useUsers } from '../../hooks/useNames';

const inputStyle = {
  background: 'transparent' as const, border: `1px solid ${T.border}`, color: T.text,
  fontFamily: T.mono, fontSize: 12, padding: '7px 10px', outline: 'none', width: '100%', boxSizing: 'border-box' as const,
};
const SLUG_RE = /^[a-z0-9][a-z0-9-]{0,62}$/;
const MEMBER_TIERS: ProjectMemberTier[] = ['viewer', 'developer', 'admin'];

function tierTone(tier: ProjectTier): 'green' | 'blue' | 'amber' | 'dim' {
  return tier === 'owner' ? 'green' : tier === 'admin' ? 'blue' : tier === 'developer' ? 'amber' : 'dim';
}
function tierColor(tier: ProjectTier): string {
  return tier === 'owner' ? T.green : tier === 'admin' ? T.blue : tier === 'developer' ? T.amber : T.dim;
}

interface Member { user_id: string; tier: ProjectMemberTier; }

export function Projects() {
  const token = useAppSelector(s => s.auth.token)!;
  const currentUserId = useAppSelector(s => s.auth.user?.user_id);
  const currentProject = useAppSelector(s => s.project.current);
  const dispatch = useAppDispatch();
  const navigate = useNavigate();
  const userNames = useUserNames(token);
  const allUsers = useUsers(token);

  const [projects, setProjects] = useState<Project[]>([]);
  const [orgs, setOrgs] = useState<Org[]>([]);
  const [teams, setTeams] = useState<Team[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  // Create form
  const [showCreate, setShowCreate] = useState(false);
  const [newSlug, setNewSlug] = useState('');
  const [newName, setNewName] = useState('');
  const [creating, setCreating] = useState(false);

  // Inline member management (owned projects) — the expanded project id + its roster.
  const [managing, setManaging] = useState<string | null>(null);
  const [members, setMembers] = useState<Member[]>([]);
  const [membersError, setMembersError] = useState<string | null>(null);
  const [addUserId, setAddUserId] = useState('');
  const [addTier, setAddTier] = useState<ProjectMemberTier>('viewer');
  const [mutating, setMutating] = useState(false);

  const [confirm, confirmEl] = useConfirm();

  const fetchAll = useCallback(async () => {
    setLoading(true); setError(null);
    try {
      const [ps, os, ts] = await Promise.all([
        listAccessibleProjects(token),
        listOrgs(token).catch(() => [] as Org[]),
        listTeams(token).catch(() => [] as Team[]),
      ]);
      setProjects(ps); setOrgs(os); setTeams(ts);
    } catch (e: unknown) { setError((e as Error).message); }
    finally { setLoading(false); }
  }, [token]);
  useEffect(() => { fetchAll(); }, [fetchAll]);

  const owned = useMemo(() => projects.filter(p => p.tier === 'owner' || p.owner_id === currentUserId), [projects, currentUserId]);
  const shared = useMemo(() => projects.filter(p => !(p.tier === 'owner' || p.owner_id === currentUserId)), [projects, currentUserId]);

  const openProject = (p: Project) => { dispatch(setCurrentProject(p.slug)); navigate('/app'); };

  const loadMembers = useCallback(async (p: Project) => {
    setMembersError(null);
    const tiers: [ProjectMemberTier, string][] = [['viewer', p.viewer_role_id], ['developer', p.developer_role_id], ['admin', p.admin_role_id]];
    try {
      const rosters = await Promise.all(tiers.map(([, id]) => listRoleMembers(token, id).catch(() => [])));
      const merged: Member[] = [];
      rosters.forEach((roster, i) => { for (const m of roster) merged.push({ user_id: m.user_id, tier: tiers[i][0] }); });
      setMembers(merged);
    } catch (e: unknown) { setMembersError((e as Error).message); }
  }, [token]);

  const toggleManage = (p: Project) => {
    if (managing === p.project_id) { setManaging(null); return; }
    setManaging(p.project_id); setMembers([]); setAddUserId(''); setAddTier('viewer'); loadMembers(p);
  };

  const handleCreate = async () => {
    const slug = newSlug.trim(), name = newName.trim();
    if (!slug || !name) return;
    if (!SLUG_RE.test(slug)) { setError('slug must match ^[a-z0-9][a-z0-9-]{0,62}$'); return; }
    setCreating(true); setError(null);
    try {
      const p = await createProject(token, slug, name);
      setProjects(prev => [p, ...prev]);
      setNewSlug(''); setNewName(''); setShowCreate(false);
    } catch (e: unknown) { setError((e as Error).message); }
    finally { setCreating(false); }
  };

  const handleDelete = async (p: Project) => {
    if (!(await confirm({ message: `Delete project ${p.name} (${p.slug})? Its namespace and tier roles are removed and members lose access.`, requireText: p.slug, confirmLabel: 'delete project' }))) return;
    try {
      await deleteProject(token, p.project_id);
      setProjects(prev => prev.filter(x => x.project_id !== p.project_id));
      if (managing === p.project_id) setManaging(null);
    } catch (e: unknown) { setError((e as Error).message); }
  };

  const addMember = async (p: Project) => {
    if (!addUserId.trim()) return;
    setMutating(true); setMembersError(null);
    try { await addProjectMember(token, p.project_id, addUserId.trim(), addTier); await loadMembers(p); setAddUserId(''); }
    catch (e: unknown) { setMembersError((e as Error).message); }
    finally { setMutating(false); }
  };
  const changeTier = async (p: Project, userId: string, tier: ProjectMemberTier) => {
    setMutating(true); setMembersError(null);
    try { await addProjectMember(token, p.project_id, userId, tier); await loadMembers(p); }
    catch (e: unknown) { setMembersError((e as Error).message); }
    finally { setMutating(false); }
  };
  const removeMember = async (p: Project, userId: string) => {
    const name = userNames[userId] ?? shortId(userId);
    if (!(await confirm({ message: `Remove @${name} from ${p.name}?`, confirmLabel: 'remove' }))) return;
    setMutating(true); setMembersError(null);
    try { await removeProjectMember(token, p.project_id, userId); await loadMembers(p); }
    catch (e: unknown) { setMembersError((e as Error).message); }
    finally { setMutating(false); }
  };

  // ── card + section renderers ───────────────────────────────────────────────
  const projectCard = (p: Project) => {
    const isOwned = p.tier === 'owner' || p.owner_id === currentUserId;
    const isCurrent = currentProject === p.slug;
    const isManaging = managing === p.project_id;
    return (
      <div key={p.project_id} style={{ background: T.card, border: `1px solid ${isManaging ? T.green : T.border}`, borderRadius: 8, overflow: 'hidden' }}>
        <div style={{ padding: '13px 15px' }}>
          <div style={{ display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between', gap: 8 }}>
            <div style={{ minWidth: 0 }}>
              <div style={{ fontFamily: T.mono, fontSize: 15, fontWeight: 700, color: T.textHi, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{p.name}</div>
              <div style={{ fontFamily: T.mono, fontSize: 11.5, color: T.faint, marginTop: 2 }}>{p.slug}</div>
            </div>
            {p.tier && <Pill tone={tierTone(p.tier)}>{p.tier}</Pill>}
          </div>
          <div style={{ display: 'flex', gap: 8, marginTop: 12 }}>
            <button onClick={() => openProject(p)}
              style={{ background: isCurrent ? T.greenSoft : T.green, color: isCurrent ? T.green : T.bg, border: isCurrent ? `1px solid ${T.green}` : 'none', fontFamily: T.mono, fontSize: 12, fontWeight: 600, padding: '6px 14px', cursor: 'pointer' }}>
              {isCurrent ? '✓ enter →' : 'open →'}
            </button>
            {isOwned && (
              <button onClick={() => toggleManage(p)}
                style={{ background: 'transparent', border: `1px solid ${isManaging ? T.green : T.border}`, color: isManaging ? T.green : T.dim, fontFamily: T.mono, fontSize: 12, padding: '6px 12px', cursor: 'pointer' }}>
                manage
              </button>
            )}
            {isOwned && (
              <button onClick={() => handleDelete(p)} title="delete project"
                style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.faint, fontFamily: T.mono, fontSize: 12, padding: '6px 10px', cursor: 'pointer', marginLeft: 'auto' }}
                onMouseEnter={e => { (e.currentTarget as HTMLButtonElement).style.color = T.red; (e.currentTarget as HTMLButtonElement).style.borderColor = T.red; }}
                onMouseLeave={e => { (e.currentTarget as HTMLButtonElement).style.color = T.faint; (e.currentTarget as HTMLButtonElement).style.borderColor = T.border; }}>✕</button>
            )}
          </div>
        </div>
        {isManaging && isOwned && (
          <div style={{ borderTop: `1px solid ${T.border}`, background: T.bgAlt, padding: '11px 15px' }}>
            <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 8 }}>MEMBERS · {members.length}</div>
            {membersError && <div style={{ color: T.red, fontSize: 11, marginBottom: 8 }}>{membersError}</div>}
            <div style={{ display: 'flex', gap: 6, marginBottom: 10, flexWrap: 'wrap' }}>
              <input list="proj-users" value={addUserId} onChange={e => setAddUserId(e.target.value)} placeholder="user id or @username" style={{ ...inputStyle, flex: 1, minWidth: 160, background: T.cardHi }} />
              <datalist id="proj-users">{allUsers.filter(u => u.user_id !== p.owner_id).map(u => <option key={u.user_id} value={u.user_id}>@{u.username}</option>)}</datalist>
              <select value={addTier} onChange={e => setAddTier(e.target.value as ProjectMemberTier)} style={{ ...inputStyle, width: 'auto', background: T.cardHi, cursor: 'pointer' }}>{MEMBER_TIERS.map(t => <option key={t} value={t}>{t}</option>)}</select>
              <button onClick={() => addMember(p)} disabled={mutating || !addUserId.trim()} style={{ background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 12, fontWeight: 600, padding: '6px 12px', cursor: 'pointer', opacity: mutating || !addUserId.trim() ? 0.6 : 1 }}>add</button>
            </div>
            {members.map(m => (
              <div key={m.user_id} style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 8, padding: '5px 0', borderTop: `1px solid ${T.border}` }}>
                <span style={{ fontFamily: T.mono, fontSize: 12.5, color: T.textHi, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>@{userNames[m.user_id] ?? shortId(m.user_id)}</span>
                <span style={{ display: 'flex', gap: 6, flexShrink: 0 }}>
                  <select value={m.tier} disabled={mutating} onChange={e => changeTier(p, m.user_id, e.target.value as ProjectMemberTier)} style={{ ...inputStyle, width: 'auto', fontSize: 11, padding: '3px 6px', background: T.cardHi, cursor: 'pointer', color: tierColor(m.tier) }}>{MEMBER_TIERS.map(t => <option key={t} value={t} style={{ color: T.text }}>{t}</option>)}</select>
                  <button onClick={() => removeMember(p, m.user_id)} disabled={mutating} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.faint, fontFamily: T.mono, fontSize: 11, padding: '3px 8px', cursor: 'pointer' }}>remove</button>
                </span>
              </div>
            ))}
            {members.length === 0 && <div style={{ fontSize: 11.5, color: T.faint }}>No members yet.</div>}
          </div>
        )}
      </div>
    );
  };

  const sideList = (title: string, items: { key: string; primary: string; secondary?: string }[], empty: string) => (
    <div style={{ marginBottom: 20 }}>
      <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, textTransform: 'uppercase', marginBottom: 8 }}>{title} · {items.length}</div>
      {items.length === 0 ? <div style={{ fontSize: 12, color: T.faint }}>{empty}</div> : (
        <div style={{ border: `1px solid ${T.border}`, borderRadius: 6, overflow: 'hidden' }}>
          {items.map((it, i) => (
            <div key={it.key} style={{ padding: '8px 11px', borderTop: i ? `1px solid ${T.border}` : 'none', background: T.card }}>
              <div style={{ fontFamily: T.mono, fontSize: 12.5, color: T.textHi, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{it.primary}</div>
              {it.secondary && <div style={{ fontSize: 10.5, color: T.faint, fontFamily: T.mono }}>{it.secondary}</div>}
            </div>
          ))}
        </div>
      )}
    </div>
  );

  return (
    <div style={{ height: '100%', overflow: 'auto' }}>
      {confirmEl}
      <div style={{ maxWidth: 1080, margin: '0 auto', padding: '24px 24px 40px' }}>
        <div style={{ display: 'flex', alignItems: 'baseline', justifyContent: 'space-between', marginBottom: 4 }}>
          <h1 style={{ fontSize: 20, color: T.textHi }}>Projects</h1>
          <button onClick={() => setShowCreate(v => !v)} style={{ background: showCreate ? T.greenSoft : T.green, color: showCreate ? T.green : T.bg, border: showCreate ? `1px solid ${T.green}` : 'none', fontFamily: T.mono, fontSize: 12, fontWeight: 600, padding: '6px 14px', cursor: 'pointer' }}>{showCreate ? 'cancel' : '+ new project'}</button>
        </div>
        <p style={{ color: T.dim, fontSize: 13, marginBottom: 18 }}>Pick a project to enter, or manage who has access. Your organisations, teams and people are on the right.</p>

        {error && <div style={{ color: T.red, background: T.redSoft, padding: '7px 11px', borderRadius: 4, marginBottom: 14, fontSize: 13 }}>{error}</div>}

        {showCreate && (
          <div style={{ background: T.card, border: `1px solid ${T.borderHi}`, borderRadius: 6, padding: 14, marginBottom: 18, display: 'flex', gap: 8, alignItems: 'flex-end', flexWrap: 'wrap' }}>
            <div style={{ flex: 1, minWidth: 200 }}>
              <div style={{ fontSize: 10, color: T.faint, marginBottom: 4 }}>SLUG</div>
              <input value={newSlug} onChange={e => setNewSlug(e.target.value)} placeholder="platform-team" autoFocus style={{ ...inputStyle }} />
              <div style={{ fontSize: 9, color: newSlug && !SLUG_RE.test(newSlug.trim()) ? T.red : T.faint, marginTop: 3 }}>lowercase a–z, 0–9, dashes · max 63</div>
            </div>
            <div style={{ flex: 1, minWidth: 200 }}>
              <div style={{ fontSize: 10, color: T.faint, marginBottom: 4 }}>NAME</div>
              <input value={newName} onChange={e => setNewName(e.target.value)} placeholder="Platform Team" onKeyDown={e => e.key === 'Enter' && handleCreate()} style={{ ...inputStyle }} />
            </div>
            <button onClick={handleCreate} disabled={creating || !newSlug.trim() || !newName.trim() || !SLUG_RE.test(newSlug.trim())} style={{ background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 12, fontWeight: 600, padding: '8px 16px', cursor: 'pointer', opacity: (creating || !newSlug.trim() || !newName.trim() || !SLUG_RE.test(newSlug.trim())) ? 0.6 : 1 }}>{creating ? '…' : 'create'}</button>
          </div>
        )}

        <div style={{ display: 'flex', gap: 24, alignItems: 'flex-start', flexWrap: 'wrap' }}>
          {/* Centre: projects */}
          <div style={{ flex: '3 1 460px', minWidth: 0 }}>
            {loading ? <div style={{ color: T.faint, fontSize: 13 }}>→ loading · · ·</div>
              : projects.length === 0 ? <div style={{ color: T.faint, fontSize: 13 }}>No projects yet — create one to begin.</div>
              : (
                <>
                  {owned.length > 0 && <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, textTransform: 'uppercase', marginBottom: 8 }}>owned by me</div>}
                  <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(240px, 1fr))', gap: 12, marginBottom: owned.length && shared.length ? 20 : 0 }}>{owned.map(projectCard)}</div>
                  {shared.length > 0 && <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, textTransform: 'uppercase', margin: '4px 0 8px' }}>shared with me</div>}
                  <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(240px, 1fr))', gap: 12 }}>{shared.map(projectCard)}</div>
                </>
              )}
          </div>
          {/* Side: orgs / teams / people */}
          <div style={{ flex: '1 1 240px', minWidth: 240 }}>
            {sideList('organisations', orgs.map(o => ({ key: o.org_id, primary: o.org_name, secondary: o.owner_id === currentUserId ? 'owner' : 'member' })), 'None.')}
            {sideList('teams', teams.map(t => ({ key: t.team_id, primary: t.team_name, secondary: t.owner_id === currentUserId ? 'owner' : 'member' })), 'None.')}
            {/* People isn't a full user list — that doesn't scale on a big instance.
                The directory (search/paginate) lives in Settings → users & roles. */}
            <div style={{ marginBottom: 20 }}>
              <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, textTransform: 'uppercase', marginBottom: 8 }}>people</div>
              <button onClick={() => navigate('/app/settings?tab=users')}
                style={{ width: '100%', textAlign: 'left', background: T.card, border: `1px solid ${T.border}`, borderRadius: 6, padding: '9px 11px', cursor: 'pointer', color: T.dim, fontFamily: T.mono, fontSize: 12.5 }}
                onMouseEnter={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.green; (e.currentTarget as HTMLButtonElement).style.color = T.textHi; }}
                onMouseLeave={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.border; (e.currentTarget as HTMLButtonElement).style.color = T.dim; }}>
                browse the user directory →
              </button>
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}

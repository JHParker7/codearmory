/**
 * projects page — the first-class gatekeeper Project (a resource-grouping RBAC
 * scope) control surface. A master/detail view: the rail lists the projects the
 * caller can reach (split into "owned by me" vs "shared with me"), plus a create
 * form; the detail pane shows a project's namespace/slug/tier and, for projects
 * the caller OWNS, its merged member roster (viewer/developer/admin tier roles)
 * with add/change-tier/remove controls. Non-owned projects are read-only.
 *
 * All data goes through the typed bff client (src/api/bff.ts), which proxies
 * /api/* to conductor; auth token comes from the redux auth store.
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
} from '../../api/bff';
import type { Project, ProjectTier, ProjectMemberTier } from '../../api/bff';
import { shortId } from '../../utils';
import { useUserNames, useUsers } from '../../hooks/useNames';
import { useResizableWidth } from '../../components/ResizeHandle';

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

const SLUG_RE = /^[a-z0-9][a-z0-9-]{0,62}$/;
const MEMBER_TIERS: ProjectMemberTier[] = ['viewer', 'developer', 'admin'];

/** Badge tone for a caller's/member's tier — owner green, admin blue, developer amber, viewer dim. */
function tierTone(tier: ProjectTier): 'green' | 'blue' | 'amber' | 'dim' {
  return tier === 'owner' ? 'green' : tier === 'admin' ? 'blue' : tier === 'developer' ? 'amber' : 'dim';
}

/** A project member resolved from a tier role, tagged with the tier that listed them. */
interface Member {
  user_id: string;
  tier: ProjectMemberTier;
}

// ── Projects page ───────────────────────────────────────────────────────────────

/** projects page: rail of accessible projects (owned/shared) + create form; detail pane with per-project members (owned only). */
export function Projects() {
  const token = useAppSelector(s => s.auth.token)!;
  const currentUserId = useAppSelector(s => s.auth.user?.user_id);
  const currentProject = useAppSelector(s => s.project.current);
  const dispatch = useAppDispatch();
  const navigate = useNavigate();

  // Enter a project: make it the active scope and go to the app, which lands on
  // the first resource module — now showing only this project's resources.
  const openProject = (p: Project) => {
    dispatch(setCurrentProject(p.slug));
    navigate('/app');
  };
  const userNames = useUserNames(token);
  const allUsers = useUsers(token);

  const [projects, setProjects] = useState<Project[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<string | null>(null);

  // Create form
  const [showCreate, setShowCreate] = useState(false);
  const [newSlug, setNewSlug] = useState('');
  const [newName, setNewName] = useState('');
  const [creating, setCreating] = useState(false);

  // Members (detail pane, owned projects only)
  const [members, setMembers] = useState<Member[]>([]);
  const [membersLoading, setMembersLoading] = useState(false);
  const [membersError, setMembersError] = useState<string | null>(null);
  const [addUserId, setAddUserId] = useState('');
  const [addTier, setAddTier] = useState<ProjectMemberTier>('viewer');
  const [addingMember, setAddingMember] = useState(false);
  const [mutatingUser, setMutatingUser] = useState<string | null>(null);

  const fetchProjects = useCallback(async () => {
    setLoading(true); setError(null);
    try { setProjects(await listAccessibleProjects(token)); }
    catch (e: unknown) { setError((e as Error).message); }
    finally { setLoading(false); }
  }, [token]);

  useEffect(() => { fetchProjects(); }, [fetchProjects]);

  const selectedProject = projects.find(p => p.project_id === selected);
  const isOwned = !!selectedProject && (selectedProject.tier === 'owner' || selectedProject.owner_id === currentUserId);

  const owned = useMemo(() => projects.filter(p => p.tier === 'owner' || p.owner_id === currentUserId), [projects, currentUserId]);
  const shared = useMemo(() => projects.filter(p => !(p.tier === 'owner' || p.owner_id === currentUserId)), [projects, currentUserId]);

  // Merge a project's members from its three tier roles, labelling each with the
  // tier whose role listed them. Best-effort per role: a role the caller can't
  // read is skipped rather than failing the whole roster.
  const loadMembers = useCallback(async (p: Project) => {
    setMembersLoading(true); setMembersError(null);
    const tiers: [ProjectMemberTier, string][] = [
      ['viewer', p.viewer_role_id],
      ['developer', p.developer_role_id],
      ['admin', p.admin_role_id],
    ];
    try {
      const rosters = await Promise.all(tiers.map(([, roleId]) => listRoleMembers(token, roleId).catch(() => [])));
      const merged: Member[] = [];
      rosters.forEach((roster, i) => {
        const tier = tiers[i][0];
        for (const m of roster) merged.push({ user_id: m.user_id, tier });
      });
      setMembers(merged);
    } catch (e: unknown) { setMembersError((e as Error).message); }
    finally { setMembersLoading(false); }
  }, [token]);

  // Load (or clear) members when the selection changes. Only owned projects have
  // a manageable roster; shared projects are read-only.
  useEffect(() => {
    setMembers([]); setMembersError(null); setAddUserId(''); setAddTier('viewer');
    if (selectedProject && (selectedProject.tier === 'owner' || selectedProject.owner_id === currentUserId)) {
      loadMembers(selectedProject);
    }
  }, [selected, selectedProject, currentUserId, loadMembers]);

  const handleCreate = async () => {
    const slug = newSlug.trim();
    const name = newName.trim();
    if (!slug || !name) return;
    if (!SLUG_RE.test(slug)) { setError('slug must match ^[a-z0-9][a-z0-9-]{0,62}$'); return; }
    setCreating(true); setError(null);
    try {
      const p = await createProject(token, slug, name);
      setProjects(prev => [p, ...prev]);
      setNewSlug(''); setNewName(''); setShowCreate(false);
      setSelected(p.project_id);
    } catch (e: unknown) { setError((e as Error).message); }
    finally { setCreating(false); }
  };

  const [confirm, confirmEl] = useConfirm();
  const [railW, railHandle] = useResizableWidth('rail.projects', 280, { min: 220, max: 480 });

  const handleDelete = async (p: Project) => {
    if (!(await confirm({ message: `Delete project ${p.name} (${p.slug})? Its namespace and tier roles are removed and members lose access.`, requireText: p.slug, confirmLabel: 'delete project' }))) return;
    try {
      await deleteProject(token, p.project_id);
      setProjects(prev => prev.filter(x => x.project_id !== p.project_id));
      if (selected === p.project_id) setSelected(null);
    } catch (e: unknown) { setError((e as Error).message); }
  };

  const handleAddMember = async () => {
    if (!selectedProject || !addUserId.trim()) return;
    setAddingMember(true); setMembersError(null);
    try {
      await addProjectMember(token, selectedProject.project_id, addUserId.trim(), addTier);
      await loadMembers(selectedProject);
      setAddUserId('');
    } catch (e: unknown) { setMembersError((e as Error).message); }
    finally { setAddingMember(false); }
  };

  const handleChangeTier = async (userId: string, tier: ProjectMemberTier) => {
    if (!selectedProject) return;
    setMutatingUser(userId); setMembersError(null);
    try {
      await addProjectMember(token, selectedProject.project_id, userId, tier);
      await loadMembers(selectedProject);
    } catch (e: unknown) { setMembersError((e as Error).message); }
    finally { setMutatingUser(null); }
  };

  const handleRemoveMember = async (userId: string) => {
    if (!selectedProject) return;
    const name = userNames[userId] ?? shortId(userId);
    if (!(await confirm({ message: `Remove @${name} from ${selectedProject.name}? They lose all tiers on this project.`, confirmLabel: 'remove' }))) return;
    setMutatingUser(userId); setMembersError(null);
    try {
      await removeProjectMember(token, selectedProject.project_id, userId);
      await loadMembers(selectedProject);
    } catch (e: unknown) { setMembersError((e as Error).message); }
    finally { setMutatingUser(null); }
  };

  const renderRailItem = (p: Project) => {
    const isActive = selected === p.project_id;
    return (
      <button key={p.project_id} onClick={() => setSelected(p.project_id)}
        style={{ width: '100%', textAlign: 'left', padding: '10px 14px', background: isActive ? T.greenSoft : 'transparent', border: 0, borderLeft: `2px solid ${isActive ? T.green : 'transparent'}`, fontFamily: T.mono, cursor: 'pointer', color: T.text, display: 'block', transition: 'background .12s' }}>
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 6 }}>
          <span style={{ fontSize: 13, fontWeight: 600, color: isActive ? T.textHi : T.text, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{p.name}</span>
          {p.tier && <span style={{ fontSize: 9, color: tierColor(p.tier), border: `1px solid ${tierColor(p.tier)}`, padding: '0 4px', flexShrink: 0, textTransform: 'uppercase', letterSpacing: 0.5 }}>{p.tier}</span>}
        </div>
        <div style={{ fontSize: 11, color: T.faint, marginTop: 2 }}>{p.slug}</div>
      </button>
    );
  };

  return (
    <div style={{ display: 'flex', height: '100%', overflow: 'hidden' }}>
      {confirmEl}
      <div style={{ width: railW, flexShrink: 0, borderRight: `1px solid ${T.border}`, display: 'flex', flexDirection: 'column', background: T.bgAlt, overflow: 'auto' }}>
        <div style={{ padding: '12px 14px', borderBottom: `1px solid ${T.border}`, display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
          <span style={{ fontFamily: T.mono, fontSize: 10, color: T.faint }}>{projects.length > 0 ? `${projects.length} project${projects.length !== 1 ? 's' : ''}` : ''}</span>
          <div style={{ display: 'flex', gap: 6 }}>
            <button onClick={() => setShowCreate(v => !v)}
              style={{ background: showCreate ? T.greenSoft : 'transparent', border: `1px solid ${showCreate ? T.green : T.border}`, color: showCreate ? T.green : T.dim, fontFamily: T.mono, fontSize: 10, padding: '2px 6px', cursor: 'pointer' }}>+</button>
            <button onClick={fetchProjects} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '2px 6px', cursor: 'pointer' }}>↻</button>
          </div>
        </div>
        {showCreate && (
          <div style={{ padding: '10px 14px', borderBottom: `1px solid ${T.border}`, background: T.card }}>
            {error && <div style={{ color: T.red, fontFamily: T.mono, fontSize: 10, marginBottom: 6 }}>{error}</div>}
            <input value={newSlug} onChange={e => setNewSlug(e.target.value)} placeholder="slug (e.g. platform-team)" autoFocus
              style={{ ...inputStyle, fontSize: 11, marginBottom: 4 }} />
            <div style={{ fontFamily: T.mono, fontSize: 9, color: newSlug && !SLUG_RE.test(newSlug.trim()) ? T.red : T.faint, marginBottom: 6 }}>
              lowercase a–z, 0–9, dashes · max 63 chars
            </div>
            <input value={newName} onChange={e => setNewName(e.target.value)} placeholder="display name" onKeyDown={e => e.key === 'Enter' && handleCreate()}
              style={{ ...inputStyle, fontSize: 11, marginBottom: 6 }} />
            <div style={{ display: 'flex', gap: 6 }}>
              <button onClick={handleCreate} disabled={creating || !newSlug.trim() || !newName.trim() || !SLUG_RE.test(newSlug.trim())}
                style={{ flex: 1, background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 10, fontWeight: 600, padding: '5px 0', cursor: 'pointer', opacity: (creating || !newSlug.trim() || !newName.trim() || !SLUG_RE.test(newSlug.trim())) ? 0.6 : 1 }}>
                {creating ? '[ · · · ]' : '[ create ]'}
              </button>
              <button onClick={() => setShowCreate(false)} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '5px 8px', cursor: 'pointer' }}>✕</button>
            </div>
          </div>
        )}
        {loading ? <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint, animation: 'pulse 1s ease-in-out infinite' }}>→ loading · · ·</div>
          : error && projects.length === 0 ? <div style={{ padding: '14px', fontFamily: T.mono, fontSize: 11, color: T.red }}>{error}</div>
          : projects.length === 0 ? <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint }}>→ no projects</div>
          : (
            <>
              {owned.length > 0 && (
                <>
                  <div style={{ padding: '8px 14px 4px', fontFamily: T.mono, fontSize: 9, color: T.faint, letterSpacing: 1, textTransform: 'uppercase' }}>owned by me</div>
                  {owned.map(renderRailItem)}
                </>
              )}
              {shared.length > 0 && (
                <>
                  <div style={{ padding: '12px 14px 4px', fontFamily: T.mono, fontSize: 9, color: T.faint, letterSpacing: 1, textTransform: 'uppercase' }}>shared with me</div>
                  {shared.map(renderRailItem)}
                </>
              )}
            </>
          )}
      </div>
      {railHandle}
      <div style={{ flex: 1, overflow: 'auto' }}>
        {!selectedProject ? (
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', height: '100%' }}>
            <div style={{ fontFamily: T.mono, fontSize: 12, color: T.faint }}>→ select a project</div>
          </div>
        ) : (
          <div style={{ padding: '20px 24px' }}>
            <div style={{ display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between', marginBottom: 20 }}>
              <div>
                <div style={{ fontFamily: T.mono, fontSize: 20, fontWeight: 700, color: T.textHi, marginBottom: 4 }}>{selectedProject.name}</div>
                <div style={{ fontFamily: T.mono, fontSize: 13, color: T.dim }}>{selectedProject.slug}</div>
              </div>
              <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
                {currentProject === selectedProject.slug ? (
                  <button onClick={() => navigate('/app')}
                    style={{ background: T.greenSoft, border: `1px solid ${T.green}`, color: T.green, fontFamily: T.mono, fontSize: 12, fontWeight: 600, padding: '6px 14px', cursor: 'pointer' }}>
                    ✓ current · enter →
                  </button>
                ) : (
                  <button onClick={() => openProject(selectedProject)}
                    style={{ background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 12, fontWeight: 600, padding: '6px 16px', cursor: 'pointer' }}>
                    [ open project ]
                  </button>
                )}
                {selectedProject.tier && <Pill tone={tierTone(selectedProject.tier)}>{selectedProject.tier}</Pill>}
                {isOwned && (
                  <button onClick={() => handleDelete(selectedProject)}
                    style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}
                    onMouseEnter={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.red; (e.currentTarget as HTMLButtonElement).style.color = T.red; }}
                    onMouseLeave={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.border; (e.currentTarget as HTMLButtonElement).style.color = T.dim; }}>
                    [ delete ]
                  </button>
                )}
              </div>
            </div>

            <div style={{ display: 'grid', gridTemplateColumns: 'repeat(3, 1fr)', gap: 12, marginBottom: 20 }}>
              {([
                ['namespace', selectedProject.namespace],
                ['owner', selectedProject.owner_id === currentUserId ? 'you' : (userNames[selectedProject.owner_id] ? `@${userNames[selectedProject.owner_id]}` : shortId(selectedProject.owner_id))],
                ['project id', shortId(selectedProject.project_id)],
              ] as [string, string][]).map(([k, v]) => (
                <div key={k} style={{ background: T.card, border: `1px solid ${T.border}`, padding: '10px 14px' }}>
                  <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 4, textTransform: 'uppercase' }}>{k}</div>
                  <div style={{ fontFamily: T.mono, fontSize: 13, color: T.textHi, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{v}</div>
                </div>
              ))}
            </div>

            {!isOwned ? (
              <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '12px 16px', fontFamily: T.mono, fontSize: 12, color: T.faint }}>
                → you have <span style={{ color: selectedProject.tier ? tierColor(selectedProject.tier) : T.dim }}>{selectedProject.tier ?? 'no'}</span> access · member management is available to the owner only
              </div>
            ) : (
              <>
                <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 10 }}>
                  <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1 }}>MEMBERS · {members.length}</div>
                  <button onClick={() => loadMembers(selectedProject)} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '2px 8px', cursor: 'pointer' }}>↻</button>
                </div>

                {membersError && <div style={{ background: T.redSoft, border: `1px solid ${T.red}`, padding: '8px 12px', fontFamily: T.mono, fontSize: 11, color: T.red, marginBottom: 12 }}>{membersError}</div>}

                {/* Add member */}
                <div style={{ background: T.card, border: `1px solid ${T.borderHi}`, padding: '12px 14px', marginBottom: 16, display: 'flex', gap: 8, alignItems: 'flex-end', flexWrap: 'wrap' }}>
                  <div style={{ flex: 1, minWidth: 180 }}>
                    <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, marginBottom: 6, letterSpacing: 0.5 }}>USER</div>
                    <input list="project-user-options" value={addUserId} onChange={e => setAddUserId(e.target.value)} placeholder="user id or @username"
                      style={{ ...inputStyle, background: T.cardHi }} />
                    <datalist id="project-user-options">
                      {allUsers.filter(u => u.user_id !== selectedProject.owner_id).map(u => (
                        <option key={u.user_id} value={u.user_id}>@{u.username}</option>
                      ))}
                    </datalist>
                  </div>
                  <div>
                    <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, marginBottom: 6, letterSpacing: 0.5 }}>TIER</div>
                    <select value={addTier} onChange={e => setAddTier(e.target.value as ProjectMemberTier)}
                      style={{ ...inputStyle, background: T.cardHi, width: 'auto', cursor: 'pointer' }}>
                      {MEMBER_TIERS.map(t => <option key={t} value={t}>{t}</option>)}
                    </select>
                  </div>
                  <button onClick={handleAddMember} disabled={addingMember || !addUserId.trim()}
                    style={{ background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 12, fontWeight: 600, padding: '7px 16px', cursor: 'pointer', opacity: (addingMember || !addUserId.trim()) ? 0.6 : 1 }}>
                    {addingMember ? '[ · · · ]' : '[ add ]'}
                  </button>
                </div>

                {membersLoading ? (
                  <div style={{ padding: '16px', fontFamily: T.mono, fontSize: 11, color: T.faint, animation: 'pulse 1s ease-in-out infinite' }}>→ loading members · · ·</div>
                ) : members.length === 0 ? (
                  <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '12px 14px', fontFamily: T.mono, fontSize: 12, color: T.faint }}>→ no members yet</div>
                ) : (
                  <div style={{ background: T.card, border: `1px solid ${T.border}`, overflow: 'hidden' }}>
                    {members.map((m, i) => (
                      <div key={m.user_id} style={{ padding: '10px 14px', borderBottom: i < members.length - 1 ? `1px solid ${T.border}` : 'none', display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 10 }}>
                        <div style={{ minWidth: 0 }}>
                          <div style={{ fontFamily: T.mono, fontSize: 13, color: T.textHi, fontWeight: 600, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>@{userNames[m.user_id] ?? shortId(m.user_id)}</div>
                          <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint }}>{shortId(m.user_id)}</div>
                        </div>
                        <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexShrink: 0 }}>
                          <select value={m.tier} disabled={mutatingUser === m.user_id} onChange={e => handleChangeTier(m.user_id, e.target.value as ProjectMemberTier)}
                            style={{ ...inputStyle, width: 'auto', fontSize: 11, padding: '4px 8px', background: T.cardHi, cursor: 'pointer', color: tierColor(m.tier) }}>
                            {MEMBER_TIERS.map(t => <option key={t} value={t} style={{ color: T.text }}>{t}</option>)}
                          </select>
                          <button onClick={() => handleRemoveMember(m.user_id)} disabled={mutatingUser === m.user_id}
                            style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '4px 10px', cursor: 'pointer' }}
                            onMouseEnter={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.red; (e.currentTarget as HTMLButtonElement).style.color = T.red; }}
                            onMouseLeave={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.border; (e.currentTarget as HTMLButtonElement).style.color = T.dim; }}>
                            remove
                          </button>
                        </div>
                      </div>
                    ))}
                  </div>
                )}
              </>
            )}
          </div>
        )}
      </div>
    </div>
  );
}

/** Raw theme colour for a tier (for inline text/borders where a Pill would be too heavy). */
function tierColor(tier: ProjectTier): string {
  return tier === 'owner' ? T.green : tier === 'admin' ? T.blue : tier === 'developer' ? T.amber : T.dim;
}

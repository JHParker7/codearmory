import { useState, useEffect, useCallback } from 'react';
import { T } from '../../theme';
import { Pill } from '../../components/Pill';
import { useAppSelector } from '../../store/hooks';
import {
  getGiteaAccount, linkGiteaAccount, unlinkGiteaAccount,
  listGiteaRepos, createGiteaRepo, deleteGiteaRepo,
  listBranches, listGitTags, listCommits, listPulls, createPull, mergePull,
} from '../../api/bff';
import type { GiteaAccount, GiteaRepo, Branch, GitTag, GitCommit, PullRequest } from '../../api/bff';
import { timeAgo } from '../../utils';

type RepoTab = 'branches' | 'tags' | 'commits' | 'pulls';

function prStatusTone(status: string): 'green' | 'amber' | 'red' | 'dim' {
  if (status === 'merged') return 'green';
  if (status === 'open') return 'amber';
  if (status === 'closed') return 'dim';
  return 'dim';
}

function AccountPanel({ onLinked }: { onLinked: () => void }) {
  const token = useAppSelector(s => s.auth.token)!;
  const [account, setAccount] = useState<GiteaAccount | null>(null);
  const [loading, setLoading] = useState(true);
  const [gitToken, setGitToken] = useState('');
  const [linking, setLinking] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    getGiteaAccount(token).then(setAccount).catch(() => setAccount(null)).finally(() => setLoading(false));
  }, [token]);

  const handleLink = async () => {
    if (!gitToken.trim()) return;
    setLinking(true);
    setError(null);
    try {
      const acc = await linkGiteaAccount(token, gitToken.trim());
      setAccount(acc);
      setGitToken('');
      onLinked();
    } catch (e: unknown) {
      setError((e as Error).message);
    } finally {
      setLinking(false);
    }
  };

  const handleUnlink = async () => {
    setError(null);
    try {
      await unlinkGiteaAccount(token);
      setAccount(null);
      onLinked();
    } catch (e: unknown) {
      setError((e as Error).message);
    }
  };

  if (loading) return null;

  return (
    <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '12px 16px', marginBottom: 16 }}>
      {account ? (
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
          <div>
            <div style={{ fontFamily: T.mono, fontSize: 11, color: T.faint, marginBottom: 2 }}>LINKED ACCOUNT</div>
            <div style={{ fontFamily: T.mono, fontSize: 13, color: T.green, fontWeight: 600 }}>{account.username}</div>
            {account.url && <div style={{ fontFamily: T.mono, fontSize: 10, color: T.dim }}>{account.url}</div>}
          </div>
          <button onClick={handleUnlink}
            style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}
            onMouseEnter={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.red; (e.currentTarget as HTMLButtonElement).style.color = T.red; }}
            onMouseLeave={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.border; (e.currentTarget as HTMLButtonElement).style.color = T.dim; }}>
            [ unlink ]
          </button>
        </div>
      ) : (
        <div>
          <div style={{ fontFamily: T.mono, fontSize: 11, color: T.faint, marginBottom: 8 }}>LINK FORGEJO ACCOUNT</div>
          {error && <div style={{ background: T.redSoft, border: `1px solid ${T.red}`, padding: '6px 10px', fontFamily: T.mono, fontSize: 11, color: T.red, marginBottom: 10 }}>{error}</div>}
          <div style={{ display: 'flex', gap: 8 }}>
            <input value={gitToken} onChange={e => setGitToken(e.target.value)} type="password" placeholder="Forgejo API token"
              onKeyDown={e => e.key === 'Enter' && handleLink()}
              style={{ flex: 1, background: T.cardHi, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 12, padding: '7px 10px', outline: 'none' }} />
            <button onClick={handleLink} disabled={!gitToken.trim() || linking}
              style={{ background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 11, fontWeight: 600, padding: '7px 14px', cursor: 'pointer', opacity: (!gitToken.trim() || linking) ? 0.6 : 1 }}>
              {linking ? '[ · · · ]' : '[ link ]'}
            </button>
          </div>
        </div>
      )}
    </div>
  );
}

export function Gitea() {
  const token = useAppSelector(s => s.auth.token)!;
  const [repos, setRepos] = useState<GiteaRepo[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<GiteaRepo | null>(null);
  const [repoTab, setRepoTab] = useState<RepoTab>('branches');
  const [showCreate, setShowCreate] = useState(false);

  const [branches, setBranches] = useState<Branch[]>([]);
  const [gitTags, setGitTags] = useState<GitTag[]>([]);
  const [commits, setCommits] = useState<GitCommit[]>([]);
  const [pulls, setPulls] = useState<PullRequest[]>([]);
  const [tabLoading, setTabLoading] = useState(false);
  const [tabError, setTabError] = useState<string | null>(null);

  const [newName, setNewName] = useState('');
  const [newDesc, setNewDesc] = useState('');
  const [newPrivate, setNewPrivate] = useState(false);
  const [creating, setCreating] = useState(false);
  const [createError, setCreateError] = useState<string | null>(null);

  const [showCreatePR, setShowCreatePR] = useState(false);
  const [prTitle, setPrTitle] = useState('');
  const [prHead, setPrHead] = useState('');
  const [prBase, setPrBase] = useState('');
  const [prBody, setPrBody] = useState('');
  const [creatingPR, setCreatingPR] = useState(false);

  const fetchRepos = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      setRepos(await listGiteaRepos(token));
    } catch (e: unknown) {
      setError((e as Error).message);
    } finally {
      setLoading(false);
    }
  }, [token]);

  useEffect(() => { fetchRepos(); }, [fetchRepos]);

  const loadTab = useCallback(async (repo: GiteaRepo, tab: RepoTab) => {
    setTabLoading(true);
    setTabError(null);
    try {
      if (tab === 'branches') setBranches(await listBranches(token, repo.owner, repo.name));
      if (tab === 'tags') setGitTags(await listGitTags(token, repo.owner, repo.name));
      if (tab === 'commits') setCommits(await listCommits(token, repo.owner, repo.name));
      if (tab === 'pulls') setPulls(await listPulls(token, repo.owner, repo.name));
    } catch (e: unknown) {
      setTabError((e as Error).message);
    } finally {
      setTabLoading(false);
    }
  }, [token]);

  const selectRepo = useCallback((repo: GiteaRepo) => {
    setSelected(repo);
    setRepoTab('branches');
    setShowCreatePR(false);
    loadTab(repo, 'branches');
  }, [loadTab]);

  const switchTab = (tab: RepoTab) => {
    setRepoTab(tab);
    if (selected) loadTab(selected, tab);
  };

  const handleCreateRepo = async () => {
    if (!newName.trim()) return;
    setCreating(true);
    setCreateError(null);
    try {
      const repo = await createGiteaRepo(token, { name: newName.trim(), description: newDesc.trim() || undefined, private: newPrivate });
      setRepos(prev => [repo, ...prev]);
      setNewName('');
      setNewDesc('');
      setNewPrivate(false);
      setShowCreate(false);
    } catch (e: unknown) {
      setCreateError((e as Error).message);
    } finally {
      setCreating(false);
    }
  };

  const handleDeleteRepo = async (repo: GiteaRepo) => {
    try {
      await deleteGiteaRepo(token, repo.owner, repo.name);
      setRepos(prev => prev.filter(r => r.full_name !== repo.full_name));
      if (selected?.full_name === repo.full_name) setSelected(null);
    } catch (e: unknown) {
      setError((e as Error).message);
    }
  };

  const handleMergePR = async (index: number) => {
    if (!selected) return;
    try {
      await mergePull(token, selected.owner, selected.name, index);
      setPulls(prev => prev.map(p => p.index === index ? { ...p, status: 'merged' } : p));
    } catch (e: unknown) {
      setTabError((e as Error).message);
    }
  };

  const handleCreatePR = async () => {
    if (!selected || !prTitle.trim() || !prHead.trim() || !prBase.trim()) return;
    setCreatingPR(true);
    try {
      const pr = await createPull(token, selected.owner, selected.name, {
        title: prTitle.trim(), head: prHead.trim(), base: prBase.trim(), body: prBody.trim() || undefined,
      });
      setPulls(prev => [pr, ...prev]);
      setPrTitle(''); setPrHead(''); setPrBase(''); setPrBody('');
      setShowCreatePR(false);
    } catch (e: unknown) {
      setTabError((e as Error).message);
    } finally {
      setCreatingPR(false);
    }
  };

  return (
    <div style={{ display: 'flex', height: '100%', overflow: 'hidden' }}>
      {/* Repo list */}
      <div style={{ width: 260, flexShrink: 0, borderRight: `1px solid ${T.border}`, display: 'flex', flexDirection: 'column', background: T.bgAlt }}>
        <div style={{ padding: '14px 14px 10px', borderBottom: `1px solid ${T.border}` }}>
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 4 }}>
            <span style={{ fontFamily: T.mono, fontSize: 13, fontWeight: 700, color: T.textHi }}>git/</span>
            <div style={{ display: 'flex', gap: 6 }}>
              <button onClick={() => setShowCreate(v => !v)}
                style={{ background: showCreate ? T.greenSoft : 'transparent', border: `1px solid ${showCreate ? T.green : T.border}`, color: showCreate ? T.green : T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 7px', cursor: 'pointer' }}>+</button>
              <button onClick={fetchRepos} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 7px', cursor: 'pointer' }}>↻</button>
            </div>
          </div>
          <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint }}>
            {repos.length > 0 && `${repos.length} repo${repos.length !== 1 ? 's' : ''}`}
          </div>
        </div>

        {showCreate && (
          <div style={{ padding: '12px 14px', borderBottom: `1px solid ${T.border}`, background: T.card }}>
            {createError && <div style={{ background: T.redSoft, border: `1px solid ${T.red}`, padding: '6px 8px', fontFamily: T.mono, fontSize: 10, color: T.red, marginBottom: 8 }}>{createError}</div>}
            <input value={newName} onChange={e => setNewName(e.target.value)} placeholder="repo name" autoFocus
              style={{ width: '100%', background: T.cardHi, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 12, padding: '6px 8px', outline: 'none', boxSizing: 'border-box', marginBottom: 6 }} />
            <input value={newDesc} onChange={e => setNewDesc(e.target.value)} placeholder="description (optional)"
              style={{ width: '100%', background: T.cardHi, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 11, padding: '6px 8px', outline: 'none', boxSizing: 'border-box', marginBottom: 8 }} />
            <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 8 }}>
              <button onClick={() => setNewPrivate(v => !v)}
                style={{ background: newPrivate ? T.greenSoft : 'transparent', border: `1px solid ${newPrivate ? T.green : T.border}`, color: newPrivate ? T.green : T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 8px', cursor: 'pointer' }}>
                {newPrivate ? '⊙ private' : '○ public'}
              </button>
            </div>
            <div style={{ display: 'flex', gap: 6 }}>
              <button onClick={handleCreateRepo} disabled={!newName.trim() || creating}
                style={{ flex: 1, background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 11, fontWeight: 600, padding: '6px 0', cursor: 'pointer', opacity: (!newName.trim() || creating) ? 0.6 : 1 }}>
                {creating ? '[ · · · ]' : '[ create ]'}
              </button>
              <button onClick={() => setShowCreate(false)} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '6px 8px', cursor: 'pointer' }}>✕</button>
            </div>
          </div>
        )}

        <div style={{ flex: 1, overflow: 'auto' }}>
          {loading ? (
            <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint, animation: 'pulse 1s ease-in-out infinite' }}>→ loading · · ·</div>
          ) : error ? (
            <div style={{ padding: '14px', fontFamily: T.mono, fontSize: 11, color: T.red }}>{error}</div>
          ) : repos.length === 0 ? (
            <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint }}>→ no repositories</div>
          ) : repos.map(repo => {
            const isActive = selected?.full_name === repo.full_name;
            return (
              <button key={repo.full_name} onClick={() => selectRepo(repo)}
                style={{ width: '100%', textAlign: 'left', padding: '10px 14px', background: isActive ? T.greenSoft : 'transparent', border: 0, borderLeft: `2px solid ${isActive ? T.green : 'transparent'}`, fontFamily: T.mono, cursor: 'pointer', color: T.text, display: 'block', transition: 'background .12s' }}>
                <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
                  <span style={{ fontSize: 12, fontWeight: 600, color: isActive ? T.textHi : T.text }}>{repo.name}</span>
                  {repo.private && <span style={{ fontSize: 9, color: T.amber, border: `1px solid ${T.amber}`, padding: '0 4px' }}>private</span>}
                </div>
                <div style={{ fontSize: 10, color: T.faint, marginTop: 2 }}>{repo.owner}</div>
                {repo.default_branch && <div style={{ fontSize: 10, color: T.dim, marginTop: 1 }}>⌥ {repo.default_branch}</div>}
              </button>
            );
          })}
        </div>
      </div>

      {/* Detail panel */}
      <div style={{ flex: 1, display: 'flex', flexDirection: 'column', overflow: 'hidden' }}>
        {!selected ? (
          <div style={{ flex: 1, padding: '20px' }}>
            <AccountPanel onLinked={fetchRepos} />
            <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', height: 200 }}>
              <div style={{ fontFamily: T.mono, fontSize: 12, color: T.faint }}>→ select a repository</div>
            </div>
          </div>
        ) : (
          <>
            {/* Repo header */}
            <div style={{ padding: '12px 20px', borderBottom: `1px solid ${T.border}`, background: T.bgAlt, flexShrink: 0, display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between' }}>
              <div>
                <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                  <span style={{ fontFamily: T.mono, fontSize: 15, fontWeight: 700, color: T.textHi }}>{selected.full_name}</span>
                  {selected.private && <Pill tone="amber">private</Pill>}
                </div>
                {selected.description && <div style={{ fontFamily: T.mono, fontSize: 11, color: T.dim, marginTop: 2 }}>{selected.description}</div>}
                {selected.updated_at && <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, marginTop: 2 }}>updated {timeAgo(selected.updated_at)} ago</div>}
              </div>
              <button onClick={() => handleDeleteRepo(selected)}
                style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}
                onMouseEnter={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.red; (e.currentTarget as HTMLButtonElement).style.color = T.red; }}
                onMouseLeave={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.border; (e.currentTarget as HTMLButtonElement).style.color = T.dim; }}>
                [ delete ]
              </button>
            </div>

            {/* Tabs */}
            <div style={{ display: 'flex', borderBottom: `1px solid ${T.border}`, background: T.bgAlt, flexShrink: 0 }}>
              {(['branches', 'tags', 'commits', 'pulls'] as RepoTab[]).map(t => (
                <button key={t} onClick={() => switchTab(t)}
                  style={{ background: repoTab === t ? T.card : 'transparent', border: 'none', borderBottom: `2px solid ${repoTab === t ? T.green : 'transparent'}`, color: repoTab === t ? T.textHi : T.dim, fontFamily: T.mono, fontSize: 12, padding: '10px 18px', cursor: 'pointer', letterSpacing: 0.3 }}>
                  {t}
                </button>
              ))}
              {repoTab === 'pulls' && (
                <button onClick={() => setShowCreatePR(v => !v)}
                  style={{ marginLeft: 'auto', background: showCreatePR ? T.greenSoft : 'transparent', border: 'none', borderBottom: '2px solid transparent', color: showCreatePR ? T.green : T.dim, fontFamily: T.mono, fontSize: 11, padding: '10px 16px', cursor: 'pointer' }}>
                  + new PR
                </button>
              )}
            </div>

            {/* Create PR form */}
            {repoTab === 'pulls' && showCreatePR && (
              <div style={{ padding: '12px 20px', borderBottom: `1px solid ${T.border}`, background: T.card, flexShrink: 0 }}>
                <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 8, marginBottom: 8 }}>
                  <input value={prHead} onChange={e => setPrHead(e.target.value)} placeholder="head branch"
                    style={{ background: T.cardHi, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 12, padding: '6px 8px', outline: 'none' }} />
                  <input value={prBase} onChange={e => setPrBase(e.target.value)} placeholder="base branch"
                    style={{ background: T.cardHi, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 12, padding: '6px 8px', outline: 'none' }} />
                </div>
                <input value={prTitle} onChange={e => setPrTitle(e.target.value)} placeholder="PR title"
                  style={{ width: '100%', background: T.cardHi, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 12, padding: '6px 8px', outline: 'none', boxSizing: 'border-box', marginBottom: 8 }} />
                <textarea value={prBody} onChange={e => setPrBody(e.target.value)} placeholder="description (optional)" rows={2}
                  style={{ width: '100%', background: T.cardHi, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 11, padding: '6px 8px', outline: 'none', resize: 'none', boxSizing: 'border-box', marginBottom: 8 }} />
                <div style={{ display: 'flex', gap: 8 }}>
                  <button onClick={handleCreatePR} disabled={!prTitle.trim() || !prHead.trim() || !prBase.trim() || creatingPR}
                    style={{ background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 11, fontWeight: 600, padding: '6px 14px', cursor: 'pointer', opacity: (!prTitle.trim() || !prHead.trim() || !prBase.trim() || creatingPR) ? 0.6 : 1 }}>
                    {creatingPR ? '[ · · · ]' : '[ create PR ]'}
                  </button>
                  <button onClick={() => setShowCreatePR(false)} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '6px 12px', cursor: 'pointer' }}>cancel</button>
                </div>
              </div>
            )}

            <div style={{ flex: 1, overflow: 'auto', padding: '16px 20px' }}>
              {tabError && <div style={{ background: T.redSoft, border: `1px solid ${T.red}`, padding: '8px 12px', fontFamily: T.mono, fontSize: 11, color: T.red, marginBottom: 12 }}>{tabError}</div>}
              {tabLoading ? (
                <div style={{ fontFamily: T.mono, fontSize: 11, color: T.faint, animation: 'pulse 1s ease-in-out infinite' }}>→ loading · · ·</div>
              ) : (
                <>
                  {repoTab === 'branches' && (
                    branches.length === 0 ? (
                      <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '16px', fontFamily: T.mono, fontSize: 12, color: T.faint, textAlign: 'center' }}>→ no branches</div>
                    ) : (
                      <div style={{ background: T.card, border: `1px solid ${T.border}`, overflow: 'hidden' }}>
                        {branches.map((b, i) => (
                          <div key={b.name} style={{ display: 'flex', alignItems: 'center', padding: '9px 14px', borderBottom: i < branches.length - 1 ? `1px solid ${T.border}` : 'none', gap: 12, fontFamily: T.mono }}>
                            <span style={{ fontSize: 13, color: b.name === selected.default_branch ? T.green : T.textHi, fontWeight: b.name === selected.default_branch ? 600 : 400 }}>⌥ {b.name}</span>
                            {b.name === selected.default_branch && <span style={{ fontSize: 9, color: T.green, border: `1px solid ${T.green}`, padding: '0 4px' }}>default</span>}
                            {b.commit_sha && <span style={{ fontSize: 10, color: T.faint, marginLeft: 'auto' }}>{b.commit_sha.slice(0, 8)}</span>}
                          </div>
                        ))}
                      </div>
                    )
                  )}

                  {repoTab === 'tags' && (
                    gitTags.length === 0 ? (
                      <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '16px', fontFamily: T.mono, fontSize: 12, color: T.faint, textAlign: 'center' }}>→ no tags</div>
                    ) : (
                      <div style={{ background: T.card, border: `1px solid ${T.border}`, overflow: 'hidden' }}>
                        {gitTags.map((tag, i) => (
                          <div key={tag.name} style={{ display: 'flex', alignItems: 'center', padding: '9px 14px', borderBottom: i < gitTags.length - 1 ? `1px solid ${T.border}` : 'none', fontFamily: T.mono, gap: 12 }}>
                            <span style={{ fontSize: 13, color: T.blue }}>⊙ {tag.name}</span>
                            {tag.commit_sha && <span style={{ fontSize: 10, color: T.faint, marginLeft: 'auto' }}>{tag.commit_sha.slice(0, 8)}</span>}
                          </div>
                        ))}
                      </div>
                    )
                  )}

                  {repoTab === 'commits' && (
                    commits.length === 0 ? (
                      <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '16px', fontFamily: T.mono, fontSize: 12, color: T.faint, textAlign: 'center' }}>→ no commits</div>
                    ) : (
                      <div style={{ background: T.card, border: `1px solid ${T.border}`, overflow: 'hidden' }}>
                        {commits.map((c, i) => (
                          <div key={c.sha} style={{ padding: '10px 14px', borderBottom: i < commits.length - 1 ? `1px solid ${T.border}` : 'none', fontFamily: T.mono }}>
                            <div style={{ display: 'flex', alignItems: 'center', gap: 10, marginBottom: 2 }}>
                              <span style={{ fontSize: 11, color: T.amber }}>{c.sha.slice(0, 8)}</span>
                              {c.author && <span style={{ fontSize: 10, color: T.faint }}>{c.author}</span>}
                              {c.date && <span style={{ fontSize: 10, color: T.faint, marginLeft: 'auto' }}>{timeAgo(c.date)} ago</span>}
                            </div>
                            <div style={{ fontSize: 12, color: T.text, lineHeight: 1.4 }}>{c.message.split('\n')[0]}</div>
                          </div>
                        ))}
                      </div>
                    )
                  )}

                  {repoTab === 'pulls' && (
                    pulls.length === 0 ? (
                      <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '16px', fontFamily: T.mono, fontSize: 12, color: T.faint, textAlign: 'center' }}>→ no pull requests</div>
                    ) : (
                      <div style={{ background: T.card, border: `1px solid ${T.border}`, overflow: 'hidden' }}>
                        {pulls.map((pr, i) => (
                          <div key={pr.index} style={{ padding: '10px 14px', borderBottom: i < pulls.length - 1 ? `1px solid ${T.border}` : 'none', fontFamily: T.mono }}>
                            <div style={{ display: 'flex', alignItems: 'center', gap: 10, marginBottom: 4 }}>
                              <Pill tone={prStatusTone(pr.status)}>{pr.status}</Pill>
                              <span style={{ fontSize: 13, color: T.textHi, fontWeight: 600, flex: 1 }}>{pr.title}</span>
                              <span style={{ fontSize: 10, color: T.faint }}>#{pr.index}</span>
                            </div>
                            <div style={{ fontSize: 11, color: T.dim, marginBottom: 4 }}>
                              <span style={{ color: T.green }}>{pr.head}</span>
                              <span style={{ color: T.faint }}> → </span>
                              <span style={{ color: T.blue }}>{pr.base}</span>
                            </div>
                            {pr.status === 'open' && (
                              <button onClick={() => handleMergePR(pr.index)}
                                style={{ background: T.greenSoft, border: `1px solid ${T.green}`, color: T.green, fontFamily: T.mono, fontSize: 10, padding: '3px 10px', cursor: 'pointer' }}>
                                [ merge ]
                              </button>
                            )}
                          </div>
                        ))}
                      </div>
                    )
                  )}
                </>
              )}
            </div>
          </>
        )}
      </div>
    </div>
  );
}

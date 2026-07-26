/**
 * Containers page — browse the OCI registry: repos in a sidebar, and per-repo
 * tags + image manifests (with layers) in a tabbed detail panel. supports
 * deleting a manifest by digest when permitted. data via the bff.
 */
import { useState, useEffect, useCallback } from 'react';
import { useUrlState, useUrlParam } from '../../hooks/useUrlState';
import { T } from '../../theme';
import { useResizableWidth } from '../../components/ResizeHandle';
import { useConfirm } from '../../components/ConfirmDialog';
import { useAppSelector } from '../../store/hooks';
import { listContainerRepos, listImageTags, getManifest, deleteManifest } from '../../api/bff';
import type { ContainerRepo, ImageTag, ImageManifest } from '../../api/bff';
import { timeAgo } from '../../utils';

type RepoTab = 'tags' | 'manifest';

/** Container registry browser: repo list + tags/manifest tabs with layer detail and manifest delete. */
export function Containers() {
  const token = useAppSelector(s => s.auth.token)!;
  const canDelete = useAppSelector(s => s.auth.permissions?.['containers:deleteManifest'] === true);
  const [repos, setRepos] = useState<ContainerRepo[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<ContainerRepo | null>(null);
  const [selFull, setSelFull] = useUrlParam('repo');
  const [tab, setTab] = useUrlState<RepoTab>('tab', 'tags');

  const [tags, setTags] = useState<ImageTag[]>([]);
  const [tagsLoading, setTagsLoading] = useState(false);
  const [selectedTag, setSelectedTag] = useUrlParam('tag');
  const [manifest, setManifest] = useState<ImageManifest | null>(null);
  const [manifestLoading, setManifestLoading] = useState(false);
  const [manifestError, setManifestError] = useState<string | null>(null);
  const [deleting, setDeleting] = useState<string | null>(null);

  const fetchRepos = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      setRepos(await listContainerRepos(token));
    } catch (e: unknown) {
      setError((e as Error).message);
    } finally {
      setLoading(false);
    }
  }, [token]);

  useEffect(() => { fetchRepos(); }, [fetchRepos]);

  const selectRepo = useCallback(async (repo: ContainerRepo) => {
    setSelected(repo);
    setSelFull(repo.full_name); // mirror the selection into the URL so a refresh reopens it
    setTab('tags');
    setSelectedTag(null);
    setManifest(null);
    setTagsLoading(true);
    try {
      setTags(await listImageTags(token, repo.namespace, repo.name));
    } catch {
      setTags([]);
    } finally {
      setTagsLoading(false);
    }
  }, [token, setSelFull, setTab, setSelectedTag]);

  // Reopen the repo named in the URL once the list has loaded (e.g. after a refresh).
  useEffect(() => {
    if (selected || !selFull || repos.length === 0) return;
    const r = repos.find((x) => x.full_name === selFull);
    if (r) selectRepo(r);
  }, [selected, selFull, repos, selectRepo]);

  const viewManifest = useCallback(async (ref: string) => {
    if (!selected) return;
    setSelectedTag(ref);
    setTab('manifest');
    setManifest(null);
    setManifestError(null);
    setManifestLoading(true);
    try {
      setManifest(await getManifest(token, selected.namespace, selected.name, ref));
    } catch (e: unknown) {
      setManifestError((e as Error).message);
    } finally {
      setManifestLoading(false);
    }
  }, [token, selected]);

  const [confirm, confirmEl] = useConfirm();
  const [railW, railHandle] = useResizableWidth('rail.containers.main', 260, { min: 200, max: 480 });

  const handleDeleteManifest = async (digest: string) => {
    if (!selected) return;
    const tagName = tags.find(t => t.digest === digest)?.name;
    if (!(await confirm({ message: `Delete image ${selected.name}${tagName ? `:${tagName}` : ''}? This removes the manifest from the registry.` }))) return;
    setDeleting(digest);
    try {
      await deleteManifest(token, selected.namespace, selected.name, digest);
      setTags(prev => prev.filter(t => t.digest !== digest));
      if (manifest?.digest === digest) { setManifest(null); setSelectedTag(null); }
    } catch (e: unknown) {
      setError((e as Error).message);
    } finally {
      setDeleting(null);
    }
  };

  /** Format a byte count as a human-readable size (B/KB/MB), or — when null. */
  function formatSize(bytes: number | null | undefined): string {
    if (bytes == null) return '—';
    if (bytes < 1024) return `${bytes}B`;
    if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)}KB`;
    return `${(bytes / (1024 * 1024)).toFixed(1)}MB`;
  }

  return (
    <div style={{ display: 'flex', height: '100%', overflow: 'hidden' }}>
      {confirmEl}
      {/* Repo list */}
      <div style={{ width: railW, flexShrink: 0, borderRight: `1px solid ${T.border}`, display: 'flex', flexDirection: 'column', background: T.bgAlt }}>
        <div style={{ padding: '14px 14px 10px', borderBottom: `1px solid ${T.border}` }}>
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 4 }}>
            <span style={{ fontFamily: T.mono, fontSize: 13, fontWeight: 700, color: T.textHi }}>containers/</span>
            <button onClick={fetchRepos} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 7px', cursor: 'pointer' }}>↻</button>
          </div>
          <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint }}>
            {repos.length > 0 && `${repos.length} repo${repos.length !== 1 ? 's' : ''}`}
          </div>
        </div>

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
                <div style={{ fontSize: 12, fontWeight: 600, color: isActive ? T.textHi : T.text, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{repo.name}</div>
                <div style={{ fontSize: 10, color: T.faint, marginTop: 2 }}>{repo.namespace}</div>
                {repo.tags_count != null && <div style={{ fontSize: 10, color: T.dim, marginTop: 1 }}>{repo.tags_count} tag{repo.tags_count !== 1 ? 's' : ''}</div>}
              </button>
            );
          })}
        </div>
      </div>
      {railHandle}

      {/* Detail panel */}
      <div style={{ flex: 1, display: 'flex', flexDirection: 'column', overflow: 'hidden' }}>
        {!selected ? (
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', height: '100%' }}>
            <div style={{ fontFamily: T.mono, fontSize: 12, color: T.faint }}>→ select a repository</div>
          </div>
        ) : (
          <>
            {/* Repo header */}
            <div style={{ padding: '12px 20px', borderBottom: `1px solid ${T.border}`, background: T.bgAlt, flexShrink: 0 }}>
              <div style={{ fontFamily: T.mono, fontSize: 15, fontWeight: 700, color: T.textHi }}>{selected.full_name}</div>
              {selected.last_pushed && (
                <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, marginTop: 2 }}>last pushed {timeAgo(selected.last_pushed)} ago</div>
              )}
            </div>

            {/* Tabs */}
            <div style={{ display: 'flex', borderBottom: `1px solid ${T.border}`, background: T.bgAlt, flexShrink: 0 }}>
              {(['tags', 'manifest'] as RepoTab[]).map(t => (
                <button key={t} onClick={() => setTab(t)}
                  style={{ background: tab === t ? T.card : 'transparent', border: 'none', borderBottom: `2px solid ${tab === t ? T.green : 'transparent'}`, color: tab === t ? T.textHi : T.dim, fontFamily: T.mono, fontSize: 12, padding: '10px 18px', cursor: 'pointer', letterSpacing: 0.3 }}>
                  {t}
                  {t === 'tags' && tags.length > 0 && <span style={{ marginLeft: 6, fontSize: 10, color: T.faint }}>({tags.length})</span>}
                </button>
              ))}
            </div>

            {/* Tab content */}
            <div style={{ flex: 1, overflow: 'auto', padding: '16px 20px' }}>
              {tab === 'tags' && (
                tagsLoading ? (
                  <div style={{ fontFamily: T.mono, fontSize: 11, color: T.faint, animation: 'pulse 1s ease-in-out infinite' }}>→ loading · · ·</div>
                ) : tags.length === 0 ? (
                  <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '20px', fontFamily: T.mono, fontSize: 12, color: T.faint, textAlign: 'center' }}>→ no tags</div>
                ) : (
                  <div style={{ background: T.card, border: `1px solid ${T.border}`, overflow: 'hidden' }}>
                    {tags.map((tag, i) => (
                      <div key={tag.digest} style={{ display: 'flex', alignItems: 'center', padding: '10px 14px', borderBottom: i < tags.length - 1 ? `1px solid ${T.border}` : 'none', gap: 12 }}>
                        <span style={{ fontFamily: T.mono, fontSize: 13, color: T.blue, fontWeight: 600, flex: 1 }}>{tag.name}</span>
                        <span style={{ fontFamily: T.mono, fontSize: 10, color: T.faint }}>{formatSize(tag.size)}</span>
                        {tag.pushed_at && <span style={{ fontFamily: T.mono, fontSize: 10, color: T.faint }}>{timeAgo(tag.pushed_at)} ago</span>}
                        <button onClick={() => viewManifest(tag.name)}
                          style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 8px', cursor: 'pointer' }}>
                          manifest
                        </button>
                        {canDelete && (
                          <button onClick={() => handleDeleteManifest(tag.digest)} disabled={deleting === tag.digest}
                            style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 8px', cursor: 'pointer', opacity: deleting === tag.digest ? 0.5 : 1 }}
                            onMouseEnter={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.red; (e.currentTarget as HTMLButtonElement).style.color = T.red; }}
                            onMouseLeave={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.border; (e.currentTarget as HTMLButtonElement).style.color = T.dim; }}>
                            [ delete ]
                          </button>
                        )}
                      </div>
                    ))}
                  </div>
                )
              )}

              {tab === 'manifest' && (
                !selectedTag ? (
                  <div style={{ fontFamily: T.mono, fontSize: 12, color: T.faint }}>→ click "manifest" on a tag to view it</div>
                ) : manifestLoading ? (
                  <div style={{ fontFamily: T.mono, fontSize: 11, color: T.faint, animation: 'pulse 1s ease-in-out infinite' }}>→ loading · · ·</div>
                ) : manifestError ? (
                  <div style={{ background: T.redSoft, border: `1px solid ${T.red}`, padding: '10px 14px', fontFamily: T.mono, fontSize: 11, color: T.red }}>{manifestError}</div>
                ) : manifest ? (
                  <>
                    <div style={{ fontFamily: T.mono, fontSize: 11, color: T.faint, marginBottom: 12 }}>tag: <span style={{ color: T.blue }}>{selectedTag}</span></div>
                    <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 12, marginBottom: 16 }}>
                      {([
                        ['digest', manifest.digest.slice(0, 24) + '…'],
                        ['size', formatSize(manifest.size)],
                        ...(manifest.media_type ? [['media type', manifest.media_type] as [string, string]] : []),
                        ...(manifest.created ? [['created', timeAgo(manifest.created) + ' ago'] as [string, string]] : []),
                      ] as [string, string][]).map(([k, v]) => (
                        <div key={k} style={{ background: T.card, border: `1px solid ${T.border}`, padding: '10px 14px' }}>
                          <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 4, textTransform: 'uppercase' }}>{k}</div>
                          <div style={{ fontFamily: T.mono, fontSize: 12, color: T.textHi, wordBreak: 'break-all' }}>{v}</div>
                        </div>
                      ))}
                    </div>
                    {manifest.layers && manifest.layers.length > 0 && (
                      <>
                        <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 8 }}>LAYERS · {manifest.layers.length}</div>
                        <div style={{ background: T.card, border: `1px solid ${T.border}`, overflow: 'hidden' }}>
                          {manifest.layers.map((layer, i) => (
                            <div key={layer.digest} style={{ display: 'flex', padding: '8px 12px', borderBottom: i < manifest.layers!.length - 1 ? `1px solid ${T.border}` : 'none', fontFamily: T.mono, fontSize: 11, gap: 12 }}>
                              <span style={{ color: T.dim, flex: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{layer.digest}</span>
                              <span style={{ color: T.faint, flexShrink: 0 }}>{formatSize(layer.size)}</span>
                            </div>
                          ))}
                        </div>
                      </>
                    )}
                  </>
                ) : null
              )}
            </div>
          </>
        )}
      </div>
    </div>
  );
}

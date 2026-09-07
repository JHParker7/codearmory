/**
 * Wiki page: browse and edit a project's source-of-truth wiki. Matches the portal
 * browse idiom (Projects/Repos): a left rail lists the pages grouped by type, and
 * the main pane shows either the Contents index (the root, when nothing is
 * selected), a rendered page, or the edit form. Markdown pages render through the
 * shared Markdown component; other formats (openapi/sql/ts/yaml) show as a code
 * block. Content lives in git behind the wiki service (every save is a bot
 * commit); the git history is intentionally not surfaced. Calls go through the
 * BFF to /api/wiki/*.
 */
import { useState, useEffect, useCallback, useMemo } from 'react';
import { useUrlParam } from '../../hooks/useUrlState';
import { T } from '../../theme';
import { useConfirm } from '../../components/ConfirmDialog';
import { useResizableWidth } from '../../components/ResizeHandle';
import { useAppSelector } from '../../store/hooks';
import { Markdown } from './Repos';
import { listWikiPages, getWikiPage, putWikiPage, deleteWikiPage } from '../../api/bff';
import type { WikiPageMeta, WikiPageType, WikiPagePayload } from '../../api/bff';

const TYPES: WikiPageType[] = ['overview', 'architecture', 'contract', 'model', 'service', 'component', 'decision', 'ticket'];
const STACKS = ['shared', 'frontend', 'backend', 'infra'];
const FORMATS = ['md', 'openapi', 'sql', 'ts', 'yaml', 'html'];

const box: React.CSSProperties = { background: 'transparent', border: `1px solid ${T.border}`, color: T.text, padding: '6px 8px', borderRadius: 4, fontSize: 13, fontFamily: 'inherit' };
const btn: React.CSSProperties = { background: T.cardHi, border: `1px solid ${T.border}`, color: T.textHi, padding: '5px 12px', borderRadius: 4, cursor: 'pointer', fontSize: 12, fontFamily: T.mono };

interface Draft { id: string; type: WikiPageType; stack: string; format: string; title: string; status: string; content: string; }
const emptyDraft: Draft = { id: '', type: 'overview', stack: 'shared', format: 'md', title: '', status: 'draft', content: '' };

export function Wiki() {
  const token = useAppSelector(s => s.auth.token)!;
  const [confirm, confirmEl] = useConfirm();
  const project = useAppSelector(s => s.project.current);
  const [pageId, setPageId] = useUrlParam('page');
  const [pages, setPages] = useState<WikiPageMeta[]>([]);
  const [draft, setDraft] = useState<Draft>(emptyDraft);
  const [creating, setCreating] = useState(false);
  const [editing, setEditing] = useState(false);
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState(false);
  const [railW, railHandle] = useResizableWidth('rail.wiki', 260, { min: 200, max: 420 });
  // The rail stacks next to the main sidebar nav, so it folds away once you're
  // reading a page (focused on content) and peeks back open on hover. It stays
  // fully open on the Contents root and while editing, where you're navigating.
  const [railHover, setRailHover] = useState(false);

  const loadManifest = useCallback(async (proj: string) => {
    setErr('');
    try { setPages((await listWikiPages(token, proj)).pages ?? []); }
    catch (e: unknown) { setErr(e instanceof Error ? e.message : 'failed to load'); setPages([]); }
  }, [token]);

  useEffect(() => { if (project) loadManifest(project); }, [project, loadManifest]);

  // Load the selected page's content. Land in the rendered view, not edit.
  useEffect(() => {
    if (!project || !pageId) return;
    setCreating(false); setEditing(false);
    (async () => {
      try {
        const p = await getWikiPage(token, project, pageId);
        setDraft({ id: p.id, type: p.type, stack: p.stack ?? 'shared', format: p.format, title: p.title, status: p.status, content: p.content });
      } catch (e: unknown) { setErr(e instanceof Error ? e.message : 'failed to load page'); }
    })();
  }, [token, project, pageId]);

  const openContents = () => { setPageId(null); setCreating(false); setEditing(false); setDraft(emptyDraft); setErr(''); };
  const newPage = () => { setCreating(true); setEditing(true); setPageId(null); setDraft(emptyDraft); setErr(''); };
  const startEdit = () => { setEditing(true); setErr(''); };
  const cancelEdit = () => { setEditing(false); setCreating(false); setErr(''); if (!pageId) setDraft(emptyDraft); };

  const save = async () => {
    if (!project || !draft.id.trim() || !draft.title.trim()) { setErr('id and title are required'); return; }
    setBusy(true); setErr('');
    try {
      const payload: WikiPagePayload = { type: draft.type, stack: draft.stack, format: draft.format, title: draft.title, status: draft.status, content: draft.content };
      await putWikiPage(token, project, draft.id.trim(), payload);
      setCreating(false); setEditing(false);
      await loadManifest(project);
      setPageId(draft.id.trim());
    } catch (e: unknown) { setErr(e instanceof Error ? e.message : 'save failed'); }
    setBusy(false);
  };

  const del = async () => {
    if (!project || !draft.id) return;
    if (!(await confirm({ message: `Delete wiki page "${draft.id}"?`, confirmLabel: 'delete' }))) return;
    setBusy(true);
    try { await deleteWikiPage(token, project, draft.id); await loadManifest(project); openContents(); }
    catch (e: unknown) { setErr(e instanceof Error ? e.message : 'delete failed'); }
    setBusy(false);
  };

  const set = <K extends keyof Draft>(k: K, v: Draft[K]) => setDraft(d => ({ ...d, [k]: v }));

  const editMode = creating || editing;
  // Minimise the rail while reading a page; keep it open on Contents/edit. Hover peeks.
  const railMin = !!pageId && !editMode;
  const railOpen = !railMin || railHover;
  const selected = pages.find(p => p.id === pageId);
  // Pages grouped by type, in the canonical type order, for the rail + contents.
  const groups = useMemo(
    () => TYPES.map(t => [t, pages.filter(p => p.type === t)] as [WikiPageType, WikiPageMeta[]]).filter(([, ps]) => ps.length),
    [pages],
  );

  if (!project) {
    return (
      <div style={{ padding: 24, color: T.dim, fontSize: 13 }}>
        <h1 style={{ fontSize: 18, color: T.textHi, marginBottom: 8 }}>wiki/</h1>
        Select a project from the sidebar to view its wiki.
      </div>
    );
  }

  const railItem = (p: WikiPageMeta) => {
    const active = pageId === p.id && !editMode;
    return (
      <button key={p.id} onClick={() => setPageId(p.id)}
        style={{ display: 'block', width: '100%', textAlign: 'left', padding: '6px 10px 6px 16px', background: active ? T.greenSoft : 'transparent', border: 0, borderLeft: `2px solid ${active ? T.green : 'transparent'}`, color: active ? T.textHi : T.text, cursor: 'pointer', fontSize: 12.5, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
        {p.title}
      </button>
    );
  };

  return (
    <div style={{ display: 'flex', height: '100%', overflow: 'hidden' }}>
      {confirmEl}
      {/* Rail: contents. Minimises to a thin strip while reading a page; hover peeks it open. */}
      <div onMouseEnter={() => setRailHover(true)} onMouseLeave={() => setRailHover(false)}
        style={{ width: railOpen ? railW : 30, flexShrink: 0, borderRight: `1px solid ${T.border}`, background: T.bgAlt, display: 'flex', flexDirection: 'column', overflow: railOpen ? 'auto' : 'hidden', transition: 'width .14s ease' }}>
        {railOpen ? (
          <>
            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', padding: '10px 12px', borderBottom: `1px solid ${T.border}` }}>
              <span style={{ fontFamily: T.mono, fontSize: 11, color: T.faint }}>{project} · {pages.length} page{pages.length !== 1 ? 's' : ''}</span>
              <button onClick={newPage} title="new page" style={{ ...btn, padding: '2px 8px' }}>+</button>
            </div>
            <button onClick={openContents}
              style={{ display: 'block', width: '100%', textAlign: 'left', padding: '8px 12px', background: !pageId && !editMode ? T.greenSoft : 'transparent', border: 0, borderLeft: `2px solid ${!pageId && !editMode ? T.green : 'transparent'}`, borderBottom: `1px solid ${T.border}`, color: !pageId && !editMode ? T.textHi : T.dim, cursor: 'pointer', fontFamily: T.mono, fontSize: 12 }}>
              ▤ Contents
            </button>
            {groups.map(([type, ps]) => (
              <div key={type}>
                <div style={{ padding: '8px 12px 3px', fontFamily: T.mono, fontSize: 9, color: T.faint, letterSpacing: 1, textTransform: 'uppercase' }}>{type}</div>
                {ps.map(railItem)}
              </div>
            ))}
            {pages.length === 0 && <div style={{ padding: 12, color: T.faint, fontSize: 12 }}>No pages yet.</div>}
          </>
        ) : (
          <div style={{ flex: 1, display: 'flex', flexDirection: 'column', alignItems: 'center', paddingTop: 12, color: T.faint }} title="contents (hover to open)">
            <span style={{ fontSize: 14 }}>▤</span>
          </div>
        )}
      </div>
      {railOpen && railHandle}

      {/* Main pane */}
      <div style={{ flex: 1, overflow: 'auto' }}>
        {err && <div style={{ color: T.red, background: T.redSoft, padding: '6px 10px', margin: '12px 20px 0', borderRadius: 4, fontSize: 13 }}>{err}</div>}

        {editMode ? (
          <div style={{ padding: '18px 24px', maxWidth: 900 }}>
            <div style={{ fontFamily: T.mono, fontSize: 12, color: T.dim, marginBottom: 12 }}>{creating ? 'new page' : `editing ${draft.id}`}</div>
            <div style={{ display: 'grid', gridTemplateColumns: 'repeat(2, 1fr)', gap: 8, marginBottom: 8 }}>
              <label style={{ fontSize: 12, color: T.dim }}>id
                <input value={draft.id} disabled={!creating} onChange={e => set('id', e.target.value)} placeholder="contract-api" style={{ ...box, width: '100%', marginTop: 2, opacity: creating ? 1 : 0.6 }} /></label>
              <label style={{ fontSize: 12, color: T.dim }}>title
                <input value={draft.title} onChange={e => set('title', e.target.value)} style={{ ...box, width: '100%', marginTop: 2 }} /></label>
              <label style={{ fontSize: 12, color: T.dim }}>type
                <select value={draft.type} onChange={e => set('type', e.target.value as WikiPageType)} style={{ ...box, width: '100%', marginTop: 2 }}>{TYPES.map(t => <option key={t} value={t}>{t}</option>)}</select></label>
              <label style={{ fontSize: 12, color: T.dim }}>stack
                <select value={draft.stack} onChange={e => set('stack', e.target.value)} style={{ ...box, width: '100%', marginTop: 2 }}>{STACKS.map(s => <option key={s} value={s}>{s}</option>)}</select></label>
              <label style={{ fontSize: 12, color: T.dim }}>format
                <select value={draft.format} onChange={e => set('format', e.target.value)} style={{ ...box, width: '100%', marginTop: 2 }}>{FORMATS.map(f => <option key={f} value={f}>{f}</option>)}</select></label>
              <label style={{ fontSize: 12, color: T.dim }}>status
                <input value={draft.status} onChange={e => set('status', e.target.value)} style={{ ...box, width: '100%', marginTop: 2 }} /></label>
            </div>
            <textarea value={draft.content} onChange={e => set('content', e.target.value)} spellCheck={false}
              placeholder={draft.format === 'md' ? '# Markdown…' : 'source…'}
              style={{ ...box, width: '100%', minHeight: 360, fontFamily: T.mono, fontSize: 12.5, resize: 'vertical' }} />
            <div style={{ display: 'flex', gap: 8, marginTop: 8 }}>
              <button onClick={save} disabled={busy} style={{ ...btn, background: T.greenSoft, color: T.green, borderColor: T.green }}>{busy ? 'saving…' : 'save'}</button>
              <button onClick={cancelEdit} disabled={busy} style={btn}>cancel</button>
            </div>
          </div>
        ) : pageId ? (
          <div style={{ padding: '18px 24px', maxWidth: 900 }}>
            <div style={{ display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between', gap: 12, marginBottom: 16 }}>
              <div>
                <div style={{ fontSize: 20, fontWeight: 700, color: T.textHi }}>{draft.title}</div>
                <div style={{ fontSize: 11, color: T.faint, fontFamily: T.mono, marginTop: 3 }}>
                  {draft.type}{draft.stack ? ` · ${draft.stack}` : ''} · {draft.format}{selected ? ` · v${selected.version}` : ''}
                </div>
              </div>
              <div style={{ display: 'flex', gap: 8, flexShrink: 0 }}>
                <button onClick={startEdit} style={btn}>edit</button>
                <button onClick={del} disabled={busy} style={{ ...btn, background: T.redSoft, color: T.red, borderColor: T.red }}>delete</button>
              </div>
            </div>
            {draft.format === 'md' ? (
              <Markdown source={draft.content} />
            ) : draft.format === 'html' ? (
              // Archify diagram: a self-contained HTML doc. Render it isolated in a
              // sandboxed iframe (scripts allowed, but no same-origin — no access to
              // the portal's cookies/DOM) so an untrusted diagram can't reach out.
              <iframe title={draft.title} srcDoc={draft.content} sandbox="allow-scripts"
                style={{ width: '100%', height: '72vh', border: `1px solid ${T.border}`, borderRadius: 4, background: '#fff' }} />
            ) : (
              <pre style={{ background: T.bgAlt, border: `1px solid ${T.border}`, borderRadius: 4, padding: '12px 14px', overflow: 'auto', fontFamily: T.mono, fontSize: 12, lineHeight: 1.55, color: T.text, whiteSpace: 'pre-wrap' }}>{draft.content}</pre>
            )}
          </div>
        ) : (
          // Contents root
          <div style={{ padding: '18px 24px', maxWidth: 900 }}>
            <h1 style={{ fontSize: 18, color: T.textHi, marginBottom: 4 }}>Contents</h1>
            <p style={{ color: T.dim, fontSize: 13, marginBottom: 20 }}>A project's source of truth — architecture, API contracts, data models, decisions. Git-backed; every save is a commit.</p>
            {pages.length === 0 ? (
              <div style={{ color: T.faint, fontSize: 13 }}>No pages yet — use <b style={{ color: T.dim }}>+</b> in the rail to create one.</div>
            ) : groups.map(([type, ps]) => (
              <div key={type} style={{ marginBottom: 22 }}>
                <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, textTransform: 'uppercase', marginBottom: 8 }}>{type}</div>
                <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(220px, 1fr))', gap: 10 }}>
                  {ps.map(p => (
                    <button key={p.id} onClick={() => setPageId(p.id)}
                      style={{ textAlign: 'left', background: T.card, border: `1px solid ${T.border}`, borderRadius: 6, padding: '11px 13px', cursor: 'pointer', color: T.text }}
                      onMouseEnter={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.green; }}
                      onMouseLeave={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.border; }}>
                      <div style={{ fontSize: 13.5, fontWeight: 600, color: T.textHi, marginBottom: 3, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{p.title}</div>
                      <div style={{ fontSize: 10.5, color: T.faint, fontFamily: T.mono }}>{p.stack ? `${p.stack} · ` : ''}{p.format} · v{p.version}</div>
                    </button>
                  ))}
                </div>
              </div>
            ))}
          </div>
        )}
      </div>
    </div>
  );
}

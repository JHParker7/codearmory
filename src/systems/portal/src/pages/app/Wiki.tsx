/**
 * Wiki page: browse and edit a project's source-of-truth wiki. The wiki is
 * project-scoped (the sidebar switcher picks the project); a dropdown picks the
 * page. A selected page RENDERS — markdown pages through the shared Markdown
 * component, other formats (openapi/sql/ts/yaml) as a code block — with an edit
 * mode for the raw source + metadata. Content lives in git behind the wiki
 * service (every save is a bot commit); the per-page git history is intentionally
 * not surfaced here. All calls go through the BFF to /api/wiki/*.
 */
import { useState, useEffect, useCallback } from 'react';
import { useUrlParam } from '../../hooks/useUrlState';
import { T } from '../../theme';
import { useConfirm } from '../../components/ConfirmDialog';
import { useAppSelector } from '../../store/hooks';
import { Markdown } from './Repos';
import { listWikiPages, getWikiPage, putWikiPage, deleteWikiPage } from '../../api/bff';
import type { WikiPageMeta, WikiPageType, WikiPagePayload } from '../../api/bff';

const TYPES: WikiPageType[] = ['overview', 'architecture', 'contract', 'model', 'service', 'component', 'decision', 'ticket'];
const STACKS = ['shared', 'frontend', 'backend', 'infra'];
const FORMATS = ['md', 'openapi', 'sql', 'ts', 'yaml'];

const box: React.CSSProperties = { background: 'transparent', border: `1px solid ${T.border}`, color: T.text, padding: '6px 8px', borderRadius: 4, fontSize: 13, fontFamily: 'inherit' };
const btn: React.CSSProperties = { background: T.cardHi, border: `1px solid ${T.border}`, color: T.textHi, padding: '6px 12px', borderRadius: 4, cursor: 'pointer', fontSize: 13 };

interface Draft { id: string; type: WikiPageType; stack: string; format: string; title: string; status: string; content: string; }
const emptyDraft: Draft = { id: '', type: 'overview', stack: 'shared', format: 'md', title: '', status: 'draft', content: '' };

export function Wiki() {
  const token = useAppSelector(s => s.auth.token)!;
  const [confirm, confirmEl] = useConfirm();
  // The project is the globally-selected one from the sidebar switcher.
  const project = useAppSelector(s => s.project.current);
  const [pageId, setPageId] = useUrlParam('page');
  const [pages, setPages] = useState<WikiPageMeta[]>([]);
  const [draft, setDraft] = useState<Draft>(emptyDraft);
  const [creating, setCreating] = useState(false);
  const [editing, setEditing] = useState(false);
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState(false);

  const loadManifest = useCallback(async (proj: string) => {
    setErr('');
    try {
      const m = await listWikiPages(token, proj);
      setPages(m.pages ?? []);
    } catch (e: unknown) { setErr(e instanceof Error ? e.message : 'failed to load'); setPages([]); }
  }, [token]);

  useEffect(() => { if (project) loadManifest(project); }, [project, loadManifest]);

  // Load the selected page's content. Land in view (rendered) mode, not edit.
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

  const newPage = () => { setCreating(true); setEditing(true); setPageId(null); setDraft(emptyDraft); setErr(''); };
  const startEdit = () => { setEditing(true); setErr(''); };
  const cancelEdit = () => { setEditing(false); setCreating(false); setErr(''); };

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
    try { await deleteWikiPage(token, project, draft.id); await loadManifest(project); setPageId(null); setDraft(emptyDraft); }
    catch (e: unknown) { setErr(e instanceof Error ? e.message : 'delete failed'); }
    setBusy(false);
  };

  const set = <K extends keyof Draft>(k: K, v: Draft[K]) => setDraft(d => ({ ...d, [k]: v }));

  const editMode = creating || editing;
  const selected = pages.find(p => p.id === pageId);

  return (
    <div style={{ padding: 20, color: T.text, maxWidth: 1000 }}>
      {confirmEl}
      <h1 style={{ fontSize: 18, color: T.textHi, marginBottom: 4 }}>wiki/</h1>
      <p style={{ color: T.dim, fontSize: 13, marginBottom: 16 }}>A project's source of truth — architecture, API contracts, data models, decisions. Git-backed; every save is a commit.</p>

      {!project && (
        <div style={{ color: T.dim, fontSize: 13, padding: '10px 0 4px' }}>
          Select a project from the sidebar to view its wiki.
        </div>
      )}

      {err && <div style={{ color: T.red, background: T.redSoft, padding: '6px 10px', borderRadius: 4, marginBottom: 12, fontSize: 13 }}>{err}</div>}

      {project && (
        <>
          {/* Toolbar: page dropdown + actions */}
          <div style={{ display: 'flex', gap: 8, alignItems: 'center', marginBottom: 16, flexWrap: 'wrap' }}>
            <select
              value={editMode && creating ? '' : (pageId ?? '')}
              disabled={editMode}
              onChange={e => { const v = e.target.value; setPageId(v || null); }}
              style={{ ...box, minWidth: 260, cursor: editMode ? 'default' : 'pointer', opacity: editMode ? 0.6 : 1 }}>
              <option value="">{pages.length ? '— select a page —' : 'No pages yet'}</option>
              {pages.map(p => (
                <option key={p.id} value={p.id}>{p.title} · {p.type}{p.stack ? ` · ${p.stack}` : ''}</option>
              ))}
            </select>

            {!editMode && <button onClick={newPage} style={btn}>+ new page</button>}
            {!editMode && pageId && <button onClick={startEdit} style={btn}>edit</button>}
            {!editMode && pageId && <button onClick={del} disabled={busy} style={{ ...btn, background: T.redSoft, color: T.red, borderColor: T.red }}>delete</button>}
          </div>

          {/* Edit / create form */}
          {editMode && (
            <div>
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
          )}

          {/* View (rendered) mode */}
          {!editMode && pageId && (
            <div>
              <div style={{ display: 'flex', alignItems: 'baseline', gap: 10, marginBottom: 12, flexWrap: 'wrap' }}>
                <span style={{ fontSize: 20, fontWeight: 700, color: T.textHi }}>{draft.title}</span>
                <span style={{ fontSize: 11, color: T.faint, fontFamily: T.mono }}>
                  {draft.type}{draft.stack ? ` · ${draft.stack}` : ''} · {draft.format}{selected ? ` · v${selected.version}` : ''}
                </span>
              </div>
              {draft.format === 'md'
                ? <Markdown source={draft.content} />
                : <pre style={{ background: T.bgAlt, border: `1px solid ${T.border}`, borderRadius: 4, padding: '12px 14px', overflow: 'auto', fontFamily: T.mono, fontSize: 12, lineHeight: 1.55, color: T.text, whiteSpace: 'pre-wrap' }}>{draft.content}</pre>}
            </div>
          )}

          {!editMode && !pageId && pages.length > 0 && (
            <div style={{ color: T.faint, fontSize: 13 }}>Select a page above to read it.</div>
          )}
        </>
      )}
    </div>
  );
}

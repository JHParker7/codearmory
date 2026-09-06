/**
 * Wiki page: browse and edit a project's source-of-truth wiki. The wiki is
 * project-scoped, so you pick a project, then its manifest (the page index) shows
 * on the left and the selected page's editor on the right. Content lives in git
 * behind the wiki service (every save is a commit by the wiki bot), so there is a
 * per-page History. All calls go through the BFF to /api/wiki/*.
 */
import { useState, useEffect, useCallback } from 'react';
import { useUrlParam } from '../../hooks/useUrlState';
import { T } from '../../theme';
import { useConfirm } from '../../components/ConfirmDialog';
import { useAppSelector } from '../../store/hooks';
import {
  listWikiPages, getWikiPage, putWikiPage, deleteWikiPage, getWikiHistory,
} from '../../api/bff';
import type { WikiPageMeta, WikiCommit, WikiPageType, WikiPagePayload } from '../../api/bff';

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
  const [project, setProject] = useUrlParam('project');
  const [pageId, setPageId] = useUrlParam('page');
  const [projInput, setProjInput] = useState(project ?? '');
  const [pages, setPages] = useState<WikiPageMeta[]>([]);
  const [draft, setDraft] = useState<Draft>(emptyDraft);
  const [history, setHistory] = useState<WikiCommit[]>([]);
  const [creating, setCreating] = useState(false);
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

  // Load the selected page's content + history.
  useEffect(() => {
    if (!project || !pageId) return;
    setCreating(false);
    (async () => {
      try {
        const p = await getWikiPage(token, project, pageId);
        setDraft({ id: p.id, type: p.type, stack: p.stack ?? 'shared', format: p.format, title: p.title, status: p.status, content: p.content });
        setHistory(await getWikiHistory(token, project, pageId));
      } catch (e: unknown) { setErr(e instanceof Error ? e.message : 'failed to load page'); }
    })();
  }, [token, project, pageId]);

  const openProject = () => { const p = projInput.trim(); if (p) { setProject(p); setPageId(null); setDraft(emptyDraft); setHistory([]); } };
  const newPage = () => { setCreating(true); setPageId(null); setDraft(emptyDraft); setHistory([]); };

  const save = async () => {
    if (!project || !draft.id.trim() || !draft.title.trim()) { setErr('id and title are required'); return; }
    setBusy(true); setErr('');
    try {
      const payload: WikiPagePayload = { type: draft.type, stack: draft.stack, format: draft.format, title: draft.title, status: draft.status, content: draft.content };
      await putWikiPage(token, project, draft.id.trim(), payload);
      setCreating(false);
      await loadManifest(project);
      setPageId(draft.id.trim());
    } catch (e: unknown) { setErr(e instanceof Error ? e.message : 'save failed'); }
    setBusy(false);
  };

  const del = async () => {
    if (!project || !draft.id) return;
    if (!(await confirm({ message: `Delete wiki page "${draft.id}"?`, confirmLabel: 'delete' }))) return;
    setBusy(true);
    try { await deleteWikiPage(token, project, draft.id); await loadManifest(project); setPageId(null); setDraft(emptyDraft); setHistory([]); }
    catch (e: unknown) { setErr(e instanceof Error ? e.message : 'delete failed'); }
    setBusy(false);
  };

  const set = <K extends keyof Draft>(k: K, v: Draft[K]) => setDraft(d => ({ ...d, [k]: v }));

  return (
    <div style={{ padding: 20, color: T.text }}>
      {confirmEl}
      <h1 style={{ fontSize: 18, color: T.textHi, marginBottom: 4 }}>wiki/</h1>
      <p style={{ color: T.dim, fontSize: 13, marginBottom: 16 }}>A project's source of truth — architecture, API contracts, data models, decisions. Git-backed; every save is a commit.</p>

      <div style={{ display: 'flex', gap: 8, marginBottom: 16 }}>
        <input value={projInput} onChange={e => setProjInput(e.target.value)} onKeyDown={e => e.key === 'Enter' && openProject()}
          placeholder="project (namespace)" style={{ ...box, minWidth: 220 }} />
        <button onClick={openProject} style={btn}>open</button>
      </div>

      {err && <div style={{ color: T.red, background: T.redSoft, padding: '6px 10px', borderRadius: 4, marginBottom: 12, fontSize: 13 }}>{err}</div>}

      {project && (
        <div style={{ display: 'flex', gap: 16, alignItems: 'flex-start' }}>
          {/* Left: manifest */}
          <div style={{ width: 260, flexShrink: 0, border: `1px solid ${T.border}`, borderRadius: 6 }}>
            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', padding: '8px 10px', borderBottom: `1px solid ${T.border}` }}>
              <span style={{ color: T.dim, fontSize: 12 }}>{project} · {pages.length} pages</span>
              <button onClick={newPage} style={{ ...btn, padding: '2px 8px', fontSize: 12 }}>+ new</button>
            </div>
            {pages.map(p => (
              <button key={p.id} onClick={() => setPageId(p.id)}
                style={{ display: 'block', width: '100%', textAlign: 'left', padding: '7px 10px', background: pageId === p.id ? T.cardHi : 'transparent', border: 'none', borderBottom: `1px solid ${T.border}`, color: T.text, cursor: 'pointer' }}>
                <div style={{ fontSize: 13, color: T.textHi }}>{p.title}</div>
                <div style={{ fontSize: 11, color: T.faint, fontFamily: T.mono }}>{p.type}{p.stack ? ` · ${p.stack}` : ''} · v{p.version}</div>
              </button>
            ))}
            {pages.length === 0 && <div style={{ padding: 10, color: T.faint, fontSize: 12 }}>No pages yet.</div>}
          </div>

          {/* Right: editor */}
          {(pageId || creating) && (
            <div style={{ flex: 1, minWidth: 0 }}>
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
                style={{ ...box, width: '100%', minHeight: 320, fontFamily: T.mono, fontSize: 12.5, resize: 'vertical' }} />
              <div style={{ display: 'flex', gap: 8, marginTop: 8 }}>
                <button onClick={save} disabled={busy} style={{ ...btn, background: T.greenSoft, color: T.green, borderColor: T.green }}>{busy ? 'saving…' : 'save'}</button>
                {!creating && <button onClick={del} disabled={busy} style={{ ...btn, background: T.redSoft, color: T.red, borderColor: T.red }}>delete</button>}
              </div>

              {history.length > 0 && (
                <div style={{ marginTop: 16 }}>
                  <div style={{ color: T.dim, fontSize: 12, marginBottom: 6 }}>History</div>
                  {history.map(c => (
                    <div key={c.sha} style={{ fontSize: 12, padding: '3px 0', borderBottom: `1px solid ${T.border}` }}>
                      <span style={{ color: T.green, fontFamily: T.mono }}>{c.sha.slice(0, 7)}</span>{' '}
                      <span style={{ color: T.text }}>{c.subject}</span>{' '}
                      <span style={{ color: T.faint }}>· {c.author}</span>
                    </div>
                  ))}
                </div>
              )}
            </div>
          )}
        </div>
      )}
    </div>
  );
}

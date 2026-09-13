/**
 * Repos page — `codearmory_git_factory`, the platform's own git host. Left is the
 * repository list (with a create form); right is the selected repo as a real git
 * host page: a code tab (branch switcher, file tree, file reader/editor, README),
 * a commits tab (filterable, paged history and per-commit diffs), a pulls tab
 * (open/review/merge/close, with a compare preview before the PR exists) and a
 * settings tab (default branch, tags, branch protections, collaborators).
 *
 * This replaces the sandboxed iframe the service used to be surfaced through: the
 * frame lost `allow-same-origin`, so a framed mini-portal can no longer read the
 * session token, and a bundled page is the honest answer for a core service. All
 * calls go through the BFF (`/api/codearmory_git_factory/repos/...`) via the typed
 * `…GitFactory…` helpers in api/bff.
 *
 * Everything the page renders from repository contents (README, files, diffs) is
 * untrusted — whoever can push writes it — so it is parsed into token trees by
 * repoContent.ts and rendered as React text nodes; no markup is ever injected.
 */
import { useState, useEffect, useCallback, useMemo, useRef, Fragment } from 'react';
import type { CSSProperties, ReactNode } from 'react';
import { useUrlParam, useUrlState } from '../../hooks/useUrlState';
import { T } from '../../theme';
import { useResizableWidth } from '../../components/ResizeHandle';
import { Pill } from '../../components/Pill';
import { useConfirm } from '../../components/ConfirmDialog';
import { useAppSelector } from '../../store/hooks';
import {
  listGitFactoryRepos, createGitFactoryRepo, deleteGitFactoryRepo,
  listGitFactoryBranches, listGitFactoryTags, setGitFactoryDefaultBranch,
  listGitFactoryCommits, getGitFactoryCommit, compareGitFactoryRefs,
  getGitFactoryReadme, getGitFactoryTree, getGitFactoryBlob, writeGitFactoryBlob,
  listGitFactoryPulls, createGitFactoryPull, getGitFactoryPull, mergeGitFactoryPull, closeGitFactoryPull,
  listGitFactoryPRComments, createGitFactoryPRComment, submitGitFactoryPRReview,
  listGitFactoryCollaborators, addGitFactoryCollaborator, removeGitFactoryCollaborator,
  listGitFactoryProtections, setGitFactoryProtection, deleteGitFactoryProtection,
  listRuns, listWorkflows, listUsers, getRun,
} from '../../api/bff';
import type {
  GitFactoryRepo, GitFactoryRef, GitFactoryCommit, GitFactoryCommitDetail,
  GitFactoryFileChange, GitFactoryTreeEntry, GitFactoryBlob, GitFactoryPull,
  GitFactoryPullDetail, GitFactoryProtection,
  GitFactoryPRComment, WorkflowRun,
} from '../../api/bff';
import { timeAgo, shortId } from '../../utils';
import {
  parseMarkdown, highlight, extOf, splitDiffByFile, diffLineKind, formatBytes,
  type MdBlock, type MdInline, type TokenClass,
} from './repoContent';

type RepoTab = 'code' | 'commits' | 'pulls' | 'settings';

/** Commits per page — the history pager's stride. */
const PAGE = 50;
/** Diff lines rendered per file, so one enormous commit cannot lock up the DOM. */
const MAX_DIFF_LINES = 4000;

// ── shared styling ───────────────────────────────────────────────────────────

const input: CSSProperties = {
  width: '100%', background: T.cardHi, border: `1px solid ${T.border}`, color: T.text,
  fontFamily: T.mono, fontSize: 12, padding: '6px 8px', outline: 'none', boxSizing: 'border-box',
};
const primaryBtn: CSSProperties = {
  background: T.greenSoft, border: `1px solid ${T.green}`, color: T.green,
  fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer',
};
const ghostBtn: CSSProperties = {
  background: 'transparent', border: `1px solid ${T.border}`, color: T.dim,
  fontFamily: T.mono, fontSize: 10, padding: '3px 8px', cursor: 'pointer',
};
const linkBtn: CSSProperties = {
  background: 'transparent', border: 'none', color: T.green,
  fontFamily: T.mono, fontSize: 11, padding: 0, cursor: 'pointer',
};
const sectionLabel: CSSProperties = {
  fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 0.6,
  textTransform: 'uppercase', marginBottom: 6,
};

/** Loading/empty line, in the app's terminal idiom. */
function Hint({ children, busy }: { children: ReactNode; busy?: boolean }) {
  return (
    <div style={{ fontFamily: T.mono, fontSize: 11, color: T.faint, padding: '10px 0', animation: busy ? 'pulse 1s ease-in-out infinite' : undefined }}>
      {children}
    </div>
  );
}

/** Inline error banner. */
function ErrorBox({ children }: { children: ReactNode }) {
  return (
    <div style={{ background: T.redSoft, border: `1px solid ${T.red}`, padding: '8px 12px', fontFamily: T.mono, fontSize: 11, color: T.red, marginBottom: 12 }}>
      {children}
    </div>
  );
}

/**
 * Turn a BFF error into something worth reading. A 403 is an ordinary outcome
 * here — repository access is scoped per repo, so a user can legitimately see a
 * repo in one list and be refused a tab of it — and reads as a permission
 * statement rather than a failure.
 */
function errorMessage(e: unknown): string {
  const err = e as (Error & { status?: number }) | undefined;
  if (err?.status === 403) return 'not permitted — ask an owner to share this repository with you';
  if (err?.status === 401) return 'session expired — sign in again';
  if (err?.status === 404) return 'not found — it may have been deleted';
  return err?.message?.trim() || 'request failed';
}

/** Trim a string to n chars with an ellipsis, flattening whitespace for one-line labels. */
function truncateText(s: string, n: number): string {
  const flat = s.replace(/\s+/g, ' ').trim();
  return flat.length > n ? flat.slice(0, n - 1) + '…' : flat;
}

/** The automated author behind a commit, from the author email's domain suffix:
 *  <role>@blacksmith.agent → a coding agent; <step>@forge.cicd → a CI/CD step. The
 *  local part is the specific identity (the role or the step). Returns null for a
 *  human commit (any other domain). Identity-based, not a parsed display name. */
function commitAgent(email?: string): { local: string; kind: 'agent' | 'cicd' } | null {
  const e = (email || '').trim().toLowerCase();
  const m = /^([^@]+)@[a-z0-9._-]*\.(agent|cicd)$/.exec(e);
  if (!m) return null;
  return { local: m[1], kind: m[2] as 'agent' | 'cicd' };
}

/** Relative time that tolerates a missing/unparsable timestamp. */
function ago(iso?: string | null): string {
  if (!iso) return 'never';
  const t = new Date(iso).getTime();
  if (Number.isNaN(t)) return 'never';
  return `${timeAgo(iso)} ago`;
}

/** Tone for a pull request state (open=green, merged=blue, closed=red). */
function pullTone(state: string): 'green' | 'blue' | 'red' | 'dim' {
  if (state === 'open') return 'green';
  if (state === 'merged') return 'blue';
  if (state === 'closed') return 'red';
  return 'dim';
}

// ── content rendering ────────────────────────────────────────────────────────

const TOKEN_COLOR: Record<TokenClass, string | undefined> = {
  '': undefined,
  comment: T.faint,
  string: T.green,
  number: T.amber,
  keyword: T.blue,
  literal: T.amber,
};

/** Syntax-coloured spans for one chunk of source. */
function Tokens({ text, ext }: { text: string; ext: string }) {
  const tokens = useMemo(() => highlight(text, ext), [text, ext]);
  return (
    <>
      {tokens.map((t, i) => (
        t.cls
          ? <span key={i} style={{ color: TOKEN_COLOR[t.cls], fontStyle: t.cls === 'comment' ? 'italic' : undefined }}>{t.text}</span>
          : <Fragment key={i}>{t.text}</Fragment>
      ))}
    </>
  );
}

/** Inline markdown nodes → React. Links are already restricted to http(s) by the parser. */
function Inline({ nodes }: { nodes: MdInline[] }) {
  return (
    <>
      {nodes.map((n, i) => {
        switch (n.kind) {
          case 'text': return <Fragment key={i}>{n.text}</Fragment>;
          case 'code': return <code key={i} style={{ background: T.bgAlt, border: `1px solid ${T.border}`, padding: '1px 4px', fontSize: 11.5, color: T.dim }}>{n.text}</code>;
          case 'strong': return <strong key={i} style={{ color: T.textHi }}><Inline nodes={n.children} /></strong>;
          case 'em': return <em key={i}><Inline nodes={n.children} /></em>;
          case 'del': return <del key={i} style={{ color: T.faint }}><Inline nodes={n.children} /></del>;
          case 'link': return <a key={i} href={n.href} target="_blank" rel="noopener noreferrer" style={{ color: T.green }}><Inline nodes={n.children} /></a>;
        }
      })}
    </>
  );
}

/** Rendered markdown — the README, any .md file opened in the reader, and the wiki. */
export function Markdown({ source }: { source: string }) {
  const blocks = useMemo(() => parseMarkdown(source), [source]);
  const heading = (level: number, children: ReactNode, key: number) => {
    const size = [19, 16, 14, 13][level - 1] ?? 13;
    const style: CSSProperties = {
      color: T.textHi, fontWeight: 600, fontSize: size, lineHeight: 1.3, margin: '18px 0 8px',
      borderBottom: level <= 2 ? `1px solid ${T.border}` : undefined, paddingBottom: level <= 2 ? 6 : undefined,
    };
    return <div key={key} style={style}>{children}</div>;
  };
  return (
    <div style={{ fontFamily: T.mono, fontSize: 12.5, lineHeight: 1.62, color: T.text }}>
      {blocks.map((b: MdBlock, i) => {
        switch (b.kind) {
          case 'heading': return heading(b.level, <Inline nodes={b.children} />, i);
          case 'paragraph': return <p key={i} style={{ margin: '0 0 10px' }}><Inline nodes={b.children} /></p>;
          case 'rule': return <hr key={i} style={{ border: 0, borderTop: `1px solid ${T.border}`, margin: '16px 0' }} />;
          case 'quote': return (
            <blockquote key={i} style={{ margin: '0 0 10px', padding: '2px 0 2px 12px', borderLeft: `2px solid ${T.border}`, color: T.dim }}>
              <Inline nodes={b.children} />
            </blockquote>
          );
          case 'code': return (
            <pre key={i} style={{ background: T.bgAlt, border: `1px solid ${T.border}`, padding: '10px 12px', overflow: 'auto', margin: '0 0 12px', fontSize: 11.5, lineHeight: 1.5 }}>
              <code><Tokens text={b.text} ext={b.lang} /></code>
            </pre>
          );
          case 'list': {
            const items = b.items.map((it, j) => <li key={j} style={{ margin: '2px 0' }}><Inline nodes={it} /></li>);
            return b.ordered
              ? <ol key={i} style={{ margin: '0 0 10px', paddingLeft: 22 }}>{items}</ol>
              : <ul key={i} style={{ margin: '0 0 10px', paddingLeft: 22 }}>{items}</ul>;
          }
          case 'table': {
            const cell: CSSProperties = { border: `1px solid ${T.border}`, padding: '4px 9px', textAlign: 'left' };
            return (
              <div key={i} style={{ overflowX: 'auto', marginBottom: 12 }}>
                <table style={{ borderCollapse: 'collapse', fontSize: 11.5 }}>
                  <thead>
                    <tr>{b.head.map((h, j) => <th key={j} style={{ ...cell, color: T.textHi, background: T.bgAlt }}><Inline nodes={h} /></th>)}</tr>
                  </thead>
                  <tbody>
                    {b.rows.map((r, j) => <tr key={j}>{r.map((c, k) => <td key={k} style={cell}><Inline nodes={c} /></td>)}</tr>)}
                  </tbody>
                </table>
              </div>
            );
          }
        }
      })}
    </div>
  );
}

/** A file's contents with a line-number gutter, like a real git host. */
function CodeView({ text, path }: { text: string; path: string }) {
  // Drop a single trailing newline so the gutter has no phantom final line.
  const body = text.endsWith('\n') ? text.slice(0, -1) : text;
  const count = body === '' ? 1 : body.split('\n').length;
  const gutter = Array.from({ length: count }, (_, i) => i + 1).join('\n');
  return (
    <div style={{ display: 'flex', border: `1px solid ${T.border}`, overflow: 'auto', maxHeight: 'calc(100vh - 250px)', background: T.bgAlt }}>
      <pre style={{ margin: 0, padding: '10px 8px 10px 12px', color: T.faint, textAlign: 'right', fontSize: 12, lineHeight: 1.55, whiteSpace: 'pre', userSelect: 'none', borderRight: `1px solid ${T.border}`, flex: 'none', fontFamily: T.mono }}>
        {gutter}
      </pre>
      <pre style={{ margin: 0, padding: '10px 14px', fontSize: 12, lineHeight: 1.55, flex: 1, fontFamily: T.mono, color: T.text }}>
        <code><Tokens text={body} ext={extOf(path)} /></code>
      </pre>
    </div>
  );
}

/** One file's unified diff, tinted per line and syntax-coloured after the marker. */
function DiffLines({ patch, path }: { patch: string; path: string }) {
  const ext = extOf(path);
  const lines = useMemo(() => patch.split('\n'), [patch]);
  if (!patch.trim()) return <Hint>no textual changes</Hint>;
  const shown = lines.slice(0, MAX_DIFF_LINES);
  return (
    <div style={{ border: `1px solid ${T.border}`, overflow: 'auto', maxHeight: 'calc(100vh - 320px)', background: T.bgAlt, fontFamily: T.mono, fontSize: 12, lineHeight: 1.55 }}>
      {shown.map((l, i) => {
        const kind = diffLineKind(l);
        const style: CSSProperties = {
          whiteSpace: 'pre', padding: '0 12px',
          color: kind === 'hunk' ? T.green : kind === 'head' ? T.dim : T.text,
          fontWeight: kind === 'head' ? 600 : undefined,
          background: kind === 'add' ? 'rgba(46,160,67,.16)'
            : kind === 'del' ? 'rgba(248,81,73,.16)'
              : kind === 'hunk' || kind === 'head' ? T.cardHi : undefined,
        };
        // Keep the +/-/space marker literal, syntax-colour the code after it.
        return (
          <div key={i} style={style}>
            {kind === 'add' || kind === 'del' || kind === 'context'
              ? <>{l.slice(0, 1)}<Tokens text={l.slice(1)} ext={ext} /></>
              : (l || ' ')}
          </div>
        );
      })}
      {lines.length > MAX_DIFF_LINES && <Hint>diff truncated — {lines.length - MAX_DIFF_LINES} more lines</Hint>}
    </div>
  );
}

/**
 * The changed-file list of a commit/PR plus ONE file's diff at a time — a whole
 * commit's patch at once is unreadable and, on a big merge, unrenderable.
 */
function DiffPanel({ files, diff }: { files?: GitFactoryFileChange[] | null; diff?: string }) {
  const sections = useMemo(() => splitDiffByFile(diff ?? ''), [diff]);
  const [active, setActive] = useState(0);
  useEffect(() => { setActive(0); }, [diff]);

  const stat = (path: string) => {
    const f = (files ?? []).find((x) => x.path === path);
    if (!f) return null;
    return f.binary
      ? <span style={{ color: T.faint, fontSize: 10.5 }}>binary</span>
      : <span style={{ fontSize: 10.5 }}><span style={{ color: T.green }}>+{f.additions}</span> <span style={{ color: T.red }}>−{f.deletions}</span></span>;
  };

  if (!sections.length) {
    const rows = files ?? [];
    if (!rows.length) return <Hint>no changes</Hint>;
    return (
      <>
        <div style={{ border: `1px solid ${T.border}`, marginBottom: 14 }}>
          {rows.map((f) => (
            <div key={f.path} style={{ display: 'flex', gap: 12, alignItems: 'center', padding: '6px 11px', borderBottom: `1px solid ${T.border}`, fontSize: 11.5 }}>
              <span style={{ flex: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', color: T.text }}>{f.path}</span>
              {stat(f.path)}
            </div>
          ))}
        </div>
        <Hint>no textual diff to show</Hint>
      </>
    );
  }

  const current = sections[Math.min(active, sections.length - 1)];
  return (
    <>
      <div style={{ border: `1px solid ${T.border}`, marginBottom: 14 }}>
        {sections.map((s, i) => {
          const on = i === Math.min(active, sections.length - 1);
          return (
            <button key={s.path + i} onClick={() => setActive(i)}
              style={{ display: 'flex', width: '100%', textAlign: 'left', gap: 12, alignItems: 'center', padding: '6px 11px', borderBottom: `1px solid ${T.border}`, border: 0, background: on ? T.greenSoft : 'transparent', fontFamily: T.mono, fontSize: 11.5, cursor: 'pointer' }}>
              <span style={{ flex: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', color: on ? T.green : T.text }}>{s.path}</span>
              {stat(s.path)}
            </button>
          );
        })}
      </div>
      <DiffLines patch={current.patch} path={current.path} />
    </>
  );
}

// ── create repository ────────────────────────────────────────────────────────

/** Rail create form: name + description (+ visibility/org), filed into the active project. */
function CreateRepo({ onCreated, onCancel }: { onCreated: (r: GitFactoryRepo) => void; onCancel: () => void }) {
  const token = useAppSelector(s => s.auth.token)!;
  const project = useAppSelector(s => s.project.current);
  const [name, setName] = useState('');
  const [description, setDescription] = useState('');
  const [visibility, setVisibility] = useState('private');
  const [orgRepo, setOrgRepo] = useState(false);
  const [creating, setCreating] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const submit = async () => {
    if (!name.trim() || creating) return;
    setCreating(true);
    setError(null);
    try {
      onCreated(await createGitFactoryRepo(token, {
        name: name.trim(),
        description: description.trim(),
        visibility,
        org_repo: orgRepo,
        ...(project ? { project } : {}),
      }));
    } catch (e: unknown) {
      setError(errorMessage(e));
    } finally {
      setCreating(false);
    }
  };

  return (
    <div style={{ padding: '12px 14px', borderBottom: `1px solid ${T.border}`, background: T.card }}>
      {error && <div style={{ background: T.redSoft, border: `1px solid ${T.red}`, padding: '6px 8px', fontFamily: T.mono, fontSize: 10, color: T.red, marginBottom: 8 }}>{error}</div>}
      <input value={name} onChange={e => setName(e.target.value)} onKeyDown={e => { if (e.key === 'Enter') submit(); }}
        placeholder="repository name" autoFocus spellCheck={false} style={{ ...input, marginBottom: 8 }} />
      <input value={description} onChange={e => setDescription(e.target.value)} onKeyDown={e => { if (e.key === 'Enter') submit(); }}
        placeholder="what lives in here (optional)" style={{ ...input, marginBottom: 8 }} />
      <div style={sectionLabel}>visibility</div>
      <div style={{ display: 'flex', gap: 6, marginBottom: 8 }}>
        {['private', 'public'].map(v => (
          <button key={v} onClick={() => setVisibility(v)}
            style={{ background: visibility === v ? T.greenSoft : 'transparent', border: `1px solid ${visibility === v ? T.green : T.border}`, color: visibility === v ? T.green : T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 9px', cursor: 'pointer' }}>
            {v}
          </button>
        ))}
      </div>
      <label style={{ display: 'flex', alignItems: 'center', gap: 6, fontFamily: T.mono, fontSize: 10.5, color: T.dim, marginBottom: 8, cursor: 'pointer' }}>
        <input type="checkbox" checked={orgRepo} onChange={e => setOrgRepo(e.target.checked)} style={{ width: 'auto' }} />
        own it as the org (clone URL uses the org name)
      </label>
      {project && <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, marginBottom: 8 }}>filed into project {project}</div>}
      <div style={{ display: 'flex', gap: 6 }}>
        <button onClick={submit} disabled={!name.trim() || creating}
          style={{ flex: 1, background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 11, fontWeight: 600, padding: '6px 0', cursor: 'pointer', opacity: (!name.trim() || creating) ? 0.6 : 1 }}>
          {creating ? '[ · · · ]' : '[ create ]'}
        </button>
        <button onClick={onCancel} style={{ ...ghostBtn, padding: '6px 8px' }}>✕</button>
      </div>
    </div>
  );
}

// ── code tab ─────────────────────────────────────────────────────────────────

/** Nerd Font glyph for a tree entry — folder/link/submodule by type, else by file extension.
 *  Rendered inside a `.nf` span (see nerdfont.css); an unknown extension gets a generic file. */
const FILE_GLYPHS: Record<string, number> = {
  go: 0xe627, js: 0xe60c, jsx: 0xe60c, mjs: 0xe60c, cjs: 0xe60c,
  ts: 0xe628, tsx: 0xe628, md: 0xe609, markdown: 0xe609,
  json: 0xe60b, py: 0xe606, html: 0xe60e, htm: 0xe60e,
  css: 0xe614, sh: 0xe691, bash: 0xe691, zsh: 0xe691,
  yml: 0xe6a8, yaml: 0xe6a8, rs: 0xe7a8,
};
function entryIcon(type: string, name = ''): string {
  if (type === 'dir') return String.fromCodePoint(0xf07b);       // folder
  if (type === 'symlink') return String.fromCodePoint(0xf0c1);   // link
  if (type === 'submodule') return String.fromCodePoint(0xe702); // git
  const base = name.toLowerCase();
  if (base === 'dockerfile' || base.endsWith('.dockerfile')) return String.fromCodePoint(0xe7b0);
  if (base.endsWith('.lock') || base === 'go.sum') return String.fromCodePoint(0xf023);
  const ext = base.includes('.') ? base.slice(base.lastIndexOf('.') + 1) : '';
  return String.fromCodePoint(FILE_GLYPHS[ext] ?? 0xf15b);       // generic file
}

const parentPath = (p: string) => p.split('/').slice(0, -1).join('/');

/**
 * Code tab: the file tree at the current directory (or the open file's contents,
 * with an editor), plus the README below and a details sidebar beside it. Opening
 * a file hands the whole width to the reader, as a git host does.
 */
function CodeTab({ repo, refName }: { repo: GitFactoryRepo; refName: string }) {
  const token = useAppSelector(s => s.auth.token)!;
  const [path, setPath] = useUrlParam('path');
  const [file, setFile] = useUrlParam('file');

  const [entries, setEntries] = useState<GitFactoryTreeEntry[] | null>(null);
  const [treeError, setTreeError] = useState<string | null>(null);
  const [blob, setBlob] = useState<GitFactoryBlob | null>(null);
  const [blobError, setBlobError] = useState<string | null>(null);
  const [readme, setReadme] = useState<{ found: boolean; path: string; content: string } | null>(null);
  const [readmeError, setReadmeError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [copied, setCopied] = useState(false);
  // Bumped after a commit so the tree/blob/readme are refetched from the new tip.
  const [version, setVersion] = useState(0);

  // Editing state for the open file.
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState('');
  const [message, setMessage] = useState('');
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);

  const dir = path ?? '';

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setTreeError(null);
    setBlobError(null);
    if (file) {
      setEntries(null);
      getGitFactoryBlob(token, repo.id, file, refName)
        .then(b => { if (!cancelled) { setBlob(b); setDraft(b.content); } })
        .catch(e => { if (!cancelled) { setBlob(null); setBlobError(errorMessage(e)); } })
        .finally(() => { if (!cancelled) setLoading(false); });
    } else {
      setBlob(null);
      setEditing(false);
      getGitFactoryTree(token, repo.id, dir, refName)
        .then(t => { if (!cancelled) setEntries(t.entries ?? []); })
        .catch(e => { if (!cancelled) { setEntries(null); setTreeError(errorMessage(e)); } })
        .finally(() => { if (!cancelled) setLoading(false); });
    }
    return () => { cancelled = true; };
  }, [token, repo.id, refName, dir, file, version]);

  useEffect(() => {
    let cancelled = false;
    setReadmeError(null);
    getGitFactoryReadme(token, repo.id)
      .then(r => { if (!cancelled) setReadme(r); })
      .catch(e => { if (!cancelled) { setReadme(null); setReadmeError(errorMessage(e)); } });
    return () => { cancelled = true; };
  }, [token, repo.id, version]);

  const copyClone = async () => {
    try { await navigator.clipboard.writeText(repo.http_url); } catch { /* clipboard may be blocked */ }
    setCopied(true);
    setTimeout(() => setCopied(false), 1200);
  };

  const save = async () => {
    if (!blob || saving) return;
    setSaving(true);
    setSaveError(null);
    try {
      await writeGitFactoryBlob(token, repo.id, { ref: refName, path: blob.path, content: draft, message: message.trim() });
      setEditing(false);
      setMessage('');
      setVersion(v => v + 1); // the branch tip moved — refetch everything derived from it
    } catch (e: unknown) {
      setSaveError(errorMessage(e));
    } finally {
      setSaving(false);
    }
  };

  // ── open file ──
  if (file) {
    const ext = extOf(file);
    const isMarkdown = ext === 'md' || ext === 'markdown';
    return (
      <div>
        <div style={{ display: 'flex', alignItems: 'center', gap: 10, marginBottom: 10, flexWrap: 'wrap' }}>
          <button onClick={() => { setFile(null); setEditing(false); setPath(parentPath(file) || null); }} style={linkBtn}>← files</button>
          <span style={{ fontFamily: T.mono, fontSize: 11.5, color: T.dim, wordBreak: 'break-all' }}>{file}</span>
          <span style={{ flex: 1 }} />
          {blob && !blob.binary && !blob.truncated && !editing && (
            <button onClick={() => { setDraft(blob.content); setEditing(true); }} style={ghostBtn}>edit</button>
          )}
        </div>
        {blobError && <ErrorBox>could not load file — {blobError}</ErrorBox>}
        {saveError && <ErrorBox>could not save — {saveError}</ErrorBox>}
        {loading && !blob && <Hint busy>→ loading {file} · · ·</Hint>}
        {blob && blob.binary && <Hint>binary file ({formatBytes(blob.size)}) — not shown</Hint>}
        {blob && !blob.binary && editing && (
          <>
            <div style={{ fontFamily: T.mono, fontSize: 10.5, color: T.faint, marginBottom: 8 }}>editing on {refName}</div>
            <input value={message} onChange={e => setMessage(e.target.value)} placeholder={`commit message — default: Update ${blob.path}`}
              style={{ ...input, marginBottom: 8 }} />
            <textarea value={draft} onChange={e => setDraft(e.target.value)} spellCheck={false}
              style={{ ...input, minHeight: 'calc(100vh - 380px)', fontSize: 12.5, lineHeight: 1.55, whiteSpace: 'pre', resize: 'vertical', background: T.bgAlt }} />
            <div style={{ display: 'flex', gap: 8, marginTop: 8 }}>
              <button onClick={save} disabled={saving} style={{ ...primaryBtn, opacity: saving ? 0.6 : 1 }}>
                {saving ? '[ · · · ]' : '[ commit changes ]'}
              </button>
              <button onClick={() => { setEditing(false); setDraft(blob.content); }} style={{ ...ghostBtn, padding: '5px 10px', fontSize: 11 }}>cancel</button>
            </div>
          </>
        )}
        {blob && !blob.binary && !editing && (
          <>
            {blob.truncated && <Hint>file is large — showing the first part only</Hint>}
            {isMarkdown ? <Markdown source={blob.content} /> : <CodeView text={blob.content} path={blob.path} />}
          </>
        )}
      </div>
    );
  }

  // ── directory listing + readme + details ──
  const crumbs = dir ? dir.split('/') : [];
  let acc = '';
  return (
    <div style={{ display: 'flex', gap: 28, alignItems: 'flex-start', flexWrap: 'wrap' }}>
      <div style={{ flex: 1, minWidth: 320 }}>
        {repo.description && <div style={{ fontFamily: T.mono, fontSize: 11.5, color: T.dim, marginBottom: 12 }}>{repo.description}</div>}

        <div style={{ display: 'flex', gap: 8, alignItems: 'center', background: T.bgAlt, border: `1px solid ${T.border}`, padding: '7px 9px', marginBottom: 16 }}>
          <code style={{ flex: 1, color: T.dim, fontSize: 11, overflow: 'auto', whiteSpace: 'nowrap' }}>{repo.http_url}</code>
          <button onClick={copyClone} style={ghostBtn}>{copied ? 'copied' : 'copy'}</button>
        </div>

        <div style={{ fontFamily: T.mono, fontSize: 11.5, color: T.dim, marginBottom: 8, wordBreak: 'break-all' }}>
          <button onClick={() => setPath(null)} style={linkBtn}>root</button>
          {crumbs.map((c, i) => {
            acc = acc ? `${acc}/${c}` : c;
            const target = acc;
            return (
              <Fragment key={target}>
                {' / '}
                {i === crumbs.length - 1
                  ? <span>{c}</span>
                  : <button onClick={() => setPath(target)} style={linkBtn}>{c}</button>}
              </Fragment>
            );
          })}
        </div>

        {treeError && <ErrorBox>could not load files — {treeError}</ErrorBox>}
        {loading && entries === null && !treeError && <Hint busy>→ loading files · · ·</Hint>}
        {entries !== null && entries.length === 0 && !dir && (
          <Hint>no files yet — push a commit to the clone URL to populate this repository</Hint>
        )}
        {entries !== null && (entries.length > 0 || !!dir) && (
          <div style={{ border: `1px solid ${T.border}` }}>
            {dir && (
              <button onClick={() => setPath(parentPath(dir) || null)}
                style={{ display: 'flex', alignItems: 'center', gap: 9, width: '100%', textAlign: 'left', background: 'transparent', border: 0, borderBottom: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11.5, padding: '6px 10px', cursor: 'pointer' }}>
                <span style={{ width: 14, textAlign: 'center' }}>↩</span><span>..</span>
              </button>
            )}
            {(entries ?? []).map(e => (
              <button key={e.path}
                onClick={() => { if (e.type === 'dir') setPath(e.path); else setFile(e.path); }}
                style={{ display: 'flex', alignItems: 'center', gap: 9, width: '100%', textAlign: 'left', background: 'transparent', border: 0, borderBottom: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 11.5, padding: '6px 10px', cursor: 'pointer' }}>
                <span className="nf" style={{ width: 16, fontSize: 13, color: e.type === 'dir' ? T.green : T.faint }}>{entryIcon(e.type, e.name)}</span>
                <span style={{ flex: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{e.name}</span>
                {e.type !== 'dir' && <span style={{ color: T.faint, fontSize: 10.5 }}>{formatBytes(e.size)}</span>}
              </button>
            ))}
          </div>
        )}

        <div style={{ marginTop: 22 }}>
          {readmeError && <ErrorBox>could not load readme — {readmeError}</ErrorBox>}
          {readme && !readme.found && <Hint>no README at the repository root — add one and push to see it here</Hint>}
          {readme && readme.found && (
            <>
              <div style={{ fontFamily: T.mono, fontSize: 10.5, color: T.faint, marginBottom: 8 }}>{readme.path}</div>
              <Markdown source={readme.content} />
            </>
          )}
        </div>
      </div>

      {/* Details sidebar */}
      <aside style={{ width: 230, flexShrink: 0, fontFamily: T.mono, fontSize: 11.5 }}>
        <div style={sectionLabel}>details</div>
        {([
          ['default branch', repo.default_branch || 'main'],
          ['visibility', repo.visibility || 'private'],
          ['size', formatBytes(repo.size_bytes)],
          ...(repo.project ? [['project', repo.project]] as [string, string][] : []),
          ...(repo.kind === 'mirror' ? [['mirror of', repo.upstream_url ?? '—']] as [string, string][] : []),
          ['last updated', ago(repo.updated_at)],
          ['created', (repo.created_at || '').slice(0, 10) || '—'],
          ['id', repo.id],
        ] as [string, string][]).map(([k, v]) => (
          <div key={k} style={{ marginBottom: 8 }}>
            <div style={{ color: T.faint, fontSize: 10.5 }}>{k}</div>
            <div style={{ color: k === 'id' ? T.faint : T.text, fontSize: k === 'id' ? 10.5 : 11.5, wordBreak: 'break-all' }}>{v}</div>
          </div>
        ))}
      </aside>
    </div>
  );
}

// ── commits tab ──────────────────────────────────────────────────────────────

/** Commits tab: filterable, paged history; clicking a commit shows its diff. */
function CommitsTab({ repo, refName }: { repo: GitFactoryRepo; refName: string }) {
  const token = useAppSelector(s => s.auth.token)!;
  const [sha, setSha] = useUrlParam('commit');
  const [page, setPage] = useState(0);
  const [q, setQ] = useState('');
  const [author, setAuthor] = useState('');
  // Debounced copies — a keystroke should not fire a request per character.
  const [filters, setFilters] = useState({ q: '', author: '' });
  const [commits, setCommits] = useState<GitFactoryCommit[] | null>(null);
  const [total, setTotal] = useState(0);
  const [listError, setListError] = useState<string | null>(null);
  const [detail, setDetail] = useState<GitFactoryCommitDetail | null>(null);
  const [detailError, setDetailError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  // Applied filters, so the debounce fires a request only when the values really
  // changed — not once on mount, and not again when a keystroke is undone.
  const applied = useRef({ q: '', author: '' });
  useEffect(() => {
    const timer = setTimeout(() => {
      const next = { q: q.trim(), author: author.trim() };
      if (applied.current.q === next.q && applied.current.author === next.author) return;
      applied.current = next;
      setFilters(next);
      setPage(0); // a new filter means a new result set; staying on page 7 would show nothing
    }, 250);
    return () => clearTimeout(timer);
  }, [q, author]);

  // Reset the pager when the branch changes: page 7 of another branch is meaningless.
  useEffect(() => { setPage(0); }, [refName]);

  useEffect(() => {
    if (sha) return; // the detail view drives its own fetch
    let cancelled = false;
    setLoading(true);
    setListError(null);
    listGitFactoryCommits(token, repo.id, { ref: refName, limit: PAGE, skip: page * PAGE, q: filters.q, author: filters.author })
      .then(res => { if (!cancelled) { setCommits(res.commits ?? []); setTotal(res.total); } })
      .catch(e => { if (!cancelled) { setCommits(null); setListError(errorMessage(e)); } })
      .finally(() => { if (!cancelled) setLoading(false); });
    return () => { cancelled = true; };
  }, [token, repo.id, refName, page, filters, sha]);

  useEffect(() => {
    if (!sha) { setDetail(null); return; }
    let cancelled = false;
    setDetail(null);
    setDetailError(null);
    getGitFactoryCommit(token, repo.id, sha)
      .then(d => { if (!cancelled) setDetail(d); })
      .catch(e => { if (!cancelled) setDetailError(errorMessage(e)); });
    return () => { cancelled = true; };
  }, [token, repo.id, sha]);

  if (sha) {
    return (
      <div>
        <button onClick={() => setSha(null)} style={{ ...linkBtn, marginBottom: 12 }}>← commits</button>
        {detailError && <ErrorBox>could not load commit — {detailError}</ErrorBox>}
        {!detail && !detailError && <Hint busy>→ loading changes · · ·</Hint>}
        {detail && (
          <>
            <div style={{ marginBottom: 14 }}>
              <div style={{ fontFamily: T.mono, fontSize: 14, color: T.textHi, fontWeight: 600, marginBottom: 5 }}>{detail.subject}</div>
              <div style={{ fontFamily: T.mono, fontSize: 10.5, color: T.faint }}>
                <span style={{ color: T.green }}>{detail.short}</span> · {detail.author} · {ago(detail.date)}
                {detail.parent && <> · parent <span style={{ color: T.green }}>{detail.parent.slice(0, 8)}</span></>}
              </div>
              {detail.body && (
                <pre style={{ margin: '10px 0 0', padding: '9px 11px', background: T.bgAlt, border: `1px solid ${T.border}`, fontSize: 11.5, color: T.dim, whiteSpace: 'pre-wrap', fontFamily: T.mono }}>{detail.body}</pre>
              )}
            </div>
            <DiffPanel files={detail.files} diff={detail.diff} />
          </>
        )}
      </div>
    );
  }

  const shown = commits?.length ?? 0;
  const from = page * PAGE + 1;
  const to = page * PAGE + shown;
  const pages = Math.max(1, Math.ceil(total / PAGE));

  return (
    <div>
      <div style={{ display: 'flex', gap: 8, marginBottom: 12, flexWrap: 'wrap' }}>
        <input value={q} onChange={e => setQ(e.target.value)} placeholder="search commit messages" spellCheck={false} style={{ ...input, flex: 1, minWidth: 160 }} />
        <input value={author} onChange={e => setAuthor(e.target.value)} placeholder="author" spellCheck={false} style={{ ...input, flex: 1, minWidth: 120 }} />
      </div>
      {listError && <ErrorBox>could not load history — {listError}</ErrorBox>}
      {loading && commits === null && !listError && <Hint busy>→ loading history · · ·</Hint>}
      {commits !== null && commits.length === 0 && (
        <Hint>{filters.q || filters.author ? 'no commits match those filters' : 'no commits yet — push to the clone URL to see history here'}</Hint>
      )}
      {(commits ?? []).map(c => (
        <button key={c.sha} onClick={() => setSha(c.sha)} title="view changes"
          style={{ display: 'flex', gap: 12, alignItems: 'baseline', width: '100%', textAlign: 'left', padding: '6px 6px', background: 'transparent', border: 0, borderBottom: `1px solid ${T.border}`, fontFamily: T.mono, fontSize: 11.5, color: T.text, cursor: 'pointer' }}>
          <span style={{ color: T.green, fontSize: 10.5, flex: 'none' }}>{c.short}</span>
          <span style={{ flex: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{c.subject}</span>
          <span style={{ color: T.faint, fontSize: 10.5, flex: 'none' }}>{c.author} · {ago(c.date)}</span>
        </button>
      ))}
      {total > 0 && (
        <div style={{ display: 'flex', alignItems: 'center', gap: 10, marginTop: 14, paddingTop: 12, borderTop: `1px solid ${T.border}` }}>
          <span style={{ flex: 1, fontFamily: T.mono, fontSize: 10.5, color: T.faint }}>
            {from}–{to} of {total} commit{total === 1 ? '' : 's'} · page {page + 1} of {pages}
          </span>
          <button onClick={() => setPage(0)} disabled={page === 0} style={{ ...ghostBtn, opacity: page === 0 ? 0.4 : 1 }}>« first</button>
          <button onClick={() => setPage(p => Math.max(0, p - 1))} disabled={page === 0} style={{ ...ghostBtn, opacity: page === 0 ? 0.4 : 1 }}>‹ newer</button>
          <button onClick={() => setPage(p => p + 1)} disabled={to >= total} style={{ ...ghostBtn, opacity: to >= total ? 0.4 : 1 }}>older ›</button>
          <button onClick={() => setPage(pages - 1)} disabled={to >= total} style={{ ...ghostBtn, opacity: to >= total ? 0.4 : 1 }}>last »</button>
        </div>
      )}
    </div>
  );
}

// ── pulls tab ────────────────────────────────────────────────────────────────

/** New-pull form with a live compare preview of what the PR would contain. */
function NewPull({ repo, branches, onCreated, onCancel }: {
  repo: GitFactoryRepo;
  branches: string[];
  onCreated: (p: GitFactoryPull) => void;
  onCancel: () => void;
}) {
  const token = useAppSelector(s => s.auth.token)!;
  const def = repo.default_branch || 'main';
  const [title, setTitle] = useState('');
  const [body, setBody] = useState('');
  const [target, setTarget] = useState(def);
  const [source, setSource] = useState(() => branches.find(b => b !== def) ?? def);
  const [creating, setCreating] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [preview, setPreview] = useState<{ commits: number; files?: GitFactoryFileChange[] | null; diff?: string } | null>(null);
  const [previewError, setPreviewError] = useState<string | null>(null);

  // Seed the source branch once the branch list arrives (the form can open first).
  useEffect(() => {
    if (source === def) {
      const other = branches.find(b => b !== def);
      if (other) setSource(other);
    }
  }, [branches]); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    if (!source || !target || source === target) { setPreview(null); return; }
    let cancelled = false;
    setPreview(null);
    setPreviewError(null);
    compareGitFactoryRefs(token, repo.id, target, source)
      .then(res => { if (!cancelled) setPreview(res); })
      .catch(e => { if (!cancelled) setPreviewError(errorMessage(e)); });
    return () => { cancelled = true; };
  }, [token, repo.id, source, target]);

  const submit = async () => {
    if (!title.trim() || creating) return;
    if (source === target) { setError('source and target must differ'); return; }
    setCreating(true);
    setError(null);
    try {
      onCreated(await createGitFactoryPull(token, repo.id, { title: title.trim(), source_ref: source, target_ref: target, body: body.trim() }));
    } catch (e: unknown) {
      setError(errorMessage(e));
    } finally {
      setCreating(false);
    }
  };

  const options = branches.length ? branches : [def];
  const select = (value: string, onChange: (v: string) => void) => (
    <select value={value} onChange={e => onChange(e.target.value)} style={{ ...input, cursor: 'pointer', maxWidth: 320 }}>
      {options.map(b => <option key={b} value={b}>{b}</option>)}
    </select>
  );

  return (
    <div>
      <button onClick={onCancel} style={{ ...linkBtn, marginBottom: 12 }}>← pull requests</button>
      {error && <ErrorBox>{error}</ErrorBox>}
      <div style={{ maxWidth: 540, marginBottom: 18 }}>
        <div style={sectionLabel}>title</div>
        <input value={title} onChange={e => setTitle(e.target.value)} placeholder="what does this change" autoFocus style={{ ...input, marginBottom: 10 }} />
        <div style={sectionLabel}>description (optional)</div>
        <textarea value={body} onChange={e => setBody(e.target.value)} rows={3} style={{ ...input, resize: 'vertical', marginBottom: 10 }} />
        <div style={sectionLabel}>merge from (source)</div>
        <div style={{ marginBottom: 10 }}>{select(source, setSource)}</div>
        <div style={sectionLabel}>into (target)</div>
        <div style={{ marginBottom: 12 }}>{select(target, setTarget)}</div>
        <button onClick={submit} disabled={!title.trim() || creating} style={{ ...primaryBtn, opacity: (!title.trim() || creating) ? 0.6 : 1 }}>
          {creating ? '[ · · · ]' : '[ create ]'}
        </button>
      </div>

      {source === target
        ? <Hint>pick two different branches to see the changes</Hint>
        : previewError
          ? <ErrorBox>could not compare — {previewError}</ErrorBox>
          : !preview
            ? <Hint busy>→ comparing · · ·</Hint>
            : (
              <>
                <div style={{ fontFamily: T.mono, fontSize: 10.5, color: T.faint, marginBottom: 10 }}>
                  {source} → {target} · {preview.commits} commit{preview.commits === 1 ? '' : 's'}
                </div>
                {preview.diff
                  ? <DiffPanel files={preview.files} diff={preview.diff} />
                  : <Hint>these branches are identical — nothing to merge</Hint>}
              </>
            )}
    </div>
  );
}

// ── pull-request review: checks, reviews, conversation ───────────────────────

function statusTone(state: string): 'green' | 'red' | 'amber' | 'dim' {
  if (state === 'success') return 'green';
  if (state === 'failure' || state === 'error') return 'red';
  if (state === 'pending') return 'amber';
  return 'dim';
}
function statusColor(state: string): string {
  if (state === 'success') return T.green;
  if (state === 'failure' || state === 'error') return T.red;
  if (state === 'pending') return T.amber;
  return T.faint;
}
function reviewTone(state: string): 'green' | 'red' | 'dim' {
  if (state === 'approved') return 'green';
  if (state === 'changes_requested') return 'red';
  return 'dim';
}

/** A titled card — the shared frame for the PR-detail review sections. */
function Section({ title, children }: { title: ReactNode; children: ReactNode }) {
  return (
    <div style={{ border: `1px solid ${T.border}`, marginTop: 14 }}>
      <div style={{ padding: '7px 11px', borderBottom: `1px solid ${T.border}`, background: T.cardHi, ...sectionLabel, marginBottom: 0 }}>{title}</div>
      <div style={{ padding: 11 }}>{children}</div>
    </div>
  );
}

/** A node on the timeline's rail: a coloured dot on the vertical line, then content. */
function Node({ color, children, card }: { color: string; children: ReactNode; card?: boolean }) {
  return (
    <div style={{ display: 'flex', alignItems: 'flex-start', gap: 10, padding: card ? '6px 0' : '5px 0' }}>
      <div style={{ position: 'relative', width: 12, flex: 'none' }}>
        <div style={{ position: 'absolute', left: -1, top: 3, width: 11, height: 11, borderRadius: '50%', background: T.bg, border: `2px solid ${color}`, boxSizing: 'border-box' }} />
      </div>
      <div style={{ flex: 1, minWidth: 0, fontFamily: T.mono, fontSize: 11.5, color: T.dim, paddingTop: card ? 0 : 1 }}>{children}</div>
    </div>
  );
}

/** One PR event stream: opened, commits, comments, reviews, checks and the merge/close
 *  lifecycle, interleaved in time order — the GitHub "Conversation" model — with a
 *  composer. Commits come from listing the source ref limited to the ahead-of-target
 *  count (git-factory has no PR-commits endpoint); comments load on their own, so the
 *  stream fills in progressively. */
function Timeline({ repo, number, pr, commitCount, reviews, status, canReview, onChanged }: {
  repo: GitFactoryRepo; number: string; pr: GitFactoryPull; commitCount?: number;
  reviews?: GitFactoryPullDetail['reviews']; status?: GitFactoryPullDetail['status'];
  canReview: boolean; onChanged: () => void;
}) {
  const token = useAppSelector(s => s.auth.token)!;
  const [comments, setComments] = useState<GitFactoryPRComment[] | null>(null);
  const [commits, setCommits] = useState<GitFactoryCommit[] | null>(null);
  const [runs, setRuns] = useState<WorkflowRun[]>([]);
  const [wfNames, setWfNames] = useState<Record<string, string>>({});
  const [wfSteps, setWfSteps] = useState<Record<string, number>>({}); // workflow_id → total step count, for the progress bar
  const [nowTick, setNowTick] = useState(() => Date.now()); // ticks every 1s while a run is live, so the elapsed clock moves
  const [userNames, setUserNames] = useState<Record<string, string>>({});
  const [plannedTasks, setPlannedTasks] = useState<Record<string, string[]>>({}); // reviewer run_id → the fix tasks its map will run (in order)
  const [error, setError] = useState<string | null>(null);
  const [body, setBody] = useState('');
  const [busy, setBusy] = useState<string | null>(null);
  const [version, setVersion] = useState(0);

  useEffect(() => {
    let cancelled = false;
    setComments(null);
    listGitFactoryPRComments(token, repo.id, number)
      .then(list => { if (!cancelled) setComments(list ?? []); })
      .catch(e => { if (!cancelled) setError(errorMessage(e)); });
    return () => { cancelled = true; };
  }, [token, repo.id, number, version]);

  useEffect(() => {
    let cancelled = false;
    if (!commitCount) { setCommits([]); return; }
    // The source ref's top N commits are exactly the N it is ahead of target — the PR's commits.
    listGitFactoryCommits(token, repo.id, { ref: pr.source_ref, limit: commitCount })
      .then(pg => { if (!cancelled) setCommits(pg.commits ?? []); })
      .catch(() => { if (!cancelled) setCommits([]); }); // commits are best-effort — the rest of the timeline stands without them
    return () => { cancelled = true; };
  }, [token, repo.id, pr.source_ref, commitCount, version]);

  // The workflows this PR set off. Runs carry the PR identity in their inputs
  // (repo + number, the provenance link), so a filter surfaces the whole automation
  // chain: pr-review, and the fix-arm-c runs it fanned out. A fix run is the one that
  // carries inputs.task (the finding it is fixing) — that discriminator avoids
  // hard-coding workflow ids. Polled while any run is active so the live indicator moves.
  useEffect(() => {
    let cancelled = false;
    const mine = (r: WorkflowRun) => {
      const i = r.inputs || {};
      return i.repo === repo.name && String(i.number ?? '') === String(number);
    };
    const load = () => listRuns(token)
      .then(async all => {
        if (cancelled) return;
        const ours = (all ?? []).filter(mine);
        setRuns(ours);
        // For each reviewer flow (a run that others were spawned by — i.e. has children,
        // or is the pr-review that carries no fix task), read how many fixes its map WILL
        // run, from its extract step's tasks output, so the plan (incl. not-yet-started
        // fixers) is visible. Best-effort, one extra fetch per reviewer flow.
        const flows = ours.filter(r => !r.inputs?.task); // pr-review has no inputs.task; fix runs do
        const planned: Record<string, string[]> = {};
        await Promise.all(flows.map(async f => {
          try {
            const full = await getRun(token, f.run_id);
            const ex = (full.step_runs || []).find(s => s.step_name.split(' [')[0] === 'extract');
            let raw: unknown = ex?.output;
            if (typeof raw === 'string') { try { raw = JSON.parse(raw); } catch { /* not json */ } }
            const tasksStr = (raw && typeof raw === 'object') ? (raw as Record<string, unknown>).tasks : undefined;
            if (typeof tasksStr === 'string') { try { const arr = JSON.parse(tasksStr); if (Array.isArray(arr)) planned[f.run_id] = arr.map(String); } catch { /* */ } }
          } catch { /* best-effort */ }
        }));
        if (!cancelled) setPlannedTasks(planned);
      })
      .catch(() => { /* runs are best-effort — the timeline stands without them */ });
    load();
    const iv = setInterval(load, 8000); // cheap poll; keeps the in-progress indicator live
    return () => { cancelled = true; clearInterval(iv); };
  }, [token, repo.name, number, version]);

  // A 1s clock so a running workflow's elapsed time advances between the 8s run polls.
  // Only ticks while something is actually live, so a settled PR view does no work.
  useEffect(() => {
    const anyLive = runs.some(r => r.status === 'running' || r.status === 'pending' || r.status === 'awaiting_approval');
    if (!anyLive) return;
    const iv = setInterval(() => setNowTick(Date.now()), 1000);
    return () => clearInterval(iv);
  }, [runs]);

  // workflow_id → name, so a run entry can read "pr-review" / "fix-arm-c" not a uuid.
  useEffect(() => {
    let cancelled = false;
    listWorkflows(token)
      .then(ws => {
        if (cancelled) return;
        setWfNames(Object.fromEntries((ws ?? []).map(w => [w.workflow_id, w.name])));
        setWfSteps(Object.fromEntries((ws ?? []).map(w => [w.workflow_id, (w.steps ?? []).length])));
      })
      .catch(() => { /* names are best-effort; fall back to the short id */ });
    return () => { cancelled = true; };
  }, [token]);

  // gatekeeper user_id → username, so a comment/review/PR author shows the agent (or
  // person) name in full — git-factory records authors as user_ids, which otherwise
  // render as a truncated uuid.
  useEffect(() => {
    let cancelled = false;
    listUsers(token)
      .then(us => { if (!cancelled) setUserNames(Object.fromEntries((us ?? []).map(u => [u.user_id, u.username]))); })
      .catch(() => { /* best-effort; fall back to the id */ });
    return () => { cancelled = true; };
  }, [token]);
  const authorName = (id: string) => userNames[id] || id; // full, never truncated

  const submitReview = async (state: string) => {
    setBusy(state); setError(null);
    try { await submitGitFactoryPRReview(token, repo.id, number, state, body); setBody(''); setVersion(v => v + 1); onChanged(); }
    catch (e) { setError(errorMessage(e)); } finally { setBusy(null); }
  };
  const addComment = async () => {
    if (!body.trim()) return;
    setBusy('comment'); setError(null);
    try { await createGitFactoryPRComment(token, repo.id, number, body); setBody(''); setVersion(v => v + 1); }
    catch (e) { setError(errorMessage(e)); } finally { setBusy(null); }
  };

  const t = (iso: string) => new Date(iso).getTime();
  const items: { t: number; key: string; node: ReactNode }[] = [];
  items.push({ t: t(pr.created_at), key: '0opened', node: (
    <Node color={T.green}><span style={{ color: T.textHi }}>{authorName(pr.author)}</span> opened this pull request · <span style={{ color: T.faint }}>{ago(pr.created_at)}</span></Node>
  ) });
  for (const c of commits ?? []) { const agent = commitAgent(c.author_email); items.push({ t: t(c.date), key: 'c' + c.sha, node: (
    <Node color={T.dim}>
      {/* Order: SHA, then the bot/cicd tag, then the commit message — the tag sits
          between the sha and the subject so an automated commit is flagged up front. */}
      <span style={{ color: T.green }}>{c.short}</span>{' '}
      {agent && <><span title={`made by an automated ${agent.kind === 'cicd' ? 'CI/CD step' : 'agent'}, not a human`}
            style={{ fontFamily: T.mono, fontSize: 9.5, textTransform: 'uppercase', letterSpacing: '.04em', padding: '1px 5px', border: `1px solid ${agent.kind === 'cicd' ? T.amber : T.blue}`, color: agent.kind === 'cicd' ? T.amber : T.blue, borderRadius: 3 }}>
            {agent.kind === 'cicd' ? '⚙' : '🤖'} {agent.local}</span>{' '}</>}
      <span style={{ color: T.text }}>{c.subject}</span>
      {!agent && <span style={{ color: T.faint }}> · {c.author}</span>}
      <span style={{ color: T.faint }}> · {ago(c.date)}</span>
    </Node>
  ) }); }
  for (const s of (status?.statuses ?? [])) items.push({ t: t(s.created_at), key: 's' + s.id, node: (
    <Node color={statusColor(s.state)}>
      <Pill tone={statusTone(s.state)}>{s.state}</Pill> <span style={{ color: T.textHi }}>{s.context}</span>
      {s.description && <span style={{ color: T.faint }}> · {s.description}</span>}
      {s.target_url && <a href={s.target_url} target="_blank" rel="noreferrer" style={{ color: T.green, fontSize: 10, marginLeft: 6 }}>details ↗</a>}
      <span style={{ color: T.faint }}> · {ago(s.created_at)}</span>
    </Node>
  ) });
  for (const r of (reviews?.reviews ?? [])) items.push({ t: t(r.created_at), key: 'r' + r.id, node: (
    <Node color={reviewTone(r.state) === 'green' ? T.green : reviewTone(r.state) === 'red' ? T.red : T.dim} card={!!r.body}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
        <Pill tone={reviewTone(r.state)}>{r.state === 'changes_requested' ? 'changes requested' : r.state}</Pill>
        <span style={{ color: T.textHi }}>{authorName(r.reviewer)}</span>
        <span style={{ color: T.faint, fontSize: 10 }}>{ago(r.created_at)}</span>
      </div>
      {r.body && <div style={{ border: `1px solid ${T.border}`, background: T.bgAlt, padding: '4px 9px', marginTop: 5, color: T.text }}><Markdown source={r.body} /></div>}
    </Node>
  ) });
  for (const c of (comments ?? [])) items.push({ t: t(c.created_at), key: 'm' + c.id, node: (
    <Node color={T.blue} card>
      <div style={{ border: `1px solid ${T.border}`, background: T.bgAlt }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 8, padding: '5px 9px', borderBottom: `1px solid ${T.border}`, fontSize: 10.5 }}>
          <span style={{ color: T.textHi, fontWeight: 600 }}>{authorName(c.author)}</span>
          <span style={{ color: T.faint }}>commented · {ago(c.created_at)}</span>
        </div>
        <div style={{ padding: '2px 11px 6px' }}><Markdown source={c.body} /></div>
      </div>
    </Node>
  ) });
  // Workflow runs this PR triggered (via the provenance link). Each renders as a compact
  // <workflow name> · <status> · <run id> entry. The fixers spawned by a reviewer flow are
  // NESTED under it ("started by <flow>"), and the ones the map still WILL run — from the
  // reviewer flow's planned count minus those already started — show as 'todo', so the whole
  // fan-out plan is visible, not just what's begun. Running entries sort to the live end.
  const stFor = (status: string) => status === 'completed' ? 'success'
    : status === 'failed' || status === 'error' ? 'fail'
    : status === 'pending' ? 'todo' : status === 'cancelled' ? 'cancelled' : status;
  const toneFor = (st: string, running: boolean): 'green' | 'amber' | 'red' | 'dim' =>
    st === 'success' ? 'green' : st === 'fail' ? 'red' : running ? 'amber' : 'dim';
  const colorFor = (tone: string) => tone === 'green' ? T.green : tone === 'red' ? T.red : tone === 'amber' ? T.amber : T.dim;
  const fmtDur = (ms: number) => {
    if (!isFinite(ms) || ms < 0) ms = 0;
    const s = Math.floor(ms / 1000);
    if (s < 60) return `${s}s`;
    const m = Math.floor(s / 60);
    if (m < 60) return `${m}m ${s % 60}s`;
    const h = Math.floor(m / 60);
    return `${h}h ${m % 60}m`;
  };
  const runNode = (r: WorkflowRun, opts?: { child?: boolean; parentName?: string }) => {
    const name = wfNames[r.workflow_id] || r.workflow_id.slice(0, 8);
    const running = r.status === 'running' || r.status === 'pending' || r.status === 'awaiting_approval';
    const st = stFor(r.status);
    const tone = toneFor(st, running);
    // Progress: steps done of the pipeline's total, and how long the run has been going.
    // current_step advances as steps start; a finished run counts as all-done. total comes
    // from the pipeline definition (wfSteps); 0 when the pipeline isn't loaded yet.
    const total = wfSteps[r.workflow_id] ?? 0;
    const done = r.status === 'completed' ? (total || (r.current_step ?? 0))
      : Math.min(Math.max(r.current_step ?? 0, 0), total || Infinity);
    const startMs = new Date(r.started_at || r.created_at).getTime();
    const endMs = running ? nowTick : new Date(r.ended_at || r.started_at || r.created_at).getTime();
    const elapsed = isFinite(startMs) ? fmtDur(endMs - startMs) : '';
    const pct = total ? Math.round((done / total) * 100) : (running ? 100 : 0);
    const showBar = total > 0 || running;
    return (
      <div style={{ marginLeft: opts?.child ? 18 : 0 }}>
        <Node color={colorFor(tone)}>
          {opts?.child && <span style={{ color: T.faint }}>↳ </span>}
          <span style={{ color: T.textHi, fontWeight: 600 }}>{name}</span>
          <span style={{ color: T.faint }}> · </span><Pill tone={tone}>{st}</Pill>
          <span style={{ color: T.faint }}> · {r.run_id.slice(0, 8)}</span>
          {opts?.parentName && <span style={{ color: T.faint, fontSize: 10 }}> · started by {opts.parentName}</span>}
        </Node>
        {showBar && (
          <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginTop: 2, marginLeft: 14 }}>
            <div style={{ width: 90, height: 4, borderRadius: 2, background: T.border, overflow: 'hidden', flexShrink: 0 }}
              title={total ? `${done} of ${total} steps` : undefined}>
              <div style={{ width: `${pct}%`, height: '100%', background: colorFor(tone), transition: 'width .3s' }} />
            </div>
            <span style={{ fontFamily: T.mono, fontSize: 10, color: T.faint }}>
              {total ? `${done}/${total} steps` : `${done} steps`}{elapsed && <> · {running ? '' : 'ran '}{elapsed}</>}
            </span>
          </div>
        )}
      </div>
    );
  };
  const childrenOf = (id: string) => runs.filter(r => r.parent_run_id === id);
  const topRuns = runs.filter(r => !r.parent_run_id || !runs.some(p => p.run_id === r.parent_run_id));
  for (const r of topRuns) {
    const running = r.status === 'running' || r.status === 'pending';
    const when = r.ended_at || r.started_at || r.created_at;
    const kids = childrenOf(r.run_id);
    const flowName = wfNames[r.workflow_id] || 'review';
    // one timeline item per reviewer flow, carrying its nested fixers + the queued plan.
    // Each planned fix the map WILL run but hasn't started — shown individually (not
    // collapsed to a count) so the whole fan-out plan is visible. A planned task is
    // "started" once a child fix run carries it as inputs.task.
    const startedTasks = new Set(kids.map(k => typeof k.inputs?.task === 'string' ? k.inputs!.task as string : ''));
    const queuedTasks = (plannedTasks[r.run_id] || []).filter(tk => !startedTasks.has(tk));
    const findingOf = (task: string) => task.replace(/^Fix this review finding in the code \(at [^)]*\)\.\s*/, '').replace(/^Issue:\s*/, '');
    items.push({ t: running ? Date.now() : t(when), key: 'w' + r.run_id, node: (
      <div>
        {runNode(r)}
        {kids.map(k => <div key={k.run_id}>{runNode(k, { child: true, parentName: flowName })}</div>)}
        {queuedTasks.map((tk, i) => (
          <div key={'q' + i} style={{ marginLeft: 18 }}><Node color={T.dim}>
            <span style={{ color: T.faint }}>↳ </span>
            <span style={{ color: T.textHi, fontWeight: 600 }}>autofix</span>
            <span style={{ color: T.faint }}> · </span><Pill tone="dim">todo</Pill>
            <span style={{ color: T.faint }}> · queued by {flowName} · {truncateText(findingOf(tk), 80)}</span>
          </Node></div>
        ))}
      </div>
    ) });
  }
  if (pr.state === 'merged') items.push({ t: t(pr.updated_at), key: 'zmerged', node: (
    <Node color={T.blue}><span style={{ color: T.blue }}>merged</span>{pr.merge_commit && <> as <span style={{ color: T.green }}>{pr.merge_commit.slice(0, 8)}</span></>} · <span style={{ color: T.faint }}>{ago(pr.updated_at)}</span></Node>
  ) });
  else if (pr.state === 'closed') items.push({ t: t(pr.updated_at), key: 'zclosed', node: (
    <Node color={T.red}><span style={{ color: T.red }}>closed</span> · <span style={{ color: T.faint }}>{ago(pr.updated_at)}</span></Node>
  ) });
  items.sort((a, b) => a.t - b.t || a.key.localeCompare(b.key));

  const loading = comments === null || commits === null;
  return (
    <Section title={<>timeline · {items.length} event{items.length === 1 ? '' : 's'}{loading && <span style={{ color: T.faint }}> · loading…</span>}</>}>
      {error && <ErrorBox>{error}</ErrorBox>}
      <div style={{ position: 'relative' }}>
        <div style={{ position: 'absolute', left: 5, top: 8, bottom: 8, borderLeft: `1px solid ${T.border}` }} />
        {items.map(it => <div key={it.key}>{it.node}</div>)}
      </div>
      <div style={{ marginTop: 12, borderTop: `1px solid ${T.border}`, paddingTop: 12 }}>
        <textarea value={body} onChange={e => setBody(e.target.value)} placeholder={canReview ? 'leave a comment, or comment with your review…' : 'add a comment…'} rows={3} style={{ ...input, resize: 'vertical' }} />
        <div style={{ display: 'flex', gap: 8, marginTop: 8, justifyContent: 'flex-end', flexWrap: 'wrap' }}>
          {canReview && <>
            <button onClick={() => submitReview('approved')} disabled={!!busy} style={{ ...primaryBtn, opacity: busy ? 0.5 : 1 }}>{busy === 'approved' ? '[ · · · ]' : '[ approve ]'}</button>
            <button onClick={() => submitReview('changes_requested')} disabled={!!busy}
              style={{ background: T.redSoft, border: `1px solid ${T.red}`, color: T.red, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer', opacity: busy ? 0.5 : 1 }}>
              {busy === 'changes_requested' ? '[ · · · ]' : 'request changes'}
            </button>
          </>}
          <button onClick={addComment} disabled={!!busy || !body.trim()}
            style={{ ...(canReview ? ghostBtn : primaryBtn), padding: '5px 12px', fontSize: 11, opacity: (busy || !body.trim()) ? 0.5 : 1 }}>
            {busy === 'comment' ? '[ · · · ]' : '[ comment ]'}
          </button>
        </div>
      </div>
    </Section>
  );
}

/** One pull request: metadata, merge state, review diff, and merge/close actions. */
function PullDetail({ repo, number, onBack, onChanged }: {
  repo: GitFactoryRepo;
  number: string;
  onBack: () => void;
  onChanged: () => void;
}) {
  const token = useAppSelector(s => s.auth.token)!;
  const [detail, setDetail] = useState<GitFactoryPullDetail | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [busy, setBusy] = useState<'merge' | 'close' | null>(null);
  const [version, setVersion] = useState(0);
  const [showDiff, setShowDiff] = useState(false);

  useEffect(() => {
    let cancelled = false;
    setDetail(null);
    setError(null);
    getGitFactoryPull(token, repo.id, number)
      .then(d => { if (!cancelled) setDetail(d); })
      .catch(e => { if (!cancelled) setError(errorMessage(e)); });
    return () => { cancelled = true; };
  }, [token, repo.id, number, version]);

  const act = async (verb: 'merge' | 'close') => {
    setBusy(verb);
    setActionError(null);
    try {
      if (verb === 'merge') await mergeGitFactoryPull(token, repo.id, number);
      else await closeGitFactoryPull(token, repo.id, number);
      setVersion(v => v + 1);
      onChanged();
    } catch (e: unknown) {
      setActionError(`${verb} failed — ${errorMessage(e)}`);
    } finally {
      setBusy(null);
    }
  };

  if (error) {
    return (
      <div>
        <button onClick={onBack} style={{ ...linkBtn, marginBottom: 12 }}>← pull requests</button>
        <ErrorBox>could not load pull request — {error}</ErrorBox>
      </div>
    );
  }
  if (!detail) {
    return (
      <div>
        <button onClick={onBack} style={{ ...linkBtn, marginBottom: 12 }}>← pull requests</button>
        <Hint busy>→ loading pull request · · ·</Hint>
      </div>
    );
  }

  const pr = detail.pull_request;
  const open = pr.state === 'open';
  const blocked = open && detail.merge?.mergeable === false;

  return (
    <div>
      <button onClick={onBack} style={{ ...linkBtn, marginBottom: 12 }}>← pull requests</button>
      {actionError && <ErrorBox>{actionError}</ErrorBox>}
      <div style={{ marginBottom: 14 }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 10, marginBottom: 6, flexWrap: 'wrap' }}>
          <Pill tone={pullTone(pr.state)}>{pr.state}</Pill>
          <span style={{ fontFamily: T.mono, fontSize: 14, color: T.textHi, fontWeight: 600 }}>#{pr.number} {pr.title}</span>
        </div>
        <div style={{ fontFamily: T.mono, fontSize: 10.5, color: T.faint }}>
          {pr.source_ref} → {pr.target_ref}
          {detail.commits !== undefined && <> · {detail.commits} commit{detail.commits === 1 ? '' : 's'}</>}
          {pr.merge_commit && <> · merged as <span style={{ color: T.green }}>{pr.merge_commit.slice(0, 8)}</span></>}
          {' · opened '}{ago(pr.created_at)}
        </div>
        {blocked && (
          <div style={{ fontFamily: T.mono, fontSize: 11, color: T.red, marginTop: 6 }}>
            cannot merge: {detail.merge?.reason || (detail.merge?.conflicts?.length ? `conflicts in ${detail.merge.conflicts.join(', ')}` : 'conflicts')}
          </div>
        )}
        {pr.body && (
          <pre style={{ margin: '10px 0 0', padding: '9px 11px', background: T.bgAlt, border: `1px solid ${T.border}`, fontSize: 11.5, color: T.dim, whiteSpace: 'pre-wrap', fontFamily: T.mono }}>{pr.body}</pre>
        )}
        {open && (
          <div style={{ display: 'flex', gap: 8, marginTop: 12 }}>
            <button onClick={() => act('merge')} disabled={!!busy || blocked} title={blocked ? (detail.merge?.reason || 'not mergeable') : undefined}
              style={{ ...primaryBtn, opacity: (busy || blocked) ? 0.5 : 1 }}>
              {busy === 'merge' ? '[ · · · ]' : '[ merge ]'}
            </button>
            <button onClick={() => act('close')} disabled={!!busy}
              style={{ ...ghostBtn, padding: '5px 12px', fontSize: 11, opacity: busy ? 0.5 : 1 }}>
              {busy === 'close' ? '[ · · · ]' : 'close'}
            </button>
          </div>
        )}
      </div>
      {detail.files_error && <ErrorBox>could not summarise the changes — {detail.files_error}</ErrorBox>}
      {detail.diff !== undefined && (
        <div style={{ marginTop: 14 }}>
          <button onClick={() => setShowDiff(v => !v)} style={{ ...ghostBtn, padding: '5px 12px', fontSize: 11 }}>
            {showDiff ? '▾' : '▸'} files changed{detail.files ? ` (${detail.files.length})` : ''}
          </button>
          {showDiff && <div style={{ marginTop: 10 }}><DiffPanel files={detail.files} diff={detail.diff} /></div>}
        </div>
      )}
      <Timeline
        repo={repo} number={number} pr={pr} commitCount={detail.commits}
        reviews={detail.reviews} status={detail.status}
        canReview={open} onChanged={() => setVersion(v => v + 1)}
      />
    </div>
  );
}

/** Pulls tab: the list, the new-pull form, or one pull's review view. */
function PullsTab({ repo, branches }: { repo: GitFactoryRepo; branches: string[] }) {
  const token = useAppSelector(s => s.auth.token)!;
  const [pr, setPr] = useUrlParam('pr');
  const [pulls, setPulls] = useState<GitFactoryPull[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [version, setVersion] = useState(0);

  useEffect(() => {
    if (pr) return;
    let cancelled = false;
    setError(null);
    listGitFactoryPulls(token, repo.id)
      .then(list => { if (!cancelled) setPulls(list ?? []); })
      .catch(e => { if (!cancelled) { setPulls(null); setError(errorMessage(e)); } });
    return () => { cancelled = true; };
  }, [token, repo.id, pr, version]);

  if (pr === 'new') {
    return <NewPull repo={repo} branches={branches} onCancel={() => setPr(null)}
      onCreated={(created) => { setVersion(v => v + 1); setPr(String(created.number)); }} />;
  }
  if (pr) {
    return <PullDetail repo={repo} number={pr} onBack={() => setPr(null)} onChanged={() => setVersion(v => v + 1)} />;
  }

  return (
    <div>
      <div style={{ marginBottom: 14 }}>
        <button onClick={() => setPr('new')} style={{ ...primaryBtn, fontWeight: 600, padding: '6px 14px' }}>[ + new pull request ]</button>
      </div>
      {error && <ErrorBox>could not load pull requests — {error}</ErrorBox>}
      {!pulls && !error && <Hint busy>→ loading pull requests · · ·</Hint>}
      {pulls && pulls.length === 0 && <Hint>no pull requests yet — open one to propose merging a branch</Hint>}
      {(pulls ?? []).map(p => (
        <button key={p.id} onClick={() => setPr(String(p.number))} title="review"
          style={{ display: 'flex', gap: 12, alignItems: 'center', width: '100%', textAlign: 'left', padding: '7px 6px', background: 'transparent', border: 0, borderBottom: `1px solid ${T.border}`, fontFamily: T.mono, fontSize: 11.5, color: T.text, cursor: 'pointer' }}>
          <Pill tone={pullTone(p.state)}>{p.state}</Pill>
          <span style={{ flex: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>#{p.number} {p.title}</span>
          <span style={{ color: T.faint, fontSize: 10.5, flex: 'none' }}>{p.source_ref} → {p.target_ref} · {ago(p.created_at)}</span>
        </button>
      ))}
    </div>
  );
}

// ── settings tab ─────────────────────────────────────────────────────────────

/**
 * Settings tab: the repo-administration surface the mini-portal never exposed —
 * default branch, tags, branch protections and collaborators. Each block loads
 * and fails independently, so a user permitted to see one and not another gets
 * the part they hold rather than a blanked page.
 */
function SettingsTab({ repo, branches, onChanged }: { repo: GitFactoryRepo; branches: string[]; onChanged: () => void }) {
  const token = useAppSelector(s => s.auth.token)!;
  const [confirm, confirmEl] = useConfirm();

  const [defBranch, setDefBranch] = useState(repo.default_branch || 'main');
  const [savingDef, setSavingDef] = useState(false);
  const [defError, setDefError] = useState<string | null>(null);

  const [tags, setTags] = useState<GitFactoryRef[] | null>(null);
  const [tagsError, setTagsError] = useState<string | null>(null);

  const [protections, setProtections] = useState<GitFactoryProtection[] | null>(null);
  const [protError, setProtError] = useState<string | null>(null);
  const [pattern, setPattern] = useState('');

  const [collabs, setCollabs] = useState<{ owner: string; collaborators: { user_id: string; level: string }[] } | null>(null);
  const [collabError, setCollabError] = useState<string | null>(null);
  const [newCollab, setNewCollab] = useState('');
  const [newLevel, setNewLevel] = useState('read');

  useEffect(() => { setDefBranch(repo.default_branch || 'main'); }, [repo.default_branch]);

  useEffect(() => {
    let cancelled = false;
    listGitFactoryTags(token, repo.id)
      .then(t => { if (!cancelled) setTags(t ?? []); })
      .catch(e => { if (!cancelled) setTagsError(errorMessage(e)); });
    return () => { cancelled = true; };
  }, [token, repo.id]);

  const loadProtections = useCallback(() => {
    listGitFactoryProtections(token, repo.id)
      .then(p => { setProtections(p ?? []); setProtError(null); })
      .catch(e => { setProtections(null); setProtError(errorMessage(e)); });
  }, [token, repo.id]);

  const loadCollabs = useCallback(() => {
    listGitFactoryCollaborators(token, repo.id)
      .then(c => { setCollabs(c); setCollabError(null); })
      .catch(e => { setCollabs(null); setCollabError(errorMessage(e)); });
  }, [token, repo.id]);

  useEffect(() => { loadProtections(); }, [loadProtections]);
  useEffect(() => { loadCollabs(); }, [loadCollabs]);

  const applyDefault = async () => {
    if (savingDef || defBranch === repo.default_branch) return;
    setSavingDef(true);
    setDefError(null);
    try {
      await setGitFactoryDefaultBranch(token, repo.id, defBranch);
      onChanged();
    } catch (e: unknown) {
      setDefError(errorMessage(e));
    } finally {
      setSavingDef(false);
    }
  };

  const addProtection = async () => {
    if (!pattern.trim()) return;
    try {
      await setGitFactoryProtection(token, repo.id, { pattern: pattern.trim(), block_force_push: true, block_deletion: true });
      setPattern('');
      loadProtections();
    } catch (e: unknown) {
      setProtError(errorMessage(e));
    }
  };

  const removeProtection = async (p: GitFactoryProtection) => {
    if (!(await confirm({ message: `Remove branch protection for ${p.pattern}? Force pushes and deletions will be allowed again.`, confirmLabel: 'remove' }))) return;
    try {
      await deleteGitFactoryProtection(token, repo.id, p.pattern);
      loadProtections();
    } catch (e: unknown) {
      setProtError(errorMessage(e));
    }
  };

  const addCollaborator = async () => {
    if (!newCollab.trim()) return;
    try {
      await addGitFactoryCollaborator(token, repo.id, newCollab.trim(), newLevel);
      setNewCollab('');
      loadCollabs();
    } catch (e: unknown) {
      setCollabError(errorMessage(e));
    }
  };

  const removeCollaborator = async (userId: string) => {
    if (!(await confirm({ message: `Revoke ${shortId(userId)}'s access to ${repo.name}?`, confirmLabel: 'revoke' }))) return;
    try {
      await removeGitFactoryCollaborator(token, repo.id, userId);
      loadCollabs();
    } catch (e: unknown) {
      setCollabError(errorMessage(e));
    }
  };

  const options = branches.length ? branches : [repo.default_branch || 'main'];

  return (
    <div style={{ maxWidth: 620 }}>
      {confirmEl}

      <div style={{ marginBottom: 26 }}>
        <div style={sectionLabel}>default branch</div>
        {defError && <ErrorBox>{defError}</ErrorBox>}
        <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
          <select value={defBranch} onChange={e => setDefBranch(e.target.value)} style={{ ...input, cursor: 'pointer', maxWidth: 260 }}>
            {options.map(b => <option key={b} value={b}>{b}</option>)}
          </select>
          <button onClick={applyDefault} disabled={savingDef || defBranch === repo.default_branch}
            style={{ ...primaryBtn, opacity: (savingDef || defBranch === repo.default_branch) ? 0.5 : 1 }}>
            {savingDef ? '[ · · · ]' : '[ set ]'}
          </button>
        </div>
        <div style={{ fontFamily: T.mono, fontSize: 10.5, color: T.faint, marginTop: 6 }}>
          where HEAD points — what a fresh clone checks out.
        </div>
      </div>

      <div style={{ marginBottom: 26 }}>
        <div style={sectionLabel}>branch protections</div>
        {protError && <ErrorBox>{protError}</ErrorBox>}
        {protections === null && !protError && <Hint busy>→ loading · · ·</Hint>}
        {protections?.length === 0 && <Hint>no protected branches — force pushes and deletions are allowed everywhere</Hint>}
        {(protections ?? []).map(p => (
          <div key={p.pattern} style={{ display: 'flex', gap: 12, alignItems: 'center', padding: '6px 0', borderBottom: `1px solid ${T.border}`, fontFamily: T.mono, fontSize: 11.5 }}>
            <span style={{ flex: 1, color: T.text }}>{p.pattern}</span>
            <span style={{ color: T.faint, fontSize: 10.5 }}>
              {[p.block_force_push && 'no force-push', p.block_deletion && 'no delete'].filter(Boolean).join(' · ') || 'no rules'}
            </span>
            <button onClick={() => removeProtection(p)} style={ghostBtn}>remove</button>
          </div>
        ))}
        <div style={{ display: 'flex', gap: 8, marginTop: 10 }}>
          <input value={pattern} onChange={e => setPattern(e.target.value)} onKeyDown={e => { if (e.key === 'Enter') addProtection(); }}
            placeholder="branch name or glob (e.g. main, release/*)" style={{ ...input, flex: 1 }} />
          <button onClick={addProtection} disabled={!pattern.trim()} style={{ ...primaryBtn, opacity: pattern.trim() ? 1 : 0.5 }}>[ protect ]</button>
        </div>
      </div>

      <div style={{ marginBottom: 26 }}>
        <div style={sectionLabel}>collaborators</div>
        {collabError && <ErrorBox>{collabError}</ErrorBox>}
        {collabs === null && !collabError && <Hint busy>→ loading · · ·</Hint>}
        {collabs && (
          <>
            <div style={{ display: 'flex', gap: 12, alignItems: 'center', padding: '6px 0', borderBottom: `1px solid ${T.border}`, fontFamily: T.mono, fontSize: 11.5 }}>
              <span style={{ flex: 1, color: T.text }}>{shortId(collabs.owner)}</span>
              <Pill tone="dim">owner</Pill>
            </div>
            {collabs.collaborators.length === 0 && <Hint>not shared with anyone else</Hint>}
            {collabs.collaborators.map(c => (
              <div key={`${c.user_id}:${c.level}`} style={{ display: 'flex', gap: 12, alignItems: 'center', padding: '6px 0', borderBottom: `1px solid ${T.border}`, fontFamily: T.mono, fontSize: 11.5 }}>
                <span style={{ flex: 1, color: T.text }} title={c.user_id}>{shortId(c.user_id)}</span>
                <Pill tone={c.level === 'write' ? 'amber' : 'dim'}>{c.level}</Pill>
                <button onClick={() => removeCollaborator(c.user_id)} style={ghostBtn}>revoke</button>
              </div>
            ))}
          </>
        )}
        <div style={{ display: 'flex', gap: 8, marginTop: 10 }}>
          <input value={newCollab} onChange={e => setNewCollab(e.target.value)} onKeyDown={e => { if (e.key === 'Enter') addCollaborator(); }}
            placeholder="user id to share with" style={{ ...input, flex: 1 }} />
          <select value={newLevel} onChange={e => setNewLevel(e.target.value)} style={{ ...input, cursor: 'pointer', width: 100 }}>
            <option value="read">read</option>
            <option value="write">write</option>
          </select>
          <button onClick={addCollaborator} disabled={!newCollab.trim()} style={{ ...primaryBtn, opacity: newCollab.trim() ? 1 : 0.5 }}>[ share ]</button>
        </div>
      </div>

      <div>
        <div style={sectionLabel}>tags</div>
        {tagsError && <ErrorBox>{tagsError}</ErrorBox>}
        {tags === null && !tagsError && <Hint busy>→ loading · · ·</Hint>}
        {tags?.length === 0 && <Hint>no tags — push one to see it here</Hint>}
        {(tags ?? []).map(t => (
          <div key={t.name} style={{ display: 'flex', gap: 12, alignItems: 'baseline', padding: '5px 0', borderBottom: `1px solid ${T.border}`, fontFamily: T.mono, fontSize: 11.5 }}>
            <span style={{ color: T.text, flex: 'none' }}>{t.name}</span>
            <span style={{ color: T.green, fontSize: 10.5, flex: 'none' }}>{(t.sha || '').slice(0, 8)}</span>
            <span style={{ flex: 1, color: T.dim, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{t.subject}</span>
            <span style={{ color: T.faint, fontSize: 10.5, flex: 'none' }}>{ago(t.date)}</span>
          </div>
        ))}
      </div>
    </div>
  );
}

// ── repo detail ──────────────────────────────────────────────────────────────

/** The selected repository: header, branch switcher, tabs, and the active tab's view. */
function RepoDetail({ repo, onDeleted, onChanged }: {
  repo: GitFactoryRepo;
  onDeleted: (id: string) => void;
  onChanged: () => void;
}) {
  const token = useAppSelector(s => s.auth.token)!;
  const [tab, setTab] = useUrlState<RepoTab>('tab', 'code');
  const [refName, setRefName] = useUrlParam('ref');
  const [, setPath] = useUrlParam('path');
  const [, setFile] = useUrlParam('file');
  const [, setCommit] = useUrlParam('commit');
  const [branches, setBranches] = useState<string[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [confirm, confirmEl] = useConfirm();

  // The empty ref means "the repo's default branch"; resolving it here keeps the
  // tree, blob, history and the switcher agreeing on which branch is in view.
  const effRef = refName || repo.default_branch || 'main';

  useEffect(() => {
    let cancelled = false;
    listGitFactoryBranches(token, repo.id)
      .then(res => { if (!cancelled) setBranches((res.branches ?? []).map(b => b.name)); })
      .catch(() => { /* a branch list failing is not worth a banner — the picker just shows the current ref */ });
    return () => { cancelled = true; };
  }, [token, repo.id]);

  const switchBranch = (name: string) => {
    setRefName(name === (repo.default_branch || 'main') ? null : name);
    // A new branch starts at its root, not wherever the last one was.
    setPath(null);
    setFile(null);
    setCommit(null);
  };

  const handleDelete = async () => {
    if (!(await confirm({
      message: `Delete ${repo.namespace}/${repo.name}? The repository and all of its history are removed from disk — this cannot be undone.`,
      requireText: repo.name,
    }))) return;
    try {
      await deleteGitFactoryRepo(token, repo.id);
      onDeleted(repo.id);
    } catch (e: unknown) {
      setError(errorMessage(e));
    }
  };

  const options = branches.length ? branches : [effRef];
  const tabs: RepoTab[] = ['code', 'commits', 'pulls', 'settings'];

  return (
    <div style={{ flex: 1, display: 'flex', flexDirection: 'column', overflow: 'hidden' }}>
      {confirmEl}
      <div style={{ padding: '12px 20px 0', borderBottom: `1px solid ${T.border}`, background: T.bgAlt, flexShrink: 0 }}>
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 12, flexWrap: 'wrap' }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 10, minWidth: 0 }}>
            <span style={{ fontFamily: T.mono, fontSize: 15, fontWeight: 700, color: T.textHi, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
              <span style={{ color: T.dim, fontWeight: 400 }}>{repo.namespace}/</span>{repo.name}
            </span>
            {repo.visibility === 'public' && <Pill tone="blue">public</Pill>}
            {repo.kind === 'mirror' && <Pill tone="dim">mirror</Pill>}
          </div>
          <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
            {(tab === 'code' || tab === 'commits') && (
              <select value={effRef} onChange={e => switchBranch(e.target.value)} title="switch branch"
                style={{ background: T.greenSoft, border: `1px solid ${T.green}`, color: T.green, fontFamily: T.mono, fontSize: 11.5, fontWeight: 600, padding: '4px 8px', cursor: 'pointer', outline: 'none', maxWidth: 220 }}>
                {options.map(b => <option key={b} value={b} style={{ background: T.bgAlt, color: T.text, fontWeight: 400 }}>{b}</option>)}
              </select>
            )}
            <button onClick={handleDelete}
              style={{ ...ghostBtn, fontSize: 11, padding: '5px 12px' }}
              onMouseEnter={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.red; (e.currentTarget as HTMLButtonElement).style.color = T.red; }}
              onMouseLeave={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.border; (e.currentTarget as HTMLButtonElement).style.color = T.dim; }}>
              [ delete ]
            </button>
          </div>
        </div>

        <div style={{ display: 'flex', gap: 2, marginTop: 10 }}>
          {tabs.map(t => (
            <button key={t} onClick={() => setTab(t)}
              style={{ background: 'transparent', border: 0, borderBottom: `2px solid ${tab === t ? T.green : 'transparent'}`, color: tab === t ? T.textHi : T.dim, fontFamily: T.mono, fontSize: 12, padding: '6px 12px', cursor: 'pointer' }}>
              {t}
            </button>
          ))}
        </div>
      </div>

      <div style={{ flex: 1, overflow: 'auto', padding: '18px 20px 32px' }}>
        {error && <ErrorBox>{error}</ErrorBox>}
        {tab === 'code' && <CodeTab repo={repo} refName={effRef} />}
        {tab === 'commits' && <CommitsTab repo={repo} refName={effRef} />}
        {tab === 'pulls' && <PullsTab repo={repo} branches={branches} />}
        {tab === 'settings' && <SettingsTab repo={repo} branches={branches} onChanged={onChanged} />}
      </div>
    </div>
  );
}

// ── page ─────────────────────────────────────────────────────────────────────

/** Repos route: the repository rail (list + create) beside the selected repo's detail. */
export function Repos() {
  const token = useAppSelector(s => s.auth.token)!;
  const project = useAppSelector(s => s.project.current);
  // The effective display scope: the selected project's slug plus its ancestors', so a
  // child project shows repos it inherits from its parents (view/use, not edit).
  const chain = useAppSelector(s => s.project.chain);
  const [repos, setRepos] = useState<GitFactoryRepo[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selId, setSelId] = useUrlParam('repo');
  const [showCreate, setShowCreate] = useState(false);
  const [query, setQuery] = useState('');
  const [railW, railHandle] = useResizableWidth('rail.repos.main', 260, { min: 200, max: 480 });
  const [, setTab] = useUrlState<RepoTab>('tab', 'code');
  const [, setRefName] = useUrlParam('ref');
  const [, setPath] = useUrlParam('path');
  const [, setFile] = useUrlParam('file');
  const [, setPr] = useUrlParam('pr');
  const [, setCommit] = useUrlParam('commit');
  // Guards the auto-select below so it only runs on the first load, leaving a
  // deliberate "nothing selected" alone afterwards.
  const seeded = useRef(false);

  const fetchRepos = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      setRepos(await listGitFactoryRepos(token));
    } catch (e: unknown) {
      setError(errorMessage(e));
    } finally {
      setLoading(false);
    }
  }, [token]);

  useEffect(() => { fetchRepos(); }, [fetchRepos]);

  // The project switcher is a view filter across the portal; repos carry the same
  // label, so an active project narrows this list too. On top of that, the search
  // box narrows by name/namespace, and the list is ordered most-recently-updated
  // first so the repo you last touched is at the top.
  const visible = useMemo(() => {
    const q = query.trim().toLowerCase();
    return repos
      .filter(r => (chain.length ? (!!r.project && chain.includes(r.project)) : true))
      .filter(r => !q || r.name.toLowerCase().includes(q) || (r.namespace ?? '').toLowerCase().includes(q))
      .slice()
      .sort((a, b) => new Date(b.updated_at ?? 0).getTime() - new Date(a.updated_at ?? 0).getTime());
  }, [repos, project, query]);

  const selected = visible.find(r => r.id === selId) ?? null;

  // Land on the first repo when nothing is selected, so the page opens on content
  // rather than an empty pane.
  useEffect(() => {
    if (seeded.current || loading) return;
    seeded.current = true;
    if (!selId && visible.length) setSelId(visible[0].id);
  }, [loading, visible, selId, setSelId]);

  /** Open a repo, resetting the per-repo view state the URL carries. */
  const open = (r: GitFactoryRepo) => {
    setSelId(r.id);
    setTab('code');
    setRefName(null);
    setPath(null);
    setFile(null);
    setPr(null);
    setCommit(null);
  };

  return (
    <div style={{ display: 'flex', height: '100%', overflow: 'hidden' }}>
      {/* Repo list */}
      <div style={{ width: railW, flexShrink: 0, borderRight: `1px solid ${T.border}`, display: 'flex', flexDirection: 'column', background: T.bgAlt }}>
        <div style={{ padding: '14px 14px 10px', borderBottom: `1px solid ${T.border}` }}>
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 4 }}>
            <span style={{ fontFamily: T.mono, fontSize: 13, fontWeight: 700, color: T.textHi }}>repos/</span>
            <div style={{ display: 'flex', gap: 6 }}>
              <button onClick={() => setShowCreate(v => !v)} title="new repository"
                style={{ ...ghostBtn, background: showCreate ? T.greenSoft : 'transparent', borderColor: showCreate ? T.green : T.border, color: showCreate ? T.green : T.dim, padding: '3px 7px' }}>+</button>
              <button onClick={fetchRepos} title="refresh" style={{ ...ghostBtn, padding: '3px 7px' }}>↻</button>
            </div>
          </div>
          <div style={{ position: 'relative', margin: '8px 0 6px' }}>
            <span style={{ position: 'absolute', left: 8, top: '50%', transform: 'translateY(-50%)', fontFamily: T.mono, fontSize: 11, color: T.faint, pointerEvents: 'none' }}>⌕</span>
            <input value={query} onChange={e => setQuery(e.target.value)} placeholder="search repos…" spellCheck={false}
              style={{ width: '100%', boxSizing: 'border-box', padding: '5px 22px 5px 22px', fontFamily: T.mono, fontSize: 11, color: T.text, background: T.bg, border: `1px solid ${T.border}`, borderRadius: 3, outline: 'none' }} />
            {query && (
              <button onClick={() => setQuery('')} title="clear" aria-label="clear search"
                style={{ position: 'absolute', right: 4, top: '50%', transform: 'translateY(-50%)', background: 'transparent', border: 0, cursor: 'pointer', color: T.faint, fontFamily: T.mono, fontSize: 12, padding: '0 4px', lineHeight: 1 }}>×</button>
            )}
          </div>
          <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint }}>
            {visible.length > 0 && `${visible.length} repositor${visible.length === 1 ? 'y' : 'ies'}${project ? ` in ${project}` : ''}`}
          </div>
        </div>

        {showCreate && (
          <CreateRepo onCancel={() => setShowCreate(false)}
            onCreated={(r) => { setShowCreate(false); setRepos(prev => [r, ...prev]); open(r); }} />
        )}

        <div style={{ flex: 1, overflow: 'auto' }}>
          {loading ? (
            <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint, animation: 'pulse 1s ease-in-out infinite' }}>→ loading · · ·</div>
          ) : error ? (
            <div style={{ padding: '14px', fontFamily: T.mono, fontSize: 11, color: T.red }}>{error}</div>
          ) : visible.length === 0 ? (
            <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint, lineHeight: 1.7 }}>
              {query.trim()
                ? <>→ no repositories match “{query.trim()}”</>
                : <>→ no repositories{project ? ` in ${project}` : ''}<br />press + to create one</>}
            </div>
          ) : visible.map(r => {
            const isActive = selected?.id === r.id;
            return (
              <button key={r.id} onClick={() => open(r)}
                style={{ width: '100%', textAlign: 'left', padding: '10px 14px', background: isActive ? T.greenSoft : 'transparent', border: 0, borderLeft: `2px solid ${isActive ? T.green : 'transparent'}`, fontFamily: T.mono, cursor: 'pointer', color: T.text, display: 'block', transition: 'background .12s' }}>
                <div style={{ fontSize: 12.5, fontWeight: 600, color: isActive ? T.textHi : T.text, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{r.name}</div>
                <div style={{ fontSize: 10, color: T.faint, marginTop: 3 }}>
                  {r.namespace} · <span style={{ color: T.dim }}>{r.default_branch || 'main'}</span>
                </div>
                <div style={{ fontSize: 10, color: T.faint, marginTop: 1 }}>updated {ago(r.updated_at)}</div>
              </button>
            );
          })}
        </div>
      </div>
      {railHandle}

      {/* Detail */}
      {selected ? (
        <RepoDetail key={selected.id} repo={selected}
          onDeleted={(id) => { setRepos(prev => prev.filter(r => r.id !== id)); setSelId(null); }}
          onChanged={fetchRepos} />
      ) : (
        <div style={{ flex: 1, display: 'flex', alignItems: 'center', justifyContent: 'center', padding: 24 }}>
          <div style={{ fontFamily: T.mono, fontSize: 12, color: T.faint, textAlign: 'center', lineHeight: 1.8 }}>
            → select a repository<br />or press + to create one
          </div>
        </div>
      )}
    </div>
  );
}

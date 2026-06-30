/**
 * Git page: the `git` core service — a backend-agnostic git credential broker.
 * Left is the list of registered backends plus a create form whose auth fields
 * reveal themselves per (type, mode); right is the selected backend's detail with
 * a "test" probe, a confirm-gated delete, and a small "mint credential" tool that
 * resolves a repo URL to a clone URL (masking the minted secret by default). All
 * calls go through the BFF (listGitBackends / createGitBackend / testGitBackend /
 * mintGitCredential / …) which passes through verbatim to /api/git/*.
 */
import { useState, useEffect, useCallback } from 'react';
import { T } from '../../theme';
import { useResizableWidth } from '../../components/ResizeHandle';
import { Pill } from '../../components/Pill';
import { useConfirm } from '../../components/ConfirmDialog';
import { useAppSelector } from '../../store/hooks';
import {
  listGitBackends, createGitBackend, deleteGitBackend, testGitBackend, mintGitCredential, listGitRepos,
} from '../../api/bff';
import type { GitBackend, GitBackendType, GitBackendAuth, GitBackendTest, GitCredential, GitRepo } from '../../api/bff';
import { RepoSelect } from '../../components/RepoSelect';
import { timeAgo } from '../../utils';

const BACKEND_TYPES: GitBackendType[] = ['github', 'gitlab', 'forgejo', 'generic'];

// Auth modes available per backend type (first is the default selection).
const AUTH_MODES: Record<GitBackendType, string[]> = {
  github: ['app', 'pat'],
  gitlab: ['token', 'oauth'],
  forgejo: ['token', 'admin'],
  generic: ['basic'],
};

// A field shown in the create form for a given (type, mode). `secret` fields
// render as password inputs; `num` fields submit as integers.
interface AuthField {
  key: keyof GitBackendAuth;
  label: string;
  secret?: boolean;
  num?: boolean;
  area?: boolean;
  optional?: boolean;
}

// Field set per (type:mode), mirroring the broker's accepted auth shapes.
const AUTH_FIELDS: Record<string, AuthField[]> = {
  'github:app': [
    { key: 'app_id', label: 'app id', num: true },
    { key: 'installation_id', label: 'installation id', num: true },
    { key: 'private_key', label: 'private key (PEM)', secret: true, area: true },
  ],
  'github:pat': [
    { key: 'token', label: 'token', secret: true },
    { key: 'username', label: 'username (optional)', optional: true },
  ],
  'gitlab:token': [
    { key: 'token', label: 'token', secret: true },
  ],
  'gitlab:oauth': [
    { key: 'refresh_token', label: 'refresh token', secret: true },
    { key: 'client_id', label: 'client id' },
    { key: 'client_secret', label: 'client secret', secret: true },
  ],
  'forgejo:token': [
    { key: 'token', label: 'token', secret: true },
    { key: 'username', label: 'username' },
  ],
  'forgejo:admin': [
    { key: 'admin_token', label: 'admin token', secret: true },
    { key: 'username', label: 'username' },
  ],
  'generic:basic': [
    { key: 'username', label: 'username' },
    { key: 'password', label: 'password', secret: true },
  ],
};

/** maps a backend's connectivity test verdict to a status tone (ok=green, fail=red). */
function testTone(ok: boolean): 'green' | 'red' {
  return ok ? 'green' : 'red';
}

/** Backend create form: a type selector + per-(type,mode) auth fields, submitting via createGitBackend; calls onCreated with the new backend so the list refreshes. */
function CreateBackend({ onCreated, onCancel }: { onCreated: (b: GitBackend) => void; onCancel: () => void }) {
  const token = useAppSelector(s => s.auth.token)!;
  const [name, setName] = useState('');
  const [type, setType] = useState<GitBackendType>('github');
  const [baseUrl, setBaseUrl] = useState('');
  const [mode, setMode] = useState<string>(AUTH_MODES.github[0]);
  // Auth field values, keyed by field key (always stored as strings in the form).
  const [fields, setFields] = useState<Record<string, string>>({});
  const [creating, setCreating] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const activeFields = AUTH_FIELDS[`${type}:${mode}`] ?? [];

  // Reset the mode (and clear field values) whenever the backend type changes so
  // a stale mode/field set can't leak into the next type's payload.
  const changeType = (t: GitBackendType) => {
    setType(t);
    setMode(AUTH_MODES[t][0]);
    setFields({});
  };

  const changeMode = (m: string) => {
    setMode(m);
    setFields({});
  };

  const setField = (key: string, value: string) => setFields(prev => ({ ...prev, [key]: value }));

  const valid = name.trim() && baseUrl.trim() &&
    activeFields.every(f => f.optional || (fields[f.key] ?? '').trim());

  const handleCreate = async () => {
    if (!valid) return;
    setCreating(true);
    setError(null);
    try {
      const auth: GitBackendAuth = { mode };
      for (const f of activeFields) {
        const raw = (fields[f.key] ?? '').trim();
        if (!raw) continue;
        (auth as unknown as Record<string, unknown>)[f.key] = f.num ? parseInt(raw, 10) : raw;
      }
      const backend = await createGitBackend(token, { name: name.trim(), type, base_url: baseUrl.trim(), auth });
      onCreated(backend);
    } catch (e: unknown) {
      setError((e as Error).message);
    } finally {
      setCreating(false);
    }
  };

  const inputStyle = { width: '100%', background: T.cardHi, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 12, padding: '6px 8px', outline: 'none', boxSizing: 'border-box' as const, marginBottom: 8 };

  return (
    <div style={{ padding: '12px 14px', borderBottom: `1px solid ${T.border}`, background: T.card }}>
      {error && <div style={{ background: T.redSoft, border: `1px solid ${T.red}`, padding: '6px 8px', fontFamily: T.mono, fontSize: 10, color: T.red, marginBottom: 8 }}>{error}</div>}

      <input value={name} onChange={e => setName(e.target.value)} placeholder="backend name" autoFocus style={inputStyle} />

      <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, marginBottom: 4 }}>TYPE</div>
      <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6, marginBottom: 8 }}>
        {BACKEND_TYPES.map(t => (
          <button key={t} onClick={() => changeType(t)}
            style={{ background: type === t ? T.greenSoft : 'transparent', border: `1px solid ${type === t ? T.green : T.border}`, color: type === t ? T.green : T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 9px', cursor: 'pointer' }}>
            {t}
          </button>
        ))}
      </div>

      <input value={baseUrl} onChange={e => setBaseUrl(e.target.value)} placeholder="base url (e.g. https://github.com)" style={inputStyle} />

      <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, marginBottom: 4 }}>AUTH MODE</div>
      <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6, marginBottom: 8 }}>
        {AUTH_MODES[type].map(m => (
          <button key={m} onClick={() => changeMode(m)}
            style={{ background: mode === m ? T.greenSoft : 'transparent', border: `1px solid ${mode === m ? T.green : T.border}`, color: mode === m ? T.green : T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 9px', cursor: 'pointer' }}>
            {m}
          </button>
        ))}
      </div>

      {activeFields.map(f => (
        f.area ? (
          <textarea key={f.key} value={fields[f.key] ?? ''} onChange={e => setField(f.key, e.target.value)} placeholder={f.label} rows={4}
            style={{ ...inputStyle, fontSize: 11, resize: 'vertical' }} />
        ) : (
          <input key={f.key} value={fields[f.key] ?? ''} onChange={e => setField(f.key, e.target.value)} placeholder={f.label}
            type={f.secret ? 'password' : f.num ? 'number' : 'text'} style={inputStyle} />
        )
      ))}

      <div style={{ display: 'flex', gap: 6 }}>
        <button onClick={handleCreate} disabled={!valid || creating}
          style={{ flex: 1, background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 11, fontWeight: 600, padding: '6px 0', cursor: 'pointer', opacity: (!valid || creating) ? 0.6 : 1 }}>
          {creating ? '[ · · · ]' : '[ create ]'}
        </button>
        <button onClick={onCancel} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '6px 8px', cursor: 'pointer' }}>✕</button>
      </div>
    </div>
  );
}

/** Mint-credential tool: a repo URL input that resolves to a clone credential via mintGitCredential; the minted secret is masked by default with a reveal toggle. */
function MintTool() {
  const token = useAppSelector(s => s.auth.token)!;
  const [repoUrl, setRepoUrl] = useState('');
  const [repos, setRepos] = useState<GitRepo[]>([]);
  const [cred, setCred] = useState<GitCredential | null>(null);
  const [minting, setMinting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [reveal, setReveal] = useState(false);

  // Repo list (enumerated across backends + pinned) backs the picker; best effort,
  // so a typed URL still works when the list is empty/unavailable.
  useEffect(() => { listGitRepos(token).then(setRepos).catch(() => setRepos([])); }, [token]);

  const handleMint = async () => {
    if (!repoUrl.trim()) return;
    setMinting(true);
    setError(null);
    setCred(null);
    setReveal(false);
    try {
      setCred(await mintGitCredential(token, repoUrl.trim()));
    } catch (e: unknown) {
      setError((e as Error).message);
    } finally {
      setMinting(false);
    }
  };

  return (
    <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '12px 16px', marginBottom: 16 }}>
      <div style={{ fontFamily: T.mono, fontSize: 11, color: T.faint, marginBottom: 8 }}>MINT CLONE CREDENTIAL</div>
      {error && <div style={{ background: T.redSoft, border: `1px solid ${T.red}`, padding: '6px 10px', fontFamily: T.mono, fontSize: 11, color: T.red, marginBottom: 10 }}>{error}</div>}
      <div style={{ display: 'flex', gap: 8, alignItems: 'flex-start' }}>
        <div style={{ flex: 1 }}>
          <RepoSelect value={repoUrl} onChange={setRepoUrl} repos={repos} placeholder="select or paste a repo url (e.g. https://github.com/org/repo)" />
        </div>
        <button onClick={handleMint} disabled={!repoUrl.trim() || minting}
          style={{ background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 11, fontWeight: 600, padding: '7px 14px', cursor: 'pointer', opacity: (!repoUrl.trim() || minting) ? 0.6 : 1 }}>
          {minting ? '[ · · · ]' : '[ mint ]'}
        </button>
      </div>

      {cred && (
        <div style={{ marginTop: 12, fontFamily: T.mono, fontSize: 11, display: 'grid', gridTemplateColumns: 'auto 1fr', gap: '6px 12px', alignItems: 'baseline' }}>
          <span style={{ color: T.faint }}>backend</span>
          <span style={{ color: T.text }}>{cred.backend} <span style={{ color: T.dim }}>({cred.backend_type})</span></span>
          <span style={{ color: T.faint }}>clone url</span>
          <span style={{ color: T.green, wordBreak: 'break-all' }}>{cred.clone_url}</span>
          <span style={{ color: T.faint }}>type</span>
          <span style={{ color: T.text }}>{cred.type}</span>
          <span style={{ color: T.faint }}>username</span>
          <span style={{ color: T.text }}>{cred.username}</span>
          <span style={{ color: T.faint }}>secret</span>
          <span style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
            <span style={{ color: T.amber, wordBreak: 'break-all' }}>{reveal ? cred.secret : '•••••••••••• (redacted)'}</span>
            <button onClick={() => setReveal(v => !v)}
              style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 9, padding: '2px 7px', cursor: 'pointer', flexShrink: 0 }}>
              {reveal ? 'hide' : 'reveal'}
            </button>
          </span>
          {cred.expires_at && <>
            <span style={{ color: T.faint }}>expires</span>
            <span style={{ color: T.dim }}>{cred.expires_at}</span>
          </>}
        </div>
      )}
    </div>
  );
}

/** Git route: backend list + create form on the left, the selected backend's detail (test/delete) on the right; the empty state hosts the mint-credential tool. */
export function Git() {
  const token = useAppSelector(s => s.auth.token)!;
  const [backends, setBackends] = useState<GitBackend[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<GitBackend | null>(null);
  const [showCreate, setShowCreate] = useState(false);

  const [testing, setTesting] = useState(false);
  const [testResult, setTestResult] = useState<GitBackendTest | null>(null);
  const [testError, setTestError] = useState<string | null>(null);

  const [confirm, confirmEl] = useConfirm();
  const [railW, railHandle] = useResizableWidth('rail.git.main', 260, { min: 200, max: 480 });

  const fetchBackends = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      setBackends(await listGitBackends(token));
    } catch (e: unknown) {
      setError((e as Error).message);
    } finally {
      setLoading(false);
    }
  }, [token]);

  useEffect(() => { fetchBackends(); }, [fetchBackends]);

  const selectBackend = (b: GitBackend) => {
    setSelected(b);
    setTestResult(null);
    setTestError(null);
  };

  const handleCreated = (b: GitBackend) => {
    setBackends(prev => [b, ...prev]);
    setShowCreate(false);
    selectBackend(b);
  };

  const handleTest = async (b: GitBackend) => {
    setTesting(true);
    setTestError(null);
    setTestResult(null);
    try {
      setTestResult(await testGitBackend(token, b.id));
    } catch (e: unknown) {
      setTestError((e as Error).message);
    } finally {
      setTesting(false);
    }
  };

  const handleDelete = async (b: GitBackend) => {
    if (!(await confirm({ message: `Delete git backend ${b.name} (${b.type})? This removes its stored credentials and any clone access through it.` }))) return;
    try {
      await deleteGitBackend(token, b.id);
      setBackends(prev => prev.filter(x => x.id !== b.id));
      if (selected?.id === b.id) setSelected(null);
    } catch (e: unknown) {
      setError((e as Error).message);
    }
  };

  return (
    <div style={{ display: 'flex', height: '100%', overflow: 'hidden' }}>
      {confirmEl}
      {/* Backend list */}
      <div style={{ width: railW, flexShrink: 0, borderRight: `1px solid ${T.border}`, display: 'flex', flexDirection: 'column', background: T.bgAlt }}>
        <div style={{ padding: '14px 14px 10px', borderBottom: `1px solid ${T.border}` }}>
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 4 }}>
            <span style={{ fontFamily: T.mono, fontSize: 13, fontWeight: 700, color: T.textHi }}>git/</span>
            <div style={{ display: 'flex', gap: 6 }}>
              <button onClick={() => setShowCreate(v => !v)}
                style={{ background: showCreate ? T.greenSoft : 'transparent', border: `1px solid ${showCreate ? T.green : T.border}`, color: showCreate ? T.green : T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 7px', cursor: 'pointer' }}>+</button>
              <button onClick={fetchBackends} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 7px', cursor: 'pointer' }}>↻</button>
            </div>
          </div>
          <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint }}>
            {backends.length > 0 && `${backends.length} backend${backends.length !== 1 ? 's' : ''}`}
          </div>
        </div>

        {showCreate && <CreateBackend onCreated={handleCreated} onCancel={() => setShowCreate(false)} />}

        <div style={{ flex: 1, overflow: 'auto' }}>
          {loading ? (
            <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint, animation: 'pulse 1s ease-in-out infinite' }}>→ loading · · ·</div>
          ) : error ? (
            <div style={{ padding: '14px', fontFamily: T.mono, fontSize: 11, color: T.red }}>{error}</div>
          ) : backends.length === 0 ? (
            <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint }}>→ no backends</div>
          ) : backends.map(b => {
            const isActive = selected?.id === b.id;
            return (
              <button key={b.id} onClick={() => selectBackend(b)}
                style={{ width: '100%', textAlign: 'left', padding: '10px 14px', background: isActive ? T.greenSoft : 'transparent', border: 0, borderLeft: `2px solid ${isActive ? T.green : 'transparent'}`, fontFamily: T.mono, cursor: 'pointer', color: T.text, display: 'block', transition: 'background .12s' }}>
                <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
                  <span style={{ fontSize: 12, fontWeight: 600, color: isActive ? T.textHi : T.text }}>{b.name}</span>
                  <span style={{ fontSize: 9, color: T.blue, border: `1px solid ${T.blue}`, padding: '0 4px' }}>{b.type}</span>
                </div>
                <div style={{ fontSize: 10, color: T.faint, marginTop: 2 }}>{b.host}</div>
                <div style={{ fontSize: 10, color: T.dim, marginTop: 1 }}>auth: {b.auth_mode}</div>
              </button>
            );
          })}
        </div>
      </div>
      {railHandle}

      {/* Detail panel */}
      <div style={{ flex: 1, display: 'flex', flexDirection: 'column', overflow: 'hidden' }}>
        {!selected ? (
          <div style={{ flex: 1, padding: '20px' }}>
            <MintTool />
            <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', height: 200 }}>
              <div style={{ fontFamily: T.mono, fontSize: 12, color: T.faint }}>→ select a backend</div>
            </div>
          </div>
        ) : (
          <>
            {/* Backend header */}
            <div style={{ padding: '12px 20px', borderBottom: `1px solid ${T.border}`, background: T.bgAlt, flexShrink: 0, display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between' }}>
              <div>
                <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                  <span style={{ fontFamily: T.mono, fontSize: 15, fontWeight: 700, color: T.textHi }}>{selected.name}</span>
                  <Pill tone="dim">{selected.type}</Pill>
                </div>
                <div style={{ fontFamily: T.mono, fontSize: 11, color: T.dim, marginTop: 2 }}>{selected.base_url}</div>
                <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, marginTop: 2 }}>host {selected.host} · auth {selected.auth_mode}</div>
                {selected.updated_at && <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, marginTop: 2 }}>updated {timeAgo(selected.updated_at)} ago</div>}
              </div>
              <div style={{ display: 'flex', gap: 8 }}>
                <button onClick={() => handleTest(selected)} disabled={testing}
                  style={{ background: T.greenSoft, border: `1px solid ${T.green}`, color: T.green, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer', opacity: testing ? 0.6 : 1 }}>
                  {testing ? '[ · · · ]' : '[ test ]'}
                </button>
                <button onClick={() => handleDelete(selected)}
                  style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}
                  onMouseEnter={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.red; (e.currentTarget as HTMLButtonElement).style.color = T.red; }}
                  onMouseLeave={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.border; (e.currentTarget as HTMLButtonElement).style.color = T.dim; }}>
                  [ delete ]
                </button>
              </div>
            </div>

            <div style={{ flex: 1, overflow: 'auto', padding: '16px 20px' }}>
              {testError && <div style={{ background: T.redSoft, border: `1px solid ${T.red}`, padding: '8px 12px', fontFamily: T.mono, fontSize: 11, color: T.red, marginBottom: 12 }}>{testError}</div>}
              {testResult && (
                <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '12px 16px', marginBottom: 16 }}>
                  <div style={{ display: 'flex', alignItems: 'center', gap: 10, marginBottom: 8 }}>
                    <Pill tone={testTone(testResult.ok)}>{testResult.ok ? 'ok' : 'failed'}</Pill>
                    <span style={{ fontFamily: T.mono, fontSize: 11, color: T.dim }}>{testResult.backend_type} · {testResult.auth_mode}</span>
                  </div>
                  {testResult.expires_at && <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint }}>credential expires {testResult.expires_at}</div>}
                </div>
              )}

              {/* Mint a clone credential through this (or any matching) backend. */}
              <MintTool />
            </div>
          </>
        )}
      </div>
    </div>
  );
}

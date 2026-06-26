/**
 * `/app/settings` page — the account/profile screen. Edits identity (email, username,
 * optional name, password) via the `saveUser` thunk, shows read-only org/team/role
 * membership, and hosts preference cards for theme selection and session inspect/revoke
 * (all session calls go through the BFF client).
 */
import { useState } from 'react';
import type { ReactNode } from 'react';
import { T, THEMES, applyTheme, getStoredTheme } from '../../theme';
import { useConfirm } from '../../components/ConfirmDialog';
import { useAppDispatch, useAppSelector } from '../../store/hooks';
import { saveUser, logout } from '../../store/authSlice';
import { getSession, deleteSession } from '../../api/bff';
import type { Session } from '../../api/bff';
import { decodeJwtPayload } from '../../utils';

/** Pulls the session id out of the JWT payload, trying session_id/sid/jti in order; returns '' if none is a string. */
function decodeSessionId(token: string): string {
  const p = decodeJwtPayload(token);
  const id = p?.session_id ?? p?.sid ?? p?.jti;
  return typeof id === 'string' ? id : '';
}

/** Theme picker — applies live and persists to localStorage. */
function ThemeCard() {
  const [theme, setTheme] = useState(getStoredTheme());
  const choose = (name: string) => { applyTheme(name); setTheme(name); };
  return (
    <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '20px 22px', marginBottom: 20 }}>
      <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 14 }}>THEME</div>
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(150px, 1fr))', gap: 10 }}>
        {THEMES.map(t => {
          const active = theme === t.name;
          return (
            <button key={t.name} type="button" onClick={() => choose(t.name)}
              style={{ display: 'flex', alignItems: 'center', gap: 10, background: active ? T.greenSoft : 'transparent', border: `1px solid ${active ? T.green : T.border}`, padding: '8px 12px', cursor: 'pointer', textAlign: 'left' }}>
              <span style={{ width: 14, height: 14, borderRadius: 3, background: t.bg, border: `2px solid ${t.accent}`, flexShrink: 0, display: 'inline-block' }} />
              <span style={{ fontFamily: T.mono, fontSize: 12, color: active ? T.textHi : T.text }}>{t.label}</span>
              {active && <span style={{ marginLeft: 'auto', color: T.green, fontFamily: T.mono, fontSize: 11 }}>✓</span>}
            </button>
          );
        })}
      </div>
    </div>
  );
}

/**
 * Inspect or revoke a session by id (current session id is prefilled from the JWT).
 * Inspect/revoke hit the BFF; revoking your own current session dispatches logout so
 * the UI signs out immediately.
 */
function SessionsCard({ token }: { token: string }) {
  const dispatch = useAppDispatch();
  const currentSessionId = decodeSessionId(token);
  const [id, setId] = useState(currentSessionId);
  const [session, setSession] = useState<Session | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [msg, setMsg] = useState('');
  const [confirm, confirmEl] = useConfirm();

  const inspect = async () => {
    if (!id.trim()) return;
    setBusy(true); setError(''); setMsg(''); setSession(null);
    try { setSession(await getSession(token, id.trim())); }
    catch (e) { setError((e as Error).message); }
    finally { setBusy(false); }
  };
  const revoke = async () => {
    if (!id.trim()) return;
    const own = id.trim() === currentSessionId;
    if (!(await confirm({ message: `Revoke session ${id.trim()}?${own ? ' This is your current session — you will be signed out immediately.' : ''}`, confirmLabel: 'revoke' }))) return;
    setBusy(true); setError(''); setMsg('');
    try {
      await deleteSession(token, id.trim());
      setSession(null); setMsg('session revoked');
      // Revoking your own current session invalidates this token server-side;
      // clear local auth so the UI signs out immediately (as the hint promises)
      // instead of operating with a dead token until the next request 401s.
      if (id.trim() === currentSessionId) dispatch(logout());
    }
    catch (e) { setError((e as Error).message); }
    finally { setBusy(false); }
  };

  return (
    <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '20px 22px', marginBottom: 20 }}>
      {confirmEl}
      <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 6 }}>SESSIONS</div>
      <div style={{ fontFamily: T.mono, fontSize: 11, color: T.faint, marginBottom: 12 }}>inspect or revoke a session by id · revoking your current session signs you out</div>
      {error && <div style={{ background: T.redSoft, border: `1px solid ${T.red}`, padding: '8px 12px', fontFamily: T.mono, fontSize: 11, color: T.red, marginBottom: 12 }}>ERR · {error}</div>}
      {msg && <div style={{ background: T.greenSoft, border: `1px solid ${T.green}`, padding: '8px 12px', fontFamily: T.mono, fontSize: 11, color: T.green, marginBottom: 12 }}>→ {msg}</div>}
      <div style={{ display: 'flex', gap: 8, marginBottom: session ? 12 : 0 }}>
        <input value={id} onChange={e => setId(e.target.value)} placeholder="session id"
          style={{ flex: 1, background: T.cardHi, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 12, padding: '8px 12px', outline: 'none' }} />
        <button type="button" onClick={inspect} disabled={!id.trim() || busy}
          style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '8px 14px', cursor: 'pointer', opacity: (!id.trim() || busy) ? 0.6 : 1 }}>
          [ inspect ]
        </button>
        <button type="button" onClick={revoke} disabled={!id.trim() || busy}
          style={{ background: 'transparent', border: `1px solid ${T.red}`, color: T.red, fontFamily: T.mono, fontSize: 11, padding: '8px 14px', cursor: 'pointer', opacity: (!id.trim() || busy) ? 0.6 : 1 }}>
          [ revoke ]
        </button>
      </div>
      {session && (
        <div style={{ background: T.cardHi, border: `1px solid ${T.border}`, padding: '12px 14px', fontFamily: T.mono, fontSize: 12 }}>
          {([
            ['session_id', session.session_id],
            ['user_id', session.user_id],
            ['created', session.created_at],
            ['last_seen', session.last_seen ?? '—'],
            ['ip', session.ip ?? '—'],
            ['user_agent', session.user_agent ?? '—'],
          ] as [string, string][]).map(([k, v]) => (
            <div key={k} style={{ display: 'flex', gap: 16, padding: '3px 0' }}>
              <span style={{ color: T.faint, width: 90, flexShrink: 0 }}>{k}</span>
              <span style={{ color: T.text, wordBreak: 'break-all' }}>{v}</span>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

interface FieldProps {
  name: string;
  label: string;
  value: string;
  onChange: (v: string) => void;
  type?: string;
  placeholder?: string;
  rightSlot?: ReactNode;
  focused: string | null;
  onFocus: (name: string) => void;
  onBlur: () => void;
}

/** Labeled prompt-style text input with focus highlight and an optional right slot (e.g. show/hide toggle); a controlled field used across the settings form. */
function Field({ name, label, value, onChange, type = 'text', placeholder, rightSlot, focused, onFocus, onBlur }: FieldProps) {
  const active = focused === name;
  return (
    <div style={{ marginBottom: 14 }}>
      <div style={{ fontSize: 11, color: active ? T.green : T.faint, fontFamily: T.mono, letterSpacing: 0.5, marginBottom: 5, textTransform: 'uppercase' }}>{label}</div>
      <div style={{ display: 'flex', alignItems: 'center', background: T.cardHi, border: `1px solid ${active ? T.green : T.border}`, boxShadow: active ? `0 0 0 3px ${T.greenSoft}` : 'none', transition: 'all .15s', padding: '8px 12px' }}>
        <span style={{ color: T.green, fontFamily: T.mono, fontSize: 13, marginRight: 8, userSelect: 'none' }}>›</span>
        <input type={type} value={value} onChange={(e) => onChange(e.target.value)}
          onFocus={() => onFocus(name)} onBlur={onBlur}
          placeholder={placeholder} style={{ flex: 1, background: 'transparent', border: 0, outline: 'none', color: T.text, fontFamily: T.mono, fontSize: 13, padding: 0 }} />
        {rightSlot}
      </div>
    </div>
  );
}

/** Settings page component: renders the identity/name/password form (submits via saveUser), read-only membership info, and the theme + sessions preference cards. */
export function Settings() {
  const dispatch = useAppDispatch();
  const { user, token, userId } = useAppSelector(s => s.auth);

  const [email, setEmail] = useState(user?.email ?? '');
  const [username, setUsername] = useState(user?.username ?? '');
  const [firstname, setFirstname] = useState(user?.firstname ?? '');
  const [lastname, setLastname] = useState(user?.lastname ?? '');
  const [password, setPassword] = useState('');
  const [showPw, setShowPw] = useState(false);
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);
  const [error, setError] = useState('');
  const [focused, setFocused] = useState<string | null>(null);

  if (!user || !token || !userId) return null;

  const handleSave = async (e: React.FormEvent) => {
    e.preventDefault();
    setSaving(true);
    setError('');
    setSaved(false);
    const result = await dispatch(saveUser({
      token,
      userId,
      patch: {
        email, username,
        ...(firstname && { firstname }),
        ...(lastname && { lastname }),
        ...(password && { password }),
      },
    }));
    setSaving(false);
    if (saveUser.fulfilled.match(result)) {
      setSaved(true);
      setPassword('');
      setTimeout(() => setSaved(false), 3000);
    } else {
      setError((result.payload as string) ?? 'update failed');
    }
  };

  return (
    <div style={{ padding: '28px 32px', maxWidth: 560, overflow: 'auto' }}>
      {/* Header */}
      <div style={{ marginBottom: 28 }}>
        <div style={{ fontFamily: T.mono, fontSize: 11, color: T.green, marginBottom: 6, letterSpacing: 0.5 }}>// settings</div>
        <h1 style={{ fontFamily: T.mono, fontSize: 22, fontWeight: 700, color: T.textHi, margin: 0, letterSpacing: -0.5 }}>profile/</h1>
        <div style={{ fontFamily: T.mono, fontSize: 12, color: T.faint, marginTop: 6 }}>user_id: {user.user_id}</div>
      </div>

      {saved && (
        <div style={{ background: T.greenSoft, border: `1px solid ${T.green}`, padding: '10px 14px', marginBottom: 16, fontFamily: T.mono, fontSize: 12, color: T.green }}>
          → profile updated successfully
        </div>
      )}
      {error && (
        <div style={{ background: T.redSoft, border: `1px solid ${T.red}`, padding: '10px 14px', marginBottom: 16, fontFamily: T.mono, fontSize: 12, color: T.red }}>
          ERR · {error}
        </div>
      )}

      <form onSubmit={handleSave}>
        <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '20px 22px', marginBottom: 20 }}>
          <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 14 }}>IDENTITY</div>
          <Field name="email" label="email" value={email} onChange={setEmail} type="email" placeholder="you@company.dev" focused={focused} onFocus={setFocused} onBlur={() => setFocused(null)} />
          <Field name="username" label="username" value={username} onChange={(v) => setUsername(v.toLowerCase().replace(/[^a-z0-9_-]/g, ''))} placeholder="janedoe" focused={focused} onFocus={setFocused} onBlur={() => setFocused(null)} />
        </div>

        <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '20px 22px', marginBottom: 20 }}>
          <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 14 }}>NAME <span style={{ color: T.faint, fontWeight: 400 }}>· optional</span></div>
          <Field name="firstname" label="first name" value={firstname} onChange={setFirstname} placeholder="Jane" focused={focused} onFocus={setFocused} onBlur={() => setFocused(null)} />
          <Field name="lastname" label="last name" value={lastname} onChange={setLastname} placeholder="Doe" focused={focused} onFocus={setFocused} onBlur={() => setFocused(null)} />
        </div>

        <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '20px 22px', marginBottom: 20 }}>
          <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 6 }}>PASSWORD</div>
          <div style={{ fontFamily: T.mono, fontSize: 11, color: T.faint, marginBottom: 12 }}>leave blank to keep current password</div>
          <Field name="password" label="new password" value={password} onChange={setPassword} type={showPw ? 'text' : 'password'} placeholder="••••••••" focused={focused} onFocus={setFocused} onBlur={() => setFocused(null)}
            rightSlot={
              <button type="button" onClick={() => setShowPw((s) => !s)} tabIndex={-1}
                style={{ background: 'transparent', border: 0, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '2px 6px', cursor: 'pointer', letterSpacing: 0.4 }}>
                {showPw ? '--hide' : '--show'}
              </button>
            }
          />
        </div>

        {/* Org & team info (read-only) */}
        <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '20px 22px', marginBottom: 24 }}>
          <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 14 }}>MEMBERSHIP <span style={{ color: T.faint }}>· read-only</span></div>
          {[
            ['org_id', user.org_id ?? '—'],
            ['team_id', user.team_id ?? '—'],
            ['role_id', user.role_id ?? '—'],
          ].map(([k, v]) => (
            <div key={k} style={{ display: 'flex', gap: 16, fontFamily: T.mono, fontSize: 12, padding: '5px 0', borderBottom: `1px solid ${T.border}` }}>
              <span style={{ color: T.faint, width: 80, flexShrink: 0 }}>{k}</span>
              <span style={{ color: v === '—' ? T.faint : T.text }}>{v}</span>
            </div>
          ))}
          <div style={{ fontFamily: T.mono, fontSize: 11, color: T.faint, marginTop: 10 }}>
            change org/team via invite flow · <span style={{ color: T.dim }}>POST /orgs/{'{id}'}/invites</span>
          </div>
        </div>

        <button type="submit" disabled={saving} style={{ width: '100%', background: saving ? 'transparent' : T.green, color: saving ? T.faint : T.bg, border: `1px solid ${saving ? T.border : T.green}`, fontFamily: T.mono, fontSize: 13, fontWeight: 600, padding: '10px 14px', cursor: saving ? 'not-allowed' : 'pointer', letterSpacing: 0.5, transition: 'all .15s' }}>
          {saving ? '[ saving · · · ]' : '[ ↵ save changes ]'}
        </button>
      </form>

      <div style={{ marginTop: 28, marginBottom: 14 }}>
        <div style={{ fontFamily: T.mono, fontSize: 11, color: T.green, marginBottom: 6, letterSpacing: 0.5 }}>// preferences</div>
      </div>
      <ThemeCard />
      <SessionsCard token={token} />
    </div>
  );
}

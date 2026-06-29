/**
 * `/signup` page — terminal-styled registration form (email, username, password,
 * terms) that submits via the `signupAndLogin` thunk through the BFF, then lands the
 * new user in `/app`. If the account is created but auto-login fails it redirects to
 * /login instead. Used for self-serve registration on an already-initialized instance.
 */
import { useState } from 'react';
import { Link, useNavigate } from 'react-router-dom';
import { T } from '../theme';
import { Logo } from '../components/Logo';
import { PromptField, StrengthBar } from '../components/AuthFields';
import { useAppDispatch } from '../store/hooks';
import { signupAndLogin } from '../store/authSlice';
import { passwordScore } from '../utils';

/** Signup page component: validates email/username/password-strength/terms, then dispatches signupAndLogin and routes to /app (or /login when auto-login fails). */
export function Signup() {
  const navigate = useNavigate();
  const dispatch = useAppDispatch();

  const [email, setEmail] = useState('');
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [confirm, setConfirm] = useState('');
  const [showPw, setShowPw] = useState(false);
  const [terms, setTerms] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState('');

  const { score, checks } = passwordScore(password);
  const passwordsMatch = password === confirm;
  const formValid = /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email)
    && /^[a-zA-Z0-9_-]{1,64}$/.test(username)
    && score >= 3
    && passwordsMatch
    && terms
    && !submitting;

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!formValid) return;
    setSubmitting(true);
    setError('');

    const result = await dispatch(signupAndLogin({ email, username, password }));

    if (signupAndLogin.fulfilled.match(result)) {
      navigate('/app');
      return;
    }

    const payload = result.payload as { status?: number; message: string } | undefined;
    if (payload?.status === -1) {
      // Account created but auto-login failed
      navigate('/login', { state: { message: 'Account created. Please log in.' } });
      return;
    }
    if (payload?.status === 409) setError('ERR · email or username already taken');
    else setError(`ERR · ${payload?.message ?? 'signup failed'}`);
    setSubmitting(false);
  };

  return (
    <div style={{ width: '100%', minHeight: '100vh', background: T.bg, fontFamily: T.mono, color: T.text, position: 'relative', overflow: 'hidden', display: 'flex', flexDirection: 'column' }}>
      <div style={{ position: 'absolute', inset: 0, backgroundImage: `linear-gradient(180deg, ${T.greenSoft} 0%, transparent 30%)`, pointerEvents: 'none' }} />

      {/* Status bar */}
      <div style={{ position: 'relative', height: 28, display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '0 16px', fontSize: 11, color: T.faint, letterSpacing: 0.3, borderBottom: `1px solid ${T.border}`, background: 'rgba(0,0,0,0.3)' }}>
        <span><span style={{ color: T.green }}>●</span>&nbsp;&nbsp;armory-prod-us-east · TLS</span>
        <span>codearmory v2.4.1 · region: iad-1</span>
      </div>

      <div style={{ flex: 1, display: 'flex', alignItems: 'center', justifyContent: 'center', padding: '40px 24px' }}>
        <div style={{ width: 540, background: T.card, border: `1px solid ${T.borderHi}`, boxShadow: '0 24px 60px rgba(0,0,0,0.6)' }}>
          {/* Window chrome */}
          <div style={{ display: 'flex', alignItems: 'center', gap: 8, padding: '10px 14px', borderBottom: `1px solid ${T.border}`, background: T.cardHi }}>
            <span style={{ display: 'flex', gap: 6 }}>
              <span style={{ width: 10, height: 10, borderRadius: 5, background: '#3a3530' }} />
              <span style={{ width: 10, height: 10, borderRadius: 5, background: '#3a3530' }} />
              <span style={{ width: 10, height: 10, borderRadius: 5, background: T.green }} />
            </span>
            <span style={{ flex: 1, textAlign: 'center', fontSize: 11.5, color: T.dim, letterSpacing: 0.4 }}>codearmory ~ signup.sh</span>
            <span style={{ fontSize: 11, color: T.faint }}>72×24</span>
          </div>

          <div style={{ padding: '24px 28px 22px' }}>
            <div style={{ display: 'flex', alignItems: 'center', gap: 10, marginBottom: 16 }}>
              <Logo size={18} />
              <pre style={{ margin: 0, color: T.green, fontSize: 11, lineHeight: 1.25 }}>{`c o d e · a r m o r y · signup`}</pre>
            </div>
            <div style={{ fontSize: 12, color: T.dim, marginBottom: 22, lineHeight: 1.6 }}>
              <span style={{ color: T.green }}>#</span> register a new identity — opentofu state, ci/cd, on one host.<br />
              <span style={{ color: T.green }}>#</span> free to self-host · up to 3 collaborators on day one.
            </div>

            {error && (
              <div style={{ background: T.redSoft, border: `1px solid ${T.red}`, padding: '8px 12px', marginBottom: 16, fontFamily: T.mono, fontSize: 12, color: T.red }}>{error}</div>
            )}

            <form onSubmit={submit}>
              <PromptField prompt="email --primary" value={email} onChange={setEmail} type="email" placeholder="you@company.dev" hint="we'll log you in automatically after signup." />
              <PromptField prompt="username --handle" value={username} onChange={(v) => setUsername(v.toLowerCase().replace(/[^a-z0-9_-]/g, ''))} placeholder="janedoe" hint={`/^[a-z0-9_-]{1,64}$/ · your workspace path`} />
              <PromptField prompt="password --new" value={password} onChange={setPassword} type={showPw ? 'text' : 'password'} placeholder="••••••••••••"
                rightSlot={
                  <button type="button" tabIndex={-1} onClick={() => setShowPw((s) => !s)}
                    style={{ background: 'transparent', border: 0, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '2px 6px', cursor: 'pointer', letterSpacing: 0.4 }}>
                    {showPw ? '--hide' : '--show'}
                  </button>
                }
              />
              <StrengthBar score={score} checks={checks} />

              <PromptField prompt="password --confirm" value={confirm} onChange={setConfirm} type={showPw ? 'text' : 'password'} placeholder="••••••••••••"
                hint={confirm && passwordsMatch ? '✓ passwords match' : 're-enter the password above to confirm.'} />
              {confirm && !passwordsMatch && (
                <div style={{ fontSize: 11, color: T.red, fontFamily: T.mono, marginTop: -12, marginBottom: 16, letterSpacing: 0.2 }}>✗ passwords do not match</div>
              )}

              <label style={{ display: 'flex', alignItems: 'flex-start', gap: 10, marginBottom: 18, cursor: 'pointer', fontSize: 12, color: T.dim, lineHeight: 1.5 }} onClick={() => setTerms((t) => !t)}>
                <span style={{ color: T.green, fontFamily: T.mono, marginTop: 0 }}>{terms ? '[x]' : '[ ]'}</span>
                <span>
                  I accept the <a href="#" style={{ color: T.text, borderBottom: `1px solid ${T.borderHi}`, textDecoration: 'none' }}>terms</a>{' '}
                  and <a href="#" style={{ color: T.text, borderBottom: `1px solid ${T.borderHi}`, textDecoration: 'none' }}>privacy policy</a>.
                </span>
              </label>

              <button type="submit" disabled={!formValid}
                style={{ width: '100%', padding: '11px 14px', background: formValid ? T.green : 'transparent', color: formValid ? T.bg : T.faint, border: `1px solid ${formValid ? T.green : T.border}`, fontFamily: T.mono, fontSize: 13, fontWeight: 600, letterSpacing: 0.5, cursor: formValid ? 'pointer' : 'not-allowed', transition: 'all .15s' }}>
                {submitting ? '[ initializing · · · ]' : formValid ? '[ ↵ ./create-account ]' : '[ complete required fields ]'}
              </button>

              <div style={{ marginTop: 16, fontSize: 11, color: T.faint, textAlign: 'center', letterSpacing: 0.3 }}>
                already enlisted?{' '}
                <Link to="/login" style={{ color: T.dim, textDecoration: 'none', borderBottom: `1px solid ${T.border}` }}>./login</Link>
              </div>
            </form>
          </div>

          <div style={{ borderTop: `1px solid ${T.border}`, padding: '8px 14px', display: 'flex', justifyContent: 'space-between', fontSize: 10.5, color: T.faint, letterSpacing: 0.3, background: T.cardHi }}>
            <span>tab · navigate</span>
            <span>esc · cancel</span>
            <span>⏎ submit</span>
          </div>
        </div>
      </div>
    </div>
  );
}

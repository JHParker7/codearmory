import { useState } from 'react';
import { Link, useNavigate, useLocation } from 'react-router-dom';
import { T } from '../theme';
import { Logo } from '../components/Logo';
import { useAppDispatch } from '../store/hooks';
import { loginAndFetch } from '../store/authSlice';

export function Login() {
  const navigate = useNavigate();
  const location = useLocation();
  const dispatch = useAppDispatch();
  const locationMessage = (location.state as { message?: string } | null)?.message ?? '';

  const [email, setEmail] = useState('');
  const [password, setPassword] = useState('');
  const [showPw, setShowPw] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState('');
  const [focusedField, setFocusedField] = useState<string | null>(null);

  const formValid = email.trim().length > 0 && password.length > 0 && !submitting;

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!formValid) return;
    setSubmitting(true);
    setError('');
    const result = await dispatch(loginAndFetch({ email, password }));
    if (loginAndFetch.fulfilled.match(result)) {
      navigate('/app');
    } else {
      const payload = result.payload as { status?: number; message: string } | undefined;
      if (payload?.status === 401) setError('ERR · invalid credentials');
      else setError(`ERR · ${payload?.message ?? 'login failed'}`);
      setSubmitting(false);
    }
  };

  const fieldStyle = (name: string) => ({
    display: 'flex', alignItems: 'center',
    background: T.cardHi,
    border: `1px solid ${focusedField === name ? T.green : T.border}`,
    boxShadow: focusedField === name ? `0 0 0 3px ${T.greenSoft}` : 'none',
    transition: 'border-color .15s, box-shadow .15s',
    padding: '8px 12px',
  });

  return (
    <div style={{ width: '100%', minHeight: '100vh', background: T.bg, fontFamily: T.mono, color: T.text, display: 'flex', flexDirection: 'column' }}>
      <div style={{ position: 'absolute', inset: 0, backgroundImage: `linear-gradient(180deg, ${T.greenSoft} 0%, transparent 30%)`, pointerEvents: 'none' }} />

      {/* Status bar */}
      <div style={{ position: 'relative', height: 28, display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '0 16px', fontSize: 11, color: T.faint, letterSpacing: 0.3, borderBottom: `1px solid ${T.border}`, background: 'rgba(0,0,0,0.3)' }}>
        <span><span style={{ color: T.green }}>●</span>&nbsp;&nbsp;armory-prod-us-east · TLS</span>
        <span>codearmory v2.4.1 · region: iad-1</span>
      </div>

      <div style={{ flex: 1, display: 'flex', alignItems: 'center', justifyContent: 'center', padding: '40px 24px', position: 'relative' }}>
        <div style={{ width: 480, background: T.card, border: `1px solid ${T.borderHi}`, boxShadow: '0 24px 60px rgba(0,0,0,0.6)' }}>
          {/* Window chrome */}
          <div style={{ display: 'flex', alignItems: 'center', gap: 8, padding: '10px 14px', borderBottom: `1px solid ${T.border}`, background: T.cardHi }}>
            <span style={{ display: 'flex', gap: 6 }}>
              <span style={{ width: 10, height: 10, borderRadius: 5, background: '#3a3530' }} />
              <span style={{ width: 10, height: 10, borderRadius: 5, background: '#3a3530' }} />
              <span style={{ width: 10, height: 10, borderRadius: 5, background: T.green }} />
            </span>
            <span style={{ flex: 1, textAlign: 'center', fontSize: 11.5, color: T.dim, letterSpacing: 0.4 }}>codearmory ~ login.sh</span>
            <span style={{ fontSize: 11, color: T.faint }}>72×24</span>
          </div>

          <div style={{ padding: '24px 28px 22px' }}>
            <div style={{ display: 'flex', alignItems: 'center', gap: 10, marginBottom: 16 }}>
              <Logo size={18} />
              <pre style={{ margin: 0, color: T.green, fontSize: 11, lineHeight: 1.25 }}>{`c o d e · a r m o r y · login`}</pre>
            </div>
            <div style={{ fontSize: 12, color: T.dim, marginBottom: 22, lineHeight: 1.6 }}>
              <span style={{ color: T.green }}>#</span> authenticate to your codearmory instance.<br />
              <span style={{ color: T.green }}>#</span> session expires in 24h · ES256 JWT.
            </div>

            {locationMessage && (
              <div style={{ background: T.greenSoft, border: `1px solid ${T.green}`, padding: '8px 12px', marginBottom: 16, fontFamily: T.mono, fontSize: 12, color: T.green }}>→ {locationMessage}</div>
            )}
            {error && (
              <div style={{ background: T.redSoft, border: `1px solid ${T.red}`, padding: '8px 12px', marginBottom: 16, fontFamily: T.mono, fontSize: 12, color: T.red }}>{error}</div>
            )}

            <form onSubmit={submit}>
              {/* Email */}
              <div style={{ marginBottom: 16 }}>
                <div style={{ fontSize: 12, color: focusedField === 'email' ? T.green : T.dim, marginBottom: 5, letterSpacing: 0.3 }}>
                  <span style={{ color: T.green }}>$</span> email --primary
                </div>
                <div style={fieldStyle('email')}>
                  <span style={{ color: T.green, fontFamily: T.mono, fontSize: 13.5, marginRight: 8, userSelect: 'none' }}>›</span>
                  <input type="email" value={email} onChange={(e) => setEmail(e.target.value)}
                    onFocus={() => setFocusedField('email')} onBlur={() => setFocusedField(null)}
                    placeholder="you@company.dev" autoComplete="email"
                    style={{ flex: 1, background: 'transparent', border: 0, outline: 'none', color: T.text, fontFamily: T.mono, fontSize: 13.5, letterSpacing: 0.2, padding: 0 }} />
                </div>
              </div>

              {/* Password */}
              <div style={{ marginBottom: 20 }}>
                <div style={{ fontSize: 12, color: focusedField === 'pw' ? T.green : T.dim, marginBottom: 5, letterSpacing: 0.3 }}>
                  <span style={{ color: T.green }}>$</span> password --verify
                </div>
                <div style={fieldStyle('pw')}>
                  <span style={{ color: T.green, fontFamily: T.mono, fontSize: 13.5, marginRight: 8, userSelect: 'none' }}>›</span>
                  <input type={showPw ? 'text' : 'password'} value={password} onChange={(e) => setPassword(e.target.value)}
                    onFocus={() => setFocusedField('pw')} onBlur={() => setFocusedField(null)}
                    placeholder="••••••••••••" autoComplete="current-password"
                    style={{ flex: 1, background: 'transparent', border: 0, outline: 'none', color: T.text, fontFamily: T.mono, fontSize: 13.5, letterSpacing: 0.2, padding: 0 }} />
                  <button type="button" tabIndex={-1} onClick={() => setShowPw((s) => !s)}
                    style={{ background: 'transparent', border: 0, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '2px 6px', cursor: 'pointer', letterSpacing: 0.4 }}>
                    {showPw ? '--hide' : '--show'}
                  </button>
                </div>
              </div>

              <button type="submit" disabled={!formValid}
                style={{ width: '100%', padding: '11px 14px', background: formValid ? T.green : 'transparent', color: formValid ? T.bg : T.faint, border: `1px solid ${formValid ? T.green : T.border}`, fontFamily: T.mono, fontSize: 13, fontWeight: 600, letterSpacing: 0.5, cursor: formValid ? 'pointer' : 'not-allowed', transition: 'all .15s' }}>
                {submitting ? '[ authenticating · · · ]' : formValid ? '[ ↵ ./login ]' : '[ enter credentials ]'}
              </button>

              <div style={{ marginTop: 16, fontSize: 11, color: T.faint, textAlign: 'center', letterSpacing: 0.3 }}>
                no account?{' '}
                <Link to="/signup" style={{ color: T.dim, textDecoration: 'none', borderBottom: `1px solid ${T.border}` }}>./signup</Link>
                {' '}· enterprise / SSO?{' '}
                <a href="#" style={{ color: T.dim, textDecoration: 'none', borderBottom: `1px solid ${T.border}` }}>contact</a>
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

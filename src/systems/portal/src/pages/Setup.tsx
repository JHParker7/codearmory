/**
 * `/setup` page — first-run initialization. Creates the very first account on an
 * empty instance (which Gatekeeper grants the bootstrap admin role), submits via the
 * `signupAndLogin` thunk through the BFF, marks the instance initialized, and lands
 * the new admin in `builder/`. Also shows an orientation panel of the always-on core
 * services the admin will govern.
 */
import { useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { T } from '../theme';
import { Logo } from '../components/Logo';
import { PromptField, StrengthBar } from '../components/AuthFields';
import { useAppDispatch } from '../store/hooks';
import { signupAndLogin } from '../store/authSlice';
import { markInitialized } from '../store/setupSlice';
import { passwordScore } from '../utils';

/**
 * The control-plane core that ships with every codearmory deployment and is always
 * on (it cannot be toggled off). Shown on first run so the operator knows what their
 * administrator account will govern. Every other capability — forge, workflows,
 * blueprints, and the rest — is deployed and registered at runtime via builder/, so
 * it is deliberately not listed here.
 */
const CORE_SERVICES: { name: string; blurb: string }[] = [
  { name: 'gatekeeper/', blurb: 'identity, RBAC, orgs & teams, sessions' },
  { name: 'conductor/', blurb: 'API gateway · the single entry point' },
  { name: 'registry/', blurb: 'service discovery · conductor routing table' },
  { name: 'builder/', blurb: 'service control plane · deploys & enables the rest' },
];

/** Setup page component: renders the admin-account form + core-services panel and, on submit, creates the bootstrap admin then routes to /app/builder. */
export function Setup() {
  const navigate = useNavigate();
  const dispatch = useAppDispatch();

  const [email, setEmail] = useState('');
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [confirm, setConfirm] = useState('');
  const [showPw, setShowPw] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState('');

  const { score, checks } = passwordScore(password);
  const passwordsMatch = password === confirm;
  const formValid = /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email)
    && /^[a-zA-Z0-9_-]{1,64}$/.test(username)
    && score >= 3
    && passwordsMatch
    && !submitting;

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!formValid) return;
    setSubmitting(true);
    setError('');

    // The very first account created on an empty instance is granted the bootstrap
    // admin role by Gatekeeper (wildcard permission), so this both completes setup
    // and logs the operator in as administrator.
    const result = await dispatch(signupAndLogin({ email, username, password }));

    if (signupAndLogin.fulfilled.match(result)) {
      dispatch(markInitialized());
      // Land in builder/ — the always-available control-plane core where the admin
      // enables the rest of the platform. A fresh instance has no other service
      // deployed yet, so any module page would just show a "not available" wall.
      navigate('/app/builder');
      return;
    }

    const payload = result.payload as { status?: number; message: string } | undefined;
    if (payload?.status === -1) {
      // Admin account created but auto-login failed — setup is done; go log in.
      dispatch(markInitialized());
      navigate('/login', { state: { message: 'Admin account created. Please log in.' } });
      return;
    }
    if (payload?.status === 409) setError('ERR · email or username already taken');
    else setError(`ERR · ${payload?.message ?? 'setup failed'}`);
    setSubmitting(false);
  };

  return (
    <div style={{ width: '100%', minHeight: '100vh', background: T.bg, fontFamily: T.mono, color: T.text, position: 'relative', overflow: 'hidden', display: 'flex', flexDirection: 'column' }}>
      <div style={{ position: 'absolute', inset: 0, backgroundImage: `linear-gradient(180deg, ${T.greenSoft} 0%, transparent 30%)`, pointerEvents: 'none' }} />

      {/* Status bar */}
      <div style={{ position: 'relative', height: 28, display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '0 16px', fontSize: 11, color: T.faint, letterSpacing: 0.3, borderBottom: `1px solid ${T.border}`, background: 'rgba(0,0,0,0.3)' }}>
        <span><span style={{ color: T.amber }}>●</span>&nbsp;&nbsp;new instance · uninitialized</span>
        <span>codearmory · first-run setup</span>
      </div>

      <div style={{ flex: 1, display: 'flex', alignItems: 'center', justifyContent: 'center', padding: '40px 24px' }}>
        <div style={{ width: 620, background: T.card, border: `1px solid ${T.borderHi}`, boxShadow: '0 24px 60px rgba(0,0,0,0.6)' }}>
          {/* Window chrome */}
          <div style={{ display: 'flex', alignItems: 'center', gap: 8, padding: '10px 14px', borderBottom: `1px solid ${T.border}`, background: T.cardHi }}>
            <span style={{ display: 'flex', gap: 6 }}>
              <span style={{ width: 10, height: 10, borderRadius: 5, background: '#3a3530' }} />
              <span style={{ width: 10, height: 10, borderRadius: 5, background: '#3a3530' }} />
              <span style={{ width: 10, height: 10, borderRadius: 5, background: T.green }} />
            </span>
            <span style={{ flex: 1, textAlign: 'center', fontSize: 11.5, color: T.dim, letterSpacing: 0.4 }}>codearmory ~ setup.sh</span>
            <span style={{ fontSize: 11, color: T.faint }}>80×30</span>
          </div>

          <div style={{ padding: '24px 28px 22px' }}>
            <div style={{ display: 'flex', alignItems: 'center', gap: 10, marginBottom: 14 }}>
              <Logo size={18} />
              <pre style={{ margin: 0, color: T.green, fontSize: 11, lineHeight: 1.25 }}>{`c o d e · a r m o r y · first-run setup`}</pre>
            </div>
            <div style={{ fontSize: 12, color: T.dim, marginBottom: 18, lineHeight: 1.6 }}>
              <span style={{ color: T.green }}>#</span> no users exist yet — let's initialize this instance.<br />
              <span style={{ color: T.green }}>#</span> the account you create becomes the platform <span style={{ color: T.textHi }}>administrator</span>.
            </div>

            {/* Core services orientation */}
            <div style={{ border: `1px solid ${T.border}`, background: T.cardHi, padding: '12px 14px', marginBottom: 18 }}>
              <div style={{ fontSize: 10.5, color: T.faint, letterSpacing: 1, textTransform: 'uppercase', marginBottom: 9 }}>control-plane core · always on, governed by your admin</div>
              <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '8px 18px' }}>
                {CORE_SERVICES.map((s) => (
                  <div key={s.name} style={{ display: 'flex', alignItems: 'baseline', gap: 8, fontSize: 12 }}>
                    <span style={{ color: T.green }}>[✓]</span>
                    <span>
                      <span style={{ color: T.textHi }}>{s.name}</span>
                      <span style={{ color: T.faint, display: 'block', fontSize: 10.5, marginTop: 1 }}>{s.blurb}</span>
                    </span>
                  </div>
                ))}
              </div>
              <div style={{ fontSize: 10.5, color: T.faint, marginTop: 11, paddingTop: 10, borderTop: `1px solid ${T.border}`, lineHeight: 1.55 }}>
                <span style={{ color: T.green }}>+</span> everything else — <span style={{ color: T.dim }}>forge, workflows, blueprints &amp; more</span> — is deployed and enabled on demand from <span style={{ color: T.textHi }}>builder/</span> once you're in.
              </div>
            </div>

            {error && (
              <div style={{ background: T.redSoft, border: `1px solid ${T.red}`, padding: '8px 12px', marginBottom: 16, fontFamily: T.mono, fontSize: 12, color: T.red }}>{error}</div>
            )}

            <form onSubmit={submit}>
              <PromptField prompt="admin email --primary" value={email} onChange={setEmail} type="email" placeholder="you@company.dev" hint="we'll log you in automatically once setup completes." />
              <PromptField prompt="admin username --handle" value={username} onChange={(v) => setUsername(v.toLowerCase().replace(/[^a-z0-9_-]/g, ''))} placeholder="admin" hint={`/^[a-z0-9_-]{1,64}$/ · your workspace path`} />
              <PromptField prompt="admin password --new" value={password} onChange={setPassword} type={showPw ? 'text' : 'password'} placeholder="••••••••••••"
                rightSlot={
                  <button type="button" tabIndex={-1} onClick={() => setShowPw((s) => !s)}
                    style={{ background: 'transparent', border: 0, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '2px 6px', cursor: 'pointer', letterSpacing: 0.4 }}>
                    {showPw ? '--hide' : '--show'}
                  </button>
                }
              />
              <StrengthBar score={score} checks={checks} />

              <PromptField prompt="admin password --confirm" value={confirm} onChange={setConfirm} type={showPw ? 'text' : 'password'} placeholder="••••••••••••"
                hint={confirm && passwordsMatch ? '✓ passwords match' : 're-enter the admin password to confirm.'} />
              {confirm && !passwordsMatch && (
                <div style={{ fontSize: 11, color: T.red, fontFamily: T.mono, marginTop: -12, marginBottom: 16, letterSpacing: 0.2 }}>✗ passwords do not match</div>
              )}

              <button type="submit" disabled={!formValid}
                style={{ width: '100%', padding: '11px 14px', background: formValid ? T.green : 'transparent', color: formValid ? T.bg : T.faint, border: `1px solid ${formValid ? T.green : T.border}`, fontFamily: T.mono, fontSize: 13, fontWeight: 600, letterSpacing: 0.5, cursor: formValid ? 'pointer' : 'not-allowed', transition: 'all .15s' }}>
                {submitting ? '[ initializing instance · · · ]' : formValid ? '[ ↵ ./initialize --admin ]' : '[ complete required fields ]'}
              </button>
            </form>
          </div>

          <div style={{ borderTop: `1px solid ${T.border}`, padding: '8px 14px', display: 'flex', justifyContent: 'space-between', fontSize: 10.5, color: T.faint, letterSpacing: 0.3, background: T.cardHi }}>
            <span>tab · navigate</span>
            <span>first account → administrator</span>
            <span>⏎ submit</span>
          </div>
        </div>
      </div>
    </div>
  );
}

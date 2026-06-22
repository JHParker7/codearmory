import { BrowserRouter, Routes, Route, Navigate } from 'react-router-dom';
import { useAppSelector } from './store/hooks';
import { Login } from './pages/Login';
import { Signup } from './pages/Signup';
import { AppLayout } from './pages/app/AppLayout';
import { Blueprints } from './pages/app/Blueprints';
import { Gatekeeper } from './pages/app/Gatekeeper';
import { Workflows } from './pages/app/Workflows';
import { Forge } from './pages/app/Forge';
import { Tickets } from './pages/app/Tickets';
import { Hooks } from './pages/app/Hooks';
import { Containers } from './pages/app/Containers';
import { Gitea } from './pages/app/Gitea';
import { Outposts } from './pages/app/Outposts';
import { Chaos } from './pages/app/Chaos';
import { Argo } from './pages/app/Argo';
import { Audit } from './pages/app/Audit';
import { Settings } from './pages/app/Settings';
import { T } from './theme';

function RequireAuth({ children }: { children: React.ReactNode }) {
  const { token, status } = useAppSelector(s => s.auth);
  if (status === 'loading') {
    return (
      <div style={{ height: '100vh', display: 'flex', alignItems: 'center', justifyContent: 'center', background: T.bg, fontFamily: T.mono, color: T.faint, fontSize: 12 }}>
        <span style={{ animation: 'pulse 1s ease-in-out infinite' }}>→ initializing · · ·</span>
      </div>
    );
  }
  if (!token) return <Navigate to="/login" replace />;
  return <>{children}</>;
}

function RedirectIfAuthed({ children }: { children: React.ReactNode }) {
  const { token } = useAppSelector(s => s.auth);
  if (token) return <Navigate to="/app/blueprints" replace />;
  return <>{children}</>;
}

// The marketing home page now lives in its own deployment (portal-www). The app
// root sends visitors to the dashboard if authed, otherwise to login.
function RootRedirect() {
  const { token, status } = useAppSelector(s => s.auth);
  if (status === 'loading') {
    return (
      <div style={{ height: '100vh', display: 'flex', alignItems: 'center', justifyContent: 'center', background: T.bg, fontFamily: T.mono, color: T.faint, fontSize: 12 }}>
        <span style={{ animation: 'pulse 1s ease-in-out infinite' }}>→ initializing · · ·</span>
      </div>
    );
  }
  return <Navigate to={token ? '/app/blueprints' : '/login'} replace />;
}

export function App() {
  return (
    <BrowserRouter>
      <Routes>
        <Route path="/" element={<RootRedirect />} />
        <Route path="/login" element={<RedirectIfAuthed><Login /></RedirectIfAuthed>} />
        <Route path="/signup" element={<RedirectIfAuthed><Signup /></RedirectIfAuthed>} />
        <Route path="/app" element={<RequireAuth><AppLayout /></RequireAuth>}>
          <Route index element={<Navigate to="blueprints" replace />} />
          <Route path="blueprints" element={<Blueprints />} />
          <Route path="forge" element={<Forge />} />
          <Route path="workflows" element={<Workflows />} />
          <Route path="tickets" element={<Tickets />} />
          <Route path="hooks" element={<Hooks />} />
          <Route path="containers" element={<Containers />} />
          <Route path="gitea" element={<Gitea />} />
          <Route path="outposts" element={<Outposts />} />
          <Route path="chaos" element={<Chaos />} />
          <Route path="argo" element={<Argo />} />
          <Route path="gatekeeper" element={<Gatekeeper />} />
          <Route path="audit" element={<Audit />} />
          <Route path="settings" element={<Settings />} />
        </Route>
        <Route path="*" element={<Navigate to="/login" replace />} />
      </Routes>
    </BrowserRouter>
  );
}

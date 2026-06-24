import { BrowserRouter, Routes, Route, Navigate, useLocation } from 'react-router-dom';
import { useAppSelector } from './store/hooks';
import { Login } from './pages/Login';
import { Signup } from './pages/Signup';
import { Setup } from './pages/Setup';
import { AppLayout } from './pages/app/AppLayout';
import { Blueprints } from './pages/app/Blueprints';
import { Gatekeeper } from './pages/app/Gatekeeper';
import { Builder } from './pages/app/Builder';
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

function Loading() {
  return (
    <div style={{ height: '100vh', display: 'flex', alignItems: 'center', justifyContent: 'center', background: T.bg, fontFamily: T.mono, color: T.faint, fontSize: 12 }}>
      <span style={{ animation: 'pulse 1s ease-in-out infinite' }}>→ initializing · · ·</span>
    </div>
  );
}

function RequireAuth({ children }: { children: React.ReactNode }) {
  const { token, status } = useAppSelector(s => s.auth);
  if (status === 'loading') return <Loading />;
  if (!token) return <Navigate to="/login" replace />;
  return <>{children}</>;
}

function RedirectIfAuthed({ children }: { children: React.ReactNode }) {
  const { token } = useAppSelector(s => s.auth);
  if (token) return <Navigate to="/app" replace />;
  return <>{children}</>;
}

// The marketing home page now lives in its own deployment (portal-www). The app
// root sends visitors to the dashboard if authed, otherwise to login.
function RootRedirect() {
  const { token, status } = useAppSelector(s => s.auth);
  if (status === 'loading') return <Loading />;
  return <Navigate to={token ? '/app' : '/login'} replace />;
}

// Ordered list of service-backed modules, mirroring the sidebar order, used to
// pick the landing page. blueprints (and the rest) are no longer always enabled,
// so /app lands on the first module whose service is actually registered, falling
// back to gatekeeper/ — a core page that is always present.
const LANDING_MODULES: { path: string; service: string }[] = [
  { path: 'blueprints', service: 'blueprints' },
  { path: 'forge', service: 'forge' },
  { path: 'workflows', service: 'workflows' },
  { path: 'tickets', service: 'tickets' },
  { path: 'hooks', service: 'hooks' },
  { path: 'containers', service: 'containers' },
  { path: 'gitea', service: 'gitea_integration' },
  { path: 'outposts', service: 'outpost-gateway' },
  { path: 'chaos', service: 'chaos' },
  { path: 'argo', service: 'argo' },
];

// DefaultAppRoute resolves the /app index to a landing page. It waits for the
// routing table to resolve, then redirects to the first available module; if the
// table is unknown (null/fail-open) or empty it falls back to the always-present
// gatekeeper/ page rather than a service that may be disabled.
function DefaultAppRoute() {
  const registered = useAppSelector(s => s.auth.registeredServices);
  const resolved = useAppSelector(s => s.auth.servicesResolved);
  if (!resolved) return <Loading />;
  const first = registered ? LANDING_MODULES.find(m => registered.includes(m.service)) : undefined;
  return <Navigate to={first ? first.path : 'gatekeeper'} replace />;
}

// SetupGate funnels the whole app to /setup until the instance has its first user.
// While the first-run check is unresolved (initialized === null) it shows the
// loading screen so no page flashes before we know which way to route. Once the
// instance is initialized, /setup is no longer reachable and redirects to login.
function SetupGate({ children }: { children: React.ReactNode }) {
  const initialized = useAppSelector(s => s.setup.initialized);
  const { pathname } = useLocation();
  if (initialized === null) return <Loading />;
  if (!initialized && pathname !== '/setup') return <Navigate to="/setup" replace />;
  if (initialized && pathname === '/setup') return <Navigate to="/login" replace />;
  return <>{children}</>;
}

export function App() {
  return (
    <BrowserRouter>
      <SetupGate>
        <Routes>
          <Route path="/setup" element={<Setup />} />
          <Route path="/" element={<RootRedirect />} />
          <Route path="/login" element={<RedirectIfAuthed><Login /></RedirectIfAuthed>} />
          <Route path="/signup" element={<RedirectIfAuthed><Signup /></RedirectIfAuthed>} />
          <Route path="/app" element={<RequireAuth><AppLayout /></RequireAuth>}>
            <Route index element={<DefaultAppRoute />} />
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
            <Route path="builder" element={<Builder />} />
            <Route path="audit" element={<Audit />} />
            <Route path="settings" element={<Settings />} />
          </Route>
          <Route path="*" element={<Navigate to="/login" replace />} />
        </Routes>
      </SetupGate>
    </BrowserRouter>
  );
}

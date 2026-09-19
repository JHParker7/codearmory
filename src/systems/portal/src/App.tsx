/**
 * Top-level router and route guards. Wraps every route in a SetupGate (first-run
 * funnel), splits public auth pages from the authenticated `/app` shell, and maps
 * each sidebar module to its page. The guards below decide where an unauthenticated,
 * authenticated, or not-yet-bootstrapped visitor lands.
 */
import { BrowserRouter, Routes, Route, Navigate, useLocation, Outlet } from 'react-router-dom';
import { useAppSelector } from './store/hooks';
import { Login } from './pages/Login';
import { Signup } from './pages/Signup';
import { Setup } from './pages/Setup';
import { AppLayout } from './pages/app/AppLayout';
import { Gatekeeper } from './pages/app/Gatekeeper';
import { Projects } from './pages/app/Projects';
import { Builder } from './pages/app/Builder';
import { Workflows } from './pages/app/Workflows';
import { RunView } from './pages/app/RunView';
import { Forge } from './pages/app/Forge';
import { Tickets } from './pages/app/Tickets';
import { Events } from './pages/app/Events';
import { Containers } from './pages/app/Containers';
import { Git } from './pages/app/Git';
import { Repos } from './pages/app/Repos';
import { Outposts } from './pages/app/Outposts';
import { ServiceFrame } from './pages/app/ServiceFrame';
import { Audit } from './pages/app/Audit';
import { SettingsHub } from './pages/app/SettingsHub';
import { BlacksmithRoles } from './pages/app/BlacksmithRoles';
import { Notifications } from './pages/app/Notifications';
import { Wiki } from './pages/app/Wiki';
import { T } from './theme';

/** Full-screen terminal-styled loading splash shown while a route guard awaits an async check. */
function Loading() {
  return (
    <div style={{ height: '100vh', display: 'flex', alignItems: 'center', justifyContent: 'center', background: T.bg, fontFamily: T.mono, color: T.faint, fontSize: 12 }}>
      <span style={{ animation: 'pulse 1s ease-in-out infinite' }}>→ initializing · · ·</span>
    </div>
  );
}

/**
 * Route guard for the `/app` shell: shows Loading while the session resolves, else
 * redirects to /login when unauthenticated.
 *
 * Gated on `userId`, NOT on `token`. The credential is the HttpOnly cookie, which this
 * code cannot read, and `token` is only populated for the tab that performed the login —
 * so a reload legitimately has an empty token while still being fully signed in.
 * Gating on the token would bounce every refresh to /login.
 */
function RequireAuth({ children }: { children: React.ReactNode }) {
  const { userId, status } = useAppSelector(s => s.auth);
  if (status === 'loading') return <Loading />;
  if (!userId) return <Navigate to="/login" replace />;
  return <>{children}</>;
}

/** Inverse guard for public auth pages (login/signup): bounces an already-authenticated visitor to /app. */
function RedirectIfAuthed({ children }: { children: React.ReactNode }) {
  const { userId } = useAppSelector(s => s.auth);
  if (userId) return <Navigate to="/app" replace />;
  return <>{children}</>;
}

/**
 * Resolves the `/` root: dashboard if authenticated, otherwise login.
 *
 * The marketing home page now lives in its own deployment (portal-www), so this
 * root has no landing page of its own.
 */
function RootRedirect() {
  const { token, status } = useAppSelector(s => s.auth);
  if (status === 'loading') return <Loading />;
  return <Navigate to={token ? '/app' : '/login'} replace />;
}

// Ordered list of bundled service-backed modules, mirroring the sidebar order,
// used to pick the landing page. These are no longer always enabled, so /app lands
// on the first module whose service is actually registered, falling back to
// gatekeeper/ — a core page that is always present. Iframe-only (non-bundled)
// services are intentionally not landing targets.
const LANDING_MODULES: { path: string; service: string }[] = [
  { path: 'forge', service: 'forge' },
  { path: 'workflows', service: 'workflows' },
  { path: 'tickets', service: 'tickets' },
  { path: 'events', service: 'events' },
  { path: 'containers', service: 'containers' },
  { path: 'git', service: 'git_connector' },
  { path: 'outposts', service: 'outpost-gateway' },
];

/**
 * Resolves the /app index to a landing page. Waits for the routing table to
 * resolve, then redirects to the first available module; if the table is unknown
 * (null/fail-open) or empty it falls back to the always-present gatekeeper/ page
 * rather than a service that may be disabled.
 */
function DefaultAppRoute() {
  const registered = useAppSelector(s => s.auth.registeredServices);
  const resolved = useAppSelector(s => s.auth.servicesResolved);
  const project = useAppSelector(s => s.project.current);
  if (!resolved) return <Loading />;
  // The portal is project-scoped: with no project chosen, land on the projects
  // page (the front door) so the user picks or creates one before entering a
  // resource view — like choosing a repo/group in other DevOps platforms.
  if (!project) return <Navigate to="projects" replace />;
  const first = registered ? LANDING_MODULES.find(m => registered.includes(m.service)) : undefined;
  return <Navigate to={first ? first.path : 'gatekeeper'} replace />;
}

/**
 * Gate for the resource modules: they show a single project's resources, so a
 * project must be selected to enter them. With none chosen, bounce to the
 * projects page. Admin/account pages (gatekeeper, projects, builder, audit,
 * settings) are intentionally NOT behind this gate — they are project-independent.
 */
function ProjectGate() {
  const project = useAppSelector(s => s.project.current);
  if (!project) return <Navigate to="/app/projects" replace />;
  return <Outlet />;
}

/**
 * Funnels the whole app to /setup until the instance has its first user.
 *
 * While the first-run check is unresolved (initialized === null) it shows the
 * loading screen so no page flashes before we know which way to route. Once the
 * instance is initialized, /setup is no longer reachable and redirects to login.
 */
function SetupGate({ children }: { children: React.ReactNode }) {
  const initialized = useAppSelector(s => s.setup.initialized);
  const { pathname } = useLocation();
  if (initialized === null) return <Loading />;
  if (!initialized && pathname !== '/setup') return <Navigate to="/setup" replace />;
  if (initialized && pathname === '/setup') return <Navigate to="/login" replace />;
  return <>{children}</>;
}

/** The app's full route tree: SetupGate → public auth pages + the guarded `/app` shell with one route per module. */
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
            {/* Resource modules: scoped to the selected project, so gated on one
                being chosen (ProjectGate bounces to /app/projects otherwise). */}
            <Route element={<ProjectGate />}>
              <Route path="forge" element={<Forge />} />
              <Route path="workflows" element={<Workflows />} />
              <Route path="workflows/runs/:runId" element={<RunView />} />
              <Route path="tickets" element={<Tickets />} />
              <Route path="wiki" element={<Wiki />} />
              <Route path="events" element={<Events />} />
              <Route path="containers" element={<Containers />} />
              <Route path="git" element={<Git />} />
              {/* The git host keeps its registry-name path: /app/codearmory_git_factory
                  is what the sidebar, existing links and bookmarks already point at —
                  only what renders there changed (bundled page, no longer an iframe). */}
              <Route path="codearmory_git_factory" element={<Repos />} />
              {/* Readable repo path, like other git hosts: /app/codearmory_git_factory/<owner>/<project>/<repo>
                  (owner = the repo's git-factory namespace). Renders the same page, which resolves the
                  segments to a repo; tab/pr stay query params. */}
              <Route path="codearmory_git_factory/:owner/:project/:name" element={<Repos />} />
              <Route path="outposts" element={<Outposts />} />
            </Route>
            <Route path="gatekeeper" element={<Gatekeeper />} />
            <Route path="projects" element={<Projects />} />
            <Route path="builder" element={<Builder />} />
            <Route path="audit" element={<Audit />} />
            <Route path="settings" element={<SettingsHub />} />
            <Route path="blacksmith-roles" element={<BlacksmithRoles />} />
            <Route path="notifications" element={<Notifications />} />
            {/* Generic iframe host for any registered, non-bundled service that
                advertises a ui_path (e.g. blueprints, chaos, argo). Static routes
                above out-rank this dynamic segment, so bundled pages always win. */}
            <Route path=":service" element={<ServiceFrame />} />
          </Route>
          <Route path="*" element={<Navigate to="/login" replace />} />
        </Routes>
      </SetupGate>
    </BrowserRouter>
  );
}

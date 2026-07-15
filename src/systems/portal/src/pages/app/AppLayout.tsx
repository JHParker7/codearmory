/**
 * `/app` shell — the authenticated layout: sidebar (signed-in user, service/permission
 * gated module nav, logout) plus the routed `<Outlet/>`. On mount it hydrates the user,
 * permissions, and registered-services routing table from the BFF, and blocks the main
 * pane when the active route's backing service isn't registered/routable in conductor.
 */
import { useEffect, useRef, useState, type ReactNode } from 'react';
import { NavLink, Outlet, useNavigate, useLocation } from 'react-router-dom';
import { T } from '../../theme';
import { Logo } from '../../components/Logo';
import { Icon, IconName } from '../../components/Icons';
import { useResizablePane } from '../../components/ResizeHandle';
import { useReloadOnReconnect } from '../../hooks/useReloadOnReconnect';
import { useAppDispatch, useAppSelector } from '../../store/hooks';
import { logout, hydrateUser, hydratePermissions, hydrateRegisteredServices } from '../../store/authSlice';
import { setCurrentProject, fetchKnownProjects } from '../../store/projectSlice';
import { shortId } from '../../utils';
import { useOrgNames } from '../../hooks/useNames';

/**
 * Maps each service-gated /app route segment to its backing platform service, so an
 * unavailable service's page is blocked on direct navigation/refresh — not just
 * hidden from the sidebar. The admin routes (gatekeeper, builder, audit) gated by
 * permission, and the account section (settings), are intentionally absent: they are
 * always reachable when the user holds the permission.
 */
const ROUTE_SERVICE: Record<string, string> = {
  forge: 'forge',
  workflows: 'workflows',
  tickets: 'tickets',
  hooks: 'hooks',
  containers: 'containers',
  git: 'git',
  outposts: 'outpost-gateway',
};

// Services with a first-class bundled page. Any OTHER registered service that
// advertises a ui_path is rendered generically via <ServiceFrame> (an iframe of
// its embedded mini-portal), so a non-core service appears with zero portal
// changes. Bundled pages always out-rank the dynamic iframe route. Admin/account
// services (gatekeeper, builder) are bundled and never iframe-hosted.
const BUNDLED_SERVICES = new Set<string>([
  'forge', 'workflows', 'hooks', 'gatekeeper', 'builder',
  'tickets', 'git', 'containers', 'outpost-gateway',
]);

/**
 * Plain-language, newcomer-friendly one-liners for the dynamically discovered
 * iframe modules (blueprints, chaos, argo, …) — rendered as a subtitle under the
 * label so someone who has never used the platform can tell what a module is for
 * rather than facing a bare service name. The bundled nav items pass their own
 * copy explicitly. Keep these short (~30 chars) so they don't wrap in a narrow
 * sidebar; anything unmapped simply shows no subtitle.
 */
const SERVICE_DESC: Record<string, string> = {
  blueprints: 'Infrastructure-as-code state',
  chaos: 'Chaos experiments on clusters',
  argo: 'GitOps app deployments',
  notifications: 'Alerts & message delivery',
  mcp: 'Model Context Protocol server',
  gitea_integration: 'Forgejo / Gitea repositories',
};

/**
 * isUnavailable reports whether a service-backed module should be hidden/blocked:
 * true only once we hold a RESOLVED, non-null routing table that omits the service.
 * While the table is in flight or errored (registeredServices === null) it stays
 * false — the sidebar fails OPEN rather than flashing live modules away.
 */
function isUnavailable(service: string, registered: string[] | null): boolean {
  return registered !== null && !registered.includes(service);
}

/**
 * NavItem renders a sidebar link. When `service` is set, the item hides itself
 * unless that platform service is currently registered/routable in conductor —
 * so modules that aren't actually deployed never appear. `desc` is a short
 * plain-language subtitle rendered under the label (expanded only) so a newcomer
 * can tell what the module does; collapsed, it is folded into the tooltip
 * instead. When `collapsed`, the item shrinks to a centred icon glyph (full label
 * kept as a tooltip) so the sidebar can minimise to a slim rail; without an
 * `icon` it falls back to a two-letter token.
 */
function NavItem({ to, label, badge, service, collapsed, icon, desc }: { to: string; label: string; badge?: string; service?: string; collapsed?: boolean; icon?: IconName; desc?: string }) {
  const registeredServices = useAppSelector(s => s.auth.registeredServices);
  if (service && isUnavailable(service, registeredServices)) return null;
  const token = label.replace(/\/$/, '').slice(0, 2);
  // Collapsed: label (+ its subtitle) become the hover tooltip since neither is
  // visible on the slim rail. Expanded: the subtitle renders inline, so no tooltip.
  const tooltip = collapsed ? (desc ? `${label} — ${desc}` : label) : undefined;
  return (
    <NavLink to={to} title={tooltip} style={({ isActive }) => ({
      display: 'flex', flexDirection: collapsed ? 'row' : 'column',
      alignItems: collapsed ? 'center' : 'stretch', justifyContent: 'center',
      padding: collapsed ? '9px 0' : '7px 14px',
      background: isActive ? T.greenSoft : 'transparent',
      borderLeft: `2px solid ${isActive ? T.green : 'transparent'}`,
      color: isActive ? T.textHi : T.text,
      fontFamily: T.mono, fontSize: 13, textDecoration: 'none',
      transition: 'background .12s',
      cursor: 'pointer',
    })}
    onMouseEnter={(e) => { if (!e.currentTarget.getAttribute('aria-current')) e.currentTarget.style.background = T.greenFaint; }}
    onMouseLeave={(e) => { if (!e.currentTarget.getAttribute('aria-current')) e.currentTarget.style.background = 'transparent'; }}
    >
      {collapsed ? (
        <span style={{ position: 'relative', display: 'flex', alignItems: 'center', justifyContent: 'center', fontSize: 12 }}>
          {icon ? <Icon name={icon} /> : token}
          {badge && <span style={{ position: 'absolute', top: -3, right: -6, width: 5, height: 5, borderRadius: '50%', background: T.amber }} />}
        </span>
      ) : (
        <>
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
            <span>{label}</span>
            {badge && <span style={{ fontSize: 9, color: T.amber, border: `1px solid ${T.amber}`, padding: '0 4px', letterSpacing: 0.5 }}>{badge}</span>}
          </div>
          {desc && <span style={{ fontSize: 10.5, color: T.faint, lineHeight: 1.3, marginTop: 2 }}>{desc}</span>}
        </>
      )}
    </NavLink>
  );
}

/**
 * ProjectSwitcher — the sidebar control for the current project (workspace): a
 * free-text label that filters every list view (pipelines, executions, tickets,
 * repos) and tags newly-created resources. It is purely a view filter, never a
 * permission boundary. Picking "all projects" clears the filter; "+ new project"
 * sets a label that hasn't been used yet.
 *
 * Collapsed, it shrinks to a single indicator dot that re-expands the sidebar on
 * click (the dropdown needs the room), keeping the slim rail uncluttered.
 */
function ProjectSwitcher({ collapsed, onExpand }: { collapsed: boolean; onExpand: () => void }) {
  const token = useAppSelector(s => s.auth.token);
  const current = useAppSelector(s => s.project.current);
  const known = useAppSelector(s => s.project.known);
  const dispatch = useAppDispatch();
  const [open, setOpen] = useState(false);
  const [creating, setCreating] = useState('');
  const newRef = useRef<HTMLInputElement>(null);

  const choose = (name: string | null) => {
    dispatch(setCurrentProject(name));
    setOpen(false);
    setCreating('');
  };

  const submitNew = () => {
    const v = creating.trim();
    if (v) choose(v);
  };

  // Collapsed rail: a dot that hints whether a filter is active and expands the
  // sidebar (where the full switcher lives) when clicked.
  if (collapsed) {
    return (
      <button onClick={onExpand} title={current ? `project: ${current}` : 'all projects'}
        style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', padding: '10px 0', background: 'transparent', border: 'none', borderBottom: `1px solid ${T.border}`, cursor: 'pointer', width: '100%' }}>
        <span style={{ width: 8, height: 8, borderRadius: 2, border: `1px solid ${current ? T.green : T.faint}`, background: current ? T.green : 'transparent' }} />
      </button>
    );
  }

  return (
    <div style={{ position: 'relative', padding: '10px 14px', borderBottom: `1px solid ${T.border}` }}>
      <div style={{ fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 5, textTransform: 'uppercase' }}>project</div>
      <button onClick={() => { const next = !open; setOpen(next); if (next && token) dispatch(fetchKnownProjects(token)); }}
        style={{ width: '100%', display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 8, background: T.card, border: `1px solid ${open ? T.green : T.border}`, color: current ? T.textHi : T.dim, fontFamily: T.mono, fontSize: 12, padding: '6px 9px', cursor: 'pointer', transition: 'border-color .12s' }}>
        <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
          {current ? <><span style={{ color: T.green }}>◆ </span>{current}</> : 'all projects'}
        </span>
        <span style={{ color: T.faint, fontSize: 10, flexShrink: 0 }}>{open ? '▴' : '▾'}</span>
      </button>

      {open && (
        <>
          {/* Click-away backdrop so the dropdown closes on any outside click. */}
          <div onClick={() => { setOpen(false); setCreating(''); }} style={{ position: 'fixed', inset: 0, zIndex: 40 }} />
          <div style={{ position: 'absolute', top: '100%', left: 14, right: 14, marginTop: 4, zIndex: 41, background: T.card, border: `1px solid ${T.borderHi}`, boxShadow: '0 8px 24px rgba(0,0,0,0.5)', maxHeight: 320, overflowY: 'auto' }}>
            <button onClick={() => choose(null)}
              style={{ width: '100%', textAlign: 'left', background: !current ? T.greenSoft : 'transparent', border: 'none', borderLeft: `2px solid ${!current ? T.green : 'transparent'}`, color: !current ? T.green : T.dim, fontFamily: T.mono, fontSize: 12, padding: '7px 10px', cursor: 'pointer' }}>
              all projects
            </button>
            {known.length > 0 && <div style={{ height: 1, background: T.border }} />}
            {known.map(p => {
              const active = p === current;
              return (
                <button key={p} onClick={() => choose(p)}
                  style={{ width: '100%', textAlign: 'left', background: active ? T.greenSoft : 'transparent', border: 'none', borderLeft: `2px solid ${active ? T.green : 'transparent'}`, color: active ? T.textHi : T.text, fontFamily: T.mono, fontSize: 12, padding: '7px 10px', cursor: 'pointer', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}
                  onMouseEnter={e => { if (!active) (e.currentTarget as HTMLButtonElement).style.background = T.greenFaint; }}
                  onMouseLeave={e => { if (!active) (e.currentTarget as HTMLButtonElement).style.background = 'transparent'; }}>
                  {active && <span style={{ color: T.green }}>◆ </span>}{p}
                </button>
              );
            })}
            <div style={{ height: 1, background: T.border }} />
            <div style={{ display: 'flex', gap: 6, padding: '8px 10px' }}>
              <input ref={newRef} value={creating} onChange={e => setCreating(e.target.value)}
                onKeyDown={e => { if (e.key === 'Enter') submitNew(); if (e.key === 'Escape') { setOpen(false); setCreating(''); } }}
                placeholder="+ new project"
                style={{ flex: 1, minWidth: 0, background: T.cardHi, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 11, padding: '5px 7px', outline: 'none' }} />
              <button onClick={submitNew} disabled={!creating.trim()}
                style={{ background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 11, fontWeight: 600, padding: '5px 9px', cursor: creating.trim() ? 'pointer' : 'default', opacity: creating.trim() ? 1 : 0.5 }}>
                use
              </button>
            </div>
          </div>
        </>
      )}
    </div>
  );
}

/**
 * NavSection groups a run of nav items under a collapsible header. In the expanded
 * sidebar the header (the section label) becomes a clickable toggle with a chevron
 * that minimises just this section — persisted per-section under `nav.section.<key>`
 * so each choice sticks across reloads independently of the whole-sidebar minimise.
 * In the collapsed rail there is no header to click, so items always render (matching
 * today's slim-rail behaviour), and per-section state is left untouched.
 */
function NavSection({ title, sidebarCollapsed, children }: { title: string; sidebarCollapsed: boolean; children: ReactNode }) {
  const storageKey = `nav.section.${title}`;
  const [collapsed, setCollapsed] = useState(() => localStorage.getItem(storageKey) === '1');
  useEffect(() => { localStorage.setItem(storageKey, collapsed ? '1' : '0'); }, [collapsed, storageKey]);

  // Slim rail: no header to toggle, so show the items as-is.
  if (sidebarCollapsed) return <>{children}</>;

  return (
    <>
      <button onClick={() => setCollapsed(c => !c)} title={collapsed ? `expand ${title}` : `minimise ${title}`}
        style={{ width: '100%', display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 8,
          background: 'transparent', border: 'none', color: T.faint, fontFamily: T.mono, fontSize: 10,
          letterSpacing: 1, padding: '6px 14px 4px', textTransform: 'uppercase', cursor: 'pointer', transition: 'color .12s' }}
        onMouseEnter={(e) => { (e.currentTarget as HTMLButtonElement).style.color = T.dim; }}
        onMouseLeave={(e) => { (e.currentTarget as HTMLButtonElement).style.color = T.faint; }}>
        <span>{title}</span>
        <span style={{ fontSize: 8, flexShrink: 0, lineHeight: 1 }}>{collapsed ? '▸' : '▾'}</span>
      </button>
      {!collapsed && children}
    </>
  );
}

/** App shell component: renders the gated sidebar + routed Outlet, hydrates user/permissions/services on mount, and walls off the pane for unavailable services. */
export function AppLayout() {
  const token = useAppSelector(s => s.auth.token)!;
  const user = useAppSelector(s => s.auth.user);
  const orgNames = useOrgNames(token);
  const permissions = useAppSelector(s => s.auth.permissions);
  const registeredServices = useAppSelector(s => s.auth.registeredServices);
  const serviceUiPaths = useAppSelector(s => s.auth.serviceUiPaths);
  const servicesResolved = useAppSelector(s => s.auth.servicesResolved);
  const dispatch = useAppDispatch();
  const navigate = useNavigate();
  const { pathname } = useLocation();

  // Sidebar minimise — collapses to a slim icon rail to hand the main pane more
  // width. Persisted so the choice sticks across reloads.
  const [navCollapsed, setNavCollapsed] = useState(() => localStorage.getItem('nav.collapsed') === '1');
  useEffect(() => { localStorage.setItem('nav.collapsed', navCollapsed ? '1' : '0'); }, [navCollapsed]);
  // When expanded, the sidebar width is user-draggable (persisted). The width
  // transition is suspended mid-drag so it tracks the cursor crisply, then restored
  // so the collapse/expand toggle still animates.
  const shellRef = useRef<HTMLDivElement>(null);
  const [navResizing, setNavResizing] = useState(false);
  const [navW, navHandle] = useResizablePane('split.appshell.sidebar.w', 220, {
    min: 180, max: 420, side: 'left', direction: 'horizontal', containerRef: shellRef, otherMin: 520,
    onDragStart: () => setNavResizing(true), onDragEnd: () => setNavResizing(false),
  });

  const activeSegment = pathname.replace(/^\/app\/?/, '').split('/')[0];
  const activeService = ROUTE_SERVICE[activeSegment];
  // Fail open while the routing table is unresolved (null), mirroring the sidebar.
  const serviceUnavailable = !!activeService && isUnavailable(activeService, registeredServices);

  // Registered services that ship a mini-portal but have no bundled page get a
  // generic iframe nav entry — so a non-core service shows up with zero portal
  // changes. ServiceFrame handles availability/loading for the routed pane.
  const iframeServices = (registeredServices ?? [])
    .filter(s => serviceUiPaths[s] && !BUNDLED_SERVICES.has(s))
    .sort();

  // Hydrate the user on mount, and again whenever the browser reconnects. Since a
  // transient hydrate failure now keeps the session (rather than logging out), the
  // reconnect retry is what fills the user back in once the connection returns.
  useReloadOnReconnect(() => { if (!user) dispatch(hydrateUser()); }, []);

  useEffect(() => {
    if (user && !permissions) dispatch(hydratePermissions());
  }, [user, permissions]); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    if (user && !servicesResolved) dispatch(hydrateRegisteredServices());
  }, [user, servicesResolved]); // eslint-disable-line react-hooks/exhaustive-deps

  // Seed the project switcher's known-label list once the user is hydrated; it
  // refreshes again each time the switcher dropdown opens.
  useEffect(() => {
    if (user && token) dispatch(fetchKnownProjects(token));
  }, [user, token]); // eslint-disable-line react-hooks/exhaustive-deps

  const handleLogout = () => {
    dispatch(logout());
    navigate('/');
  };

  return (
    <div ref={shellRef} style={{ display: 'flex', height: '100vh', background: T.bg, fontFamily: T.mono, color: T.text, overflow: 'hidden' }}>
      {/* Sidebar */}
      <aside style={{ width: navCollapsed ? 56 : navW, flexShrink: 0, background: T.bgAlt, borderRight: `1px solid ${T.border}`, display: 'flex', flexDirection: 'column', transition: navResizing ? 'none' : 'width .14s ease' }}>
        {/* Logo + minimise toggle */}
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: navCollapsed ? 'center' : 'space-between', gap: 10, padding: navCollapsed ? '14px 0' : '14px 16px', borderBottom: `1px solid ${T.border}` }}>
          {!navCollapsed && (
            <div style={{ display: 'flex', alignItems: 'center', gap: 10, minWidth: 0 }}>
              <Logo size={18} />
              <span style={{ fontSize: 13, fontWeight: 700, color: T.textHi, letterSpacing: -0.2 }}>codearmory</span>
            </div>
          )}
          <button onClick={() => setNavCollapsed(c => !c)} title={navCollapsed ? 'expand sidebar' : 'minimise sidebar'}
            style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 12, lineHeight: 1, padding: '4px 7px', cursor: 'pointer', transition: 'border-color .12s, color .12s' }}
            onMouseEnter={(e) => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.green; (e.currentTarget as HTMLButtonElement).style.color = T.green; }}
            onMouseLeave={(e) => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.border; (e.currentTarget as HTMLButtonElement).style.color = T.dim; }}>
            {navCollapsed ? '»' : '«'}
          </button>
        </div>

        {/* User */}
        {user && !navCollapsed && (
          <div style={{ padding: '12px 14px', borderBottom: `1px solid ${T.border}` }}>
            <div style={{ fontSize: 11, color: T.faint, letterSpacing: 0.5, marginBottom: 4 }}>SIGNED IN AS</div>
            <div style={{ fontSize: 13, color: T.textHi, fontWeight: 600 }}>@{user.username}</div>
            <div style={{ fontSize: 11, color: T.dim, marginTop: 2 }}>{user.email}</div>
            {user.org_id && <div style={{ fontSize: 10.5, color: T.faint, marginTop: 4 }}>org: {orgNames[user.org_id] ?? shortId(user.org_id)}</div>}
          </div>
        )}

        {/* Project switcher — current-project view filter (or a slim indicator when collapsed). */}
        <ProjectSwitcher collapsed={navCollapsed} onExpand={() => setNavCollapsed(false)} />

        {/* Nav — each section header doubles as a per-section minimise toggle when the
            sidebar is expanded; the slim rail shows every section's items as-is. */}
        <nav style={{ flex: 1, padding: '8px 0', overflowY: 'auto', overflowX: 'hidden' }}>
          <NavSection title="tools" sidebarCollapsed={navCollapsed}>
            <NavItem to="/app/workflows" label="workflows/" desc="Automate builds & deploys" service="workflows" collapsed={navCollapsed} icon="workflows" />
            <NavItem to="/app/tickets" label="tickets/" desc="Track issues on kanban boards" service="tickets" collapsed={navCollapsed} icon="tickets" />
            <NavItem to="/app/git" label="git/" desc="Connect & clone your repositories" service="git_connector" collapsed={navCollapsed} icon="git" />
          </NavSection>
          <div style={{ height: 1, background: T.border, margin: '8px 0' }} />
          <NavSection title="modules" sidebarCollapsed={navCollapsed}>
            <NavItem to="/app/forge" label="forge/" desc="Run commands in secure sandboxes" service="forge" collapsed={navCollapsed} icon="forge" />
            <NavItem to="/app/hooks" label="hooks/" desc="Trigger actions from webhooks" service="hooks" collapsed={navCollapsed} icon="hooks" />
            <NavItem to="/app/containers" label="containers/" desc="Your private image registry" service="containers" collapsed={navCollapsed} icon="containers" />
            <NavItem to="/app/outposts" label="outposts/" desc="Link your Kubernetes clusters" service="outpost-gateway" collapsed={navCollapsed} icon="outposts" />
            {/* Generic iframe-hosted services (blueprints, chaos, argo, and any future
                non-core service that advertises a ui_path) — discovered at runtime,
                no per-service code. Subtitles come from SERVICE_DESC when known. */}
            {iframeServices.map(svc => (
              <NavItem key={svc} to={`/app/${svc}`} label={`${svc}/`} desc={SERVICE_DESC[svc]} service={svc} collapsed={navCollapsed} />
            ))}
          </NavSection>
          <div style={{ height: 1, background: T.border, margin: '8px 0' }} />
          <NavSection title="admin" sidebarCollapsed={navCollapsed}>
            {permissions?.['builder:configureOrgService'] && <NavItem to="/app/builder" label="builder/" desc="Deploy & configure services" collapsed={navCollapsed} icon="builder" />}
            {permissions?.['gatekeeper:listAuditLog'] && <NavItem to="/app/audit" label="audit/" desc="Who changed what, and when" collapsed={navCollapsed} icon="audit" />}
            <NavItem to="/app/gatekeeper" label="gatekeeper/" desc="Access control — users & roles" collapsed={navCollapsed} icon="gatekeeper" />
          </NavSection>
          <div style={{ height: 1, background: T.border, margin: '8px 0' }} />
          <NavSection title="account" sidebarCollapsed={navCollapsed}>
            <NavItem to="/app/settings" label="settings/" desc="Theme, account & preferences" collapsed={navCollapsed} icon="settings" />
          </NavSection>
        </nav>

        {/* Logout */}
        <div style={{ padding: navCollapsed ? '10px 8px' : '10px 14px', borderTop: `1px solid ${T.border}` }}>
          <button onClick={handleLogout} title={navCollapsed ? 'logout' : undefined}
            style={{ width: '100%', background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 12, padding: '7px 10px', cursor: 'pointer', textAlign: navCollapsed ? 'center' : 'left', letterSpacing: 0.3, transition: 'border-color .12s, color .12s' }}
            onMouseEnter={(e) => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.red; (e.currentTarget as HTMLButtonElement).style.color = T.red; }}
            onMouseLeave={(e) => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.border; (e.currentTarget as HTMLButtonElement).style.color = T.dim; }}>
            {navCollapsed ? '⏻' : '[ ./logout ]'}
          </button>
        </div>
      </aside>

      {/* Drag to resize the sidebar (only while expanded). */}
      {!navCollapsed && navHandle}

      {/* Main content */}
      <main style={{ flex: 1, overflow: 'auto', display: 'flex', flexDirection: 'column' }}>
        {serviceUnavailable ? (
          <div style={{ flex: 1, display: 'flex', alignItems: 'center', justifyContent: 'center', padding: 24 }}>
            <div style={{ textAlign: 'center', fontFamily: T.mono }}>
              <div style={{ fontSize: 14, color: T.textHi, marginBottom: 6 }}>{activeSegment}/ is not available on this instance</div>
              <div style={{ fontSize: 12, color: T.faint }}>a system administrator can enable it in builder/</div>
            </div>
          </div>
        ) : (
          <Outlet />
        )}
      </main>
    </div>
  );
}

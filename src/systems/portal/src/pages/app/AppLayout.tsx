import { useEffect } from 'react';
import { NavLink, Outlet, useNavigate, useLocation } from 'react-router-dom';
import { T } from '../../theme';
import { Logo } from '../../components/Logo';
import { useAppDispatch, useAppSelector } from '../../store/hooks';
import { logout, hydrateUser, hydratePermissions, hydrateServices } from '../../store/authSlice';

// Maps each service-gated /app route segment to its backing platform service, so a
// disabled service's page is blocked on direct navigation/refresh — not just hidden
// from the sidebar. Routes gated by permission (gatekeeper, builder, audit) or the
// account section (settings) are intentionally absent: they are never org-disabled.
const ROUTE_SERVICE: Record<string, string> = {
  blueprints: 'blueprints',
  forge: 'forge',
  workflows: 'workflows',
  tickets: 'tickets',
  hooks: 'hooks',
  containers: 'containers',
  gitea: 'gitea_integration',
  outposts: 'outpost-gateway',
  chaos: 'chaos',
  argo: 'argo',
};

// NavItem renders a sidebar link. When `service` is set, the item hides itself
// if that platform service is disabled for the org (per builder). While the
// enablement set is still loading or unresolved (disabledServices === null) the
// item shows — the sidebar fails open rather than flashing items away.
function NavItem({ to, label, badge, service }: { to: string; label: string; badge?: string; service?: string }) {
  const disabledServices = useAppSelector(s => s.auth.disabledServices);
  if (service && disabledServices?.includes(service)) return null;
  return (
    <NavLink to={to} style={({ isActive }) => ({
      display: 'flex', alignItems: 'center', justifyContent: 'space-between',
      padding: '8px 14px',
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
      <span>{label}</span>
      {badge && <span style={{ fontSize: 9, color: T.amber, border: `1px solid ${T.amber}`, padding: '0 4px', letterSpacing: 0.5 }}>{badge}</span>}
    </NavLink>
  );
}

export function AppLayout() {
  const user = useAppSelector(s => s.auth.user);
  const permissions = useAppSelector(s => s.auth.permissions);
  const disabledServices = useAppSelector(s => s.auth.disabledServices);
  const dispatch = useAppDispatch();
  const navigate = useNavigate();
  const { pathname } = useLocation();

  const activeSegment = pathname.replace(/^\/app\/?/, '').split('/')[0];
  const activeService = ROUTE_SERVICE[activeSegment];
  // Fail open while the set is still resolving (null), mirroring the sidebar.
  const serviceDisabled = !!activeService && !!disabledServices?.includes(activeService);

  useEffect(() => {
    if (!user) dispatch(hydrateUser());
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    if (user && !permissions) dispatch(hydratePermissions());
  }, [user, permissions]); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    if (user?.org_id && disabledServices === null) dispatch(hydrateServices());
  }, [user, disabledServices]); // eslint-disable-line react-hooks/exhaustive-deps

  const handleLogout = () => {
    dispatch(logout());
    navigate('/');
  };

  return (
    <div style={{ display: 'flex', height: '100vh', background: T.bg, fontFamily: T.mono, color: T.text, overflow: 'hidden' }}>
      {/* Sidebar */}
      <aside style={{ width: 220, flexShrink: 0, background: T.bgAlt, borderRight: `1px solid ${T.border}`, display: 'flex', flexDirection: 'column' }}>
        {/* Logo */}
        <div style={{ display: 'flex', alignItems: 'center', gap: 10, padding: '14px 16px', borderBottom: `1px solid ${T.border}` }}>
          <Logo size={18} />
          <span style={{ fontSize: 13, fontWeight: 700, color: T.textHi, letterSpacing: -0.2 }}>codearmory</span>
        </div>

        {/* User */}
        {user && (
          <div style={{ padding: '12px 14px', borderBottom: `1px solid ${T.border}` }}>
            <div style={{ fontSize: 11, color: T.faint, letterSpacing: 0.5, marginBottom: 4 }}>SIGNED IN AS</div>
            <div style={{ fontSize: 13, color: T.textHi, fontWeight: 600 }}>@{user.username}</div>
            <div style={{ fontSize: 11, color: T.dim, marginTop: 2 }}>{user.email}</div>
            {user.org_id && <div style={{ fontSize: 10.5, color: T.faint, marginTop: 4 }}>org: {user.org_id.slice(0, 8)}…</div>}
          </div>
        )}

        {/* Nav */}
        <nav style={{ flex: 1, padding: '8px 0', overflowY: 'auto' }}>
          <div style={{ fontSize: 10, color: T.faint, letterSpacing: 1, padding: '6px 14px 4px', textTransform: 'uppercase' }}>modules</div>
          <NavItem to="/app/blueprints" label="blueprints/" service="blueprints" />
          <NavItem to="/app/forge" label="forge/" service="forge" />
          <NavItem to="/app/workflows" label="workflows/" service="workflows" />
          <NavItem to="/app/tickets" label="tickets/" service="tickets" />
          <NavItem to="/app/hooks" label="hooks/" service="hooks" />
          <NavItem to="/app/containers" label="containers/" service="containers" />
          <NavItem to="/app/gitea" label="git/" service="gitea_integration" />
          <NavItem to="/app/outposts" label="outposts/" service="outpost-gateway" />
          <NavItem to="/app/chaos" label="chaos/" service="chaos" />
          <NavItem to="/app/argo" label="argo/" service="argo" />
          <NavItem to="/app/gatekeeper" label="gatekeeper/" />
          {permissions?.['builder:configureOrgService'] && <NavItem to="/app/builder" label="builder/" />}
          {permissions?.['gatekeeper:listAuditLog'] && <NavItem to="/app/audit" label="audit/" />}
          <div style={{ height: 1, background: T.border, margin: '8px 0' }} />
          <div style={{ fontSize: 10, color: T.faint, letterSpacing: 1, padding: '6px 14px 4px', textTransform: 'uppercase' }}>account</div>
          <NavItem to="/app/settings" label="settings/" />
        </nav>

        {/* Logout */}
        <div style={{ padding: '10px 14px', borderTop: `1px solid ${T.border}` }}>
          <button onClick={handleLogout} style={{ width: '100%', background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 12, padding: '7px 10px', cursor: 'pointer', textAlign: 'left', letterSpacing: 0.3, transition: 'border-color .12s, color .12s' }}
            onMouseEnter={(e) => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.red; (e.currentTarget as HTMLButtonElement).style.color = T.red; }}
            onMouseLeave={(e) => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.border; (e.currentTarget as HTMLButtonElement).style.color = T.dim; }}>
            [ ./logout ]
          </button>
        </div>
      </aside>

      {/* Main content */}
      <main style={{ flex: 1, overflow: 'auto', display: 'flex', flexDirection: 'column' }}>
        {serviceDisabled ? (
          <div style={{ flex: 1, display: 'flex', alignItems: 'center', justifyContent: 'center', padding: 24 }}>
            <div style={{ textAlign: 'center', fontFamily: T.mono }}>
              <div style={{ fontSize: 14, color: T.textHi, marginBottom: 6 }}>{activeSegment}/ is disabled for this org</div>
              <div style={{ fontSize: 12, color: T.faint }}>an administrator can re-enable it in builder/</div>
            </div>
          </div>
        ) : (
          <Outlet />
        )}
      </main>
    </div>
  );
}

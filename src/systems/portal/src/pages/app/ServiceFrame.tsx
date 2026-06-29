/**
 * ServiceFrame — generic iframe host for a registered service's embedded
 * mini-portal. The shell renders this for any service that advertises a `ui_path`
 * and has no bundled page (see BUNDLED_SERVICES in AppLayout), so a non-core
 * service appears in the portal with zero changes to this repo.
 *
 * The iframe loads same-origin through the BFF (`/api/<svc>/ui/...`), which
 * forwards the bearer token on asset requests — no credential is handed to the
 * frame directly. The active theme name is passed as a query param for first paint
 * (the mini-portal reads `?theme=` to match the shell's palette).
 */
import { useParams } from 'react-router-dom';
import { useAppSelector } from '../../store/hooks';
import { getStoredTheme, T } from '../../theme';

export function ServiceFrame() {
  const { service } = useParams();
  const serviceUiPaths = useAppSelector((s) => s.auth.serviceUiPaths);
  const servicesResolved = useAppSelector((s) => s.auth.servicesResolved);
  const uiPath = service ? serviceUiPaths[service] : undefined;

  // Fail open: while the routing table is unresolved, show a loader rather than
  // flashing "not available" for a service that may well be registered.
  if (!servicesResolved) {
    return (
      <div style={{ flex: 1, display: 'flex', alignItems: 'center', justifyContent: 'center', fontFamily: T.mono, color: T.faint, fontSize: 12 }}>
        <span style={{ animation: 'pulse 1s ease-in-out infinite' }}>→ loading {service}/ · · ·</span>
      </div>
    );
  }

  if (!service || !uiPath) {
    return (
      <div style={{ flex: 1, display: 'flex', alignItems: 'center', justifyContent: 'center', padding: 24 }}>
        <div style={{ textAlign: 'center', fontFamily: T.mono }}>
          <div style={{ fontSize: 14, color: T.textHi, marginBottom: 6 }}>{service}/ is not available on this instance</div>
          <div style={{ fontSize: 12, color: T.faint }}>a system administrator can enable it in builder/</div>
        </div>
      </div>
    );
  }

  // Same-origin BFF path; the mini-portal mounts at <ui_path> under the service
  // prefix. Trailing slash so the mini-SPA resolves its relative asset URLs.
  const base = `/api/${service}${uiPath}`.replace(/\/+$/, '');
  const src = `${base}/?theme=${encodeURIComponent(getStoredTheme())}`;
  return (
    <iframe
      title={`${service} console`}
      src={src}
      style={{ flex: 1, width: '100%', height: '100%', border: 'none', background: T.bg }}
      // First-party mini-portal; allow it to run scripts, use same-origin storage,
      // submit forms, open links, and download — but keep the sandbox boundary.
      sandbox="allow-scripts allow-same-origin allow-forms allow-popups allow-downloads"
    />
  );
}

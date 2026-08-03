/**
 * ServiceFrame — generic iframe host for a registered service's embedded
 * mini-portal. The shell renders this for any service that advertises a `ui_path`
 * and has no bundled page (see BUNDLED_SERVICES in AppLayout), so a non-core
 * service appears in the portal with zero changes to this repo.
 *
 * The iframe loads through the BFF (`/api/<svc>/ui/...`), but is sandboxed WITHOUT
 * allow-same-origin, so the framed document runs in an opaque origin and cannot touch
 * the shell's storage, cookies or DOM — see the sandbox attribute below for why that
 * matters. The active theme name is passed as a query param for first paint (the
 * mini-portal reads `?theme=` to match the shell's palette).
 *
 * Consequence, and the remaining work: because the frame is no longer same-origin with
 * the shell, a mini-portal cannot read the session token out of the shell's
 * localStorage — which is how they have been authenticating — and its own fetches to
 * /api are cross-origin from a `null` origin. A mini-portal that needs to call the
 * platform API therefore needs a credential path that does not depend on sharing the
 * shell's origin. The durable fix is to serve mini-portals from a DISTINCT origin (a
 * second hostname fronted by the BFF), where allow-same-origin is safe again because
 * "same origin" no longer means the portal's; that needs an ingress/hostname change
 * outside this file. Handing the frame the shell's session token via postMessage would
 * NOT be a fix: it is the same full-privilege credential, just passed politely.
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
      // NO allow-same-origin. The src is same-origin (it goes through the BFF), so
      // "allow-scripts allow-same-origin" would not be a sandbox at all: the framed
      // document could reach into the parent, ride the session cookie against every
      // service, and even clear this very sandbox attribute on its own frame element.
      // A mini-portal is shipped by a builder-deployed service, i.e. code this repo
      // does not own.
      //
      // The session no longer sits in localStorage (see authSlice — it is an HttpOnly
      // cookie), which removes the read-the-token-out-of-storage path specifically.
      // It does NOT make same-origin framing safe: a same-origin document can simply
      // CALL /api/* and the browser attaches the cookie for it. The sandbox is what
      // denies that, so it stays.
      //
      // Without allow-same-origin the document loads into an opaque origin: it renders
      // and runs its own scripts, but has no access to the portal's storage, cookies or
      // DOM, and cannot lift the sandbox. The other tokens stay because they are not
      // what breaks the boundary (forms, links opened out of the frame, downloads).
      sandbox="allow-scripts allow-forms allow-popups allow-downloads"
    />
  );
}

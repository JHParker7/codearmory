/**
 * Settings hub — the home for everything that is NOT a project resource. The main
 * sidebar nav shows only a project's own resources; the project-independent
 * admin/account surfaces (account preferences, users & roles, project management,
 * builder, audit) live here behind a tab bar. Each tab renders the existing page
 * component inline; builder/audit tabs are permission-gated. The active tab is in
 * the URL (?tab=) so it deep-links and survives reloads.
 */
import { useAppSelector } from '../../store/hooks';
import { useUrlParam } from '../../hooks/useUrlState';
import { T } from '../../theme';
import { Settings } from './Settings';
import { Gatekeeper } from './Gatekeeper';
import { Projects } from './Projects';
import { Builder } from './Builder';
import { Audit } from './Audit';

type Tab = 'account' | 'users' | 'projects' | 'builder' | 'audit';

export function SettingsHub() {
  const permissions = useAppSelector(s => s.auth.permissions);
  const [tab, setTab] = useUrlParam('tab');
  const canBuilder = !!permissions?.['builder:configureOrgService'];
  const canAudit = !!permissions?.['gatekeeper:listAuditLog'];

  const allTabs: { id: Tab; label: string; show: boolean }[] = [
    { id: 'account', label: 'account', show: true },
    { id: 'users', label: 'users & roles', show: true },
    { id: 'projects', label: 'projects', show: true },
    { id: 'builder', label: 'builder', show: canBuilder },
    { id: 'audit', label: 'audit', show: canAudit },
  ];
  const tabs = allTabs.filter(t => t.show);

  const active = (tabs.some(t => t.id === tab) ? tab : 'account') as Tab;

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100%', overflow: 'hidden' }}>
      <div style={{ display: 'flex', gap: 2, borderBottom: `1px solid ${T.border}`, padding: '0 12px', background: T.bgAlt, flexShrink: 0, overflowX: 'auto' }}>
        {tabs.map(t => (
          <button key={t.id} onClick={() => setTab(t.id === 'account' ? null : t.id)}
            style={{ background: 'transparent', border: 0, borderBottom: `2px solid ${active === t.id ? T.green : 'transparent'}`, color: active === t.id ? T.textHi : T.dim, fontFamily: T.mono, fontSize: 12.5, padding: '11px 13px', cursor: 'pointer', whiteSpace: 'nowrap' }}>
            {t.label}
          </button>
        ))}
      </div>
      <div style={{ flex: 1, overflow: 'auto', minHeight: 0 }}>
        {active === 'account' && <Settings />}
        {active === 'users' && <Gatekeeper />}
        {active === 'projects' && <Projects />}
        {active === 'builder' && canBuilder && <Builder />}
        {active === 'audit' && canAudit && <Audit />}
      </div>
    </div>
  );
}

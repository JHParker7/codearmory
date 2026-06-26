/**
 * Builder is the SYSTEM-ADMIN-only global service control plane: the admin toggles
 * platform services on/off for the whole instance, edits their free-form config,
 * and registers custom services — all against the single "default" baseline (no
 * per-org scope). Enabling a service deploys it; disabling tears it down. Core
 * control-plane services can never be configured. Access is enforced server-side —
 * the sidebar only links here for the system admin (builder:configureOrgService on
 * builder/orgs/default, which only the wildcard admin matches).
 */
import { useState, useEffect, useCallback } from 'react';
import type { ReactNode } from 'react';
import { T } from '../../theme';
import { Pill } from '../../components/Pill';
import { useConfirm } from '../../components/ConfirmDialog';
import { useAppSelector, useAppDispatch } from '../../store/hooks';
import { hydrateRegisteredServices } from '../../store/authSlice';
import { listServices, setService, deleteService } from '../../api/bff';
import type { OrgService, SetOrgServiceBody } from '../../api/bff';

const inputStyle = {
  background: 'transparent' as const,
  border: `1px solid ${T.border}`,
  color: T.text,
  fontFamily: T.mono,
  fontSize: 12,
  padding: '7px 10px',
  outline: 'none',
  width: '100%',
  boxSizing: 'border-box' as const,
};

/**
 * Control-plane services that can never be toggled or configured. The server flags
 * these with `core` on every row (list and single-service reads alike), so we trust
 * that flag rather than mirroring builder's coreServices set here.
 */
const isCore = (s: OrgService) => !!s.core;

/**
 * A row is removable only when an actual stored baseline row exists for it — a
 * configured platform service (source "default") or a registered custom service
 * (source "custom"). Catalog defaults and core services are not removable.
 */
function isDeletable(s: OrgService): boolean {
  if (isCore(s)) return false;
  return s.source === 'default' || s.source === 'custom';
}

/**
 * parseConfigText turns the textarea into a config object. Empty → undefined
 * (config omitted). Throws on invalid JSON or a non-object so the caller can
 * surface the message.
 */
function parseConfigText(text: string): Record<string, unknown> | undefined {
  const t = text.trim();
  if (!t) return undefined;
  const v = JSON.parse(t);
  if (v === null || typeof v !== 'object' || Array.isArray(v)) throw new Error('config must be a JSON object');
  return v as Record<string, unknown>;
}

/**
 * parseSecretsText turns the secrets textarea into a write-only map of env-key → value.
 * Empty → undefined (keep existing). Every value must be a string.
 */
function parseSecretsText(text: string): Record<string, string> | undefined {
  const t = text.trim();
  if (!t) return undefined;
  const v = JSON.parse(t);
  if (v === null || typeof v !== 'object' || Array.isArray(v)) throw new Error('secrets must be a JSON object');
  const out: Record<string, string> = {};
  for (const [k, val] of Object.entries(v)) {
    if (typeof val !== 'string') throw new Error(`secret "${k}" must be a string value`);
    out[k] = val;
  }
  return out;
}

/** labelled form field wrapper: renders an uppercase mono label above its children. */
function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div style={{ marginBottom: 12 }}>
      <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, marginBottom: 6, letterSpacing: 0.5, textTransform: 'uppercase' }}>{label}</div>
      {children}
    </div>
  );
}

interface FormState {
  service: string;
  enabled: boolean;
  kind: string;
  image: string;
  port: string;
  description: string;
  config: string;
  db_url: string;
  secrets: string;
}

const BLANK_FORM: FormState = { service: '', enabled: true, kind: 'custom', image: '', port: '', description: '', config: '', db_url: '', secrets: '' };

/** Builder route: system-admin global service control plane. Left list of every platform/custom/core service against the single "default" baseline; right pane shows detail with enable/disable, configure, and remove-baseline actions, plus the register/edit form (formBlock) that submits enabled/kind/image/port/config/db_url/secrets via setService. */
export function Builder() {
  const token = useAppSelector(s => s.auth.token)!;
  const dispatch = useAppDispatch();

  // Builder manages a single global baseline for the whole instance — there is no
  // per-org scope. Every call hits the global /builder/services endpoint.
  const [services, setServices] = useState<OrgService[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [confirm, confirmEl] = useConfirm();

  const [editing, setEditing] = useState(false);
  const [creating, setCreating] = useState(false);
  const [form, setForm] = useState<FormState>(BLANK_FORM);
  const [formError, setFormError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  const fetchServices = useCallback(async (): Promise<OrgService[] | null> => {
    setLoading(true); setError(null);
    try {
      const list = await listServices(token);
      setServices(list);
      return list;
    } catch (e: unknown) {
      setError((e as Error).message); setServices([]); return null;
    } finally { setLoading(false); }
  }, [token]);

  useEffect(() => { fetchServices(); }, [fetchServices]);
  // Leaving a selection cancels any in-progress edit/confirm.
  useEffect(() => { setEditing(false); }, [selected]);

  const selectedSvc = services.find(s => s.service === selected) ?? null;

  // After any mutation: reload the baseline list, then re-resolve the sidebar's
  // routing table. Enabling a service makes builder deploy+register it; disabling
  // tears it down+unregisters — both reach the sidebar once conductor refreshes,
  // which this nudges by re-fetching.
  const afterMutation = async () => {
    await fetchServices();
    dispatch(hydrateRegisteredServices());
  };

  const toggle = async (svc: OrgService) => {
    const body: SetOrgServiceBody = { enabled: !svc.enabled, kind: svc.kind || 'platform' };
    if (svc.config && Object.keys(svc.config).length > 0) body.config = svc.config;
    if (svc.kind === 'custom') { body.image = svc.image; body.port = svc.port; body.description = svc.description; }
    setBusy(svc.service); setError(null);
    try { await setService(token, svc.service, body); await afterMutation(); }
    catch (e: unknown) { setError((e as Error).message); }
    finally { setBusy(null); }
  };

  const remove = async (svc: OrgService) => {
    if (!(await confirm({ message: `Remove the ${svc.service} baseline? The service will be deregistered and undeployed.`, confirmLabel: 'remove baseline' }))) return;
    setBusy(svc.service); setError(null);
    try {
      await deleteService(token, svc.service);
      if (selected === svc.service) setSelected(null);
      await afterMutation();
    } catch (e: unknown) { setError((e as Error).message); }
    finally { setBusy(null); }
  };

  const startEdit = (svc: OrgService) => {
    setForm({
      service: svc.service,
      enabled: svc.enabled,
      kind: svc.kind || 'platform',
      image: svc.image ?? '',
      port: svc.port ? String(svc.port) : '',
      description: svc.description ?? '',
      config: svc.config && Object.keys(svc.config).length ? JSON.stringify(svc.config, null, 2) : '',
      db_url: '',
      secrets: '',
    });
    setFormError(null); setCreating(false); setEditing(true);
  };

  const startCreate = () => {
    setForm(BLANK_FORM); setFormError(null); setSelected(null); setEditing(false); setCreating(true);
  };

  const submit = async (mode: 'edit' | 'create') => {
    const name = form.service.trim();
    if (!name) { setFormError('service name is required'); return; }
    const kind = mode === 'create' ? 'custom' : (form.kind || 'platform');

    let config: Record<string, unknown> | undefined;
    try { config = parseConfigText(form.config); }
    catch (e: unknown) { setFormError((e as Error).message); return; }

    let secrets: Record<string, string> | undefined;
    try { secrets = parseSecretsText(form.secrets); }
    catch (e: unknown) { setFormError((e as Error).message); return; }

    if (kind === 'custom') {
      if (!form.image.trim()) { setFormError('custom services require an image'); return; }
      const p = Number(form.port);
      if (!Number.isInteger(p) || p < 1 || p > 65535) { setFormError('custom services require a valid port (1-65535)'); return; }
    }

    const body: SetOrgServiceBody = { enabled: form.enabled, kind };
    if (config !== undefined) body.config = config;
    if (kind === 'custom') {
      body.image = form.image.trim();
      body.port = Number(form.port);
    }
    if (form.description.trim()) body.description = form.description.trim();
    if (form.db_url.trim()) body.db_url = form.db_url.trim();
    if (secrets !== undefined) body.secrets = secrets;

    setSaving(true); setFormError(null);
    try {
      await setService(token, name, body);
      setEditing(false); setCreating(false);
      setSelected(name);
      await afterMutation();
    } catch (e: unknown) { setFormError((e as Error).message); }
    finally { setSaving(false); }
  };

  /** renders the register/edit form body (image/port for custom, config/db_url/secrets); its submit button calls submit(mode). */
  const formBlock = (mode: 'edit' | 'create') => {
    const custom = mode === 'create' || form.kind === 'custom';
    return (
      <div style={{ background: T.card, border: `1px solid ${T.borderHi}`, padding: '16px', marginBottom: 20 }}>
        {formError && <div style={{ background: T.redSoft, border: `1px solid ${T.red}`, padding: '8px 12px', fontFamily: T.mono, fontSize: 11, color: T.red, marginBottom: 12 }}>{formError}</div>}
        {mode === 'create' && (
          <Field label="service name">
            <input value={form.service} autoFocus placeholder="my-service" onChange={e => setForm(f => ({ ...f, service: e.target.value }))} style={{ ...inputStyle, background: T.cardHi }} />
          </Field>
        )}
        <Field label="enabled">
          <div style={{ display: 'flex', gap: 8 }}>
            {([true, false] as const).map(v => (
              <button key={String(v)} onClick={() => setForm(f => ({ ...f, enabled: v }))}
                style={{ background: form.enabled === v ? T.greenSoft : 'transparent', border: `1px solid ${form.enabled === v ? T.green : T.border}`, color: form.enabled === v ? T.green : T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 14px', cursor: 'pointer' }}>
                {v ? 'enabled' : 'disabled'}
              </button>
            ))}
          </div>
        </Field>
        {custom && (
          <div style={{ display: 'grid', gridTemplateColumns: '2fr 1fr', gap: 12 }}>
            <Field label="image">
              <input value={form.image} placeholder="ghcr.io/org/image:tag" onChange={e => setForm(f => ({ ...f, image: e.target.value }))} style={{ ...inputStyle, background: T.cardHi }} />
            </Field>
            <Field label="port">
              <input value={form.port} placeholder="8080" inputMode="numeric" onChange={e => setForm(f => ({ ...f, port: e.target.value }))} style={{ ...inputStyle, background: T.cardHi }} />
            </Field>
          </div>
        )}
        {custom && (
          <Field label="description">
            <input value={form.description} placeholder="optional" onChange={e => setForm(f => ({ ...f, description: e.target.value }))} style={{ ...inputStyle, background: T.cardHi }} />
          </Field>
        )}
        <Field label="config · JSON object, optional">
          <textarea value={form.config} rows={5} placeholder={'{\n  "key": "value"\n}'} onChange={e => setForm(f => ({ ...f, config: e.target.value }))}
            style={{ ...inputStyle, background: T.cardHi, resize: 'vertical' }} />
        </Field>
        <Field label="db url · write-only, postgres://…">
          <input value={form.db_url} type="password" placeholder="leave blank to keep" onChange={e => setForm(f => ({ ...f, db_url: e.target.value }))} style={{ ...inputStyle, background: T.cardHi }} />
        </Field>
        <Field label="secrets · write-only JSON, encrypted (e.g. REDIS_URL, GITEA_ADMIN_TOKEN)">
          <textarea value={form.secrets} rows={4} placeholder={'leave blank to keep · {\n  "REDIS_URL": "redis://…"\n}'} onChange={e => setForm(f => ({ ...f, secrets: e.target.value }))}
            style={{ ...inputStyle, background: T.cardHi, resize: 'vertical' }} />
        </Field>
        <div style={{ display: 'flex', gap: 8 }}>
          <button onClick={() => submit(mode)} disabled={saving}
            style={{ background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 12, fontWeight: 600, padding: '7px 16px', cursor: 'pointer', opacity: saving ? 0.6 : 1 }}>
            {saving ? '[ · · · ]' : mode === 'create' ? '[ register ]' : '[ save ]'}
          </button>
          <button onClick={() => { setEditing(false); setCreating(false); }} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '7px 12px', cursor: 'pointer' }}>cancel</button>
        </div>
      </div>
    );
  };

  return (
    <div style={{ flex: 1, display: 'flex', flexDirection: 'column', overflow: 'hidden' }}>
      {confirmEl}
      {/* Header */}
      <div style={{ padding: '16px 24px', borderBottom: `1px solid ${T.border}`, display: 'flex', alignItems: 'center', justifyContent: 'space-between', flexWrap: 'wrap', gap: 12 }}>
        <div>
          <div style={{ fontFamily: T.mono, fontSize: 16, fontWeight: 700, color: T.textHi }}>builder/</div>
          <div style={{ fontFamily: T.mono, fontSize: 11, color: T.faint, marginTop: 2 }}>global service control plane</div>
        </div>
        <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
          <button onClick={startCreate}
            style={{ background: creating ? T.greenSoft : 'transparent', border: `1px solid ${creating ? T.green : T.border}`, color: creating ? T.green : T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}>
            + register custom
          </button>
          <button onClick={fetchServices} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 10px', cursor: 'pointer' }}>↻</button>
        </div>
      </div>

      <div style={{ padding: '8px 24px', borderBottom: `1px solid ${T.border}`, background: T.amberSoft, fontFamily: T.mono, fontSize: 11, color: T.amber }}>
        editing the global service baseline for this instance — system-admin only · enabling a service deploys it, disabling tears it down
      </div>

      <div style={{ display: 'flex', flex: 1, overflow: 'hidden' }}>
        {/* List */}
        <div style={{ width: 280, flexShrink: 0, borderRight: `1px solid ${T.border}`, display: 'flex', flexDirection: 'column', background: T.bgAlt, overflow: 'auto' }}>
          <div style={{ padding: '12px 14px', borderBottom: `1px solid ${T.border}` }}>
            <span style={{ fontFamily: T.mono, fontSize: 10, color: T.faint }}>{services.length > 0 ? `${services.length} service${services.length !== 1 ? 's' : ''}` : ''}</span>
          </div>
          {loading ? <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint, animation: 'pulse 1s ease-in-out infinite' }}>→ loading · · ·</div>
            : error ? <div style={{ padding: '14px', fontFamily: T.mono, fontSize: 11, color: T.red }}>{error}</div>
            : services.length === 0 ? <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint }}>→ no services</div>
            : services.map(svc => {
              const active = selected === svc.service && !creating;
              const core = isCore(svc);
              return (
                <button key={svc.service} onClick={() => { setSelected(svc.service); setCreating(false); }}
                  style={{ width: '100%', textAlign: 'left', padding: '10px 14px', background: active ? T.greenSoft : 'transparent', border: 0, borderLeft: `2px solid ${active ? T.green : 'transparent'}`, fontFamily: T.mono, cursor: 'pointer', color: T.text, display: 'block', transition: 'background .12s' }}>
                  <div style={{ display: 'flex', alignItems: 'center', gap: 6, marginBottom: 3 }}>
                    <span style={{ width: 6, height: 6, borderRadius: 3, flexShrink: 0, background: core ? T.dim : svc.enabled ? T.green : T.red }} />
                    <span style={{ fontSize: 13, fontWeight: 600, color: active ? T.textHi : T.text }}>{svc.service}</span>
                    {svc.kind === 'custom' && <span style={{ fontSize: 9, color: T.blue, border: `1px solid ${T.blue}`, padding: '0 4px' }}>custom</span>}
                    {core && <span style={{ fontSize: 9, color: T.dim, border: `1px solid ${T.dim}`, padding: '0 4px' }}>core</span>}
                  </div>
                  <div style={{ fontSize: 10, color: T.faint }}>{core ? 'always on' : svc.enabled ? 'enabled' : 'disabled'} · {svc.source}</div>
                </button>
              );
            })}
        </div>

        {/* Detail */}
        <div style={{ flex: 1, overflow: 'auto' }}>
          {creating ? (
            <div style={{ padding: '20px 24px' }}>
              <div style={{ fontFamily: T.mono, fontSize: 16, fontWeight: 700, color: T.textHi, marginBottom: 16 }}>register custom service</div>
              {formBlock('create')}
            </div>
          ) : !selectedSvc ? (
            <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', height: '100%' }}>
              <div style={{ fontFamily: T.mono, fontSize: 12, color: T.faint }}>→ select a service</div>
            </div>
          ) : (
            <div style={{ padding: '20px 24px' }}>
              <div style={{ display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between', marginBottom: 20, gap: 12, flexWrap: 'wrap' }}>
                <div>
                  <div style={{ fontFamily: T.mono, fontSize: 20, fontWeight: 700, color: T.textHi, marginBottom: 4 }}>{selectedSvc.service}</div>
                  <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
                    <span style={{ fontFamily: T.mono, fontSize: 11, color: T.faint }}>kind {selectedSvc.kind}</span>
                    <span style={{ fontFamily: T.mono, fontSize: 11, color: T.faint }}>· source {selectedSvc.source}</span>
                  </div>
                </div>
                <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
                  {isCore(selectedSvc)
                    ? <Pill tone="dim">core</Pill>
                    : <Pill tone={selectedSvc.enabled ? 'green' : 'red'}>{selectedSvc.enabled ? 'enabled' : 'disabled'}</Pill>}
                  {!isCore(selectedSvc) && (
                    <button onClick={() => toggle(selectedSvc)} disabled={busy === selectedSvc.service}
                      style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer', opacity: busy === selectedSvc.service ? 0.6 : 1 }}>
                      {busy === selectedSvc.service ? '[ · · · ]' : selectedSvc.enabled ? '[ disable ]' : '[ enable ]'}
                    </button>
                  )}
                  {!isCore(selectedSvc) && (
                    <button onClick={() => editing ? setEditing(false) : startEdit(selectedSvc)}
                      style={{ background: editing ? T.greenSoft : 'transparent', border: `1px solid ${editing ? T.green : T.border}`, color: editing ? T.green : T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}>
                      {editing ? '[ cancel ]' : '[ configure ]'}
                    </button>
                  )}
                  {isDeletable(selectedSvc) && (
                    <button onClick={() => remove(selectedSvc)} disabled={busy === selectedSvc.service}
                      style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}
                      onMouseEnter={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.red; (e.currentTarget as HTMLButtonElement).style.color = T.red; }}
                      onMouseLeave={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.border; (e.currentTarget as HTMLButtonElement).style.color = T.dim; }}>
                      {busy === selectedSvc.service ? '[ · · · ]' : '[ remove baseline ]'}
                    </button>
                  )}
                </div>
              </div>

              {error && <div style={{ background: T.redSoft, border: `1px solid ${T.red}`, padding: '8px 12px', fontFamily: T.mono, fontSize: 11, color: T.red, marginBottom: 16 }}>{error}</div>}

              {isCore(selectedSvc) && (
                <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '12px 14px', fontFamily: T.mono, fontSize: 12, color: T.faint, marginBottom: 16 }}>
                  → control-plane service · always enabled and cannot be configured
                </div>
              )}

              {editing ? formBlock('edit') : (
                <>
                  <div style={{ display: 'grid', gridTemplateColumns: 'repeat(2, 1fr)', gap: 12, marginBottom: selectedSvc.kind === 'custom' || selectedSvc.db_configured ? 16 : 0 }}>
                    {selectedSvc.kind === 'custom' && (
                      <>
                        <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '10px 14px' }}>
                          <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 4, textTransform: 'uppercase' }}>image</div>
                          <div style={{ fontFamily: T.mono, fontSize: 12, color: T.textHi, wordBreak: 'break-all' }}>{selectedSvc.image || '—'}</div>
                        </div>
                        <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '10px 14px' }}>
                          <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 4, textTransform: 'uppercase' }}>port</div>
                          <div style={{ fontFamily: T.mono, fontSize: 12, color: T.textHi }}>{selectedSvc.port || '—'}</div>
                        </div>
                      </>
                    )}
                    {selectedSvc.db_configured && (
                      <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '10px 14px', gridColumn: '1 / -1' }}>
                        <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 4, textTransform: 'uppercase' }}>database</div>
                        <div style={{ fontFamily: T.mono, fontSize: 12, color: T.textHi }}>{selectedSvc.db_host || 'configured'} <span style={{ color: T.faint }}>· url stored, write-only</span></div>
                      </div>
                    )}
                  </div>

                  {selectedSvc.description && (
                    <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '10px 14px', fontFamily: T.mono, fontSize: 12, color: T.text, marginBottom: 16 }}>{selectedSvc.description}</div>
                  )}

                  <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 8 }}>CONFIG</div>
                  {selectedSvc.config && Object.keys(selectedSvc.config).length > 0 ? (
                    <pre style={{ background: T.card, border: `1px solid ${T.border}`, padding: '12px 14px', fontFamily: T.mono, fontSize: 12, color: T.text, margin: 0, overflow: 'auto', whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>{JSON.stringify(selectedSvc.config, null, 2)}</pre>
                  ) : (
                    <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '12px 14px', fontFamily: T.mono, fontSize: 12, color: T.faint }}>→ no config set</div>
                  )}
                </>
              )}
            </div>
          )}
        </div>
      </div>
    </div>
  );
}

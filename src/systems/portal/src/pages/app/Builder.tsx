import { useState, useEffect, useCallback } from 'react';
import type { ReactNode } from 'react';
import { T } from '../../theme';
import { Pill } from '../../components/Pill';
import { useAppSelector, useAppDispatch } from '../../store/hooks';
import { hydrateServices, setDisabledServices } from '../../store/authSlice';
import { listOrgServices, setOrgService, deleteOrgService } from '../../api/bff';
import type { OrgService, SetOrgServiceBody } from '../../api/bff';

// Builder is the per-org service control plane: admins toggle platform services
// on/off, edit their free-form config, register custom services, and (platform
// admins) edit the "default" baseline every org inherits. Core control-plane
// services can never be configured. Access is enforced server-side — the sidebar
// only links here when the caller holds builder:configureOrgService on its org.

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

// Control-plane services that can never be toggled or configured. The server flags
// these with `core` on every row (list and single-service reads alike), so we trust
// that flag rather than mirroring builder's coreServices set here.
const isCore = (s: OrgService) => !!s.core;

// A row is removable only when an actual override exists at the current scope:
// at the org scope that means source override/custom; at the default scope the
// baseline rows themselves (source default/custom) are the stored ones.
function isDeletable(s: OrgService, scope: 'org' | 'default'): boolean {
  if (isCore(s)) return false;
  return scope === 'default'
    ? s.source === 'default' || s.source === 'custom'
    : s.source === 'override' || s.source === 'custom';
}

// parseConfigText turns the textarea into a config object. Empty → undefined
// (config omitted). Throws on invalid JSON or a non-object so the caller can
// surface the message.
function parseConfigText(text: string): Record<string, unknown> | undefined {
  const t = text.trim();
  if (!t) return undefined;
  const v = JSON.parse(t);
  if (v === null || typeof v !== 'object' || Array.isArray(v)) throw new Error('config must be a JSON object');
  return v as Record<string, unknown>;
}

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
}

const BLANK_FORM: FormState = { service: '', enabled: true, kind: 'custom', image: '', port: '', description: '', config: '', db_url: '' };

export function Builder() {
  const token = useAppSelector(s => s.auth.token)!;
  const orgId = useAppSelector(s => s.auth.user?.org_id);
  const dispatch = useAppDispatch();

  const [scope, setScope] = useState<'org' | 'default'>('org');
  const [services, setServices] = useState<OrgService[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [pendingDelete, setPendingDelete] = useState<string | null>(null);

  const [editing, setEditing] = useState(false);
  const [creating, setCreating] = useState(false);
  const [form, setForm] = useState<FormState>(BLANK_FORM);
  const [formError, setFormError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  const scopeId = scope === 'default' ? 'default' : orgId;

  const fetchServices = useCallback(async (): Promise<OrgService[] | null> => {
    if (!scopeId) { setLoading(false); setServices([]); return null; }
    setLoading(true); setError(null);
    try {
      const list = await listOrgServices(token, scopeId);
      setServices(list);
      return list;
    } catch (e: unknown) {
      setError((e as Error).message); setServices([]); return null;
    } finally { setLoading(false); }
  }, [token, scopeId]);

  useEffect(() => { fetchServices(); }, [fetchServices]);
  // Reset transient UI whenever the scope flips.
  useEffect(() => { setSelected(null); setEditing(false); setCreating(false); setPendingDelete(null); }, [scope]);
  // Leaving a selection cancels any in-progress edit/confirm.
  useEffect(() => { setEditing(false); setPendingDelete(null); }, [selected]);

  const selectedSvc = services.find(s => s.service === selected) ?? null;

  // After any mutation: reload this scope and refresh the sidebar's disabled set so a
  // service turned off here disappears from the nav (and back when re-enabled). At the
  // caller's own org scope the freshly-fetched list IS the sidebar's source, so reuse
  // it directly; a default-scope edit changes every org's effective view, so re-derive
  // the user's org from the server.
  const afterMutation = async () => {
    const list = await fetchServices();
    if (scope === 'org' && list) {
      dispatch(setDisabledServices(list.filter(s => !s.core && !s.enabled).map(s => s.service)));
    } else {
      dispatch(hydrateServices());
    }
  };

  const toggle = async (svc: OrgService) => {
    const body: SetOrgServiceBody = { enabled: !svc.enabled, kind: svc.kind || 'platform' };
    if (svc.config && Object.keys(svc.config).length > 0) body.config = svc.config;
    if (svc.kind === 'custom') { body.image = svc.image; body.port = svc.port; body.description = svc.description; }
    setBusy(svc.service); setError(null);
    try { await setOrgService(token, scopeId!, svc.service, body); await afterMutation(); }
    catch (e: unknown) { setError((e as Error).message); }
    finally { setBusy(null); }
  };

  const remove = async (svc: OrgService) => {
    setBusy(svc.service); setError(null);
    try {
      await deleteOrgService(token, scopeId!, svc.service);
      if (selected === svc.service) setSelected(null);
      setPendingDelete(null);
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

    setSaving(true); setFormError(null);
    try {
      await setOrgService(token, scopeId!, name, body);
      setEditing(false); setCreating(false);
      setSelected(name);
      await afterMutation();
    } catch (e: unknown) { setFormError((e as Error).message); }
    finally { setSaving(false); }
  };

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
      {/* Header */}
      <div style={{ padding: '16px 24px', borderBottom: `1px solid ${T.border}`, display: 'flex', alignItems: 'center', justifyContent: 'space-between', flexWrap: 'wrap', gap: 12 }}>
        <div>
          <div style={{ fontFamily: T.mono, fontSize: 16, fontWeight: 700, color: T.textHi }}>builder/</div>
          <div style={{ fontFamily: T.mono, fontSize: 11, color: T.faint, marginTop: 2 }}>per-org service control plane</div>
        </div>
        <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
          <div style={{ display: 'flex', gap: 6, marginRight: 4 }}>
            {([['org', 'this org'], ['default', 'default baseline']] as const).map(([s, label]) => (
              <button key={s} onClick={() => setScope(s)}
                style={{ background: scope === s ? T.greenSoft : 'transparent', border: `1px solid ${scope === s ? T.green : T.border}`, color: scope === s ? T.green : T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}>
                {label}
              </button>
            ))}
          </div>
          <button onClick={startCreate}
            style={{ background: creating ? T.greenSoft : 'transparent', border: `1px solid ${creating ? T.green : T.border}`, color: creating ? T.green : T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}>
            + register custom
          </button>
          <button onClick={fetchServices} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 10px', cursor: 'pointer' }}>↻</button>
        </div>
      </div>

      {scope === 'default' && (
        <div style={{ padding: '8px 24px', borderBottom: `1px solid ${T.border}`, background: T.amberSoft, fontFamily: T.mono, fontSize: 11, color: T.amber }}>
          editing the platform-wide baseline every org inherits — platform-admin only
        </div>
      )}

      {scope === 'org' && (
        <div style={{ padding: '8px 24px', borderBottom: `1px solid ${T.border}`, background: T.card, fontFamily: T.mono, fontSize: 11, color: T.faint }}>
          this org's toggles control which services it sees · what is actually deployed (config, replicas, rotation, image/port) is governed by the default baseline
        </div>
      )}

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
                  {isDeletable(selectedSvc, scope) && (
                    pendingDelete === selectedSvc.service ? (
                      <>
                        <button onClick={() => remove(selectedSvc)} disabled={busy === selectedSvc.service}
                          style={{ background: T.redSoft, border: `1px solid ${T.red}`, color: T.red, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}>
                          {busy === selectedSvc.service ? '[ · · · ]' : '[ confirm remove ]'}
                        </button>
                        <button onClick={() => setPendingDelete(null)} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 10px', cursor: 'pointer' }}>✕</button>
                      </>
                    ) : (
                      <button onClick={() => setPendingDelete(selectedSvc.service)}
                        style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}
                        onMouseEnter={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.red; (e.currentTarget as HTMLButtonElement).style.color = T.red; }}
                        onMouseLeave={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.border; (e.currentTarget as HTMLButtonElement).style.color = T.dim; }}>
                        [ remove {scope === 'default' ? 'baseline' : 'override'} ]
                      </button>
                    )
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

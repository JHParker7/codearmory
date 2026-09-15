/**
 * Notifications page: manage the delivery CHANNELS the notifications service fans
 * platform events out to. A channel is a provider (Slack/Discord/webhook/email)
 * plus the config that provider needs and the set of event types it wants — so a
 * team can get a Slack ping when a PR opens or a run fails. Left is the channel
 * list plus a "new" button; right is the selected channel's editor (Save = POST
 * for new / PUT for existing, a confirm-gated delete, and a live "test" that fires
 * a real delivery). All calls go through the BFF to /api/notifications/*.
 *
 * The config fields shown are DATA-DRIVEN: GET /providers returns each provider's
 * config_keys, so adding a provider server-side needs no portal change here.
 */
import { useState, useEffect, useCallback, useMemo } from 'react';
import { useUrlParam } from '../../hooks/useUrlState';
import { T } from '../../theme';
import { useResizableWidth } from '../../components/ResizeHandle';
import { useConfirm } from '../../components/ConfirmDialog';
import { useAppSelector } from '../../store/hooks';
import {
  listNotifProviders, listNotifChannels, createNotifChannel,
  updateNotifChannel, deleteNotifChannel, testNotifChannel,
} from '../../api/bff';
import type { NotifProvider, NotifChannel, NotifChannelInput } from '../../api/bff';

// Event types offered as one-click subscriptions. Matching is PREFIX-based in the
// service (wantsEvent), so `repo.pull_request.` catches every PR action and `*`
// catches everything. Free-text entry covers anything not listed. Names match the
// emitters: git-factory/events.go, events service.
const EVENT_SUGGESTIONS: { id: string; label: string }[] = [
  { id: '*', label: 'everything' },
  { id: 'repo.pull_request.opened', label: 'PR opened' },
  { id: 'repo.pull_request.merged', label: 'PR merged' },
  { id: 'repo.pull_request.closed', label: 'PR closed' },
  { id: 'repo.pull_request.', label: 'any PR event' },
  { id: 'repo.push', label: 'push' },
  { id: 'build.failed', label: 'build failed' },
];

// A per-provider hint for the one config field so the form is self-explanatory.
const CONFIG_HINT: Record<string, Record<string, string>> = {
  slack: { url: 'Slack incoming-webhook URL (https://hooks.slack.com/services/…)' },
  discord: { url: 'Discord channel webhook URL (https://discord.com/api/webhooks/…)' },
  webhook: { url: 'HTTPS endpoint to POST the raw event to' },
  email: { to: 'recipient email address (SMTP is configured service-side)' },
};

function blankInput(project: string | null): NotifChannelInput {
  return { name: '', provider: 'slack', config: {}, events: ['*'], project: project ?? '', enabled: true };
}

const inputStyle = { width: '100%', background: T.cardHi, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 12, padding: '6px 8px', outline: 'none', boxSizing: 'border-box' as const };
const labelStyle = { fontFamily: T.mono, fontSize: 10, color: T.faint, marginBottom: 4, marginTop: 12 };

/** Editor for one channel (existing or new). Holds a local draft; saves via POST/PUT. */
function ChannelEditor({ initial, id, providers, project, onSaved, onDeleted }: {
  initial: NotifChannelInput; id: string | null; providers: NotifProvider[];
  project: string | null;
  onSaved: (c: NotifChannel) => void; onDeleted: (id: string) => void;
}) {
  const token = useAppSelector(s => s.auth.token)!;
  const [draft, setDraft] = useState<NotifChannelInput>(initial);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [testMsg, setTestMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const [testing, setTesting] = useState(false);
  const [newEvent, setNewEvent] = useState('');
  const [confirm, confirmEl] = useConfirm();
  const isNew = id === null;

  useEffect(() => { setDraft(initial); setError(null); setTestMsg(null); }, [initial]);

  const set = <K extends keyof NotifChannelInput>(k: K, v: NotifChannelInput[K]) => setDraft(d => ({ ...d, [k]: v }));
  const setConfig = (k: string, v: string) => setDraft(d => ({ ...d, config: { ...d.config, [k]: v } }));

  const prov = providers.find(p => p.type === draft.provider);
  const configKeys = prov?.config_keys ?? ['url'];
  const requiredKey = configKeys[0];

  const toggleEvent = (id: string) => setDraft(d => {
    const has = d.events.includes(id);
    // Picking a specific event clears the catch-all "*", and vice versa, so the two
    // never fight; an empty set is normalised to "*" server-side anyway.
    let next = has ? d.events.filter(e => e !== id) : [...d.events.filter(e => e !== '*'), id];
    if (id === '*') next = ['*'];
    return { ...d, events: next.length ? next : ['*'] };
  });

  const addEvent = () => {
    const e = newEvent.trim();
    if (!e) return;
    setDraft(d => ({ ...d, events: [...d.events.filter(x => x !== '*' && x !== e), e] }));
    setNewEvent('');
  };

  const valid = draft.name.trim() && draft.provider && (draft.config[requiredKey] ?? '').trim();

  const handleSave = async () => {
    if (!valid) return;
    setSaving(true);
    setError(null);
    try {
      const payload: NotifChannelInput = { ...draft, name: draft.name.trim() };
      const saved = id ? await updateNotifChannel(token, id, payload) : await createNotifChannel(token, payload);
      onSaved(saved);
    } catch (e: unknown) {
      setError((e as Error).message);
    } finally {
      setSaving(false);
    }
  };

  const handleTest = async () => {
    if (isNew) { setTestMsg({ ok: false, text: 'Save the channel first, then test it.' }); return; }
    setTesting(true);
    setTestMsg(null);
    try {
      await testNotifChannel(token, id!);
      setTestMsg({ ok: true, text: '✅ test message delivered — check the channel.' });
    } catch (e: unknown) {
      setTestMsg({ ok: false, text: (e as Error).message || 'delivery failed' });
    } finally {
      setTesting(false);
    }
  };

  const handleDelete = async () => {
    if (!id) return;
    if (!(await confirm({ message: `Delete channel "${draft.name}"? It will stop receiving notifications immediately.` }))) return;
    try {
      await deleteNotifChannel(token, id);
      onDeleted(id);
    } catch (e: unknown) {
      setError((e as Error).message);
    }
  };

  return (
    <div style={{ flex: 1, overflow: 'auto', padding: '16px 20px' }}>
      {confirmEl}
      {error && <div style={{ background: T.redSoft, border: `1px solid ${T.red}`, padding: '8px 12px', fontFamily: T.mono, fontSize: 11, color: T.red, marginBottom: 12 }}>{error}</div>}

      <div style={{ display: 'flex', gap: 8, alignItems: 'center', marginBottom: 6 }}>
        <input value={draft.name} onChange={e => set('name', e.target.value)} placeholder="channel name (e.g. team-alerts)"
          style={{ ...inputStyle, flex: 1, fontSize: 14, fontWeight: 600 }} />
        <label style={{ display: 'flex', alignItems: 'center', gap: 6, fontFamily: T.mono, fontSize: 11, color: T.dim, cursor: 'pointer', whiteSpace: 'nowrap' }}>
          <input type="checkbox" checked={draft.enabled !== false} onChange={e => set('enabled', e.target.checked)} />
          enabled
        </label>
      </div>

      <div style={labelStyle}>PROVIDER — where it delivers</div>
      <select value={draft.provider} onChange={e => set('provider', e.target.value)} style={{ ...inputStyle, cursor: 'pointer' }}>
        {providers.map(p => <option key={p.type} value={p.type}>{p.name}</option>)}
      </select>
      {prov && <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, marginTop: 4 }}>{prov.description}</div>}

      {configKeys.map(k => (
        <div key={k}>
          <div style={labelStyle}>{k.toUpperCase()}</div>
          <input value={draft.config[k] ?? ''} onChange={e => setConfig(k, e.target.value)}
            placeholder={CONFIG_HINT[draft.provider]?.[k] ?? k} style={inputStyle} />
        </div>
      ))}

      <div style={labelStyle}>EVENTS — what it is notified about (prefix match; "everything" = all)</div>
      <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6 }}>
        {EVENT_SUGGESTIONS.map(ev => {
          const on = draft.events.includes(ev.id);
          return (
            <button key={ev.id} onClick={() => toggleEvent(ev.id)}
              style={{ background: on ? T.greenSoft : 'transparent', border: `1px solid ${on ? T.green : T.border}`, color: on ? T.green : T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 9px', cursor: 'pointer' }}>
              {ev.label}
            </button>
          );
        })}
      </div>
      {/* Any subscribed event not in the suggestion set, shown so custom entries are visible + removable. */}
      {draft.events.filter(e => !EVENT_SUGGESTIONS.some(s => s.id === e)).length > 0 && (
        <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6, marginTop: 6 }}>
          {draft.events.filter(e => !EVENT_SUGGESTIONS.some(s => s.id === e)).map(e => (
            <button key={e} onClick={() => toggleEvent(e)}
              style={{ background: T.blueSoft ?? 'transparent', border: `1px solid ${T.blue}`, color: T.blue, fontFamily: T.mono, fontSize: 10, padding: '3px 9px', cursor: 'pointer' }}>
              {e} ✕
            </button>
          ))}
        </div>
      )}
      <div style={{ display: 'flex', gap: 6, marginTop: 6 }}>
        <input value={newEvent} onChange={e => setNewEvent(e.target.value)}
          onKeyDown={e => { if (e.key === 'Enter') { e.preventDefault(); addEvent(); } }}
          placeholder="add a custom event type (e.g. workflow.run.failed)" style={{ ...inputStyle, flex: 1 }} />
        <button onClick={addEvent} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '0 12px', cursor: 'pointer' }}>add</button>
      </div>

      <div style={labelStyle}>PROJECT — the scope this channel belongs to</div>
      <input value={draft.project ?? ''} onChange={e => set('project', e.target.value)}
        placeholder={project ? project : 'none (personal, all projects)'} style={inputStyle} />

      <div style={{ display: 'flex', gap: 8, marginTop: 20, alignItems: 'center', flexWrap: 'wrap' }}>
        <button onClick={handleSave} disabled={!valid || saving}
          style={{ background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 12, fontWeight: 600, padding: '8px 20px', cursor: 'pointer', opacity: (!valid || saving) ? 0.6 : 1 }}>
          {saving ? '[ · · · ]' : isNew ? '[ create ]' : '[ save ]'}
        </button>
        <button onClick={handleTest} disabled={testing}
          style={{ background: 'transparent', border: `1px solid ${T.blue}`, color: T.blue, fontFamily: T.mono, fontSize: 12, padding: '8px 16px', cursor: 'pointer', opacity: testing ? 0.6 : 1 }}>
          {testing ? '[ sending… ]' : '[ send test ]'}
        </button>
        {!isNew && (
          <button onClick={handleDelete}
            style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 12, padding: '8px 16px', cursor: 'pointer' }}
            onMouseEnter={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.red; (e.currentTarget as HTMLButtonElement).style.color = T.red; }}
            onMouseLeave={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.border; (e.currentTarget as HTMLButtonElement).style.color = T.dim; }}>
            [ delete ]
          </button>
        )}
      </div>
      {testMsg && (
        <div style={{ marginTop: 12, fontFamily: T.mono, fontSize: 11, color: testMsg.ok ? T.green : T.red, border: `1px solid ${testMsg.ok ? T.green : T.red}`, padding: '8px 12px' }}>
          {testMsg.text}
        </div>
      )}
    </div>
  );
}

/** Notifications route: channel list on the left, the selected channel's editor on the right. */
export function Notifications() {
  const token = useAppSelector(s => s.auth.token)!;
  const project = useAppSelector(s => s.project.current);
  const [providers, setProviders] = useState<NotifProvider[]>([]);
  const [channels, setChannels] = useState<NotifChannel[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selId, setSelId] = useUrlParam('channel');
  const [creating, setCreating] = useState(false);
  const [railW, railHandle] = useResizableWidth('rail.notifications.main', 240, { min: 180, max: 420 });

  const selected = channels.find(c => c.id === selId) ?? null;

  const fetchAll = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const [provs, chans] = await Promise.all([
        listNotifProviders(token),
        listNotifChannels(token, project ?? undefined),
      ]);
      setProviders(provs);
      setChannels(chans);
    } catch (e: unknown) {
      setError((e as Error).message);
    } finally {
      setLoading(false);
    }
  }, [token, project]);

  useEffect(() => { fetchAll(); }, [fetchAll]);

  const handleSaved = (c: NotifChannel) => {
    setChannels(prev => {
      const rest = prev.filter(x => x.id !== c.id);
      return [c, ...rest];
    });
    setCreating(false);
    setSelId(c.id);
  };

  const handleDeleted = (id: string) => {
    setChannels(prev => prev.filter(x => x.id !== id));
    setSelId(null);
  };

  const startNew = () => { setCreating(true); setSelId(null); };
  const selectChannel = (id: string) => { setCreating(false); setSelId(id); };

  // Convert a stored channel into the editor's input shape.
  const toInput = (c: NotifChannel): NotifChannelInput => ({
    name: c.name, provider: c.provider, config: c.config ?? {},
    events: c.events?.length ? c.events : ['*'], project: c.project ?? '', enabled: c.enabled,
  });

  const newInitial = useMemo(() => blankInput(project), [project]);

  return (
    <div style={{ display: 'flex', height: '100%', overflow: 'hidden' }}>
      {/* Channel list */}
      <div style={{ width: railW, flexShrink: 0, borderRight: `1px solid ${T.border}`, display: 'flex', flexDirection: 'column', background: T.bgAlt }}>
        <div style={{ padding: '14px 14px 10px', borderBottom: `1px solid ${T.border}` }}>
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 4 }}>
            <span style={{ fontFamily: T.mono, fontSize: 13, fontWeight: 700, color: T.textHi }}>notifications/</span>
            <div style={{ display: 'flex', gap: 6 }}>
              <button onClick={startNew}
                style={{ background: creating ? T.greenSoft : 'transparent', border: `1px solid ${creating ? T.green : T.border}`, color: creating ? T.green : T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 7px', cursor: 'pointer' }}>+</button>
              <button onClick={fetchAll} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 7px', cursor: 'pointer' }}>↻</button>
            </div>
          </div>
          <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint }}>
            {channels.length > 0 && `${channels.length} channel${channels.length !== 1 ? 's' : ''}`}
          </div>
        </div>

        <div style={{ flex: 1, overflow: 'auto' }}>
          {loading ? (
            <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint, animation: 'pulse 1s ease-in-out infinite' }}>→ loading · · ·</div>
          ) : error ? (
            <div style={{ padding: '14px', fontFamily: T.mono, fontSize: 11, color: T.red }}>{error}</div>
          ) : channels.length === 0 ? (
            <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint }}>→ no channels — + to add one</div>
          ) : channels.map(c => {
            const isActive = !creating && selected?.id === c.id;
            return (
              <button key={c.id} onClick={() => selectChannel(c.id)}
                style={{ width: '100%', textAlign: 'left', padding: '10px 14px', background: isActive ? T.greenSoft : 'transparent', border: 0, borderLeft: `2px solid ${isActive ? T.green : 'transparent'}`, fontFamily: T.mono, cursor: 'pointer', color: T.text, display: 'block' }}>
                <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
                  <span style={{ fontSize: 12, fontWeight: 600, color: isActive ? T.textHi : T.text }}>{c.name}</span>
                  {!c.enabled && <span style={{ fontSize: 9, color: T.faint, border: `1px solid ${T.border}`, padding: '0 4px' }}>off</span>}
                </div>
                <div style={{ fontSize: 10, color: T.faint, marginTop: 2 }}>
                  {c.provider} · {c.events?.includes('*') ? 'all events' : `${c.events?.length ?? 0} event${(c.events?.length ?? 0) !== 1 ? 's' : ''}`}
                </div>
              </button>
            );
          })}
        </div>
      </div>
      {railHandle}

      {/* Editor panel */}
      <div style={{ flex: 1, display: 'flex', flexDirection: 'column', overflow: 'hidden' }}>
        {creating ? (
          <ChannelEditor initial={newInitial} id={null} providers={providers} project={project} onSaved={handleSaved} onDeleted={handleDeleted} />
        ) : selected ? (
          <ChannelEditor initial={toInput(selected)} id={selected.id} providers={providers} project={project} onSaved={handleSaved} onDeleted={handleDeleted} />
        ) : (
          <div style={{ flex: 1, display: 'flex', alignItems: 'center', justifyContent: 'center' }}>
            <div style={{ fontFamily: T.mono, fontSize: 12, color: T.faint }}>→ select a channel, or + to create one</div>
          </div>
        )}
      </div>
    </div>
  );
}

/**
 * Events page — the reaction control plane: a "triggers" tab (field filters →
 * actions) and a "log" tab (the append-only event log with each envelope's
 * payload). Data via the bff.
 *
 * Replaces the former Hooks page. A hook rule was a git-shaped tuple (source +
 * events + ref_filter); a trigger is a filter over any field of any event, so the
 * form models the common CI shape — type + subject + ref → run a pipeline — and
 * passes any richer filter through untouched.
 */
import { useState, useEffect, useCallback } from 'react';
import { useUrlState, useUrlParam } from '../../hooks/useUrlState';
import { T } from '../../theme';
import { useResizableWidth } from '../../components/ResizeHandle';
import { Pill } from '../../components/Pill';
import { useConfirm } from '../../components/ConfirmDialog';
import { useAppSelector } from '../../store/hooks';
import { listTriggers, deleteTrigger, listEvents, createTrigger, updateTrigger, listWorkflows } from '../../api/bff';
import type { EventTrigger, EventMatch, TriggerAction, PlatformEvent, Workflow } from '../../api/bff';
import { timeAgo, shortId } from '../../utils';
import { useWorkflowNames } from '../../hooks/useNames';

/**
 * Find the value of the first leaf matching `field` anywhere in a filter tree, so the edit
 * form can pre-fill from a filter it did not necessarily author.
 */
function leafValue(m: EventMatch | undefined, field: string): string {
  if (!m) return '';
  if (m.field === field) return m.value == null ? '' : String(m.value);
  for (const sub of [...(m.all ?? []), ...(m.any ?? [])]) {
    const v = leafValue(sub, field);
    if (v) return v;
  }
  return m.not ? leafValue(m.not, field) : '';
}

/** Render a filter compactly for the list rail: "type=repo.push data.ref=main". */
function matchSummary(m: EventMatch | undefined): string {
  const parts: string[] = [];
  const walk = (n: EventMatch | undefined) => {
    if (!n) return;
    if (n.field) {
      const op = !n.op || n.op === 'eq' ? '=' : ` ${n.op} `;
      parts.push(`${n.field}${op}${String(n.value ?? '')}`);
      return;
    }
    (n.all ?? []).forEach(walk);
    (n.any ?? []).forEach(walk);
    walk(n.not);
  };
  walk(m);
  return parts.length ? parts.join(' ') : '(any event)';
}

/** The pipeline the trigger's first run_pipeline action starts, if any. */
function triggerPipelineId(t: EventTrigger): string {
  const a = t.actions?.find(x => x.kind === 'run_pipeline');
  return typeof a?.config?.pipeline_id === 'string' ? a.config.pipeline_id : '';
}

/** AND the non-empty form fields into a filter document. */
function buildMatch(type: string, subject: string, ref: string): EventMatch {
  const all: EventMatch[] = [{ field: 'type', op: 'eq', value: type }];
  if (subject) all.push({ field: 'subject', op: 'eq', value: subject });
  if (ref) all.push({ field: 'data.ref', op: 'eq', value: ref });
  return { all };
}

/**
 * Trigger create/edit modal — parity with the CLI's trigger form. Actions the form does not
 * model (notify, webhook_out, …) are preserved verbatim on edit, so changing the pipeline
 * never silently drops something configured elsewhere.
 */
function TriggerFormModal({ token, trigger, onClose, onSaved }: {
  token: string;
  trigger: EventTrigger | null; // null = create, a trigger = edit
  onClose: () => void;
  onSaved: (saved: EventTrigger) => void;
}) {
  const editing = !!trigger;
  const [name, setName] = useState(trigger?.name ?? '');
  const [type, setType] = useState(leafValue(trigger?.match, 'type'));
  const [subject, setSubject] = useState(leafValue(trigger?.match, 'subject'));
  const [ref, setRef] = useState(leafValue(trigger?.match, 'data.ref'));
  const [workflowId, setWorkflowId] = useState(trigger ? triggerPipelineId(trigger) : '');
  const [workflows, setWorkflows] = useState<Workflow[]>([]);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // The pipeline picker submits a workflow_id. Best effort — a failing list just leaves the
  // current selection intact.
  useEffect(() => {
    listWorkflows(token).then(setWorkflows).catch(() => setWorkflows([]));
  }, [token]);

  const canSubmit = !!name.trim() && !!type.trim() && !!workflowId.trim() && !submitting;

  const handleSave = async () => {
    if (!canSubmit) return;
    setSubmitting(true); setError(null);
    try {
      // Keep every action the form does not model.
      const extra: TriggerAction[] = (trigger?.actions ?? []).filter(a => a.kind !== 'run_pipeline');
      const actions: TriggerAction[] = [
        { kind: 'run_pipeline', config: { pipeline_id: workflowId.trim() } },
        ...extra,
      ];
      const payload = {
        name: name.trim(),
        match: buildMatch(type.trim(), subject.trim(), ref.trim()),
        actions,
        enabled: trigger?.enabled ?? true,
      };
      const saved = editing
        ? await updateTrigger(token, trigger!.id, payload)
        : await createTrigger(token, payload);
      onSaved(saved);
    } catch (e: unknown) { setError((e as Error).message); }
    finally { setSubmitting(false); }
  };

  const label = { fontFamily: T.mono, fontSize: 10, color: T.faint, marginBottom: 4, letterSpacing: 0.5 } as const;
  const field = { width: '100%', background: T.cardHi, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 12, padding: '8px 10px', outline: 'none', boxSizing: 'border-box' } as const;
  const hint = { color: T.faint, opacity: 0.7 } as const;

  return (
    <div onClick={onClose} style={{ position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.7)', zIndex: 50, display: 'flex', alignItems: 'center', justifyContent: 'center' }}>
      <div onClick={e => e.stopPropagation()} style={{ width: 560, maxWidth: '92vw', maxHeight: '90vh', overflow: 'auto', background: T.card, border: `1px solid ${T.borderHi}` }}>
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '12px 18px', borderBottom: `1px solid ${T.border}`, background: T.cardHi }}>
          <span style={{ fontFamily: T.mono, fontSize: 12, color: T.dim }}><span style={{ color: T.green }}>$</span> {editing ? 'edit trigger' : 'new trigger'}</span>
          <button onClick={onClose} style={{ background: 'transparent', border: 0, color: T.faint, cursor: 'pointer', fontSize: 16 }}>×</button>
        </div>

        <div style={{ padding: '18px 20px 16px' }}>
          {error && <div style={{ color: T.red, fontFamily: T.mono, fontSize: 11, marginBottom: 12 }}>{error}</div>}

          <div style={label}>NAME</div>
          <input value={name} onChange={e => setName(e.target.value)} placeholder="ci-push" autoFocus style={{ ...field, marginBottom: 14 }} />

          <div style={label}>EVENT TYPE <span style={hint}>(e.g. repo.push, ticket.status_changed)</span></div>
          <input value={type} onChange={e => setType(e.target.value)} placeholder="repo.push" style={{ ...field, marginBottom: 14 }} />

          <div style={label}>SUBJECT <span style={hint}>(optional — the resource, e.g. owner/repo)</span></div>
          <input value={subject} onChange={e => setSubject(e.target.value)} placeholder="myorg/myrepo" style={{ ...field, marginBottom: 14 }} />

          <div style={label}>REF <span style={hint}>(optional — matches data.ref, e.g. main)</span></div>
          <input value={ref} onChange={e => setRef(e.target.value)} placeholder="main" style={{ ...field, marginBottom: 14 }} />

          <div style={label}>PIPELINE <span style={hint}>(run when the filter matches)</span></div>
          <select value={workflowId} onChange={e => setWorkflowId(e.target.value)} style={{ ...field, marginBottom: 18 }}>
            <option value="">select a workflow</option>
            {/* Keep the current workflow selectable even if it isn't in the fetched list. */}
            {workflowId && !workflows.some(w => w.workflow_id === workflowId) && <option value={workflowId}>{workflowId}</option>}
            {workflows.map(w => <option key={w.workflow_id} value={w.workflow_id}>{w.name}</option>)}
          </select>

          <div style={{ display: 'flex', gap: 10 }}>
            <button onClick={handleSave} disabled={!canSubmit}
              style={{ flex: 1, background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 13, fontWeight: 600, padding: '9px 0', cursor: canSubmit ? 'pointer' : 'default', letterSpacing: 0.4, opacity: canSubmit ? 1 : 0.6 }}>
              {submitting ? '[ · · · ]' : editing ? '[ save ]' : '[ create ]'}
            </button>
            <button onClick={onClose}
              style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 12, padding: '9px 16px', cursor: 'pointer' }}>
              cancel
            </button>
          </div>
        </div>
      </div>
    </div>
  );
}

/** Events viewer: master/detail over triggers and the event log; supports creating, editing, and deleting a trigger. */
export function Events() {
  const token = useAppSelector(s => s.auth.token)!;
  const workflowNames = useWorkflowNames(token);
  const [tab, setTab] = useUrlState<'triggers' | 'log'>('tab', 'triggers');

  const [triggers, setTriggers] = useState<EventTrigger[]>([]);
  const [triggersLoading, setTriggersLoading] = useState(true);
  const [triggersError, setTriggersError] = useState<string | null>(null);
  const [selectedTrigger, setSelectedTrigger] = useUrlParam('trigger');

  const [events, setEvents] = useState<PlatformEvent[]>([]);
  const [eventsLoading, setEventsLoading] = useState(false);
  const [eventsError, setEventsError] = useState<string | null>(null);
  const [selectedEvent, setSelectedEvent] = useUrlParam('event');
  const [eventsFetched, setEventsFetched] = useState(false);

  const fetchTriggers = useCallback(async () => {
    setTriggersLoading(true);
    setTriggersError(null);
    try {
      setTriggers(await listTriggers(token));
    } catch (e: unknown) {
      setTriggersError((e as Error).message);
    } finally {
      setTriggersLoading(false);
    }
  }, [token]);

  const fetchEvents = useCallback(async () => {
    setEventsLoading(true);
    setEventsError(null);
    try {
      setEvents(await listEvents(token));
      setEventsFetched(true);
    } catch (e: unknown) {
      setEventsError((e as Error).message);
    } finally {
      setEventsLoading(false);
    }
  }, [token]);

  useEffect(() => { fetchTriggers(); }, [fetchTriggers]);

  useEffect(() => {
    if (tab === 'log' && !eventsFetched) fetchEvents();
  }, [tab, eventsFetched, fetchEvents]);

  const [confirm, confirmEl] = useConfirm();
  const [railW, railHandle] = useResizableWidth('rail.events.main', 260, { min: 200, max: 480 });

  // Trigger create/edit form. null = closed; { trigger: null } = create; { trigger } = edit.
  const [triggerForm, setTriggerForm] = useState<{ trigger: EventTrigger | null } | null>(null);

  const handleTriggerSaved = (saved: EventTrigger) => {
    setTriggerForm(null);
    fetchTriggers();
    setSelectedTrigger(saved.id);
  };

  const handleDeleteTrigger = async (id: string) => {
    const name = triggers.find(t => t.id === id)?.name;
    if (!(await confirm({ message: `Delete trigger ${name ?? id}? Matching events will no longer dispatch its actions.` }))) return;
    try {
      await deleteTrigger(token, id);
      setTriggers(prev => prev.filter(t => t.id !== id));
      if (selectedTrigger === id) setSelectedTrigger(null);
    } catch (e: unknown) {
      setTriggersError((e as Error).message);
    }
  };

  const selectedTriggerData = triggers.find(t => t.id === selectedTrigger);
  const selectedEventData = events.find(e => e.id === selectedEvent);

  return (
    <div style={{ display: 'flex', height: '100%', overflow: 'hidden', flexDirection: 'column' }}>
      {confirmEl}
      {/* Tab bar */}
      <div style={{ display: 'flex', borderBottom: `1px solid ${T.border}`, background: T.bgAlt, flexShrink: 0 }}>
        {(['triggers', 'log'] as const).map(t => (
          <button key={t} onClick={() => setTab(t)}
            style={{ background: tab === t ? T.card : 'transparent', border: 'none', borderBottom: `2px solid ${tab === t ? T.green : 'transparent'}`, color: tab === t ? T.textHi : T.dim, fontFamily: T.mono, fontSize: 12, padding: '11px 20px', cursor: 'pointer', letterSpacing: 0.3 }}>
            {t === 'triggers' ? 'triggers' : 'event log'}
            {t === 'triggers' && triggers.length > 0 && <span style={{ marginLeft: 8, fontSize: 10, color: T.faint }}>({triggers.length})</span>}
            {t === 'log' && events.length > 0 && <span style={{ marginLeft: 8, fontSize: 10, color: T.faint }}>({events.length})</span>}
          </button>
        ))}
        <div style={{ flex: 1 }} />
        {tab === 'triggers' && (
          <button onClick={() => setTriggerForm({ trigger: null })}
            style={{ background: 'transparent', border: 'none', color: T.green, fontFamily: T.mono, fontSize: 11, padding: '11px 16px', cursor: 'pointer', letterSpacing: 0.3 }}>
            [ + new trigger ]
          </button>
        )}
        <button onClick={tab === 'triggers' ? fetchTriggers : fetchEvents}
          style={{ background: 'transparent', border: 'none', color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '11px 16px', cursor: 'pointer' }}>↻</button>
      </div>

      {triggerForm && (
        <TriggerFormModal token={token} trigger={triggerForm.trigger} onClose={() => setTriggerForm(null)} onSaved={handleTriggerSaved} />
      )}

      {/* Content */}
      <div style={{ flex: 1, display: 'flex', overflow: 'hidden' }}>
        {/* Left list */}
        <div style={{ width: railW, flexShrink: 0, borderRight: `1px solid ${T.border}`, background: T.bgAlt, overflow: 'auto' }}>
          {tab === 'triggers' ? (
            triggersLoading ? (
              <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint, animation: 'pulse 1s ease-in-out infinite' }}>→ loading · · ·</div>
            ) : triggersError ? (
              <div style={{ padding: '14px', fontFamily: T.mono, fontSize: 11, color: T.red }}>{triggersError}</div>
            ) : triggers.length === 0 ? (
              <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint }}>→ no triggers</div>
            ) : triggers.map(trigger => {
              const isActive = selectedTrigger === trigger.id;
              return (
                <button key={trigger.id} onClick={() => setSelectedTrigger(trigger.id)}
                  style={{ width: '100%', textAlign: 'left', padding: '10px 14px', background: isActive ? T.greenSoft : 'transparent', border: 0, borderLeft: `2px solid ${isActive ? T.green : 'transparent'}`, fontFamily: T.mono, cursor: 'pointer', color: T.text, display: 'block', transition: 'background .12s' }}>
                  <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                    <span style={{ width: 6, height: 6, borderRadius: 3, background: trigger.enabled ? T.green : T.dim, flexShrink: 0 }} />
                    <span style={{ fontSize: 13, fontWeight: 600, color: isActive ? T.textHi : T.text }}>{trigger.name}</span>
                  </div>
                  <div style={{ fontSize: 11, color: T.faint, marginTop: 2, paddingLeft: 14, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{matchSummary(trigger.match)}</div>
                </button>
              );
            })
          ) : (
            eventsLoading ? (
              <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint, animation: 'pulse 1s ease-in-out infinite' }}>→ loading · · ·</div>
            ) : eventsError ? (
              <div style={{ padding: '14px', fontFamily: T.mono, fontSize: 11, color: T.red }}>{eventsError}</div>
            ) : events.length === 0 ? (
              <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint }}>→ no events</div>
            ) : events.map(event => {
              const isActive = selectedEvent === event.id;
              return (
                <button key={event.id} onClick={() => setSelectedEvent(event.id)}
                  style={{ width: '100%', textAlign: 'left', padding: '10px 14px', background: isActive ? T.greenSoft : 'transparent', border: 0, borderLeft: `2px solid ${isActive ? T.green : 'transparent'}`, fontFamily: T.mono, cursor: 'pointer', color: T.text, display: 'block', transition: 'background .12s' }}>
                  <div style={{ fontSize: 12, fontWeight: 600, color: isActive ? T.textHi : T.text }}>{event.type}</div>
                  <div style={{ fontSize: 11, color: T.faint, marginTop: 2, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{event.subject}</div>
                  <div style={{ fontSize: 11, color: T.faint }}>{timeAgo(event.occurred_at)} ago</div>
                </button>
              );
            })
          )}
        </div>
        {railHandle}

        {/* Right detail */}
        <div style={{ flex: 1, overflow: 'auto' }}>
          {tab === 'triggers' ? (
            !selectedTriggerData ? (
              <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', height: '100%' }}>
                <div style={{ fontFamily: T.mono, fontSize: 12, color: T.faint }}>→ select a trigger</div>
              </div>
            ) : (
              <div style={{ padding: '20px 24px' }}>
                <div style={{ display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between', marginBottom: 20 }}>
                  <div>
                    <h2 style={{ margin: '0 0 4px', fontFamily: T.mono, fontSize: 18, color: T.textHi, fontWeight: 700 }}>{selectedTriggerData.name}</h2>
                    <div style={{ fontFamily: T.mono, fontSize: 11, color: T.faint }}>created {timeAgo(selectedTriggerData.created_at)} ago</div>
                  </div>
                  <div style={{ display: 'flex', gap: 8 }}>
                    <button onClick={() => setTriggerForm({ trigger: selectedTriggerData })}
                      style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}
                      onMouseEnter={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.green; (e.currentTarget as HTMLButtonElement).style.color = T.green; }}
                      onMouseLeave={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.border; (e.currentTarget as HTMLButtonElement).style.color = T.dim; }}>
                      [ edit ]
                    </button>
                    <button onClick={() => handleDeleteTrigger(selectedTriggerData.id)}
                      style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}
                      onMouseEnter={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.red; (e.currentTarget as HTMLButtonElement).style.color = T.red; }}
                      onMouseLeave={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.border; (e.currentTarget as HTMLButtonElement).style.color = T.dim; }}>
                      [ delete ]
                    </button>
                  </div>
                </div>

                <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 12, marginBottom: 16 }}>
                  {([
                    ['status', selectedTriggerData.enabled ? 'enabled' : 'disabled'],
                    ['pipeline', workflowNames[triggerPipelineId(selectedTriggerData)] ?? shortId(triggerPipelineId(selectedTriggerData))],
                  ] as [string, string][]).map(([k, v]) => (
                    <div key={k} style={{ background: T.card, border: `1px solid ${T.border}`, padding: '10px 14px' }}>
                      <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 4, textTransform: 'uppercase' }}>{k}</div>
                      <div style={{ fontFamily: T.mono, fontSize: 13, color: T.textHi }}>{v}</div>
                    </div>
                  ))}
                </div>

                <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '10px 14px', marginBottom: 16 }}>
                  <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 6, textTransform: 'uppercase' }}>actions</div>
                  <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
                    {selectedTriggerData.actions.map((a, i) => <Pill key={`${a.kind}-${i}`} tone="dim">{a.kind}</Pill>)}
                  </div>
                </div>

                <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 8 }}>MATCH</div>
                <div style={{ background: T.bg, border: `1px solid ${T.border}`, padding: '10px 12px', maxHeight: 240, overflow: 'auto' }}>
                  <pre style={{ margin: 0, fontFamily: T.mono, fontSize: 11, color: T.text, lineHeight: 1.6 }}>
                    {JSON.stringify(selectedTriggerData.match, null, 2)}
                  </pre>
                </div>
              </div>
            )
          ) : (
            !selectedEventData ? (
              <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', height: '100%' }}>
                <div style={{ fontFamily: T.mono, fontSize: 12, color: T.faint }}>→ select an event</div>
              </div>
            ) : (
              <div style={{ padding: '20px 24px' }}>
                <div style={{ display: 'flex', alignItems: 'center', gap: 12, marginBottom: 16 }}>
                  <Pill tone="dim">{selectedEventData.source}</Pill>
                  <h2 style={{ margin: 0, fontFamily: T.mono, fontSize: 18, color: T.textHi, fontWeight: 700 }}>{selectedEventData.type}</h2>
                </div>

                <div style={{ display: 'grid', gridTemplateColumns: 'repeat(3, 1fr)', gap: 12, marginBottom: 20 }}>
                  {([
                    ['subject', selectedEventData.subject],
                    ['tenant', selectedEventData.actor?.org_id || selectedEventData.actor?.user_id || '—'],
                    ['occurred', timeAgo(selectedEventData.occurred_at) + ' ago'],
                  ] as [string, string][]).map(([k, v]) => (
                    <div key={k} style={{ background: T.card, border: `1px solid ${T.border}`, padding: '10px 14px' }}>
                      <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 4, textTransform: 'uppercase' }}>{k}</div>
                      <div style={{ fontFamily: T.mono, fontSize: 13, color: T.textHi, overflow: 'hidden', textOverflow: 'ellipsis' }}>{v}</div>
                    </div>
                  ))}
                </div>

                <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '10px 14px', marginBottom: 16, fontFamily: T.mono, fontSize: 12 }}>
                  <span style={{ color: T.faint }}>id  </span><span style={{ color: T.text }}>{selectedEventData.id}</span>
                </div>

                {selectedEventData.data && Object.keys(selectedEventData.data).length > 0 && (
                  <>
                    <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 8 }}>DATA</div>
                    <div style={{ background: T.bg, border: `1px solid ${T.border}`, padding: '10px 12px', maxHeight: 240, overflow: 'auto' }}>
                      <pre style={{ margin: 0, fontFamily: T.mono, fontSize: 11, color: T.text, lineHeight: 1.6 }}>
                        {JSON.stringify(selectedEventData.data, null, 2)}
                      </pre>
                    </div>
                  </>
                )}
              </div>
            )
          )}
        </div>
      </div>
    </div>
  );
}

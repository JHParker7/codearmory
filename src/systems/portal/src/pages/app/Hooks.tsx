/**
 * Hooks page — the webhook control plane: a "pipeline rules" tab (repo/event →
 * workflow rules with ref filters and input mappings) and an "events" tab
 * (received webhook deliveries with their matched triggers and payload). data
 * via the bff.
 */
import { useState, useEffect, useCallback } from 'react';
import { T } from '../../theme';
import { Pill } from '../../components/Pill';
import { useConfirm } from '../../components/ConfirmDialog';
import { useAppSelector } from '../../store/hooks';
import { listRules, deleteRule, listHookEvents } from '../../api/bff';
import type { PipelineRule, HookEvent } from '../../api/bff';
import { timeAgo, shortId } from '../../utils';
import { useWorkflowNames } from '../../hooks/useNames';

/** Map an event/trigger status to a UI tone (delivered/success/triggered→green, pending/processing→amber, failed/error→red, else dim). */
function statusTone(status: string): 'green' | 'amber' | 'red' | 'dim' {
  if (['delivered', 'success', 'triggered'].includes(status)) return 'green';
  if (['pending', 'processing'].includes(status)) return 'amber';
  if (['failed', 'error'].includes(status)) return 'red';
  return 'dim';
}

/** Webhooks viewer: master/detail over pipeline rules and received events; supports deleting a rule. */
export function Hooks() {
  const token = useAppSelector(s => s.auth.token)!;
  const workflowNames = useWorkflowNames(token);
  const [tab, setTab] = useState<'rules' | 'events'>('rules');

  const [rules, setRules] = useState<PipelineRule[]>([]);
  const [rulesLoading, setRulesLoading] = useState(true);
  const [rulesError, setRulesError] = useState<string | null>(null);
  const [selectedRule, setSelectedRule] = useState<string | null>(null);

  const [events, setEvents] = useState<HookEvent[]>([]);
  const [eventsLoading, setEventsLoading] = useState(false);
  const [eventsError, setEventsError] = useState<string | null>(null);
  const [selectedEvent, setSelectedEvent] = useState<string | null>(null);
  const [eventsFetched, setEventsFetched] = useState(false);

  const fetchRules = useCallback(async () => {
    setRulesLoading(true);
    setRulesError(null);
    try {
      setRules(await listRules(token));
    } catch (e: unknown) {
      setRulesError((e as Error).message);
    } finally {
      setRulesLoading(false);
    }
  }, [token]);

  const fetchEvents = useCallback(async () => {
    setEventsLoading(true);
    setEventsError(null);
    try {
      setEvents(await listHookEvents(token));
      setEventsFetched(true);
    } catch (e: unknown) {
      setEventsError((e as Error).message);
    } finally {
      setEventsLoading(false);
    }
  }, [token]);

  useEffect(() => { fetchRules(); }, [fetchRules]);

  useEffect(() => {
    if (tab === 'events' && !eventsFetched) fetchEvents();
  }, [tab, eventsFetched, fetchEvents]);

  const [confirm, confirmEl] = useConfirm();

  const handleDeleteRule = async (id: string) => {
    const name = rules.find(r => r.rule_id === id)?.name;
    if (!(await confirm({ message: `Delete webhook rule ${name ?? id}? Matching events will no longer trigger.` }))) return;
    try {
      await deleteRule(token, id);
      setRules(prev => prev.filter(r => r.rule_id !== id));
      if (selectedRule === id) setSelectedRule(null);
    } catch (e: unknown) {
      setRulesError((e as Error).message);
    }
  };

  const selectedRuleData = rules.find(r => r.rule_id === selectedRule);
  const selectedEventData = events.find(e => e.event_id === selectedEvent);

  return (
    <div style={{ display: 'flex', height: '100%', overflow: 'hidden', flexDirection: 'column' }}>
      {confirmEl}
      {/* Tab bar */}
      <div style={{ display: 'flex', borderBottom: `1px solid ${T.border}`, background: T.bgAlt, flexShrink: 0 }}>
        {(['rules', 'events'] as const).map(t => (
          <button key={t} onClick={() => setTab(t)}
            style={{ background: tab === t ? T.card : 'transparent', border: 'none', borderBottom: `2px solid ${tab === t ? T.green : 'transparent'}`, color: tab === t ? T.textHi : T.dim, fontFamily: T.mono, fontSize: 12, padding: '11px 20px', cursor: 'pointer', letterSpacing: 0.3 }}>
            {t === 'rules' ? 'pipeline rules' : 'events'}
            {t === 'rules' && rules.length > 0 && <span style={{ marginLeft: 8, fontSize: 10, color: T.faint }}>({rules.length})</span>}
            {t === 'events' && events.length > 0 && <span style={{ marginLeft: 8, fontSize: 10, color: T.faint }}>({events.length})</span>}
          </button>
        ))}
        <div style={{ flex: 1 }} />
        <button onClick={tab === 'rules' ? fetchRules : fetchEvents}
          style={{ background: 'transparent', border: 'none', color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '11px 16px', cursor: 'pointer' }}>↻</button>
      </div>

      {/* Content */}
      <div style={{ flex: 1, display: 'flex', overflow: 'hidden' }}>
        {/* Left list */}
        <div style={{ width: 260, flexShrink: 0, borderRight: `1px solid ${T.border}`, background: T.bgAlt, overflow: 'auto' }}>
          {tab === 'rules' ? (
            rulesLoading ? (
              <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint, animation: 'pulse 1s ease-in-out infinite' }}>→ loading · · ·</div>
            ) : rulesError ? (
              <div style={{ padding: '14px', fontFamily: T.mono, fontSize: 11, color: T.red }}>{rulesError}</div>
            ) : rules.length === 0 ? (
              <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint }}>→ no rules</div>
            ) : rules.map(rule => {
              const isActive = selectedRule === rule.rule_id;
              return (
                <button key={rule.rule_id} onClick={() => setSelectedRule(rule.rule_id)}
                  style={{ width: '100%', textAlign: 'left', padding: '10px 14px', background: isActive ? T.greenSoft : 'transparent', border: 0, borderLeft: `2px solid ${isActive ? T.green : 'transparent'}`, fontFamily: T.mono, cursor: 'pointer', color: T.text, display: 'block', transition: 'background .12s' }}>
                  <div style={{ fontSize: 13, fontWeight: 600, color: isActive ? T.textHi : T.text }}>{rule.name}</div>
                  <div style={{ fontSize: 11, color: T.faint, marginTop: 2 }}>{rule.repo}</div>
                  <div style={{ fontSize: 10, color: T.dim, marginTop: 1 }}>{rule.events.join(', ')}</div>
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
              const isActive = selectedEvent === event.event_id;
              const tone = statusTone(event.status);
              const dotColor = tone === 'green' ? T.green : tone === 'amber' ? T.amber : tone === 'red' ? T.red : T.dim;
              return (
                <button key={event.event_id} onClick={() => setSelectedEvent(event.event_id)}
                  style={{ width: '100%', textAlign: 'left', padding: '10px 14px', background: isActive ? T.greenSoft : 'transparent', border: 0, borderLeft: `2px solid ${isActive ? T.green : 'transparent'}`, fontFamily: T.mono, cursor: 'pointer', color: T.text, display: 'block', transition: 'background .12s' }}>
                  <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                    <span style={{ width: 6, height: 6, borderRadius: 3, background: dotColor, flexShrink: 0 }} />
                    <span style={{ fontSize: 12, fontWeight: 600, color: isActive ? T.textHi : T.text }}>{event.event_type}</span>
                  </div>
                  <div style={{ fontSize: 11, color: T.faint, marginTop: 2, paddingLeft: 14, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{event.repo}</div>
                  <div style={{ fontSize: 11, color: T.faint, paddingLeft: 14 }}>{timeAgo(event.created_at)} ago</div>
                </button>
              );
            })
          )}
        </div>

        {/* Right detail */}
        <div style={{ flex: 1, overflow: 'auto' }}>
          {tab === 'rules' ? (
            !selectedRuleData ? (
              <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', height: '100%' }}>
                <div style={{ fontFamily: T.mono, fontSize: 12, color: T.faint }}>→ select a rule</div>
              </div>
            ) : (
              <div style={{ padding: '20px 24px' }}>
                <div style={{ display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between', marginBottom: 20 }}>
                  <div>
                    <h2 style={{ margin: '0 0 4px', fontFamily: T.mono, fontSize: 18, color: T.textHi, fontWeight: 700 }}>{selectedRuleData.name}</h2>
                    <div style={{ fontFamily: T.mono, fontSize: 11, color: T.faint }}>created {timeAgo(selectedRuleData.created_at)} ago</div>
                  </div>
                  <button onClick={() => handleDeleteRule(selectedRuleData.rule_id)}
                    style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}
                    onMouseEnter={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.red; (e.currentTarget as HTMLButtonElement).style.color = T.red; }}
                    onMouseLeave={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.border; (e.currentTarget as HTMLButtonElement).style.color = T.dim; }}>
                    [ delete ]
                  </button>
                </div>

                <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 12, marginBottom: 16 }}>
                  {([
                    ['repo', selectedRuleData.repo],
                    ['workflow', workflowNames[selectedRuleData.workflow_id] ?? shortId(selectedRuleData.workflow_id)],
                  ] as [string, string][]).map(([k, v]) => (
                    <div key={k} style={{ background: T.card, border: `1px solid ${T.border}`, padding: '10px 14px' }}>
                      <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 4, textTransform: 'uppercase' }}>{k}</div>
                      <div style={{ fontFamily: T.mono, fontSize: 13, color: T.textHi }}>{v}</div>
                    </div>
                  ))}
                </div>

                <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '10px 14px', marginBottom: 16 }}>
                  <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 6, textTransform: 'uppercase' }}>events</div>
                  <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
                    {selectedRuleData.events.map(ev => <Pill key={ev} tone="dim">{ev}</Pill>)}
                  </div>
                </div>

                {selectedRuleData.ref_filter && (
                  <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '10px 14px', marginBottom: 16 }}>
                    <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 4, textTransform: 'uppercase' }}>ref filter</div>
                    <div style={{ fontFamily: T.mono, fontSize: 13, color: T.text }}>{selectedRuleData.ref_filter}</div>
                  </div>
                )}

                {selectedRuleData.input_mapping && Object.keys(selectedRuleData.input_mapping).length > 0 && (
                  <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '10px 14px' }}>
                    <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 6, textTransform: 'uppercase' }}>input mapping</div>
                    {Object.entries(selectedRuleData.input_mapping).map(([k, v]) => (
                      <div key={k} style={{ fontFamily: T.mono, fontSize: 12, color: T.text, marginBottom: 4 }}>
                        <span style={{ color: T.faint }}>{k} → </span>{v}
                      </div>
                    ))}
                  </div>
                )}
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
                  <Pill tone={statusTone(selectedEventData.status)}>{selectedEventData.status}</Pill>
                  <h2 style={{ margin: 0, fontFamily: T.mono, fontSize: 18, color: T.textHi, fontWeight: 700 }}>{selectedEventData.event_type}</h2>
                </div>

                <div style={{ display: 'grid', gridTemplateColumns: 'repeat(3, 1fr)', gap: 12, marginBottom: 20 }}>
                  {([
                    ['repo', selectedEventData.repo],
                    ['rules matched', selectedEventData.rules_matched],
                    ['received', timeAgo(selectedEventData.created_at) + ' ago'],
                  ] as [string, string | number][]).map(([k, v]) => (
                    <div key={k} style={{ background: T.card, border: `1px solid ${T.border}`, padding: '10px 14px' }}>
                      <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 4, textTransform: 'uppercase' }}>{k}</div>
                      <div style={{ fontFamily: T.mono, fontSize: 13, color: T.textHi }}>{v}</div>
                    </div>
                  ))}
                </div>

                {selectedEventData.ref && (
                  <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '10px 14px', marginBottom: 16, fontFamily: T.mono, fontSize: 12 }}>
                    <span style={{ color: T.faint }}>ref  </span><span style={{ color: T.text }}>{selectedEventData.ref}</span>
                  </div>
                )}

                {selectedEventData.triggers && selectedEventData.triggers.length > 0 && (
                  <>
                    <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 8 }}>TRIGGERS</div>
                    <div style={{ background: T.card, border: `1px solid ${T.border}`, overflow: 'hidden', marginBottom: 16 }}>
                      {selectedEventData.triggers.map((trig, i) => (
                        <div key={trig.trigger_id} style={{ display: 'flex', alignItems: 'center', padding: '9px 12px', borderBottom: i < selectedEventData.triggers!.length - 1 ? `1px solid ${T.border}` : 'none', gap: 12, fontFamily: T.mono, fontSize: 11 }}>
                          <Pill tone={statusTone(trig.status)}>{trig.status}</Pill>
                          <span style={{ color: T.dim, flex: 1 }}>{workflowNames[trig.workflow_id] ?? shortId(trig.workflow_id)}</span>
                          {trig.run_id && <span style={{ color: T.faint }}>run: {trig.run_id.slice(0, 8)}…</span>}
                          {trig.error && <span style={{ color: T.red }}>{trig.error}</span>}
                        </div>
                      ))}
                    </div>
                  </>
                )}

                {selectedEventData.payload !== undefined && (
                  <>
                    <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 8 }}>PAYLOAD</div>
                    <div style={{ background: T.bg, border: `1px solid ${T.border}`, padding: '10px 12px', maxHeight: 240, overflow: 'auto' }}>
                      <pre style={{ margin: 0, fontFamily: T.mono, fontSize: 11, color: T.text, lineHeight: 1.6 }}>
                        {JSON.stringify(selectedEventData.payload, null, 2)}
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

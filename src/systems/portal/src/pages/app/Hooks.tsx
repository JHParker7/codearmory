/**
 * Hooks page — the webhook control plane: a "pipeline rules" tab (repo/event →
 * workflow rules with ref filters and input mappings) and an "events" tab
 * (received webhook deliveries with their matched triggers and payload). data
 * via the bff.
 */
import { useState, useEffect, useCallback } from 'react';
import { T } from '../../theme';
import { useResizableWidth } from '../../components/ResizeHandle';
import { Pill } from '../../components/Pill';
import { useConfirm } from '../../components/ConfirmDialog';
import { useAppSelector } from '../../store/hooks';
import { listRules, deleteRule, listHookEvents, createRule, updateRule, listWorkflows } from '../../api/bff';
import type { PipelineRule, HookEvent, Workflow } from '../../api/bff';
import { timeAgo, shortId } from '../../utils';
import { useWorkflowNames } from '../../hooks/useNames';

/** Map an event/trigger status to a UI tone (delivered/success/triggered→green, pending/processing→amber, failed/error→red, else dim). */
function statusTone(status: string): 'green' | 'amber' | 'red' | 'dim' {
  if (['delivered', 'success', 'triggered'].includes(status)) return 'green';
  if (['pending', 'processing'].includes(status)) return 'amber';
  if (['failed', 'error'].includes(status)) return 'red';
  return 'dim';
}

/**
 * Rule create/edit modal — parity with the CLI's hook-rule form. A rule maps
 * webhook deliveries from a source repo + event set to a workflow run, guarded by
 * an HMAC secret. On edit the secret field starts blank (the API never returns it)
 * and a blank secret keeps the current one; the rule's `input_mapping` is preserved
 * verbatim since the form doesn't expose it (matching the CLI).
 */
function RuleFormModal({ token, rule, onClose, onSaved }: {
  token: string;
  rule: PipelineRule | null; // null = create, a rule = edit
  onClose: () => void;
  onSaved: (saved: PipelineRule) => void;
}) {
  const editing = !!rule;
  const [name, setName] = useState(rule?.name ?? '');
  const [source, setSource] = useState(rule?.source ?? '');
  const [eventsStr, setEventsStr] = useState((rule?.events ?? []).join(' '));
  const [workflowId, setWorkflowId] = useState(rule?.workflow_id ?? '');
  const [secret, setSecret] = useState('');
  const [refFilter, setRefFilter] = useState(rule?.ref_filter ?? '');
  const [workflows, setWorkflows] = useState<Workflow[]>([]);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // The workflow picker submits a workflow_id (the rule stores an id, and the
  // service validates it resolves to a workflow in the caller's org). Best effort —
  // a failing list just leaves the current selection intact.
  useEffect(() => {
    listWorkflows(token).then(setWorkflows).catch(() => setWorkflows([]));
  }, [token]);

  const parsedEvents = eventsStr.split(/[\s,]+/).map(s => s.trim()).filter(Boolean);
  const canSubmit = !!name.trim() && !!source.trim() && parsedEvents.length > 0
    && !!workflowId.trim() && (editing || !!secret.trim()) && !submitting;

  const handleSave = async () => {
    if (!canSubmit) return;
    setSubmitting(true); setError(null);
    try {
      let saved: PipelineRule;
      if (editing) {
        saved = await updateRule(token, rule!.rule_id, {
          name: name.trim(),
          source: source.trim(),
          events: parsedEvents,
          workflow_id: workflowId.trim(),
          ref_filter: refFilter.trim(),
          // Only send a secret when one is typed — an omitted secret keeps the
          // current one (the service rejects an empty string).
          ...(secret.trim() ? { secret: secret.trim() } : {}),
          // Preserve the existing input mapping, which the form doesn't expose.
          input_mapping: rule!.input_mapping ?? {},
        });
      } else {
        saved = await createRule(token, {
          name: name.trim(),
          source: source.trim(),
          events: parsedEvents,
          workflow_id: workflowId.trim(),
          secret: secret.trim(),
          ref_filter: refFilter.trim() || undefined,
        });
      }
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
          <span style={{ fontFamily: T.mono, fontSize: 12, color: T.dim }}><span style={{ color: T.green }}>$</span> {editing ? 'edit hook rule' : 'new hook rule'}</span>
          <button onClick={onClose} style={{ background: 'transparent', border: 0, color: T.faint, cursor: 'pointer', fontSize: 16 }}>×</button>
        </div>

        <div style={{ padding: '18px 20px 16px' }}>
          {error && <div style={{ color: T.red, fontFamily: T.mono, fontSize: 11, marginBottom: 12 }}>{error}</div>}

          <div style={label}>NAME</div>
          <input value={name} onChange={e => setName(e.target.value)} placeholder="ci-push" autoFocus style={{ ...field, marginBottom: 14 }} />

          <div style={label}>SOURCE REPO <span style={hint}>(matches the delivery's repo, e.g. owner/repo)</span></div>
          <input value={source} onChange={e => setSource(e.target.value)} placeholder="myorg/myrepo" style={{ ...field, marginBottom: 14 }} />

          <div style={label}>EVENTS <span style={hint}>(space or comma separated)</span></div>
          <input value={eventsStr} onChange={e => setEventsStr(e.target.value)} placeholder="push pull_request" style={{ ...field, marginBottom: 14 }} />

          <div style={label}>WORKFLOW</div>
          <select value={workflowId} onChange={e => setWorkflowId(e.target.value)} style={{ ...field, marginBottom: 14 }}>
            <option value="">select a workflow</option>
            {/* Keep the current workflow selectable even if it isn't in the fetched list. */}
            {workflowId && !workflows.some(w => w.workflow_id === workflowId) && <option value={workflowId}>{workflowId}</option>}
            {workflows.map(w => <option key={w.workflow_id} value={w.workflow_id}>{w.name}</option>)}
          </select>

          <div style={label}>SECRET <span style={hint}>{editing ? '(leave blank to keep current)' : '(HMAC secret · required)'}</span></div>
          <input type="password" value={secret} onChange={e => setSecret(e.target.value)} placeholder={editing ? '••••••••' : 'HMAC secret'} style={{ ...field, marginBottom: 14 }} />

          <div style={label}>REF FILTER <span style={hint}>(optional, e.g. refs/heads/main)</span></div>
          <input value={refFilter} onChange={e => setRefFilter(e.target.value)} placeholder="refs/heads/main" style={{ ...field, marginBottom: 18 }} />

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

/** Webhooks viewer: master/detail over pipeline rules and received events; supports creating, editing, and deleting a rule. */
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
  const [railW, railHandle] = useResizableWidth('rail.hooks.main', 260, { min: 200, max: 480 });

  // Rule create/edit form. null = closed; { rule: null } = create; { rule } = edit.
  const [ruleForm, setRuleForm] = useState<{ rule: PipelineRule | null } | null>(null);

  const handleRuleSaved = (saved: PipelineRule) => {
    setRuleForm(null);
    fetchRules();
    setSelectedRule(saved.rule_id);
  };

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
        {tab === 'rules' && (
          <button onClick={() => setRuleForm({ rule: null })}
            style={{ background: 'transparent', border: 'none', color: T.green, fontFamily: T.mono, fontSize: 11, padding: '11px 16px', cursor: 'pointer', letterSpacing: 0.3 }}>
            [ + new rule ]
          </button>
        )}
        <button onClick={tab === 'rules' ? fetchRules : fetchEvents}
          style={{ background: 'transparent', border: 'none', color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '11px 16px', cursor: 'pointer' }}>↻</button>
      </div>

      {ruleForm && (
        <RuleFormModal token={token} rule={ruleForm.rule} onClose={() => setRuleForm(null)} onSaved={handleRuleSaved} />
      )}

      {/* Content */}
      <div style={{ flex: 1, display: 'flex', overflow: 'hidden' }}>
        {/* Left list */}
        <div style={{ width: railW, flexShrink: 0, borderRight: `1px solid ${T.border}`, background: T.bgAlt, overflow: 'auto' }}>
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
                  <div style={{ fontSize: 11, color: T.faint, marginTop: 2 }}>{rule.source}</div>
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
                  <div style={{ fontSize: 11, color: T.faint, marginTop: 2, paddingLeft: 14, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{event.source}</div>
                  <div style={{ fontSize: 11, color: T.faint, paddingLeft: 14 }}>{timeAgo(event.created_at)} ago</div>
                </button>
              );
            })
          )}
        </div>
        {railHandle}

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
                  <div style={{ display: 'flex', gap: 8 }}>
                    <button onClick={() => setRuleForm({ rule: selectedRuleData })}
                      style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}
                      onMouseEnter={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.green; (e.currentTarget as HTMLButtonElement).style.color = T.green; }}
                      onMouseLeave={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.border; (e.currentTarget as HTMLButtonElement).style.color = T.dim; }}>
                      [ edit ]
                    </button>
                    <button onClick={() => handleDeleteRule(selectedRuleData.rule_id)}
                      style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}
                      onMouseEnter={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.red; (e.currentTarget as HTMLButtonElement).style.color = T.red; }}
                      onMouseLeave={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.border; (e.currentTarget as HTMLButtonElement).style.color = T.dim; }}>
                      [ delete ]
                    </button>
                  </div>
                </div>

                <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 12, marginBottom: 16 }}>
                  {([
                    ['repo', selectedRuleData.source],
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
                    ['repo', selectedEventData.source],
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

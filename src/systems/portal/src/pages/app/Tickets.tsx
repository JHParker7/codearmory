/**
 * Tickets page — the ticket tracker: a list of tickets in a sidebar and a
 * detail panel with description, linked workflow/run/execution refs, comments,
 * and open/close + delete actions. create new tickets via a modal. data via
 * the bff.
 */
import { useState, useEffect, useCallback } from 'react';
import { T } from '../../theme';
import { Pill } from '../../components/Pill';
import { useAppSelector } from '../../store/hooks';
import { listTickets, createTicket, updateTicket, deleteTicket, addComment } from '../../api/bff';
import type { Ticket } from '../../api/bff';
import { timeAgo, shortId } from '../../utils';
import { useUserNames, useWorkflowNames } from '../../hooks/useNames';

/** Map a ticket status to a UI tone (closed→green, open→amber, blocked→red, else dim). */
function statusTone(status: string): 'green' | 'amber' | 'red' | 'dim' {
  if (status === 'closed') return 'green';
  if (status === 'open') return 'amber';
  if (status === 'blocked') return 'red';
  return 'dim';
}

/** Map a ticket priority to a theme color (high/critical→red, medium→amber, else dim). */
function priorityColor(priority: string | null | undefined): string {
  if (priority === 'high' || priority === 'critical') return T.red;
  if (priority === 'medium') return T.amber;
  return T.dim;
}

/** Modal form for a new ticket; submits title/description/priority via createTicket and hands the created ticket back. */
function CreateModal({ onCreated, onClose }: { onCreated: (t: Ticket) => void; onClose: () => void }) {
  const token = useAppSelector(s => s.auth.token)!;
  const [title, setTitle] = useState('');
  const [description, setDescription] = useState('');
  const [priority, setPriority] = useState('medium');
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const handleSubmit = async () => {
    if (!title.trim()) return;
    setSubmitting(true);
    setError(null);
    try {
      onCreated(await createTicket(token, { title: title.trim(), description: description.trim() || undefined, priority }));
    } catch (e: unknown) {
      setError((e as Error).message);
      setSubmitting(false);
    }
  };

  return (
    <div onClick={onClose} style={{ position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.7)', zIndex: 50, display: 'flex', alignItems: 'center', justifyContent: 'center' }}>
      <div onClick={e => e.stopPropagation()} style={{ width: 480, background: T.card, border: `1px solid ${T.borderHi}` }}>
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '12px 16px', borderBottom: `1px solid ${T.border}`, background: T.cardHi }}>
          <span style={{ fontFamily: T.mono, fontSize: 12, color: T.dim }}>new ticket</span>
          <button onClick={onClose} style={{ background: 'transparent', border: 0, color: T.faint, cursor: 'pointer', fontSize: 16 }}>×</button>
        </div>
        <div style={{ padding: '16px 20px' }}>
          {error && <div style={{ background: T.redSoft, border: `1px solid ${T.red}`, padding: '8px 12px', fontFamily: T.mono, fontSize: 11, color: T.red, marginBottom: 12 }}>{error}</div>}
          <div style={{ marginBottom: 12 }}>
            <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, marginBottom: 6, letterSpacing: 0.5 }}>TITLE</div>
            <input value={title} onChange={e => setTitle(e.target.value)} onKeyDown={e => e.key === 'Enter' && handleSubmit()} autoFocus
              style={{ width: '100%', background: T.cardHi, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 13, padding: '8px 10px', outline: 'none', boxSizing: 'border-box' }} />
          </div>
          <div style={{ marginBottom: 12 }}>
            <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, marginBottom: 6, letterSpacing: 0.5 }}>DESCRIPTION</div>
            <textarea value={description} onChange={e => setDescription(e.target.value)} rows={3}
              style={{ width: '100%', background: T.cardHi, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 12, padding: '8px 10px', outline: 'none', resize: 'vertical', boxSizing: 'border-box' }} />
          </div>
          <div style={{ marginBottom: 16 }}>
            <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, marginBottom: 6, letterSpacing: 0.5 }}>PRIORITY</div>
            <div style={{ display: 'flex', gap: 8 }}>
              {(['low', 'medium', 'high'] as const).map(p => (
                <button key={p} onClick={() => setPriority(p)}
                  style={{ background: priority === p ? T.greenSoft : 'transparent', border: `1px solid ${priority === p ? T.green : T.border}`, color: priority === p ? T.green : T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}>
                  {p}
                </button>
              ))}
            </div>
          </div>
          <div style={{ display: 'flex', gap: 10 }}>
            <button onClick={handleSubmit} disabled={!title.trim() || submitting}
              style={{ flex: 1, background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 13, fontWeight: 600, padding: '9px 14px', cursor: 'pointer', opacity: (!title.trim() || submitting) ? 0.6 : 1 }}>
              {submitting ? '[ · · · ]' : '[ create ticket ]'}
            </button>
            <button onClick={onClose} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 12, padding: '9px 14px', cursor: 'pointer' }}>cancel</button>
          </div>
        </div>
      </div>
    </div>
  );
}

/** Ticket tracker: sidebar list + detail panel with comments, status toggle, delete, and a create modal. */
export function Tickets() {
  const token = useAppSelector(s => s.auth.token)!;
  const userNames = useUserNames(token);
  const workflowNames = useWorkflowNames(token);
  const [tickets, setTickets] = useState<Ticket[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const [showCreate, setShowCreate] = useState(false);
  const [newComment, setNewComment] = useState('');
  const [commenting, setCommenting] = useState(false);

  const fetchTickets = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      setTickets(await listTickets(token));
    } catch (e: unknown) {
      setError((e as Error).message);
    } finally {
      setLoading(false);
    }
  }, [token]);

  useEffect(() => { fetchTickets(); }, [fetchTickets]);

  const selectedTicket = tickets.find(t => t.ticket_id === selected);

  const handleStatusToggle = async () => {
    if (!selectedTicket) return;
    const newStatus = selectedTicket.status === 'open' ? 'closed' : 'open';
    try {
      const updated = await updateTicket(token, selectedTicket.ticket_id, { status: newStatus });
      setTickets(prev => prev.map(t => t.ticket_id === updated.ticket_id ? updated : t));
    } catch (e: unknown) {
      setError((e as Error).message);
    }
  };

  const handleDelete = async () => {
    if (!selectedTicket) return;
    try {
      await deleteTicket(token, selectedTicket.ticket_id);
      setTickets(prev => prev.filter(t => t.ticket_id !== selectedTicket.ticket_id));
      setSelected(null);
    } catch (e: unknown) {
      setError((e as Error).message);
    }
  };

  const handleAddComment = async () => {
    if (!selectedTicket || !newComment.trim()) return;
    setCommenting(true);
    try {
      const comment = await addComment(token, selectedTicket.ticket_id, newComment.trim());
      setTickets(prev => prev.map(t =>
        t.ticket_id === selectedTicket.ticket_id ? { ...t, comments: [...(t.comments ?? []), comment] } : t
      ));
      setNewComment('');
    } catch (e: unknown) {
      setError((e as Error).message);
    } finally {
      setCommenting(false);
    }
  };

  return (
    <div style={{ display: 'flex', height: '100%', overflow: 'hidden' }}>
      {/* Left panel */}
      <div style={{ width: 260, flexShrink: 0, borderRight: `1px solid ${T.border}`, display: 'flex', flexDirection: 'column', background: T.bgAlt }}>
        <div style={{ padding: '14px 14px 10px', borderBottom: `1px solid ${T.border}` }}>
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 4 }}>
            <span style={{ fontFamily: T.mono, fontSize: 13, fontWeight: 700, color: T.textHi }}>tickets/</span>
            <div style={{ display: 'flex', gap: 6 }}>
              <button onClick={() => setShowCreate(true)} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 7px', cursor: 'pointer' }}>+ new</button>
              <button onClick={fetchTickets} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 7px', cursor: 'pointer' }}>↻</button>
            </div>
          </div>
          <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint }}>
            {tickets.length > 0 && `${tickets.filter(t => t.status === 'open').length} open · ${tickets.length} total`}
          </div>
        </div>

        <div style={{ flex: 1, overflow: 'auto' }}>
          {loading ? (
            <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint, animation: 'pulse 1s ease-in-out infinite' }}>→ loading · · ·</div>
          ) : error ? (
            <div style={{ padding: '14px', fontFamily: T.mono, fontSize: 11, color: T.red }}>{error}</div>
          ) : tickets.length === 0 ? (
            <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint }}>
              <div>→ no tickets</div>
              <div style={{ marginTop: 8 }}>
                <button onClick={() => setShowCreate(true)} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 10px', cursor: 'pointer' }}>[ + new ticket ]</button>
              </div>
            </div>
          ) : tickets.map(ticket => {
            const isActive = selected === ticket.ticket_id;
            return (
              <button key={ticket.ticket_id} onClick={() => setSelected(ticket.ticket_id)}
                style={{ width: '100%', textAlign: 'left', padding: '10px 14px', background: isActive ? T.greenSoft : 'transparent', border: 0, borderLeft: `2px solid ${isActive ? T.green : 'transparent'}`, fontFamily: T.mono, cursor: 'pointer', color: T.text, display: 'block', transition: 'background .12s' }}>
                <div style={{ display: 'flex', alignItems: 'center', gap: 6, marginBottom: 3 }}>
                  <span style={{ fontSize: 10, color: priorityColor(ticket.priority), flexShrink: 0 }}>■</span>
                  <span style={{ fontSize: 13, fontWeight: 600, color: isActive ? T.textHi : T.text, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{ticket.title}</span>
                </div>
                <div style={{ fontSize: 11, color: T.faint, paddingLeft: 16 }}>{ticket.status} · {timeAgo(ticket.updated_at)} ago</div>
              </button>
            );
          })}
        </div>
      </div>

      {/* Right panel */}
      <div style={{ flex: 1, display: 'flex', flexDirection: 'column', overflow: 'hidden' }}>
        <div style={{ padding: '12px 20px', borderBottom: `1px solid ${T.border}`, background: T.bgAlt, display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
          <div style={{ fontFamily: T.mono, fontSize: 11, color: T.faint }}>
            <span style={{ color: T.green }}>$</span> armory tickets{selectedTicket ? ` · ${selectedTicket.ticket_id.slice(0, 8)}…` : ''}
          </div>
          {selectedTicket && (
            <div style={{ display: 'flex', gap: 8 }}>
              <button onClick={handleStatusToggle}
                style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}>
                [ {selectedTicket.status === 'open' ? '✓ close' : '↺ reopen'} ]
              </button>
              <button onClick={handleDelete}
                style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}
                onMouseEnter={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.red; (e.currentTarget as HTMLButtonElement).style.color = T.red; }}
                onMouseLeave={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.border; (e.currentTarget as HTMLButtonElement).style.color = T.dim; }}>
                [ delete ]
              </button>
            </div>
          )}
        </div>

        <div style={{ flex: 1, overflow: 'auto' }}>
          {!selectedTicket ? (
            <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', height: '100%' }}>
              <div style={{ fontFamily: T.mono, fontSize: 12, color: T.faint, textAlign: 'center' }}>
                <div>→ select a ticket</div>
                <div style={{ marginTop: 12 }}>
                  <button onClick={() => setShowCreate(true)} style={{ background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 12, padding: '8px 16px', cursor: 'pointer', fontWeight: 600 }}>[ + new ticket ]</button>
                </div>
              </div>
            </div>
          ) : (
            <div style={{ padding: '20px 24px' }}>
              <div style={{ display: 'flex', alignItems: 'center', gap: 12, marginBottom: 6 }}>
                <h2 style={{ margin: 0, fontFamily: T.mono, fontSize: 18, color: T.textHi, fontWeight: 700 }}>{selectedTicket.title}</h2>
                <Pill tone={statusTone(selectedTicket.status)}>{selectedTicket.status}</Pill>
                {selectedTicket.priority && (
                  <span style={{ fontFamily: T.mono, fontSize: 10, color: priorityColor(selectedTicket.priority), border: `1px solid ${priorityColor(selectedTicket.priority)}`, padding: '2px 6px', letterSpacing: 0.5 }}>
                    {selectedTicket.priority}
                  </span>
                )}
              </div>
              <div style={{ fontFamily: T.mono, fontSize: 11, color: T.faint, marginBottom: 20 }}>
                created {timeAgo(selectedTicket.created_at)} ago · updated {timeAgo(selectedTicket.updated_at)} ago
              </div>

              {selectedTicket.description && (
                <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '12px 16px', fontFamily: T.mono, fontSize: 13, color: T.text, lineHeight: 1.6, marginBottom: 20, whiteSpace: 'pre-wrap' }}>
                  {selectedTicket.description}
                </div>
              )}

              {(selectedTicket.workflow_id || selectedTicket.run_id || selectedTicket.forge_execution_id) && (
                <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '10px 14px', marginBottom: 20, fontFamily: T.mono, fontSize: 11 }}>
                  {selectedTicket.workflow_id && <div><span style={{ color: T.faint }}>workflow  </span><span style={{ color: T.dim }}>{workflowNames[selectedTicket.workflow_id] ?? shortId(selectedTicket.workflow_id)}</span></div>}
                  {selectedTicket.run_id && <div><span style={{ color: T.faint }}>run       </span><span style={{ color: T.dim }}>{selectedTicket.run_id.slice(0, 8)}…</span></div>}
                  {selectedTicket.forge_execution_id && <div><span style={{ color: T.faint }}>execution </span><span style={{ color: T.dim }}>{selectedTicket.forge_execution_id.slice(0, 8)}…</span></div>}
                </div>
              )}

              <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 8 }}>
                COMMENTS{selectedTicket.comments && selectedTicket.comments.length > 0 ? ` · ${selectedTicket.comments.length}` : ''}
              </div>

              {selectedTicket.comments && selectedTicket.comments.length > 0 && (
                <div style={{ marginBottom: 16 }}>
                  {selectedTicket.comments.map(c => (
                    <div key={c.comment_id} style={{ background: T.card, border: `1px solid ${T.border}`, padding: '10px 14px', marginBottom: 8 }}>
                      <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, marginBottom: 6 }}>{userNames[c.author_id] ?? shortId(c.author_id)} · {timeAgo(c.created_at)} ago</div>
                      <div style={{ fontFamily: T.mono, fontSize: 12, color: T.text, lineHeight: 1.6, whiteSpace: 'pre-wrap' }}>{c.body}</div>
                    </div>
                  ))}
                </div>
              )}

              <div style={{ display: 'flex', gap: 8 }}>
                <textarea value={newComment} onChange={e => setNewComment(e.target.value)} rows={2} placeholder="add a comment…"
                  style={{ flex: 1, background: T.card, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 12, padding: '8px 10px', outline: 'none', resize: 'vertical' }} />
                <button onClick={handleAddComment} disabled={!newComment.trim() || commenting}
                  style={{ background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 11, padding: '8px 14px', cursor: 'pointer', opacity: (!newComment.trim() || commenting) ? 0.6 : 1, alignSelf: 'flex-end', whiteSpace: 'nowrap' }}>
                  [ comment ]
                </button>
              </div>
            </div>
          )}
        </div>
      </div>

      {showCreate && (
        <CreateModal
          onCreated={t => { setTickets(prev => [t, ...prev]); setSelected(t.ticket_id); setShowCreate(false); }}
          onClose={() => setShowCreate(false)}
        />
      )}
    </div>
  );
}

/**
 * Tickets page — a kanban board mirroring the CLI `armory tickets board`, backed
 * by first-class Board entities in the tickets service. The left sidebar is a
 * board switcher (one entry per board, plus "all" and "unassigned"); permitted
 * users can create and delete boards there. The main panel shows the selected
 * board's tickets as columns by status; drag a card between columns to change its
 * status, or open the column editor to configure the status columns. Clicking a
 * card opens a detail drawer showing the full detail set — reporter, assignee,
 * timescale, due date, project, description, refs, and comments — with edit,
 * close/reopen, and delete. The create/edit form (TicketFormModal) exposes the
 * same fields, matching the CLI board form. Data via the bff.
 */
import { useState, useEffect, useCallback, useMemo, useRef } from 'react';
import { useUrlParam } from '../../hooks/useUrlState';
import { loadDraft, saveDraft, clearDraft } from '../../draftStorage';
import type { CSSProperties } from 'react';
import { T } from '../../theme';
import { useResizableWidth } from '../../components/ResizeHandle';
import { Pill } from '../../components/Pill';
import { useConfirm } from '../../components/ConfirmDialog';
import { useAppSelector } from '../../store/hooks';
import {
  listTickets, createTicket, updateTicket, deleteTicket, addComment,
  listTicketFieldDefs, createTicketFieldDef, updateTicketFieldDef, deleteTicketFieldDef,
  listBoards, createBoard, deleteBoard,
} from '../../api/bff';
import type { Ticket, TicketFieldDef, Board, User } from '../../api/bff';
import { timeAgo, shortId } from '../../utils';
import { useUsers, useWorkflowNames } from '../../hooks/useNames';

/** Column statuses used when the org has not configured custom status field defs (mirrors the CLI board fallback). */
const DEFAULT_STATUSES: TicketFieldDef[] = [
  { field_def_id: 'open', kind: 'status', value: 'open', label: 'open', position: 0 },
  { field_def_id: 'in_progress', kind: 'status', value: 'in_progress', label: 'in progress', position: 1 },
  { field_def_id: 'resolved', kind: 'status', value: 'resolved', label: 'resolved', position: 2 },
  { field_def_id: 'closed', kind: 'status', value: 'closed', label: 'closed', position: 3 },
];

/** Priority options used when a board has none of its own configured (mirrors the seeded defaults). */
const DEFAULT_PRIORITIES: TicketFieldDef[] = [
  { field_def_id: 'low', kind: 'priority', value: 'low', label: 'low', position: 0 },
  { field_def_id: 'medium', kind: 'priority', value: 'medium', label: 'medium', position: 1 },
  { field_def_id: 'high', kind: 'priority', value: 'high', label: 'high', position: 2 },
  { field_def_id: 'critical', kind: 'priority', value: 'critical', label: 'critical', position: 3 },
];

/** localStorage key remembering the last board the user had open, restored on next visit. */
const LAST_BOARD_KEY = 'ca.tickets.lastBoard';

/** Map a ticket status to a UI tone for its pill (closed/resolved→green, open→amber, blocked→red, else dim). */
function statusTone(status: string): 'green' | 'amber' | 'red' | 'dim' {
  if (status === 'closed' || status === 'resolved') return 'green';
  if (status === 'open') return 'amber';
  if (status === 'blocked') return 'red';
  return 'dim';
}

/** Accent color for a status column header — the field def's color if set, else a sensible default per known value. */
function statusColor(value: string, color?: string): string {
  if (color) return color;
  switch (value) {
    case 'open': return T.amber;
    case 'in_progress': return T.blue;
    case 'resolved': return T.green;
    case 'closed': return T.dim;
    case 'blocked': return T.red;
    default: return T.dim;
  }
}

/** A terminal status — used to decide whether the toggle reads "close" or "reopen". */
function isTerminal(status: string): boolean {
  return status === 'resolved' || status === 'closed';
}

/** Today's date as YYYY-MM-DD in local time, for comparing against a ticket's due date. */
function todayStr(): string {
  const d = new Date();
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
}

/** A ticket is overdue when it has a past due date and is not yet in a terminal status. */
function isOverdue(t: Ticket): boolean {
  return !!t.due_date && !isTerminal(t.status) && t.due_date.slice(0, 10) < todayStr();
}

/** Map a ticket priority to a theme color (high/critical→red, medium→amber, else dim). */
function priorityColor(priority: string | null | undefined): string {
  if (priority === 'high' || priority === 'critical') return T.red;
  if (priority === 'medium') return T.amber;
  return T.dim;
}

/** Derive a stable status value (slug) from a human label. */
function slugify(s: string): string {
  return s.toLowerCase().replace(/[^a-z0-9]+/g, '_').replace(/^_+|_+$/g, '');
}

/**
 * Modal form to create or edit a ticket. In `create` mode it POSTs a new ticket
 * onto the active board; in `edit` mode it is seeded from `ticket` and PUTs the
 * changes. Both modes expose the full ticket detail set — title, description,
 * priority, status, timescale, due date, and assignee — to match the CLI form.
 */
function TicketFormModal({ mode, ticket, boardId, defaultProject, statuses, priorities, parentOptions, users, onSaved, onClose }: {
  mode: 'create' | 'edit';
  ticket?: Ticket;
  boardId?: string;
  // The selected project's slug — a new ticket defaults to it so it lands in the
  // scope that is currently being viewed (otherwise it is filtered out and "vanishes").
  defaultProject?: string;
  statuses: TicketFieldDef[];
  priorities: TicketFieldDef[];
  // Candidate parent tickets (same board, excluding this ticket) for the parent picker.
  parentOptions: Ticket[];
  users: User[];
  onSaved: (t: Ticket) => void;
  onClose: () => void;
}) {
  const token = useAppSelector(s => s.auth.token)!;
  // Status columns left-to-right; a new ticket defaults to the left-most.
  const ordered = useMemo(() => [...statuses].sort((a, b) => a.position - b.position), [statuses]);

  // ── Draft persistence ─────────────────────────────────────────────────────────
  // An in-progress ticket write-up lives only in this modal's state, so a refresh
  // (or a connection blip that bounces the app to sign-in) used to discard it. Mirror
  // the form to localStorage keyed by the ticket being edited (or the board for a new
  // one), restore it when the modal reopens, and clear it once the ticket is saved.
  const draftKey = `ci.ticket.draft:${mode}:${ticket?.ticket_id ?? boardId ?? 'new'}`;
  const baseline = useMemo(() => ({
    title: ticket?.title ?? '',
    description: ticket?.description ?? '',
    priority: ticket?.priority ?? 'medium',
    status: ticket?.status ?? ordered[0]?.value ?? 'open',
    timescale: ticket?.timescale ?? '',
    dueDate: ticket?.due_date ? ticket.due_date.slice(0, 10) : '',
    assigneeId: ticket?.assignee_id ?? '',
    parent: ticket?.parent_id ?? '',
    project: ticket?.project ?? defaultProject ?? '',
  }), [ticket, ordered, defaultProject]);
  // Read any saved draft once, on first render, and seed the fields from it. Only a
  // draft that actually differs from the saved values counts as one to restore.
  const seed = useRef<typeof baseline | null>(null);
  const seededFromDraft = useRef(false);
  if (seed.current === null) {
    const raw = loadDraft(draftKey);
    let d: typeof baseline | null = null;
    if (raw) { try { const p = JSON.parse(raw); if (p && typeof p === 'object') d = { ...baseline, ...p }; } catch { /* stale/corrupt draft — ignore */ } }
    if (d && JSON.stringify(d) !== JSON.stringify(baseline)) { seed.current = d; seededFromDraft.current = true; }
    else seed.current = baseline;
  }
  const init = seed.current;
  const [title, setTitle] = useState(init.title);
  const [description, setDescription] = useState(init.description);
  const [priority, setPriority] = useState(init.priority);
  const [status, setStatus] = useState(init.status);
  const [timescale, setTimescale] = useState(init.timescale);
  const [dueDate, setDueDate] = useState(init.dueDate);
  const [assigneeId, setAssigneeId] = useState(init.assigneeId);
  const [parent, setParent] = useState(init.parent);
  const [project, setProject] = useState(init.project);
  const [restoredDraft, setRestoredDraft] = useState(seededFromDraft.current);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Mirror the form to storage whenever it diverges from the saved baseline; clear
  // the draft when it matches, so there's nothing stale to restore next time.
  const baselineNorm = useMemo(() => JSON.stringify(baseline), [baseline]);
  useEffect(() => {
    const current = JSON.stringify({ title, description, priority, status, timescale, dueDate, assigneeId, parent, project });
    if (current !== baselineNorm) saveDraft(draftKey, current);
    else clearDraft(draftKey);
  }, [title, description, priority, status, timescale, dueDate, assigneeId, parent, project, draftKey, baselineNorm]);

  // Discard the restored draft and snap the form back to the saved values.
  const discardDraft = () => {
    setTitle(baseline.title); setDescription(baseline.description); setPriority(baseline.priority);
    setStatus(baseline.status); setTimescale(baseline.timescale); setDueDate(baseline.dueDate);
    setAssigneeId(baseline.assigneeId); setParent(baseline.parent); setProject(baseline.project);
    clearDraft(draftKey); setRestoredDraft(false);
  };

  // This board's own priority options (per-board, never merged across boards), plus
  // the ticket's current value if it isn't among them so an edit never silently
  // drops it.
  const priorityOptions = useMemo(() => {
    const base = [...priorities].sort((a, b) => a.position - b.position).map(p => p.value);
    return priority && !base.includes(priority) ? [...base, priority] : base;
  }, [priorities, priority]);

  const sortedUsers = useMemo(() => [...users].sort((a, b) => a.username.localeCompare(b.username)), [users]);

  const handleSubmit = async () => {
    if (!title.trim()) return;
    setSubmitting(true);
    setError(null);
    try {
      if (mode === 'edit' && ticket) {
        // "" clears the due date / unassigns; timescale "" is ignored server-side.
        const saved = await updateTicket(token, ticket.ticket_id, {
          title: title.trim(), description: description.trim(), status, priority,
          timescale: timescale.trim(), due_date: dueDate, assignee_id: assigneeId, parent_id: parent,
          project: project.trim(),
        });
        clearDraft(draftKey); // the write-up is saved server-side now — drop the local draft
        onSaved(saved);
      } else {
        // Place the new ticket on the active board so it shows up where the user is looking.
        const saved = await createTicket(token, {
          title: title.trim(), description: description.trim() || undefined, status, priority, board_id: boardId,
          timescale: timescale.trim() || undefined, due_date: dueDate || undefined, assignee_id: assigneeId || undefined,
          parent_id: parent || undefined, project: project.trim() || undefined,
        });
        clearDraft(draftKey);
        onSaved(saved);
      }
    } catch (e: unknown) {
      setError((e as Error).message);
      setSubmitting(false);
    }
  };

  const fieldLabel = (text: string) => (
    <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, marginBottom: 6, letterSpacing: 0.5 }}>{text}</div>
  );
  const inputStyle: CSSProperties = { width: '100%', background: T.cardHi, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 12, padding: '8px 10px', outline: 'none', boxSizing: 'border-box' };

  return (
    <div onClick={onClose} style={{ position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.7)', zIndex: 50, display: 'flex', alignItems: 'center', justifyContent: 'center' }}>
      <div onClick={e => e.stopPropagation()} style={{ width: 480, maxHeight: '90vh', overflowY: 'auto', background: T.card, border: `1px solid ${T.borderHi}` }}>
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '12px 16px', borderBottom: `1px solid ${T.border}`, background: T.cardHi, position: 'sticky', top: 0 }}>
          <span style={{ fontFamily: T.mono, fontSize: 12, color: T.dim }}>{mode === 'edit' ? 'edit ticket' : 'new ticket'}</span>
          <button onClick={onClose} style={{ background: 'transparent', border: 0, color: T.faint, cursor: 'pointer', fontSize: 16 }}>×</button>
        </div>
        <div style={{ padding: '16px 20px' }}>
          {error && <div style={{ background: T.redSoft, border: `1px solid ${T.red}`, padding: '8px 12px', fontFamily: T.mono, fontSize: 11, color: T.red, marginBottom: 12 }}>{error}</div>}
          {restoredDraft && (
            <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 12, background: T.amberSoft, border: `1px solid ${T.amber}`, padding: '7px 12px', fontFamily: T.mono, fontSize: 11, color: T.amber, marginBottom: 12 }}>
              <span>↺ restored an unsaved draft</span>
              <button onClick={discardDraft} style={{ background: 'transparent', border: `1px solid ${T.amber}`, color: T.amber, fontFamily: T.mono, fontSize: 10, padding: '2px 8px', cursor: 'pointer' }}>discard</button>
            </div>
          )}
          <div style={{ marginBottom: 12 }}>
            {fieldLabel('TITLE')}
            <input value={title} onChange={e => setTitle(e.target.value)} onKeyDown={e => e.key === 'Enter' && handleSubmit()} autoFocus
              style={{ ...inputStyle, fontSize: 13 }} />
          </div>
          <div style={{ marginBottom: 12 }}>
            {fieldLabel('DESCRIPTION')}
            <textarea value={description} onChange={e => setDescription(e.target.value)} rows={3}
              style={{ ...inputStyle, resize: 'vertical' }} />
          </div>
          <div style={{ marginBottom: 12 }}>
            {fieldLabel('PRIORITY')}
            <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
              {priorityOptions.map(p => (
                <button key={p} onClick={() => setPriority(p)}
                  style={{ background: priority === p ? T.greenSoft : 'transparent', border: `1px solid ${priority === p ? T.green : T.border}`, color: priority === p ? T.green : T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}>
                  {p}
                </button>
              ))}
            </div>
          </div>
          {ordered.length > 0 && (
            <div style={{ marginBottom: 12 }}>
              {fieldLabel('STATUS')}
              <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
                {ordered.map(s => {
                  const active = status === s.value;
                  const accent = statusColor(s.value, s.color);
                  return (
                    <button key={s.value} onClick={() => setStatus(s.value)}
                      style={{ display: 'flex', alignItems: 'center', gap: 6, background: active ? T.cardHi : 'transparent', border: `1px solid ${active ? accent : T.border}`, color: active ? T.text : T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}>
                      <span style={{ width: 8, height: 8, borderRadius: 2, background: accent, flexShrink: 0 }} />
                      {s.label}
                    </button>
                  );
                })}
              </div>
            </div>
          )}
          <div style={{ display: 'flex', gap: 12, marginBottom: 12 }}>
            <div style={{ flex: 1 }}>
              {fieldLabel('TIMESCALE')}
              <input value={timescale} onChange={e => setTimescale(e.target.value)} placeholder="e.g. Q1 2026" style={inputStyle} />
            </div>
            <div style={{ flex: 1 }}>
              {fieldLabel('DUE DATE')}
              <input type="date" value={dueDate} onChange={e => setDueDate(e.target.value)} style={{ ...inputStyle, colorScheme: 'dark' }} />
            </div>
          </div>
          <div style={{ marginBottom: 16 }}>
            {fieldLabel('PROJECT')}
            <input value={project} onChange={e => setProject(e.target.value)} placeholder="e.g. platform-migration" style={inputStyle} />
          </div>
          <div style={{ marginBottom: 16 }}>
            {fieldLabel('ASSIGNEE')}
            <select value={assigneeId} onChange={e => setAssigneeId(e.target.value)} style={{ ...inputStyle, cursor: 'pointer' }}>
              <option value="">unassigned</option>
              {/* Keep the current assignee selectable even if they aren't in the fetched catalog (e.g. no list permission). */}
              {assigneeId && !sortedUsers.some(u => u.user_id === assigneeId) && <option value={assigneeId}>{shortId(assigneeId)}</option>}
              {sortedUsers.map(u => <option key={u.user_id} value={u.user_id}>{u.username}</option>)}
            </select>
          </div>
          <div style={{ marginBottom: 16 }}>
            {fieldLabel('PARENT TICKET')}
            <select value={parent} onChange={e => setParent(e.target.value)} style={{ ...inputStyle, cursor: 'pointer' }}>
              <option value="">— none (top-level) —</option>
              {/* Keep the current parent selectable even if it's off this board / not in the list. */}
              {parent && !parentOptions.some(t => t.ticket_id === parent) && <option value={parent}>{shortId(parent)}</option>}
              {parentOptions.map(t => <option key={t.ticket_id} value={t.ticket_id}>{t.title}</option>)}
            </select>
          </div>
          <div style={{ display: 'flex', gap: 10 }}>
            <button onClick={handleSubmit} disabled={!title.trim() || submitting}
              style={{ flex: 1, background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 13, fontWeight: 600, padding: '9px 14px', cursor: 'pointer', opacity: (!title.trim() || submitting) ? 0.6 : 1 }}>
              {submitting ? '[ · · · ]' : mode === 'edit' ? '[ save changes ]' : '[ create ticket ]'}
            </button>
            <button onClick={onClose} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 12, padding: '9px 14px', cursor: 'pointer' }}>cancel</button>
          </div>
        </div>
      </div>
    </div>
  );
}

/** Modal to create a new board (name + optional description/color). */
function NewBoardModal({ onCreated, onClose }: { onCreated: (b: Board) => void; onClose: () => void }) {
  const token = useAppSelector(s => s.auth.token)!;
  const [name, setName] = useState('');
  const [description, setDescription] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const handleSubmit = async () => {
    if (!name.trim()) return;
    setSubmitting(true);
    setError(null);
    try {
      onCreated(await createBoard(token, { name: name.trim(), description: description.trim() || undefined }));
    } catch (e: unknown) {
      setError((e as Error).message);
      setSubmitting(false);
    }
  };

  return (
    <div onClick={onClose} style={{ position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.7)', zIndex: 50, display: 'flex', alignItems: 'center', justifyContent: 'center' }}>
      <div onClick={e => e.stopPropagation()} style={{ width: 420, background: T.card, border: `1px solid ${T.borderHi}` }}>
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '12px 16px', borderBottom: `1px solid ${T.border}`, background: T.cardHi }}>
          <span style={{ fontFamily: T.mono, fontSize: 12, color: T.dim }}>new board</span>
          <button onClick={onClose} style={{ background: 'transparent', border: 0, color: T.faint, cursor: 'pointer', fontSize: 16 }}>×</button>
        </div>
        <div style={{ padding: '16px 20px' }}>
          {error && <div style={{ background: T.redSoft, border: `1px solid ${T.red}`, padding: '8px 12px', fontFamily: T.mono, fontSize: 11, color: T.red, marginBottom: 12 }}>{error}</div>}
          <div style={{ marginBottom: 12 }}>
            <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, marginBottom: 6, letterSpacing: 0.5 }}>NAME</div>
            <input value={name} onChange={e => setName(e.target.value)} onKeyDown={e => e.key === 'Enter' && handleSubmit()} autoFocus
              style={{ width: '100%', background: T.cardHi, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 13, padding: '8px 10px', outline: 'none', boxSizing: 'border-box' }} />
          </div>
          <div style={{ marginBottom: 16 }}>
            <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, marginBottom: 6, letterSpacing: 0.5 }}>DESCRIPTION</div>
            <input value={description} onChange={e => setDescription(e.target.value)}
              style={{ width: '100%', background: T.cardHi, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 12, padding: '8px 10px', outline: 'none', boxSizing: 'border-box' }} />
          </div>
          <div style={{ display: 'flex', gap: 10 }}>
            <button onClick={handleSubmit} disabled={!name.trim() || submitting}
              style={{ flex: 1, background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 13, fontWeight: 600, padding: '9px 14px', cursor: 'pointer', opacity: (!name.trim() || submitting) ? 0.6 : 1 }}>
              {submitting ? '[ · · · ]' : '[ create board ]'}
            </button>
            <button onClick={onClose} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 12, padding: '9px 14px', cursor: 'pointer' }}>cancel</button>
          </div>
        </div>
      </div>
    </div>
  );
}

/**
 * Modal to configure status columns: add, rename, recolor, reorder, delete.
 * Scoped to a board when boardId is set (the board owns its own columns); on the
 * "all"/"unassigned" views it edits the org/global default columns.
 */
function ColumnsModal({ statuses, boardId, boardName, onChanged, onClose }: { statuses: TicketFieldDef[]; boardId?: string; boardName?: string; onChanged: () => void; onClose: () => void }) {
  const token = useAppSelector(s => s.auth.token)!;
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [newLabel, setNewLabel] = useState('');
  const ordered = [...statuses].sort((a, b) => a.position - b.position);

  const run = async (fn: () => Promise<unknown>) => {
    setBusy(true);
    setError(null);
    try { await fn(); onChanged(); } catch (e: unknown) { setError((e as Error).message); } finally { setBusy(false); }
  };

  const add = () => {
    const label = newLabel.trim();
    if (!label) return;
    const value = slugify(label);
    if (!value) { setError('label must contain letters or digits'); return; }
    run(async () => { await createTicketFieldDef(token, { kind: 'status', value, label, position: ordered.length, board_id: boardId }); setNewLabel(''); });
  };

  // Swap a column with its neighbour by exchanging positions (skips synthetic default rows that have no real id).
  const move = (i: number, dir: -1 | 1) => {
    const j = i + dir;
    if (j < 0 || j >= ordered.length) return;
    const a = ordered[i], b = ordered[j];
    run(async () => {
      await Promise.all([
        updateTicketFieldDef(token, a.field_def_id, { position: b.position }),
        updateTicketFieldDef(token, b.field_def_id, { position: a.position }),
      ]);
    });
  };

  return (
    <div onClick={onClose} style={{ position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.7)', zIndex: 50, display: 'flex', alignItems: 'center', justifyContent: 'center' }}>
      <div onClick={e => e.stopPropagation()} style={{ width: 520, maxHeight: '80vh', background: T.card, border: `1px solid ${T.borderHi}`, display: 'flex', flexDirection: 'column' }}>
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '12px 16px', borderBottom: `1px solid ${T.border}`, background: T.cardHi }}>
          <span style={{ fontFamily: T.mono, fontSize: 12, color: T.dim }}>configure status columns</span>
          <button onClick={onClose} style={{ background: 'transparent', border: 0, color: T.faint, cursor: 'pointer', fontSize: 16 }}>×</button>
        </div>
        <div style={{ padding: '14px 18px', overflow: 'auto' }}>
          <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, marginBottom: 12, lineHeight: 1.5 }}>
            {boardId
              ? <>Status columns for board <span style={{ color: T.green }}>◆ {boardName}</span> — they belong to this board only.</>
              : <>Default status columns for new boards and unassigned tickets.</>}
          </div>
          {error && <div style={{ background: T.redSoft, border: `1px solid ${T.red}`, padding: '8px 12px', fontFamily: T.mono, fontSize: 11, color: T.red, marginBottom: 12 }}>{error}</div>}
          {ordered.map((s, i) => (
            <div key={s.field_def_id} style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 8 }}>
              <input type="color" value={s.color || '#7d8a78'} disabled={busy}
                onChange={e => run(async () => { await updateTicketFieldDef(token, s.field_def_id, { color: e.target.value }); })}
                style={{ width: 26, height: 26, background: 'transparent', border: `1px solid ${T.border}`, cursor: 'pointer', flexShrink: 0 }} />
              <input defaultValue={s.label} disabled={busy}
                onBlur={e => { if (e.target.value.trim() && e.target.value !== s.label) run(async () => { await updateTicketFieldDef(token, s.field_def_id, { label: e.target.value.trim() }); }); }}
                style={{ flex: 1, background: T.cardHi, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 12, padding: '5px 8px', outline: 'none' }} />
              <span style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, width: 90, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{s.value}</span>
              <button onClick={() => move(i, -1)} disabled={busy || i === 0} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '3px 7px', cursor: 'pointer', opacity: i === 0 ? 0.3 : 1 }}>↑</button>
              <button onClick={() => move(i, 1)} disabled={busy || i === ordered.length - 1} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '3px 7px', cursor: 'pointer', opacity: i === ordered.length - 1 ? 0.3 : 1 }}>↓</button>
              <button onClick={() => run(async () => { await deleteTicketFieldDef(token, s.field_def_id); })} disabled={busy} style={{ background: T.redSoft, border: `1px solid ${T.red}`, color: T.red, fontFamily: T.mono, fontSize: 11, padding: '3px 7px', cursor: 'pointer' }}>✕</button>
            </div>
          ))}
          <div style={{ display: 'flex', gap: 8, marginTop: 14, borderTop: `1px solid ${T.border}`, paddingTop: 14 }}>
            <input value={newLabel} onChange={e => setNewLabel(e.target.value)} onKeyDown={e => e.key === 'Enter' && add()} placeholder="new column label…"
              style={{ flex: 1, background: T.cardHi, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 12, padding: '6px 9px', outline: 'none' }} />
            <button onClick={add} disabled={busy || !newLabel.trim()} style={{ background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 11, padding: '6px 14px', cursor: 'pointer', opacity: (busy || !newLabel.trim()) ? 0.6 : 1 }}>[ add ]</button>
          </div>
        </div>
      </div>
    </div>
  );
}

/** Ticket kanban: first-class board switcher + status columns with drag-to-move, a detail drawer, and column config. */
export function Tickets() {
  const token = useAppSelector(s => s.auth.token)!;
  // The selected project scopes the board to that project's tickets (null = all).
  const project = useAppSelector(s => s.project.current);
  const users = useUsers(token);
  // Derive the id→username map from the same catalog the assignee picker uses, to avoid a second fetch.
  const userNames = useMemo(() => Object.fromEntries(users.map(u => [u.user_id, u.username])), [users]);
  const workflowNames = useWorkflowNames(token);
  const [tickets, setTickets] = useState<Ticket[]>([]);
  const [boards, setBoards] = useState<Board[]>([]);
  const [statuses, setStatuses] = useState<TicketFieldDef[]>(DEFAULT_STATUSES);
  const [priorities, setPriorities] = useState<TicketFieldDef[]>(DEFAULT_PRIORITIES);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  // Selected board key: always a board_id (every ticket belongs to a board — there is
  // no "all" or unassigned view). null only briefly before the first board resolves.
  // Seeded from the last board the user had open; the default is picked once boards load.
  const [board, setBoard] = useState<string | null>(() => localStorage.getItem(LAST_BOARD_KEY));
  // When on, the board switcher lists only boards that have at least one open ticket.
  const [openBoardsOnly, setOpenBoardsOnly] = useState(false);
  const [selected, setSelected] = useUrlParam('ticket');
  const [showCreate, setShowCreate] = useState(false);
  // The ticket currently being edited in the form modal (null = not editing), plus
  // the status columns of that ticket's own board — which can differ from the board
  // being viewed (e.g. on the "all" view), so the edit form scopes to the ticket.
  const [editing, setEditing] = useState<Ticket | null>(null);
  const [editStatuses, setEditStatuses] = useState<TicketFieldDef[]>(DEFAULT_STATUSES);
  const [editPriorities, setEditPriorities] = useState<TicketFieldDef[]>(DEFAULT_PRIORITIES);
  const [showNewBoard, setShowNewBoard] = useState(false);
  const [showColumns, setShowColumns] = useState(false);
  const [newComment, setNewComment] = useState('');
  const [commenting, setCommenting] = useState(false);
  // Drag state: the ticket being dragged and the status column hovered over.
  const [dragId, setDragId] = useState<string | null>(null);
  const [dragOver, setDragOver] = useState<string | null>(null);

  // Resolve a board key to the board_id used for board-scoped field defs. board is
  // always a real id once loaded; null (pre-load) → undefined = org/global fallback.
  const scopedBoardId = useCallback((key: string | null) => key ?? undefined, []);

  // Status columns AND priority options are owned per-board (never merged across
  // boards), so both are fetched for the selected board — separately from the
  // board-independent ticket and board lists. Named fetchStatuses for its callers.
  const fetchStatuses = useCallback(async (key: string | null) => {
    const bid = scopedBoardId(key);
    try {
      const [st, pr] = await Promise.all([
        listTicketFieldDefs(token, 'status', bid),
        listTicketFieldDefs(token, 'priority', bid),
      ]);
      setStatuses(st.length > 0 ? [...st].sort((a, b) => a.position - b.position) : DEFAULT_STATUSES);
      setPriorities(pr.length > 0 ? [...pr].sort((a, b) => a.position - b.position) : DEFAULT_PRIORITIES);
    } catch {
      setStatuses(DEFAULT_STATUSES);
      setPriorities(DEFAULT_PRIORITIES);
    }
  }, [token, scopedBoardId]);

  // Open the edit form for a ticket, loading its own board's status columns and
  // priority options so both pickers match where the ticket lives, not the viewed board.
  const openEdit = useCallback((t: Ticket) => {
    setEditing(t);
    setEditStatuses(statuses); // sensible placeholders until the ticket's board defs load
    setEditPriorities(priorities);
    const bid = t.board_id ?? undefined;
    listTicketFieldDefs(token, 'status', bid)
      .then(defs => setEditStatuses(defs.length > 0 ? [...defs].sort((a, b) => a.position - b.position) : DEFAULT_STATUSES))
      .catch(() => setEditStatuses(DEFAULT_STATUSES));
    listTicketFieldDefs(token, 'priority', bid)
      .then(defs => setEditPriorities(defs.length > 0 ? [...defs].sort((a, b) => a.position - b.position) : DEFAULT_PRIORITIES))
      .catch(() => setEditPriorities(DEFAULT_PRIORITIES));
  }, [token, statuses, priorities]);

  // silent skips the loading flash + error banner, for the background 5s poll so it
  // doesn't blink the board or clobber a transient error the user is reading.
  const fetchData = useCallback(async (silent = false) => {
    if (!silent) { setLoading(true); setError(null); }
    try {
      const [tk, bd] = await Promise.all([
        listTickets(token, project ?? undefined),
        listBoards(token).catch(() => [] as Board[]),
      ]);
      setTickets(tk);
      setBoards(bd);
    } catch (e: unknown) {
      if (!silent) setError((e as Error).message);
    } finally {
      if (!silent) setLoading(false);
    }
  }, [token, project]);

  useEffect(() => { fetchData(); }, [fetchData]);

  // Refetch the status columns whenever the selected board changes.
  useEffect(() => { fetchStatuses(board); }, [board, fetchStatuses]);

  // Live-refresh the board every 5s so ticket changes — status moves, new tickets,
  // comments, including ones made from the CLI — appear without a manual reload.
  // Silent (no loading flash) and paused mid-drag so a background refetch never yanks
  // the board out from under a drag in progress.
  useEffect(() => {
    if (dragId) return;
    const id = setInterval(() => { fetchData(true); fetchStatuses(board); }, 5000);
    return () => clearInterval(id);
  }, [fetchData, fetchStatuses, board, dragId]);

  // Remember the open board so the next visit restores it (see LAST_BOARD_KEY).
  useEffect(() => { if (board) localStorage.setItem(LAST_BOARD_KEY, board); }, [board]);

  // Pick the default board once boards load — and re-pick if the selected one
  // disappears (deleted elsewhere). Every ticket belongs to a board, so there is no
  // "all"/unassigned fallback: choose the last board the user had open, else the
  // board with the most recent ticket activity, else the first board.
  useEffect(() => {
    if (boards.length === 0) return;
    if (board && boards.some(b => b.board_id === board)) return; // current selection still valid
    const last = localStorage.getItem(LAST_BOARD_KEY);
    if (last && boards.some(b => b.board_id === last)) { setBoard(last); return; }
    const latest = new Map<string, number>();
    for (const t of tickets) {
      if (!t.board_id) continue;
      latest.set(t.board_id, Math.max(latest.get(t.board_id) ?? 0, new Date(t.updated_at).getTime()));
    }
    const byActivity = [...boards]
      .filter(b => latest.has(b.board_id))
      .sort((a, b) => latest.get(b.board_id)! - latest.get(a.board_id)!);
    setBoard((byActivity[0] ?? boards[0]).board_id);
  }, [boards, tickets, board]);

  // Open/total for a board: prefer the server-computed counts (accurate beyond the
  // ticket-list page cap), falling back to the loaded tickets.
  const boardCounts = useCallback((b: Board) => ({
    open: b.open_count ?? tickets.filter(t => t.board_id === b.board_id && !isTerminal(t.status)).length,
    total: b.total_count ?? tickets.filter(t => t.board_id === b.board_id).length,
  }), [tickets]);

  const visibleTickets = useMemo(() => tickets.filter(t => t.board_id === board), [tickets, board]);

  const selectedTicket = tickets.find(t => t.ticket_id === selected);
  const selectedBoard = boards.find(b => b.board_id === board);
  const boardLabel = selectedBoard?.name ?? '';

  /** Replace a ticket in local state with the server's updated copy. */
  const applyUpdated = (updated: Ticket) =>
    setTickets(prev => prev.map(t => (t.ticket_id === updated.ticket_id ? { ...t, ...updated } : t)));

  /** Move a ticket to a new status (drag-drop or detail toggle), optimistically then reconciled with the server. */
  const moveTicket = async (ticketId: string, newStatus: string) => {
    const ticket = tickets.find(t => t.ticket_id === ticketId);
    if (!ticket || ticket.status === newStatus) return;
    const prev = ticket.status;
    setTickets(cur => cur.map(t => (t.ticket_id === ticketId ? { ...t, status: newStatus } : t)));
    try {
      applyUpdated(await updateTicket(token, ticketId, { status: newStatus }));
    } catch (e: unknown) {
      setTickets(cur => cur.map(t => (t.ticket_id === ticketId ? { ...t, status: prev } : t)));
      setError((e as Error).message);
    }
  };

  const handleStatusToggle = async () => {
    if (!selectedTicket) return;
    await moveTicket(selectedTicket.ticket_id, isTerminal(selectedTicket.status) ? 'open' : 'closed');
  };

  const [confirm, confirmEl] = useConfirm();
  const [railW, railHandle] = useResizableWidth('rail.tickets.main', 230, { min: 190, max: 420 });

  // Escape clears the selection, closing the detail panel. Until this existed the only
  // way out was the small × in the panel header — selecting was a click anywhere on a
  // card, deselecting was one 16px target, so the board could easily feel stuck with a
  // ticket open. Clicking the selected card again also toggles it off (see the card's
  // onClick); this covers the same intent from the keyboard.
  //
  // Suppressed while any modal or the confirm dialog is up, because those own Escape.
  // ConfirmDialog listens on window too and calls stopPropagation, which does NOT stop
  // a sibling listener on the same target — so without this guard, Escape on the delete
  // prompt would cancel the delete AND close the panel behind it.
  const anyOverlayOpen = confirmEl != null || editing != null || showCreate || showNewBoard || showColumns;
  useEffect(() => {
    if (!selected || anyOverlayOpen) return;
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') setSelected(null); };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [selected, anyOverlayOpen, setSelected]);

  const handleDelete = async () => {
    if (!selectedTicket) return;
    if (!(await confirm({ message: `Delete ticket "${selectedTicket.title}"? This cannot be undone.` }))) return;
    try {
      await deleteTicket(token, selectedTicket.ticket_id);
      setTickets(prev => prev.filter(t => t.ticket_id !== selectedTicket.ticket_id));
      setSelected(null);
    } catch (e: unknown) {
      setError((e as Error).message);
    }
  };

  const handleDeleteBoard = async (b: Board) => {
    // Deleting a board also deletes its tickets, so require the user to type the
    // board's exact name to confirm.
    if (!(await confirm({
      title: 'Delete board',
      message: <>Deleting <b>"{b.name}"</b> permanently deletes the board <b>and all of its tickets</b>. This cannot be undone. Type the board name to confirm.</>,
      confirmLabel: 'delete board',
      requireText: b.name,
    }))) return;
    try {
      await deleteBoard(token, b.board_id, b.name);
      if (board === b.board_id) setBoard(null);
      // Drop the board and its now-deleted tickets locally so nothing lingers
      // until the next refresh.
      setTickets(prev => prev.filter(t => t.board_id !== b.board_id));
      setBoards(prev => prev.filter(x => x.board_id !== b.board_id));
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
        t.ticket_id === selectedTicket.ticket_id ? { ...t, comments: [...(t.comments ?? []), comment] } : t,
      ));
      setNewComment('');
    } catch (e: unknown) {
      setError((e as Error).message);
    } finally {
      setCommenting(false);
    }
  };

  // The board_id to tag newly-created tickets with: always the active board.
  const createBoardId = board ?? undefined;

  /** A board switcher row: name + open/total ticket counts, highlighted when active, with an optional delete affordance. */
  const BoardRow = ({ label, value, open, total, deletable }: { label: string; value: string | null; open: number; total: number; deletable?: Board }) => {
    const active = board === value;
    return (
      <div style={{ display: 'flex', alignItems: 'center', background: active ? T.greenSoft : 'transparent', borderLeft: `2px solid ${active ? T.green : 'transparent'}` }}
        onMouseEnter={e => { const x = e.currentTarget.querySelector<HTMLElement>('[data-del]'); if (x) x.style.opacity = '1'; }}
        onMouseLeave={e => { const x = e.currentTarget.querySelector<HTMLElement>('[data-del]'); if (x) x.style.opacity = '0'; }}>
        <button onClick={() => setBoard(value)}
          style={{ flex: 1, textAlign: 'left', padding: '9px 6px 9px 12px', background: 'transparent', border: 0, fontFamily: T.mono, cursor: 'pointer', color: active ? T.textHi : T.text, display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 8, minWidth: 0 }}>
          <span style={{ fontSize: 13, fontWeight: active ? 700 : 500, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{label}</span>
          <span title={`${open} open · ${total} total`} style={{ fontSize: 10, flexShrink: 0, fontVariantNumeric: 'tabular-nums' }}>
            <span style={{ color: open > 0 ? T.amber : T.faint }}>{open}</span>
            <span style={{ color: T.faint }}>/{total}</span>
          </span>
        </button>
        {deletable && (
          <button data-del onClick={() => handleDeleteBoard(deletable)} title="delete board"
            style={{ opacity: 0, transition: 'opacity .12s', background: 'transparent', border: 0, color: T.faint, fontFamily: T.mono, fontSize: 12, padding: '0 10px', cursor: 'pointer' }}>✕</button>
        )}
      </div>
    );
  };

  return (
    <div style={{ display: 'flex', height: '100%', overflow: 'hidden' }}>
      {confirmEl}
      {/* Left panel — board switcher */}
      <div style={{ width: railW, flexShrink: 0, borderRight: `1px solid ${T.border}`, display: 'flex', flexDirection: 'column', background: T.bgAlt }}>
        <div style={{ padding: '14px 14px 10px', borderBottom: `1px solid ${T.border}` }}>
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 4 }}>
            <span style={{ fontFamily: T.mono, fontSize: 13, fontWeight: 700, color: T.textHi }}>boards/</span>
            <div style={{ display: 'flex', gap: 6 }}>
              <button onClick={() => setOpenBoardsOnly(v => !v)} title={openBoardsOnly ? 'showing boards with open tickets — click to show all' : 'show only boards with open tickets'} style={{ background: openBoardsOnly ? T.amberSoft : 'transparent', border: `1px solid ${openBoardsOnly ? T.amber : T.border}`, color: openBoardsOnly ? T.amber : T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 7px', cursor: 'pointer' }}>open</button>
              <button onClick={() => setShowNewBoard(true)} title="new board" style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 7px', cursor: 'pointer' }}>+ board</button>
              <button onClick={() => { fetchData(); fetchStatuses(board); }} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 7px', cursor: 'pointer' }}>↻</button>
            </div>
          </div>
          <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint }}>
            {tickets.length > 0 && `${tickets.filter(t => !isTerminal(t.status)).length} open · ${tickets.length} total`}
          </div>
        </div>

        <div style={{ flex: 1, overflow: 'auto' }}>
          {loading ? (
            <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint, animation: 'pulse 1s ease-in-out infinite' }}>→ loading · · ·</div>
          ) : error ? (
            <div style={{ padding: '14px', fontFamily: T.mono, fontSize: 11, color: T.red }}>{error}</div>
          ) : (
            <>
              {boards
                .filter(b => !openBoardsOnly || boardCounts(b).open > 0 || b.board_id === board)
                .map(b => {
                  const c = boardCounts(b);
                  return <BoardRow key={b.board_id} label={b.name} value={b.board_id} open={c.open} total={c.total} deletable={b} />;
                })}
              {boards.length === 0 && (
                <div style={{ padding: '14px', fontFamily: T.mono, fontSize: 10, color: T.faint, lineHeight: 1.6 }}>
                  → create a board to group your tickets
                </div>
              )}
            </>
          )}
        </div>
      </div>
      {railHandle}

      {/* Right panel — the kanban board for the selected board. order 3 keeps it to
          the right of the detail panel (order 2) when a ticket is open. */}
      <div style={{ order: 3, flex: 1, minWidth: 0, display: 'flex', flexDirection: 'column', overflow: 'hidden' }}>
        <div style={{ padding: '12px 20px', borderBottom: `1px solid ${T.border}`, background: T.bgAlt, display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
          <div style={{ fontFamily: T.mono, fontSize: 11, color: T.faint }}>
            <span style={{ color: T.green }}>$</span> armory tickets board{boardLabel ? ` · ${boardLabel}` : ''}
          </div>
          <div style={{ display: 'flex', gap: 8 }}>
            <button onClick={() => setShowColumns(true)} title="configure status columns" style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}>⚙ columns</button>
            <button onClick={() => setShowCreate(true)} style={{ background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer', fontWeight: 600 }}>[ + new ticket ]</button>
          </div>
        </div>

        {loading ? (
          <div style={{ padding: '20px 24px', fontFamily: T.mono, fontSize: 12, color: T.faint }}>→ loading · · ·</div>
        ) : (
          <div style={{ flex: 1, display: 'flex', gap: 0, overflowX: 'auto', overflowY: 'hidden' }}>
            {statuses.map(s => {
              const cards = visibleTickets.filter(t => t.status === s.value);
              const accent = statusColor(s.value, s.color);
              const isDropTarget = dragOver === s.value;
              return (
                <div key={s.value}
                  onDragOver={e => { e.preventDefault(); if (dragOver !== s.value) setDragOver(s.value); }}
                  onDragLeave={e => { if (!e.currentTarget.contains(e.relatedTarget as Node)) setDragOver(d => (d === s.value ? null : d)); }}
                  onDrop={e => { e.preventDefault(); setDragOver(null); if (dragId) moveTicket(dragId, s.value); setDragId(null); }}
                  style={{ width: 280, flexShrink: 0, display: 'flex', flexDirection: 'column', borderRight: `1px solid ${T.border}`, background: isDropTarget ? T.greenFaint : 'transparent', transition: 'background .12s' }}>
                  <div style={{ padding: '12px 14px 8px', display: 'flex', alignItems: 'center', justifyContent: 'space-between', borderBottom: `1px solid ${T.border}` }}>
                    <span style={{ display: 'flex', alignItems: 'center', gap: 7, fontFamily: T.mono, fontSize: 12, fontWeight: 700, color: T.textHi, textTransform: 'lowercase' }}>
                      <span style={{ width: 8, height: 8, borderRadius: 2, background: accent, flexShrink: 0 }} />
                      {s.label}
                    </span>
                    <span style={{ fontFamily: T.mono, fontSize: 10, color: T.faint }}>{cards.length}</span>
                  </div>
                  <div style={{ flex: 1, overflowY: 'auto', padding: '10px 10px 16px', display: 'flex', flexDirection: 'column', gap: 8 }}>
                    {cards.length === 0 ? (
                      <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, textAlign: 'center', padding: '16px 0', opacity: isDropTarget ? 1 : 0.5 }}>
                        {isDropTarget ? '↓ drop here' : '—'}
                      </div>
                    ) : cards.map(ticket => {
                      const onBoard = boards.find(b => b.board_id === ticket.board_id);
                      // Re-clicking the open card closes it. Selecting was a click anywhere
                      // on the card while deselecting was only the × in the detail header,
                      // so the obvious gesture did nothing at all.
                      const toggleSelect = () => setSelected(selected === ticket.ticket_id ? null : ticket.ticket_id);
                      return (
                        <div key={ticket.ticket_id} draggable
                          onDragStart={() => setDragId(ticket.ticket_id)}
                          onDragEnd={() => { setDragId(null); setDragOver(null); }}
                          onClick={toggleSelect}
                          style={{ background: T.card, border: `1px solid ${selected === ticket.ticket_id ? T.green : T.border}`, borderLeft: `2px solid ${priorityColor(ticket.priority)}`, padding: '9px 11px', cursor: 'grab', fontFamily: T.mono, opacity: dragId === ticket.ticket_id ? 0.4 : 1, transition: 'border-color .12s, opacity .12s' }}>
                          <div style={{ fontSize: 12.5, fontWeight: 600, color: T.text, lineHeight: 1.35, marginBottom: 5, wordBreak: 'break-word' }}>{ticket.title}</div>
                          <div style={{ display: 'flex', alignItems: 'center', gap: 8, fontSize: 10, color: T.faint, flexWrap: 'wrap' }}>
                            <span style={{ color: priorityColor(ticket.priority) }}>● {ticket.priority ?? 'none'}</span>
                            <span>· {timeAgo(ticket.updated_at)} ago</span>
                            {ticket.due_date && <span style={{ color: isOverdue(ticket) ? T.red : T.faint }}>· ⏱ {ticket.due_date.slice(5, 10)}</span>}
                            {ticket.assignee_id && <span style={{ color: T.dim }}>· @{userNames[ticket.assignee_id] ?? shortId(ticket.assignee_id)}</span>}
                          </div>
                          {/* Show the board tag on "all" / "unassigned" views, where cards span boards. */}
                          {board !== ticket.board_id && onBoard && (
                            <div style={{ fontSize: 10, color: T.green, marginTop: 4 }}>◆ {onBoard.name}</div>
                          )}
                        </div>
                      );
                    })}
                  </div>
                </div>
              );
            })}
          </div>
        )}
      </div>

      {/* Ticket detail — an inline panel immediately right of the board selector,
          ~1/4 of the screen wide; the kanban (order 3) fills the space to its right.
          flex `order` places it between the rail and the board without moving the JSX. */}
      {selectedTicket && (
        <div style={{ order: 2, width: '25%', minWidth: 300, flexShrink: 0, height: '100%', display: 'flex' }}>
          <div style={{ flex: 1, background: T.bg, borderRight: `1px solid ${T.borderHi}`, display: 'flex', flexDirection: 'column', overflow: 'hidden' }}>
            <div style={{ padding: '12px 20px', borderBottom: `1px solid ${T.border}`, background: T.bgAlt, display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
              <div style={{ fontFamily: T.mono, fontSize: 11, color: T.faint, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                <span style={{ color: T.green }}>$</span> {selectedTicket.title}
              </div>
              <div style={{ display: 'flex', gap: 8, flexShrink: 0 }}>
                <button onClick={() => openEdit(selectedTicket)}
                  style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}>
                  [ ✎ edit ]
                </button>
                <button onClick={handleStatusToggle}
                  style={{ background: T.greenSoft, border: `1px solid ${T.green}`, color: T.green, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}>
                  [ {isTerminal(selectedTicket.status) ? '↺ reopen' : '✓ close'} ]
                </button>
                <button onClick={handleDelete}
                  style={{ background: T.redSoft, border: `1px solid ${T.red}`, color: T.red, fontFamily: T.mono, fontSize: 11, padding: '5px 12px', cursor: 'pointer' }}>
                  [ delete ]
                </button>
                <button onClick={() => setSelected(null)} style={{ background: 'transparent', border: 0, color: T.faint, cursor: 'pointer', fontSize: 16 }}>×</button>
              </div>
            </div>

            <div style={{ flex: 1, overflow: 'auto', padding: '20px 24px' }}>
              <div style={{ display: 'flex', alignItems: 'center', gap: 12, marginBottom: 6, flexWrap: 'wrap' }}>
                <h2 style={{ margin: 0, fontFamily: T.mono, fontSize: 18, color: T.textHi, fontWeight: 700 }}>{selectedTicket.title}</h2>
                <Pill tone={statusTone(selectedTicket.status)}>{selectedTicket.status}</Pill>
                {selectedTicket.priority && (
                  <span style={{ fontFamily: T.mono, fontSize: 10, color: priorityColor(selectedTicket.priority), border: `1px solid ${priorityColor(selectedTicket.priority)}`, padding: '2px 6px', letterSpacing: 0.5 }}>
                    {selectedTicket.priority}
                  </span>
                )}
              </div>
              <div style={{ fontFamily: T.mono, fontSize: 11, color: T.faint, marginBottom: 12 }}>
                created {timeAgo(selectedTicket.created_at)} ago · updated {timeAgo(selectedTicket.updated_at)} ago
                {selectedTicket.board_id && boards.find(b => b.board_id === selectedTicket.board_id) && <> · ◆ {boards.find(b => b.board_id === selectedTicket.board_id)!.name}</>}
              </div>

              {/* Detail metadata — reporter/assignee/timescale/due date/project (edit via the ✎ edit form). */}
              <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '10px 14px', marginBottom: 20, fontFamily: T.mono, fontSize: 11, display: 'grid', gridTemplateColumns: 'max-content 1fr', columnGap: 14, rowGap: 6, alignItems: 'baseline' }}>
                <span style={{ color: T.faint }}>reporter</span>
                <span style={{ color: T.dim }}>{userNames[selectedTicket.created_by] ?? shortId(selectedTicket.created_by)}</span>
                <span style={{ color: T.faint }}>assignee</span>
                <span style={{ color: selectedTicket.assignee_id ? T.dim : T.faint }}>
                  {selectedTicket.assignee_id ? (userNames[selectedTicket.assignee_id] ?? shortId(selectedTicket.assignee_id)) : 'unassigned'}
                </span>
                {selectedTicket.timescale && <>
                  <span style={{ color: T.faint }}>timescale</span>
                  <span style={{ color: T.dim }}>{selectedTicket.timescale}</span>
                </>}
                {selectedTicket.due_date && <>
                  <span style={{ color: T.faint }}>due</span>
                  <span style={{ color: isOverdue(selectedTicket) ? T.red : T.dim }}>
                    {selectedTicket.due_date.slice(0, 10)}{isOverdue(selectedTicket) ? ' · overdue' : ''}
                  </span>
                </>}
                {selectedTicket.project && <>
                  <span style={{ color: T.faint }}>project</span>
                  <span style={{ color: T.dim }}>{selectedTicket.project}</span>
                </>}
                {selectedTicket.parent_id && <>
                  <span style={{ color: T.faint }}>parent</span>
                  <span onClick={() => setSelected(selectedTicket.parent_id!)}
                    style={{ color: T.green, cursor: 'pointer', overflow: 'hidden', textOverflow: 'ellipsis' }}>
                    ↑ {tickets.find(t => t.ticket_id === selectedTicket.parent_id)?.title ?? shortId(selectedTicket.parent_id)}
                  </span>
                </>}
              </div>

              {(() => {
                const children = tickets.filter(t => t.parent_id === selectedTicket.ticket_id);
                if (children.length === 0) return null;
                return (
                  <div style={{ marginBottom: 20 }}>
                    <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 8 }}>SUB-TICKETS · {children.length}</div>
                    {children.map(c => (
                      <div key={c.ticket_id} onClick={() => setSelected(c.ticket_id)}
                        style={{ background: T.card, border: `1px solid ${T.border}`, borderLeft: `2px solid ${priorityColor(c.priority)}`, padding: '7px 11px', marginBottom: 6, cursor: 'pointer', fontFamily: T.mono, fontSize: 12, color: T.text, display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 8 }}>
                        <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{c.title}</span>
                        <Pill tone={statusTone(c.status)}>{c.status}</Pill>
                      </div>
                    ))}
                  </div>
                );
              })()}

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
          </div>
        </div>
      )}

      {showCreate && (
        <TicketFormModal
          mode="create"
          boardId={createBoardId}
          defaultProject={project ?? ''}
          statuses={statuses}
          priorities={priorities}
          parentOptions={visibleTickets}
          users={users}
          onSaved={t => { setTickets(prev => [t, ...prev]); setSelected(t.ticket_id); setShowCreate(false); }}
          onClose={() => setShowCreate(false)}
        />
      )}
      {editing && (
        <TicketFormModal
          mode="edit"
          ticket={editing}
          // editStatuses/editPriorities are scoped to the ticket's own board (loaded in openEdit).
          statuses={editStatuses}
          priorities={editPriorities}
          parentOptions={tickets.filter(t => t.board_id === editing.board_id && t.ticket_id !== editing.ticket_id)}
          users={users}
          onSaved={t => { applyUpdated(t); setEditing(null); }}
          onClose={() => setEditing(null)}
        />
      )}
      {showNewBoard && (
        <NewBoardModal
          onCreated={b => { setBoards(prev => [...prev, b]); setBoard(b.board_id); setShowNewBoard(false); }}
          onClose={() => setShowNewBoard(false)}
        />
      )}
      {showColumns && (
        <ColumnsModal
          statuses={statuses}
          boardId={createBoardId}
          boardName={selectedBoard?.name}
          onChanged={() => { fetchStatuses(board); }}
          onClose={() => setShowColumns(false)}
        />
      )}
    </div>
  );
}

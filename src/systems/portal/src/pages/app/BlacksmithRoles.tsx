/**
 * Blacksmith Roles page: edit the agent ROLE definitions blacksmith runs. A role
 * is what makes one stage different from another — its prompt, what it may write
 * (the guard), which tools it is offered, the command that decides it succeeded,
 * its budgets. A workflow picks a role by name on the blacksmith/agent action, so
 * editing one here changes how that stage behaves everywhere it runs. Left is the
 * role list plus a "new" button; right is the selected role's editor (Save = PUT,
 * with a confirm-gated delete). All calls go through the BFF to /api/blacksmith/*.
 */
import { useState, useEffect, useCallback } from 'react';
import { useUrlParam } from '../../hooks/useUrlState';
import { T } from '../../theme';
import { useResizableWidth } from '../../components/ResizeHandle';
import { useConfirm } from '../../components/ConfirmDialog';
import { useAppSelector } from '../../store/hooks';
import {
  listBlacksmithRoles, putBlacksmithRole, deleteBlacksmithRole,
} from '../../api/bff';
import type { BlacksmithRole, BlacksmithGuard } from '../../api/bff';

// The tools blacksmith offers. A role's tool set is a subset of these; empty
// offers all. Names match internal/tools/tools.go.
const TOOL_NAMES = [
  'read_files', 'list_files', 'search_files',
  'write_file', 'undo_edit', 'run_command',
  'file_ticket', 'merge_fix',
];

// Guard editing options. The flat kinds map straight to a GuardConfig; the last
// one is the composite dev/fix guard (no test edits + Go only), the one "both" the
// roles actually use — offered as a single choice rather than a recursive editor.
const GUARD_OPTIONS = [
  { id: 'allow_all', label: 'allow all writes' },
  { id: 'deny_all', label: 'deny all writes (reviewers)' },
  { id: 'no_tests', label: 'anything but test files' },
  { id: 'only_ext', label: 'only these extensions' },
  { id: 'only_basenames', label: 'only these filenames' },
  { id: 'no_tests_ext', label: 'no test files + only these extensions' },
];

// guardToUI collapses a stored guard into (option id, exts, names) for the form.
function guardToUI(g: BlacksmithGuard | null): { mode: string; exts: string; names: string } {
  if (!g) return { mode: 'deny_all', exts: '', names: '' };
  if (g.kind === 'only_ext') return { mode: 'only_ext', exts: (g.exts ?? []).join(', '), names: '' };
  if (g.kind === 'only_basenames') return { mode: 'only_basenames', exts: '', names: (g.names ?? []).join(', ') };
  if (g.kind === 'both' && g.a?.kind === 'no_tests' && g.b?.kind === 'only_ext') {
    return { mode: 'no_tests_ext', exts: (g.b.exts ?? []).join(', '), names: '' };
  }
  return { mode: g.kind, exts: '', names: '' };
}

// uiToGuard rebuilds the stored guard from the form fields.
function uiToGuard(mode: string, exts: string, names: string): BlacksmithGuard {
  const extList = exts.split(',').map(s => s.trim()).filter(Boolean);
  const nameList = names.split(',').map(s => s.trim()).filter(Boolean);
  switch (mode) {
    case 'only_ext': return { kind: 'only_ext', exts: extList };
    case 'only_basenames': return { kind: 'only_basenames', names: nameList };
    case 'no_tests_ext': return { kind: 'both', a: { kind: 'no_tests' }, b: { kind: 'only_ext', exts: extList } };
    case 'allow_all': return { kind: 'allow_all' };
    case 'no_tests': return { kind: 'no_tests' };
    default: return { kind: 'deny_all' };
  }
}

// A blank role, for the "new" form.
function blankRole(): BlacksmithRole {
  return {
    name: '', prompt: '', class: 'large', tools: ['read_files', 'list_files', 'search_files'],
    guard: { kind: 'deny_all' }, ticket_kind: '', check: '',
    rewrite_whole: false, attempt_timeout_secs: 0, respins: 0,
    own_check: false, seed_known: false, max_iterations: 20, temperature: 0.2, max_tokens: 8000,
  };
}

const inputStyle = { width: '100%', background: T.cardHi, border: `1px solid ${T.border}`, color: T.text, fontFamily: T.mono, fontSize: 12, padding: '6px 8px', outline: 'none', boxSizing: 'border-box' as const };
const labelStyle = { fontFamily: T.mono, fontSize: 10, color: T.faint, marginBottom: 4, marginTop: 12 };

/** Editor for one role (existing or new). Holds a local draft, saves via PUT. */
function RoleEditor({ initial, isNew, onSaved, onDeleted }: {
  initial: BlacksmithRole; isNew: boolean;
  onSaved: (r: BlacksmithRole) => void; onDeleted: (name: string) => void;
}) {
  const token = useAppSelector(s => s.auth.token)!;
  const [draft, setDraft] = useState<BlacksmithRole>(initial);
  const [gui, setGui] = useState(() => guardToUI(initial.guard));
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [confirm, confirmEl] = useConfirm();

  // Reload the form when a different role is selected.
  useEffect(() => { setDraft(initial); setGui(guardToUI(initial.guard)); setError(null); }, [initial]);

  const set = <K extends keyof BlacksmithRole>(k: K, v: BlacksmithRole[K]) => setDraft(d => ({ ...d, [k]: v }));

  const toggleTool = (name: string) => setDraft(d => ({
    ...d, tools: d.tools.includes(name) ? d.tools.filter(t => t !== name) : [...d.tools, name],
  }));

  const valid = draft.name.trim() && draft.prompt.trim();

  const handleSave = async () => {
    if (!valid) return;
    setSaving(true);
    setError(null);
    try {
      const role: BlacksmithRole = { ...draft, name: draft.name.trim(), guard: uiToGuard(gui.mode, gui.exts, gui.names) };
      const saved = await putBlacksmithRole(token, role.name, role);
      onSaved(saved);
    } catch (e: unknown) {
      setError((e as Error).message);
    } finally {
      setSaving(false);
    }
  };

  const handleDelete = async () => {
    if (!(await confirm({ message: `Delete role ${draft.name}? Any workflow that names it will fail until it is recreated or repointed.` }))) return;
    try {
      await deleteBlacksmithRole(token, draft.name);
      onDeleted(draft.name);
    } catch (e: unknown) {
      setError((e as Error).message);
    }
  };

  const showExts = gui.mode === 'only_ext' || gui.mode === 'no_tests_ext';
  const showNames = gui.mode === 'only_basenames';

  return (
    <div style={{ flex: 1, overflow: 'auto', padding: '16px 20px' }}>
      {confirmEl}
      {error && <div style={{ background: T.redSoft, border: `1px solid ${T.red}`, padding: '8px 12px', fontFamily: T.mono, fontSize: 11, color: T.red, marginBottom: 12 }}>{error}</div>}

      <div style={{ display: 'flex', gap: 8, alignItems: 'center', marginBottom: 6 }}>
        <input value={draft.name} onChange={e => set('name', e.target.value)} placeholder="role name (e.g. plan-dev)"
          disabled={!isNew} style={{ ...inputStyle, flex: 1, fontSize: 14, fontWeight: 600, opacity: isNew ? 1 : 0.8 }} />
        <input value={draft.class} onChange={e => set('class', e.target.value)} placeholder="class" style={{ ...inputStyle, width: 90 }} />
      </div>

      <div style={labelStyle}>PROMPT — what this stage IS</div>
      <textarea value={draft.prompt} onChange={e => set('prompt', e.target.value)} rows={12}
        style={{ ...inputStyle, fontSize: 11, resize: 'vertical', lineHeight: 1.5 }} />

      <div style={labelStyle}>TOOLS — what it is offered</div>
      <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6 }}>
        {TOOL_NAMES.map(t => {
          const on = draft.tools.includes(t);
          return (
            <button key={t} onClick={() => toggleTool(t)}
              style={{ background: on ? T.greenSoft : 'transparent', border: `1px solid ${on ? T.green : T.border}`, color: on ? T.green : T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 9px', cursor: 'pointer' }}>
              {t}
            </button>
          );
        })}
      </div>

      <div style={labelStyle}>GUARD — what it may write</div>
      <select value={gui.mode} onChange={e => setGui(g => ({ ...g, mode: e.target.value }))} style={{ ...inputStyle, cursor: 'pointer' }}>
        {GUARD_OPTIONS.map(o => <option key={o.id} value={o.id}>{o.label}</option>)}
      </select>
      {showExts && <input value={gui.exts} onChange={e => setGui(g => ({ ...g, exts: e.target.value }))} placeholder="extensions, comma-separated (e.g. .go)" style={{ ...inputStyle, marginTop: 6 }} />}
      {showNames && <input value={gui.names} onChange={e => setGui(g => ({ ...g, names: e.target.value }))} placeholder="filenames, comma-separated (e.g. Dockerfile, .dockerignore)" style={{ ...inputStyle, marginTop: 6 }} />}

      <div style={labelStyle}>CHECK — the command that decides success (empty = ends when the model stops)</div>
      <input value={draft.check} onChange={e => set('check', e.target.value)} placeholder="go build ./... && go test ./..." style={inputStyle} />

      <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr 1fr', gap: 10, marginTop: 12 }}>
        <div><div style={{ ...labelStyle, marginTop: 0 }}>MAX ITERATIONS</div>
          <input type="number" value={draft.max_iterations} onChange={e => set('max_iterations', parseInt(e.target.value, 10) || 0)} style={inputStyle} /></div>
        <div><div style={{ ...labelStyle, marginTop: 0 }}>TEMPERATURE</div>
          <input type="number" step="0.1" value={draft.temperature} onChange={e => set('temperature', parseFloat(e.target.value) || 0)} style={inputStyle} /></div>
        <div><div style={{ ...labelStyle, marginTop: 0 }}>MAX TOKENS</div>
          <input type="number" value={draft.max_tokens} onChange={e => set('max_tokens', parseInt(e.target.value, 10) || 0)} style={inputStyle} /></div>
        <div><div style={{ ...labelStyle, marginTop: 0 }}>ATTEMPT TIMEOUT (s)</div>
          <input type="number" value={draft.attempt_timeout_secs} onChange={e => set('attempt_timeout_secs', parseInt(e.target.value, 10) || 0)} style={inputStyle} /></div>
        <div><div style={{ ...labelStyle, marginTop: 0 }}>RESPINS</div>
          <input type="number" value={draft.respins} onChange={e => set('respins', parseInt(e.target.value, 10) || 0)} style={inputStyle} /></div>
        <div><div style={{ ...labelStyle, marginTop: 0 }}>TICKET KIND</div>
          <input value={draft.ticket_kind} onChange={e => set('ticket_kind', e.target.value)} placeholder="security / quality" style={inputStyle} /></div>
      </div>

      <div style={{ display: 'flex', flexWrap: 'wrap', gap: 16, marginTop: 14 }}>
        {([['rewrite_whole', 'rewrite whole files'], ['own_check', 'own check (skip operator override)'], ['seed_known', 'seed whole tree (reviewers)']] as const).map(([k, lbl]) => (
          <label key={k} style={{ display: 'flex', alignItems: 'center', gap: 6, fontFamily: T.mono, fontSize: 11, color: T.dim, cursor: 'pointer' }}>
            <input type="checkbox" checked={draft[k] as boolean} onChange={e => set(k, e.target.checked as never)} />
            {lbl}
          </label>
        ))}
      </div>

      <div style={{ display: 'flex', gap: 8, marginTop: 20 }}>
        <button onClick={handleSave} disabled={!valid || saving}
          style={{ background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 12, fontWeight: 600, padding: '8px 20px', cursor: 'pointer', opacity: (!valid || saving) ? 0.6 : 1 }}>
          {saving ? '[ · · · ]' : isNew ? '[ create ]' : '[ save ]'}
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
    </div>
  );
}

/** Blacksmith Roles route: role list on the left, the selected role's editor on the right. */
export function BlacksmithRoles() {
  const token = useAppSelector(s => s.auth.token)!;
  const [roles, setRoles] = useState<BlacksmithRole[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selName, setSelName] = useUrlParam('role');
  const [creating, setCreating] = useState(false);
  const [railW, railHandle] = useResizableWidth('rail.blacksmith.main', 240, { min: 180, max: 420 });

  const selected = roles.find(r => r.name === selName) ?? null;

  const fetchRoles = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      setRoles(await listBlacksmithRoles(token));
    } catch (e: unknown) {
      setError((e as Error).message);
    } finally {
      setLoading(false);
    }
  }, [token]);

  useEffect(() => { fetchRoles(); }, [fetchRoles]);

  const handleSaved = (r: BlacksmithRole) => {
    setRoles(prev => {
      const rest = prev.filter(x => x.name !== r.name);
      return [...rest, r].sort((a, b) => a.name.localeCompare(b.name));
    });
    setCreating(false);
    setSelName(r.name);
  };

  const handleDeleted = (name: string) => {
    setRoles(prev => prev.filter(x => x.name !== name));
    setSelName(null);
  };

  const startNew = () => { setCreating(true); setSelName(null); };
  const selectRole = (name: string) => { setCreating(false); setSelName(name); };

  return (
    <div style={{ display: 'flex', height: '100%', overflow: 'hidden' }}>
      {/* Role list */}
      <div style={{ width: railW, flexShrink: 0, borderRight: `1px solid ${T.border}`, display: 'flex', flexDirection: 'column', background: T.bgAlt }}>
        <div style={{ padding: '14px 14px 10px', borderBottom: `1px solid ${T.border}` }}>
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 4 }}>
            <span style={{ fontFamily: T.mono, fontSize: 13, fontWeight: 700, color: T.textHi }}>roles/</span>
            <div style={{ display: 'flex', gap: 6 }}>
              <button onClick={startNew}
                style={{ background: creating ? T.greenSoft : 'transparent', border: `1px solid ${creating ? T.green : T.border}`, color: creating ? T.green : T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 7px', cursor: 'pointer' }}>+</button>
              <button onClick={fetchRoles} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 7px', cursor: 'pointer' }}>↻</button>
            </div>
          </div>
          <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint }}>
            {roles.length > 0 && `${roles.length} role${roles.length !== 1 ? 's' : ''}`}
          </div>
        </div>

        <div style={{ flex: 1, overflow: 'auto' }}>
          {loading ? (
            <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint, animation: 'pulse 1s ease-in-out infinite' }}>→ loading · · ·</div>
          ) : error ? (
            <div style={{ padding: '14px', fontFamily: T.mono, fontSize: 11, color: T.red }}>{error}</div>
          ) : roles.length === 0 ? (
            <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint }}>→ no roles</div>
          ) : roles.map(r => {
            const isActive = !creating && selected?.name === r.name;
            return (
              <button key={r.name} onClick={() => selectRole(r.name)}
                style={{ width: '100%', textAlign: 'left', padding: '10px 14px', background: isActive ? T.greenSoft : 'transparent', border: 0, borderLeft: `2px solid ${isActive ? T.green : 'transparent'}`, fontFamily: T.mono, cursor: 'pointer', color: T.text, display: 'block' }}>
                <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
                  <span style={{ fontSize: 12, fontWeight: 600, color: isActive ? T.textHi : T.text }}>{r.name}</span>
                  {r.ticket_kind && <span style={{ fontSize: 9, color: T.blue, border: `1px solid ${T.blue}`, padding: '0 4px' }}>{r.ticket_kind}</span>}
                </div>
                <div style={{ fontSize: 10, color: T.faint, marginTop: 2 }}>{r.class} · {r.tools.length} tools · {r.max_iterations} turns</div>
              </button>
            );
          })}
        </div>
      </div>
      {railHandle}

      {/* Editor panel */}
      <div style={{ flex: 1, display: 'flex', flexDirection: 'column', overflow: 'hidden' }}>
        {creating ? (
          <RoleEditor initial={blankRole()} isNew onSaved={handleSaved} onDeleted={handleDeleted} />
        ) : selected ? (
          <RoleEditor initial={selected} isNew={false} onSaved={handleSaved} onDeleted={handleDeleted} />
        ) : (
          <div style={{ flex: 1, display: 'flex', alignItems: 'center', justifyContent: 'center' }}>
            <div style={{ fontFamily: T.mono, fontSize: 12, color: T.faint }}>→ select a role, or + to create one</div>
          </div>
        )}
      </div>
    </div>
  );
}

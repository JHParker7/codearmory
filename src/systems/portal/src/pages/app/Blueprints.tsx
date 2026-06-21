import { useState, useEffect } from 'react';
import { T } from '../../theme';
import { Pill } from '../../components/Pill';
import { useAppDispatch, useAppSelector } from '../../store/hooks';
import { fetchWorkspace, addWorkspace, removeWorkspace, setSelected } from '../../store/workspacesSlice';
import type { WorkspaceDetail } from '../../store/workspacesSlice';
import type { WorkspaceView } from '../../api/bff';
import { timeAgo } from '../../utils';
import type { WorkspaceEntry } from '../../utils';

// ── Add workspace modal ───────────────────────────────────────────────────────

function AddWorkspaceModal({ onClose }: { onClose: () => void }) {
  const dispatch = useAppDispatch();
  const username = useAppSelector(s => s.auth.user?.username);
  const [path, setPath] = useState(username ? `${username}/` : '');
  const [focused, setFocused] = useState(false);

  const handleAdd = () => {
    const trimmed = path.trim().replace(/^\/state\//, '');
    if (!trimmed) return;
    const parts = trimmed.split('/').filter(Boolean);
    if (parts.length < 2) return;
    dispatch(addWorkspace({ path: trimmed, label: trimmed }));
    onClose();
  };

  return (
    <div onClick={onClose} style={{ position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.7)', zIndex: 50, display: 'flex', alignItems: 'center', justifyContent: 'center' }}>
      <div onClick={(e) => e.stopPropagation()} style={{ width: 480, background: T.card, border: `1px solid ${T.borderHi}` }}>
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '12px 16px', borderBottom: `1px solid ${T.border}`, background: T.cardHi }}>
          <span style={{ fontFamily: T.mono, fontSize: 12, color: T.dim }}>add workspace</span>
          <button onClick={onClose} style={{ background: 'transparent', border: 0, color: T.faint, cursor: 'pointer', fontSize: 16 }}>×</button>
        </div>
        <div style={{ padding: '20px 20px 16px' }}>
          <div style={{ fontFamily: T.mono, fontSize: 12, color: T.faint, marginBottom: 12, lineHeight: 1.6 }}>
            <span style={{ color: T.green }}>$</span> enter workspace path:
            <br />
            <span style={{ color: T.dim }}>· user-scoped: </span><span style={{ color: T.text }}>username/workspace</span>
            <br />
            <span style={{ color: T.dim }}>· org-scoped:  </span><span style={{ color: T.text }}>org/team/workspace</span>
          </div>
          <div style={{ display: 'flex', alignItems: 'center', background: T.cardHi, border: `1px solid ${focused ? T.green : T.border}`, boxShadow: focused ? `0 0 0 3px ${T.greenSoft}` : 'none', transition: 'all .15s', padding: '8px 12px', marginBottom: 16 }}>
            <span style={{ color: T.green, fontFamily: T.mono, fontSize: 13, marginRight: 8, userSelect: 'none' }}>›</span>
            <input value={path} onChange={(e) => setPath(e.target.value)}
              onFocus={() => setFocused(true)} onBlur={() => setFocused(false)}
              onKeyDown={(e) => { if (e.key === 'Enter') handleAdd(); if (e.key === 'Escape') onClose(); }}
              placeholder={`${username || 'alice'}/prod`} autoFocus
              style={{ flex: 1, background: 'transparent', border: 0, outline: 'none', color: T.text, fontFamily: T.mono, fontSize: 13, padding: 0 }} />
          </div>
          <div style={{ display: 'flex', gap: 10 }}>
            <button onClick={handleAdd} disabled={path.split('/').filter(Boolean).length < 2}
              style={{ flex: 1, background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 13, fontWeight: 600, padding: '9px 14px', cursor: 'pointer', letterSpacing: 0.4 }}>
              [ ↵ add workspace ]
            </button>
            <button onClick={onClose}
              style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 12, padding: '9px 14px', cursor: 'pointer' }}>
              cancel
            </button>
          </div>
        </div>
      </div>
    </div>
  );
}

// ── Workspace detail panel ────────────────────────────────────────────────────

function WorkspaceDetailPanel({ entry, detail, onRefresh, onDelete }: {
  entry: WorkspaceEntry;
  detail: WorkspaceDetail;
  onRefresh: () => void;
  onDelete: () => void;
}) {
  const [confirming, setConfirming] = useState(false);
  const { data, status, error, lastFetched } = detail;

  if (status === 'loading') {
    return (
      <div style={{ flex: 1, display: 'flex', alignItems: 'center', justifyContent: 'center', padding: 40 }}>
        <div style={{ fontFamily: T.mono, fontSize: 12, color: T.dim, textAlign: 'center' }}>
          <div style={{ color: T.green, marginBottom: 8 }}>$ GET /state/{entry.path}</div>
          <div style={{ animation: 'pulse 1s ease-in-out infinite' }}>→ fetching state · · ·</div>
        </div>
      </div>
    );
  }

  if (status === 'failed') {
    return (
      <div style={{ flex: 1, padding: 24 }}>
        <div style={{ background: T.redSoft, border: `1px solid ${T.red}`, padding: '12px 16px', fontFamily: T.mono, fontSize: 12, color: T.red, marginBottom: 16 }}>
          ERR · {error}
        </div>
        <button onClick={onRefresh} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 12, padding: '7px 12px', cursor: 'pointer' }}>
          [ retry ]
        </button>
      </div>
    );
  }

  if (!data) return null;

  return (
    <div style={{ flex: 1, padding: '20px 24px', overflow: 'auto' }}>
      {/* Header */}
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start', marginBottom: 20 }}>
        <div>
          <div style={{ fontFamily: T.mono, fontSize: 16, color: T.textHi, fontWeight: 700 }}>
            /state/{entry.path}
          </div>
          <div style={{ fontFamily: T.mono, fontSize: 11, color: T.faint, marginTop: 4 }}>
            {lastFetched ? `fetched ${timeAgo(new Date(lastFetched).toISOString())} ago` : 'not fetched'}
            {data.state && ` · serial ${data.state.serial} · v${data.state.terraform_version}`}
          </div>
        </div>
        <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
          {data.locked ? <Pill tone="amber">locked · {data.lock!.who.split('@')[0]}</Pill> : !data.isEmpty ? <Pill tone="green">unlocked</Pill> : null}
          <button onClick={onRefresh} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 10px', cursor: 'pointer', letterSpacing: 0.3 }}>[ refresh ]</button>
          <button onClick={() => setConfirming(true)} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 10px', cursor: 'pointer', letterSpacing: 0.3 }}>[ remove ]</button>
        </div>
      </div>

      {confirming && (
        <div style={{ background: T.redSoft, border: `1px solid ${T.red}`, padding: '12px 16px', marginBottom: 16, fontFamily: T.mono, fontSize: 12, color: T.red, display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
          <span>remove this workspace from your dashboard?</span>
          <div style={{ display: 'flex', gap: 8 }}>
            <button onClick={onDelete} style={{ background: T.red, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 11, padding: '4px 10px', cursor: 'pointer' }}>[ confirm ]</button>
            <button onClick={() => setConfirming(false)} style={{ background: 'transparent', border: `1px solid ${T.red}`, color: T.red, fontFamily: T.mono, fontSize: 11, padding: '4px 10px', cursor: 'pointer' }}>[ cancel ]</button>
          </div>
        </div>
      )}

      {/* Lock info */}
      {data.locked && data.lock && (
        <div style={{ background: T.amberSoft, border: `1px solid ${T.amber}`, padding: '12px 16px', marginBottom: 16, fontFamily: T.mono, fontSize: 12 }}>
          <div style={{ color: T.amber, fontWeight: 700, marginBottom: 6 }}>⚠ workspace locked</div>
          <div style={{ color: T.text, lineHeight: 1.7 }}>
            <div><span style={{ color: T.faint }}>lock id   </span>{data.lock.id}</div>
            <div><span style={{ color: T.faint }}>operation </span>{data.lock.operation}</div>
            <div><span style={{ color: T.faint }}>holder    </span>{data.lock.who}</div>
            <div><span style={{ color: T.faint }}>acquired  </span>{timeAgo(data.lock.created)} ago</div>
            {data.lock.info && <div><span style={{ color: T.faint }}>info      </span>{data.lock.info}</div>}
          </div>
        </div>
      )}

      {/* Empty state */}
      {data.isEmpty && (
        <div style={{ background: T.card, border: `1px solid ${T.border}`, padding: '16px 20px', fontFamily: T.mono, fontSize: 12, color: T.dim, textAlign: 'center' }}>
          <div style={{ color: T.faint, marginBottom: 4 }}>→ 204 No Content</div>
          <div>no state stored yet — run <span style={{ color: T.text }}>tofu apply</span> to initialize</div>
        </div>
      )}

      {/* State details */}
      {data.state && (
        <>
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(4, 1fr)', gap: 12, marginBottom: 20 }}>
            {([
              ['resources', data.state.resource_count],
              ['serial', data.state.serial],
              ['tf version', data.state.terraform_version],
              ['lineage', data.state.lineage.slice(0, 8) + '…'],
            ] as [string, string | number][]).map(([k, v]) => (
              <div key={k} style={{ background: T.card, border: `1px solid ${T.border}`, padding: '12px 14px' }}>
                <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 6, textTransform: 'uppercase' }}>{k}</div>
                <div style={{ fontFamily: T.mono, fontSize: 18, color: T.textHi, fontWeight: 700 }}>{v}</div>
              </div>
            ))}
          </div>

          {data.state.resource_types.length > 0 && (
            <>
              <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 8 }}>RESOURCE TYPES</div>
              <div style={{ background: T.card, border: `1px solid ${T.border}`, marginBottom: 16, overflow: 'hidden' }}>
                {data.state.resource_types.map(({ type, count }, i) => (
                  <div key={type} style={{ display: 'flex', alignItems: 'center', padding: '8px 12px', borderBottom: i < data.state!.resource_types.length - 1 ? `1px solid ${T.border}` : 'none', gap: 12 }}>
                    <span style={{ fontFamily: T.mono, fontSize: 12, color: T.green }}>→</span>
                    <span style={{ fontFamily: T.mono, fontSize: 12, color: T.text, flex: 1 }}>{type}</span>
                    <span style={{ fontFamily: T.mono, fontSize: 12, color: T.dim }}>{count}</span>
                  </div>
                ))}
              </div>
            </>
          )}

          {data.state.outputs && Object.keys(data.state.outputs).length > 0 && (
            <>
              <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, marginBottom: 8 }}>OUTPUTS</div>
              <div style={{ background: T.bg, border: `1px solid ${T.border}`, padding: '10px 12px', fontFamily: T.mono, fontSize: 12, lineHeight: 1.6, overflow: 'auto', maxHeight: 200 }}>
                <pre style={{ margin: 0, color: T.text, fontSize: 12 }}>
                  {(() => { try { return JSON.stringify(data.state.outputs, null, 2); } catch { return '[unprintable]'; } })()}
                </pre>
              </div>
            </>
          )}
        </>
      )}
    </div>
  );
}

// ── Blueprints page ───────────────────────────────────────────────────────────

export function Blueprints() {
  const dispatch = useAppDispatch();
  const token = useAppSelector(s => s.auth.token);
  const username = useAppSelector(s => s.auth.user?.username);
  const entries = useAppSelector(s => s.workspaces.entries);
  const details = useAppSelector(s => s.workspaces.details);
  const selected = useAppSelector(s => s.workspaces.selected);
  const [showAdd, setShowAdd] = useState(false);

  const selectedEntry = entries.find(w => w.path === selected);
  const selectedDetail = selected ? details[selected] : undefined;

  // Auto-fetch when a workspace is selected for the first time.
  // `details` intentionally excluded from deps to avoid retry loops on failure.
  useEffect(() => {
    if (!selected || !token) return;
    const det = details[selected];
    if (!det || det.status === 'idle') {
      dispatch(fetchWorkspace({ token, path: selected }));
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [selected, token, dispatch]);

  const handleRefresh = () => {
    if (!selected || !token) return;
    dispatch(fetchWorkspace({ token, path: selected }));
  };

  const handleDelete = () => {
    if (!selected) return;
    dispatch(removeWorkspace(selected));
  };

  return (
    <div style={{ display: 'flex', height: '100%', overflow: 'hidden' }}>
      {/* Left panel */}
      <div style={{ width: 260, flexShrink: 0, borderRight: `1px solid ${T.border}`, display: 'flex', flexDirection: 'column', background: T.bgAlt }}>
        <div style={{ padding: '14px 14px 10px', borderBottom: `1px solid ${T.border}` }}>
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 4 }}>
            <span style={{ fontFamily: T.mono, fontSize: 13, fontWeight: 700, color: T.textHi }}>blueprints/</span>
            <button onClick={() => setShowAdd(true)} title="Add workspace"
              style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 7px', cursor: 'pointer', letterSpacing: 0.3, transition: 'all .12s' }}
              onMouseEnter={(e) => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.green; (e.currentTarget as HTMLButtonElement).style.color = T.green; }}
              onMouseLeave={(e) => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.border; (e.currentTarget as HTMLButtonElement).style.color = T.dim; }}>
              + add
            </button>
          </div>
          <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 0.5 }}>
            {username && <span style={{ color: T.dim }}>@{username}</span>}
            {entries.length > 0 && <span> · {entries.length} workspace{entries.length !== 1 ? 's' : ''}</span>}
          </div>
        </div>

        <div style={{ flex: 1, overflow: 'auto' }}>
          {entries.length === 0 ? (
            <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11.5, color: T.faint, lineHeight: 1.6 }}>
              <div>→ no workspaces yet</div>
              <div style={{ marginTop: 8 }}>
                <button onClick={() => setShowAdd(true)} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 11, padding: '5px 10px', cursor: 'pointer' }}>
                  [ + add workspace ]
                </button>
              </div>
            </div>
          ) : (
            entries.map((ws) => {
              const det = details[ws.path];
              const isActive = selected === ws.path;
              const hasLock = det?.data?.locked ?? false;
              const tone = hasLock ? T.amber : T.green;

              const subtitle = det?.status === 'loading' ? null
                : det?.data?.isEmpty ? '→ empty workspace'
                : det?.data?.locked ? 'locked'
                : det?.data?.state ? `${det.data.state.resource_count} resources · serial ${det.data.state.serial}`
                : det?.status === 'failed' ? 'error'
                : 'click to load';

              return (
                <button key={ws.path} onClick={() => dispatch(setSelected(ws.path))}
                  style={{ width: '100%', textAlign: 'left', padding: '10px 14px', background: isActive ? T.greenSoft : 'transparent', border: 0, borderLeft: `2px solid ${isActive ? T.green : 'transparent'}`, fontFamily: T.mono, cursor: 'pointer', color: T.text, display: 'block', transition: 'background .12s' }}>
                  <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
                    <span style={{ fontSize: 13, fontWeight: 600, color: isActive ? T.textHi : T.text }}>{ws.path}</span>
                    {det && det.status !== 'loading' && (
                      <span style={{ width: 6, height: 6, borderRadius: 3, background: tone, animation: hasLock ? 'pulse 1.5s ease-in-out infinite' : 'none', flexShrink: 0 }} />
                    )}
                    {det?.status === 'loading' && <span style={{ fontSize: 10, color: T.faint, animation: 'pulse 1s ease-in-out infinite' }}>··</span>}
                  </div>
                  <div style={{ fontSize: 11, color: T.faint, marginTop: 3 }}>{subtitle}</div>
                </button>
              );
            })
          )}
        </div>

        <div style={{ padding: '10px 14px', borderTop: `1px solid ${T.border}` }}>
          <div style={{ fontFamily: T.mono, fontSize: 9.5, color: T.faint, lineHeight: 1.6, letterSpacing: 0.2 }}>
            <div>tofu backend config:</div>
            <div style={{ color: T.dim, marginTop: 2 }}>
              address = "http://localhost:8082<br />
              &nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;/state/{username || 'user'}/{'<ws>'}"
            </div>
          </div>
        </div>
      </div>

      {/* Right panel */}
      <div style={{ flex: 1, display: 'flex', flexDirection: 'column', overflow: 'hidden' }}>
        <div style={{ padding: '12px 20px', borderBottom: `1px solid ${T.border}`, background: T.bgAlt, display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
          <div style={{ fontFamily: T.mono, fontSize: 11, color: T.faint, letterSpacing: 0.3 }}>
            <span style={{ color: T.green }}>$</span> armory state
            {selected && <span style={{ color: T.dim }}> · /state/{selected}</span>}
          </div>
          <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint }}>
            <span style={{ color: T.green }}>●</span> conductor :8082
          </div>
        </div>

        <div style={{ flex: 1, overflow: 'auto', display: 'flex', flexDirection: 'column' }}>
          {!selected ? (
            <EmptyState onAdd={() => setShowAdd(true)} />
          ) : selectedEntry && selectedDetail ? (
            <WorkspaceDetailPanel
              entry={selectedEntry}
              detail={selectedDetail}
              onRefresh={handleRefresh}
              onDelete={handleDelete}
            />
          ) : selectedEntry ? (
            <div style={{ flex: 1, display: 'flex', alignItems: 'center', justifyContent: 'center', padding: 40 }}>
              <div style={{ fontFamily: T.mono, fontSize: 12, color: T.dim, animation: 'pulse 1s ease-in-out infinite' }}>→ loading · · ·</div>
            </div>
          ) : null}
        </div>
      </div>

      {showAdd && <AddWorkspaceModal onClose={() => setShowAdd(false)} />}
    </div>
  );
}

function EmptyState({ onAdd }: { onAdd: () => void }) {
  return (
    <div style={{ flex: 1, display: 'flex', alignItems: 'center', justifyContent: 'center', padding: 40 }}>
      <div style={{ textAlign: 'center', maxWidth: 420 }}>
        <div style={{ fontFamily: T.mono, fontSize: 13, color: T.dim, lineHeight: 1.7, marginBottom: 24 }}>
          <div style={{ color: T.green, marginBottom: 8 }}>$ armory setup</div>
          <div style={{ color: T.faint }}>→ no workspaces tracked yet</div>
          <div style={{ color: T.faint }}>→ add a workspace path to start monitoring state</div>
        </div>
        <div style={{ fontFamily: T.mono, fontSize: 12, color: T.dim, marginBottom: 16 }}>
          Workspace paths: <span style={{ color: T.text }}>username/workspace</span> or <span style={{ color: T.text }}>org/team/workspace</span>
        </div>
        <button onClick={onAdd} style={{ background: T.green, color: T.bg, border: 'none', fontFamily: T.mono, fontSize: 13, fontWeight: 600, padding: '9px 18px', cursor: 'pointer', letterSpacing: 0.4 }}>
          [ + add workspace ]
        </button>
      </div>
    </div>
  );
}

// ── Placeholder pages ─────────────────────────────────────────────────────────

export function ForgePlaceholder() {
  return (
    <div style={{ flex: 1, display: 'flex', alignItems: 'center', justifyContent: 'center', padding: 40 }}>
      <div style={{ textAlign: 'center', maxWidth: 400 }}>
        <div style={{ fontFamily: 'inherit', fontSize: 28, color: T.amber, marginBottom: 16 }}>forge/</div>
        <div style={{ fontFamily: 'inherit', fontSize: 13, color: T.dim, lineHeight: 1.7 }}>
          <span style={{ color: T.green }}>#</span> forge is in development.<br />
          CI/CD pipelines, hosted runners, and state-aware deploys are coming.
        </div>
        <div style={{ marginTop: 20, fontFamily: 'inherit', fontSize: 11, color: T.faint }}>planned · see roadmap →</div>
      </div>
    </div>
  );
}

export function WorkflowsPlaceholder() {
  return (
    <div style={{ flex: 1, display: 'flex', alignItems: 'center', justifyContent: 'center', padding: 40 }}>
      <div style={{ textAlign: 'center', maxWidth: 400 }}>
        <div style={{ fontFamily: 'inherit', fontSize: 28, color: T.blue, marginBottom: 16 }}>workflows/</div>
        <div style={{ fontFamily: 'inherit', fontSize: 13, color: T.dim, lineHeight: 1.7 }}>
          <span style={{ color: T.green }}>#</span> workflow automation is in development.<br />
          Trigger, chain, and monitor automated workflows across services.
        </div>
        <div style={{ marginTop: 20, fontFamily: 'inherit', fontSize: 11, color: T.faint }}>planned · see roadmap →</div>
      </div>
    </div>
  );
}

export function TicketsPlaceholder() {
  return (
    <div style={{ flex: 1, display: 'flex', alignItems: 'center', justifyContent: 'center', padding: 40 }}>
      <div style={{ textAlign: 'center', maxWidth: 400 }}>
        <div style={{ fontFamily: 'inherit', fontSize: 28, color: T.amber, marginBottom: 16 }}>tickets/</div>
        <div style={{ fontFamily: 'inherit', fontSize: 13, color: T.dim, lineHeight: 1.7 }}>
          <span style={{ color: T.green }}>#</span> issue tracking is in development.<br />
          Create, assign, and track issues linked to your repos and deployments.
        </div>
        <div style={{ marginTop: 20, fontFamily: 'inherit', fontSize: 11, color: T.faint }}>planned · see roadmap →</div>
      </div>
    </div>
  );
}

export function HooksPlaceholder() {
  return (
    <div style={{ flex: 1, display: 'flex', alignItems: 'center', justifyContent: 'center', padding: 40 }}>
      <div style={{ textAlign: 'center', maxWidth: 400 }}>
        <div style={{ fontFamily: 'inherit', fontSize: 28, color: T.green, marginBottom: 16 }}>hooks/</div>
        <div style={{ fontFamily: 'inherit', fontSize: 13, color: T.dim, lineHeight: 1.7 }}>
          <span style={{ color: T.green }}>#</span> webhook management is in development.<br />
          Register, inspect, and replay webhooks from Forgejo and external sources.
        </div>
        <div style={{ marginTop: 20, fontFamily: 'inherit', fontSize: 11, color: T.faint }}>planned · see roadmap →</div>
      </div>
    </div>
  );
}

export function ContainersPlaceholder() {
  return (
    <div style={{ flex: 1, display: 'flex', alignItems: 'center', justifyContent: 'center', padding: 40 }}>
      <div style={{ textAlign: 'center', maxWidth: 400 }}>
        <div style={{ fontFamily: 'inherit', fontSize: 28, color: T.blue, marginBottom: 16 }}>containers/</div>
        <div style={{ fontFamily: 'inherit', fontSize: 13, color: T.dim, lineHeight: 1.7 }}>
          <span style={{ color: T.green }}>#</span> container registry is in development.<br />
          Browse images, tags, and layer metadata from your private registry.
        </div>
        <div style={{ marginTop: 20, fontFamily: 'inherit', fontSize: 11, color: T.faint }}>planned · see roadmap →</div>
      </div>
    </div>
  );
}

export function GiteaIntegrationPlaceholder() {
  return (
    <div style={{ flex: 1, display: 'flex', alignItems: 'center', justifyContent: 'center', padding: 40 }}>
      <div style={{ textAlign: 'center', maxWidth: 400 }}>
        <div style={{ fontFamily: 'inherit', fontSize: 28, color: T.green, marginBottom: 16 }}>git/</div>
        <div style={{ fontFamily: 'inherit', fontSize: 13, color: T.dim, lineHeight: 1.7 }}>
          <span style={{ color: T.green }}>#</span> Forgejo integration is in development.<br />
          Manage repos, branches, and PRs without leaving the platform.
        </div>
        <div style={{ marginTop: 20, fontFamily: 'inherit', fontSize: 11, color: T.faint }}>manage via Forgejo · 192.168.53.100</div>
      </div>
    </div>
  );
}

export function GatekeeperPlaceholder() {
  return (
    <div style={{ flex: 1, display: 'flex', alignItems: 'center', justifyContent: 'center', padding: 40 }}>
      <div style={{ textAlign: 'center', maxWidth: 400 }}>
        <div style={{ fontFamily: 'inherit', fontSize: 28, color: T.green, marginBottom: 16 }}>gatekeeper/</div>
        <div style={{ fontFamily: 'inherit', fontSize: 13, color: T.dim, lineHeight: 1.7 }}>
          <span style={{ color: T.green }}>#</span> RBAC management UI coming soon.<br />
          Manage orgs, teams, roles, and permissions via the conductor API.
        </div>
        <div style={{ marginTop: 20, fontFamily: 'inherit', fontSize: 11, color: T.faint }}>manage via API · gatekeeper :8080</div>
      </div>
    </div>
  );
}

// Re-export for backwards compat with old import in tests
export type { WorkspaceView };

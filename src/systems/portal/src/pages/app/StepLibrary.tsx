/**
 * Steps tab — the top-level manager for pre-configured, reusable steps. A step is
 * a named action + its `with` config that can be dropped into any pipeline (or
 * created inline from the pipeline builder). This is the "steps section" of the
 * Workflows page: a left rail listing the org's steps, and the shared StepDefForm
 * on the right to create a new one or edit the selected one.
 *
 * Wiring between steps (connecting one step's inputs to another's output) is NOT
 * done here — it lives per-occurrence on the block in the pipeline builder. This
 * page only defines the reusable steps themselves.
 */
import { useState, useEffect, useCallback } from 'react';
import { T } from '../../theme';
import { useConfirm } from '../../components/ConfirmDialog';
import { useAppSelector } from '../../store/hooks';
import { useResizableWidth } from '../../components/ResizeHandle';
import { listSteps, deleteStep } from '../../api/bff';
import type { Step } from '../../api/bff';
import { StepDefForm } from './StepDefForm';
import { timeAgo } from '../../utils';

export function StepsTab() {
  const token = useAppSelector(s => s.auth.token)!;
  const [steps, setSteps] = useState<Step[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  // null = nothing selected; 'new' = the create form; a step_id = editing that step.
  const [selected, setSelected] = useState<string | 'new' | null>(null);
  const [railW, railHandle] = useResizableWidth('rail.workflows.steps', 280, { min: 220, max: 480 });

  const reload = useCallback(async () => {
    setLoading(true); setError(null);
    try { setSteps(await listSteps(token)); }
    catch (e: unknown) { setError((e as Error).message); }
    finally { setLoading(false); }
  }, [token]);
  useEffect(() => { reload(); }, [reload]);

  const [confirm, confirmEl] = useConfirm();
  const handleDelete = async (s: Step) => {
    if (!(await confirm({ message: `Delete step ${s.name}? Pipelines referencing it may break.` }))) return;
    try {
      await deleteStep(token, s.step_id);
      if (selected === s.step_id) setSelected(null);
      reload();
    } catch (e: unknown) { setError((e as Error).message); }
  };

  const selectedStep = selected && selected !== 'new' ? steps.find(s => s.step_id === selected) ?? null : null;

  return (
    <div style={{ display: 'flex', flex: 1, overflow: 'hidden' }}>
      {confirmEl}
      {/* Left rail: step list */}
      <div style={{ width: railW, flexShrink: 0, borderRight: `1px solid ${T.border}`, background: T.bgAlt, display: 'flex', flexDirection: 'column' }}>
        <div style={{ padding: '14px 14px 10px', borderBottom: `1px solid ${T.border}`, display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
          <span style={{ fontFamily: T.mono, fontSize: 12, fontWeight: 700, color: T.textHi }}>steps{steps.length > 0 ? ` · ${steps.length}` : ''}</span>
          <div style={{ display: 'flex', gap: 6 }}>
            <button onClick={() => setSelected('new')} title="new step"
              style={{ background: selected === 'new' ? T.greenSoft : 'transparent', border: `1px solid ${selected === 'new' ? T.green : T.border}`, color: selected === 'new' ? T.green : T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 7px', cursor: 'pointer' }}>+</button>
            <button onClick={reload} style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 7px', cursor: 'pointer' }}>↻</button>
          </div>
        </div>
        <div style={{ flex: 1, overflow: 'auto' }}>
          {loading ? (
            <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint, animation: 'pulse 1s ease-in-out infinite' }}>→ loading · · ·</div>
          ) : error ? (
            <div style={{ padding: '14px', fontFamily: T.mono, fontSize: 11, color: T.red }}>{error}</div>
          ) : steps.length === 0 ? (
            <div style={{ padding: '20px 14px', fontFamily: T.mono, fontSize: 11, color: T.faint }}>→ no steps yet — click <span style={{ color: T.green }}>+</span> to define one</div>
          ) : steps.map(s => {
            const isActive = selected === s.step_id;
            return (
              <div key={s.step_id} onClick={() => setSelected(s.step_id)}
                style={{ padding: '10px 14px', background: isActive ? T.greenSoft : 'transparent', borderLeft: `2px solid ${isActive ? T.green : 'transparent'}`, cursor: 'pointer', display: 'flex', alignItems: 'center', gap: 8, transition: 'background .12s' }}>
                <div style={{ flex: 1, minWidth: 0 }}>
                  <div style={{ fontFamily: T.mono, fontSize: 13, fontWeight: 600, color: isActive ? T.textHi : T.text, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{s.name}</div>
                  <div style={{ fontFamily: T.mono, fontSize: 10.5, color: T.faint, marginTop: 2 }}>
                    {s.action}{s.timeout != null ? ` · ${s.timeout}s` : ''} · {timeAgo(s.updated_at)} ago
                  </div>
                </div>
                <button onClick={e => { e.stopPropagation(); handleDelete(s); }} title="delete step"
                  style={{ background: 'transparent', border: `1px solid ${T.border}`, color: T.dim, fontFamily: T.mono, fontSize: 10, padding: '3px 7px', cursor: 'pointer', flexShrink: 0 }}
                  onMouseEnter={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.red; (e.currentTarget as HTMLButtonElement).style.color = T.red; }}
                  onMouseLeave={e => { (e.currentTarget as HTMLButtonElement).style.borderColor = T.border; (e.currentTarget as HTMLButtonElement).style.color = T.dim; }}>✕</button>
              </div>
            );
          })}
        </div>
      </div>

      {railHandle}

      {/* Right panel: create/edit form */}
      <div style={{ flex: 1, overflow: 'auto' }}>
        {selected === null ? (
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', height: '100%' }}>
            <div style={{ fontFamily: T.mono, fontSize: 12, color: T.faint }}>→ select a step to edit, or click <span style={{ color: T.green }}>+</span> to create one</div>
          </div>
        ) : (
          <div style={{ padding: '18px 22px', maxWidth: 560 }}>
            <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint, letterSpacing: 1, textTransform: 'uppercase', marginBottom: 12 }}>
              {selected === 'new' ? 'new step' : 'edit step'}
            </div>
            <StepDefForm
              key={selected}
              token={token}
              initial={selectedStep}
              autoFocus
              onSaved={(saved, wasCreate) => { reload(); if (wasCreate) setSelected(saved.step_id); }}
              onCancel={() => setSelected(null)}
            />
          </div>
        )}
      </div>
    </div>
  );
}

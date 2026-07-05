import { useState, useRef } from 'react';
import { T } from '../theme';
import type { Workflow } from '../api/bff';

/**
 * Pipeline picker: a text field that filters the pipeline (workflow) list by name
 * or id and shows the top matches in a dropdown. The field's value is the pipeline
 * NAME — what a workflows/trigger step's `with.pipeline` consumes (the backend
 * resolves a name or id) — so it stays editable for a name typed by hand even when
 * the list is empty or hasn't loaded. Used by the Workflows create-step form for
 * the workflows/trigger target field.
 */
export function PipelineSelect({ value, onChange, workflows, autoFocus, placeholder = 'select or type a pipeline name', fontSize = 12 }: {
  value: string;
  onChange: (v: string) => void;
  workflows: Workflow[];
  autoFocus?: boolean;
  placeholder?: string;
  fontSize?: number;
}) {
  const [open, setOpen] = useState(false);
  const [hover, setHover] = useState(-1);
  const blurTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);

  const q = value.trim().toLowerCase();
  const matches = workflows
    .filter(w => !q || w.name.toLowerCase().includes(q) || w.workflow_id.toLowerCase().includes(q))
    .slice(0, 10);

  return (
    <div style={{ position: 'relative' }}
      onFocus={() => { if (blurTimer.current) clearTimeout(blurTimer.current); setOpen(true); }}
      onBlur={() => { blurTimer.current = setTimeout(() => setOpen(false), 120); }}>
      <input value={value} onChange={e => { onChange(e.target.value); setOpen(true); }} autoFocus={autoFocus}
        placeholder={placeholder}
        style={{ width: '100%', background: T.cardHi, border: `1px solid ${open ? T.green : T.border}`, color: T.text, fontFamily: T.mono, fontSize, padding: '8px 28px 8px 10px', outline: 'none', boxSizing: 'border-box' }} />
      <span style={{ position: 'absolute', right: 10, top: 11, color: T.faint, fontSize: 10, pointerEvents: 'none' }}>▾</span>
      {open && matches.length > 0 && (
        <div style={{ position: 'absolute', top: '100%', left: 0, right: 0, zIndex: 10, marginTop: 2, maxHeight: 280, overflow: 'auto', background: T.cardHi, border: `1px solid ${T.border}`, boxShadow: '0 6px 16px rgba(0,0,0,0.4)' }}>
          {matches.map((w, i) => {
            const active = value === w.name;
            return (
              <button key={w.workflow_id} type="button"
                onMouseDown={e => { e.preventDefault(); onChange(w.name); setOpen(false); }}
                onMouseEnter={() => setHover(i)} onMouseLeave={() => setHover(-1)}
                style={{ display: 'block', width: '100%', textAlign: 'left', background: hover === i || active ? T.greenSoft : 'transparent', border: 'none', borderLeft: `2px solid ${active ? T.green : 'transparent'}`, color: active ? T.green : T.text, fontFamily: T.mono, fontSize, padding: '6px 10px', cursor: 'pointer' }}>
                <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
                  <span style={{ flex: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{w.name}</span>
                  <span style={{ fontSize: 9, color: T.faint, flexShrink: 0 }}>{w.steps?.length ?? 0} step{(w.steps?.length ?? 0) !== 1 ? 's' : ''}</span>
                </div>
                {w.description && <div style={{ fontSize: 10, color: T.faint, marginTop: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{w.description}</div>}
              </button>
            );
          })}
        </div>
      )}
    </div>
  );
}

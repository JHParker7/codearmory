import { useState, useRef } from 'react';
import { T } from '../theme';

/**
 * Image picker: a text field that filters the forge image allowlist as you type
 * and shows the top 10 matches in a dropdown. Forge enforces the allowlist
 * server-side, so this only surfaces allowed images; the field stays editable so
 * partial queries narrow the list (and so a value can still be typed when the
 * allowlist is empty/unavailable). Shared by the Forge run form and the
 * Workflows create-step form's forge/run image field.
 */
export function ImageSelect({ value, onChange, options, autoFocus, placeholder = 'ubuntu:22.04', fontSize = 12 }: {
  value: string;
  onChange: (v: string) => void;
  options: string[];
  autoFocus?: boolean;
  placeholder?: string;
  fontSize?: number;
}) {
  const [open, setOpen] = useState(false);
  const [hover, setHover] = useState(-1);
  const blurTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);

  const q = value.trim().toLowerCase();
  const matches = options.filter(o => o.toLowerCase().includes(q)).slice(0, 10);

  return (
    <div style={{ position: 'relative' }}
      onFocus={() => { if (blurTimer.current) clearTimeout(blurTimer.current); setOpen(true); }}
      onBlur={() => { blurTimer.current = setTimeout(() => setOpen(false), 120); }}>
      <input value={value} onChange={e => { onChange(e.target.value); setOpen(true); }} autoFocus={autoFocus}
        placeholder={placeholder}
        style={{ width: '100%', background: T.cardHi, border: `1px solid ${open ? T.green : T.border}`, color: T.text, fontFamily: T.mono, fontSize, padding: '8px 28px 8px 10px', outline: 'none', boxSizing: 'border-box' }} />
      <span style={{ position: 'absolute', right: 10, top: 11, color: T.faint, fontSize: 10, pointerEvents: 'none' }}>▾</span>
      {open && matches.length > 0 && (
        <div style={{ position: 'absolute', top: '100%', left: 0, right: 0, zIndex: 10, marginTop: 2, maxHeight: 264, overflow: 'auto', background: T.cardHi, border: `1px solid ${T.border}`, boxShadow: '0 6px 16px rgba(0,0,0,0.4)' }}>
          {matches.map((o, i) => {
            const active = value === o;
            return (
              <button key={o} type="button"
                onMouseDown={e => { e.preventDefault(); onChange(o); setOpen(false); }}
                onMouseEnter={() => setHover(i)} onMouseLeave={() => setHover(-1)}
                style={{ display: 'block', width: '100%', textAlign: 'left', background: hover === i || active ? T.greenSoft : 'transparent', border: 'none', borderLeft: `2px solid ${active ? T.green : 'transparent'}`, color: active ? T.green : T.text, fontFamily: T.mono, fontSize, padding: '7px 10px', cursor: 'pointer' }}>
                {o}
              </button>
            );
          })}
        </div>
      )}
    </div>
  );
}

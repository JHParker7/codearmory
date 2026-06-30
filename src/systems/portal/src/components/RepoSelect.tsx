import { useState, useRef } from 'react';
import { T } from '../theme';
import type { GitRepo } from '../api/bff';

/**
 * Repo picker: a text field that filters the git credential-broker's repo list
 * (enumerated across linked backends + manually pinned) by name or URL and shows
 * the top matches in a dropdown. The field's value is the repo's HTTPS clone URL —
 * what a forge `git:` secret_ref consumes — so it stays editable for a custom/
 * un-enumerated URL even when the list is empty or hasn't loaded. Shared by the
 * Forge run form, the Workflows create-step form (forge/run), and the Git page.
 */
export function RepoSelect({ value, onChange, repos, autoFocus, placeholder = 'select or paste a repo URL', fontSize = 12 }: {
  value: string;
  onChange: (v: string) => void;
  repos: GitRepo[];
  autoFocus?: boolean;
  placeholder?: string;
  fontSize?: number;
}) {
  const [open, setOpen] = useState(false);
  const [hover, setHover] = useState(-1);
  const blurTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);

  const q = value.trim().toLowerCase();
  const matches = repos
    .filter(r => !q || r.name.toLowerCase().includes(q) || r.url.toLowerCase().includes(q))
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
          {matches.map((r, i) => {
            const active = value === r.url;
            return (
              <button key={r.url} type="button"
                onMouseDown={e => { e.preventDefault(); onChange(r.url); setOpen(false); }}
                onMouseEnter={() => setHover(i)} onMouseLeave={() => setHover(-1)}
                style={{ display: 'block', width: '100%', textAlign: 'left', background: hover === i || active ? T.greenSoft : 'transparent', border: 'none', borderLeft: `2px solid ${active ? T.green : 'transparent'}`, color: active ? T.green : T.text, fontFamily: T.mono, fontSize, padding: '6px 10px', cursor: 'pointer' }}>
                <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
                  <span style={{ flex: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{r.name}</span>
                  {r.backend_type && <span style={{ fontSize: 9, color: T.blue, border: `1px solid ${T.blue}`, padding: '0 4px', flexShrink: 0 }}>{r.backend_type}</span>}
                  {r.source === 'manual' && <span style={{ fontSize: 9, color: T.faint, border: `1px solid ${T.faint}`, padding: '0 4px', flexShrink: 0 }}>pinned</span>}
                </div>
                <div style={{ fontSize: 10, color: T.faint, marginTop: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{r.url}</div>
              </button>
            );
          })}
        </div>
      )}
    </div>
  );
}

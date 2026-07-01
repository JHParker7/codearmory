import { useState, useRef, useEffect } from 'react';
import { T } from '../theme';
import { listGitBranches, type GitBranch } from '../api/bff';

/**
 * Branch picker: a text field that fetches the branches of `repoUrl` from the git
 * credential broker (enumerated via the owning backend's API) and filters them by
 * name in a dropdown. The field's value is the branch/tag name — what forge's
 * checkout.ref consumes — so it stays free-text editable for a tag, commit-less ref,
 * or a generic/un-enumerable backend that returns no list. Empty = the remote's
 * default branch. Shared by the Forge run form and the Workflows create-step form.
 */
export function BranchSelect({ token, repoUrl, value, onChange, fontSize = 12 }: {
  token: string;
  repoUrl: string;
  value: string;
  onChange: (v: string) => void;
  fontSize?: number;
}) {
  const [branches, setBranches] = useState<GitBranch[]>([]);
  const [open, setOpen] = useState(false);
  const [hover, setHover] = useState(-1);
  const blurTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);

  // Refetch whenever the selected repo changes; best-effort, so an error or an
  // un-enumerable backend just leaves the free-text field with no dropdown.
  useEffect(() => {
    const url = repoUrl.trim();
    if (!url) { setBranches([]); return; }
    let cancelled = false;
    listGitBranches(token, url)
      .then(bs => { if (!cancelled) setBranches(bs); })
      .catch(() => { if (!cancelled) setBranches([]); });
    return () => { cancelled = true; };
  }, [token, repoUrl]);

  const q = value.trim().toLowerCase();
  const matches = branches
    .filter(b => !q || b.name.toLowerCase().includes(q))
    .slice(0, 10);

  return (
    <div style={{ position: 'relative' }}
      onFocus={() => { if (blurTimer.current) clearTimeout(blurTimer.current); setOpen(true); }}
      onBlur={() => { blurTimer.current = setTimeout(() => setOpen(false), 120); }}>
      <input value={value} onChange={e => { onChange(e.target.value); setOpen(true); }}
        placeholder={branches.length ? 'default branch' : 'branch or tag (default branch if blank)'}
        style={{ width: '100%', background: T.cardHi, border: `1px solid ${open ? T.green : T.border}`, color: T.text, fontFamily: T.mono, fontSize, padding: '8px 28px 8px 10px', outline: 'none', boxSizing: 'border-box' }} />
      <span style={{ position: 'absolute', right: 10, top: 11, color: T.faint, fontSize: 10, pointerEvents: 'none' }}>▾</span>
      {open && matches.length > 0 && (
        <div style={{ position: 'absolute', top: '100%', left: 0, right: 0, zIndex: 10, marginTop: 2, maxHeight: 240, overflow: 'auto', background: T.cardHi, border: `1px solid ${T.border}`, boxShadow: '0 6px 16px rgba(0,0,0,0.4)' }}>
          {matches.map((b, i) => {
            const active = value === b.name;
            return (
              <button key={b.name} type="button"
                onMouseDown={e => { e.preventDefault(); onChange(b.name); setOpen(false); }}
                onMouseEnter={() => setHover(i)} onMouseLeave={() => setHover(-1)}
                style={{ display: 'flex', alignItems: 'center', gap: 6, width: '100%', textAlign: 'left', background: hover === i || active ? T.greenSoft : 'transparent', border: 'none', borderLeft: `2px solid ${active ? T.green : 'transparent'}`, color: active ? T.green : T.text, fontFamily: T.mono, fontSize, padding: '6px 10px', cursor: 'pointer' }}>
                <span style={{ flex: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{b.name}</span>
                {b.default && <span style={{ fontSize: 9, color: T.blue, border: `1px solid ${T.blue}`, padding: '0 4px', flexShrink: 0 }}>default</span>}
              </button>
            );
          })}
        </div>
      )}
    </div>
  );
}

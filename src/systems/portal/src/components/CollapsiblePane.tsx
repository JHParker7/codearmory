/**
 * CollapsiblePane — the shared pieces for panes that can be minimised out of the
 * way, used by the pipeline designer's palette / inspector / header.
 *
 * A minimised pane is not hidden: it leaves a thin rail (or, for a header, a slim
 * bar) carrying its name and the control that brings it back, so nothing becomes
 * unreachable. The open/closed choice is persisted per pane, because "I work with
 * the JSON panel shut" is a lasting preference, not a per-session one.
 */
import { useCallback, useEffect, useState } from 'react';
import { T } from '../theme';

/** Open/closed state for a pane, persisted under `storageKey`. Returns
 * `[open, setOpen, toggle]`; `defaultOpen` applies until the user first chooses. */
export function useCollapsible(storageKey: string, defaultOpen: boolean) {
  const [open, setOpen] = useState(() => {
    const v = localStorage.getItem(storageKey);
    return v === null ? defaultOpen : v === '1';
  });
  useEffect(() => { localStorage.setItem(storageKey, open ? '1' : '0'); }, [storageKey, open]);
  const toggle = useCallback(() => setOpen(o => !o), []);
  return [open, setOpen, toggle] as const;
}

/** The rail a minimised side pane leaves behind: a full-height strip with the pane's
 * name set vertically and a chevron pointing at where it will reappear. */
export function CollapsedRail({ side, label, onExpand, hint }: {
  /** Which edge the pane lives on — sets the border and the chevron's direction. */
  side: 'left' | 'right';
  label: string;
  onExpand: () => void;
  hint?: string;
}) {
  return (
    <button onClick={onExpand} title={hint ?? `show ${label}`}
      style={{
        width: 26, flexShrink: 0, alignSelf: 'stretch', background: T.bgAlt,
        border: 'none',
        borderRight: side === 'left' ? `1px solid ${T.border}` : undefined,
        borderLeft: side === 'right' ? `1px solid ${T.border}` : undefined,
        color: T.faint, cursor: 'pointer', padding: '8px 0',
        display: 'flex', flexDirection: 'column', alignItems: 'center', gap: 8,
      }}>
      <span style={{ fontSize: 11, lineHeight: 1 }}>{side === 'left' ? '›' : '‹'}</span>
      <span style={{
        fontFamily: T.mono, fontSize: 10, letterSpacing: 1, textTransform: 'uppercase',
        writingMode: 'vertical-rl', whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis',
      }}>{label}</span>
    </button>
  );
}

/** The chevron that minimises an open pane (or expands a collapsed header row). */
export function PaneToggle({ open, onToggle, hint, glyphs }: {
  open: boolean;
  onToggle: () => void;
  hint?: string;
  /** [closed, open] glyphs — defaults to a left/right chevron pair. */
  glyphs?: [string, string];
}) {
  const [shut, shown] = glyphs ?? ['›', '‹'];
  return (
    <button onClick={onToggle} title={hint ?? (open ? 'minimise' : 'expand')}
      style={{
        background: 'transparent', border: `1px solid ${T.border}`, color: T.faint,
        fontFamily: T.mono, fontSize: 10, lineHeight: 1, padding: '3px 6px', cursor: 'pointer',
      }}>
      {open ? shown : shut}
    </button>
  );
}

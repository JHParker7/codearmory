/**
 * ResizeHandle — a thin draggable divider between two horizontally-arranged panes.
 * While dragging it streams pointer movement to `onResize` as a signed pixel delta
 * (positive = moved right); the parent applies it to whichever pane's width it
 * tracks and clamps to sensible bounds. Window-level listeners keep the drag alive
 * when the cursor outruns the hit strip, and the cursor/selection are restored on
 * release. The callback is read through a ref so the listeners subscribe once.
 */
import { useCallback, useEffect, useRef, useState } from 'react';
import { T } from '../theme';

export function ResizeHandle({ onResize }: { onResize: (deltaX: number) => void }) {
  const dragging = useRef(false);
  const cb = useRef(onResize);
  cb.current = onResize;
  const [active, setActive] = useState(false);
  const [hover, setHover] = useState(false);

  useEffect(() => {
    const move = (e: MouseEvent) => { if (dragging.current) cb.current(e.movementX); };
    const up = () => {
      if (!dragging.current) return;
      dragging.current = false;
      setActive(false);
      document.body.style.cursor = '';
      document.body.style.userSelect = '';
    };
    window.addEventListener('mousemove', move);
    window.addEventListener('mouseup', up);
    return () => { window.removeEventListener('mousemove', move); window.removeEventListener('mouseup', up); };
  }, []);

  return (
    <div
      onMouseDown={() => {
        dragging.current = true;
        setActive(true);
        document.body.style.cursor = 'col-resize';
        document.body.style.userSelect = 'none';
      }}
      onMouseEnter={() => setHover(true)}
      onMouseLeave={() => setHover(false)}
      title="drag to resize"
      style={{ width: 8, flexShrink: 0, cursor: 'col-resize', alignSelf: 'stretch', display: 'flex', justifyContent: 'center', alignItems: 'stretch' }}>
      <div style={{ width: active || hover ? 2 : 1, background: active || hover ? T.green : T.border, transition: 'background .12s, width .12s' }} />
    </div>
  );
}

/**
 * useResizableWidth — manages a draggable pane width persisted under `storageKey`.
 * Returns `[width, handle]`: spread `width` onto the pane's `width` style and drop
 * `handle` as the pane's sibling on the side the divider should live (after a left
 * rail, before a right rail). `side` says which pane the handle controls — 'left'
 * grows as the divider moves right, 'right' grows as it moves left. The width is
 * clamped to [min, max] so neither pane collapses.
 */
export function useResizableWidth(
  storageKey: string,
  initial: number,
  opts?: { min?: number; max?: number; side?: 'left' | 'right' },
) {
  const min = opts?.min ?? 180;
  const max = opts?.max ?? 560;
  const side = opts?.side ?? 'left';
  const [width, setWidth] = useState(() => {
    const v = Number(localStorage.getItem(storageKey));
    return v >= min && v <= max ? v : initial;
  });
  useEffect(() => { localStorage.setItem(storageKey, String(width)); }, [storageKey, width]);
  const onResize = useCallback((dx: number) => setWidth(w => {
    const next = side === 'left' ? w + dx : w - dx;
    return Math.max(min, Math.min(next, max));
  }), [min, max, side]);
  return [width, <ResizeHandle key="rh" onResize={onResize} />] as const;
}

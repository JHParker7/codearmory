/**
 * ResizeHandle — a thin draggable divider between two panes. Horizontal (the
 * default) sits between side-by-side panes and resizes their width; vertical sits
 * between stacked panes and resizes their height. While dragging it streams pointer
 * movement to `onResize` as a signed pixel delta (positive = right / down); the
 * parent applies it to whichever pane's size it tracks and clamps to sensible
 * bounds. Window-level listeners keep the drag alive when the cursor outruns the hit
 * strip, and the cursor/selection are restored on release. The callback is read
 * through a ref so the listeners subscribe once.
 */
import { useCallback, useEffect, useRef, useState } from 'react';
import type { RefObject } from 'react';
import { T } from '../theme';

export type ResizeDirection = 'horizontal' | 'vertical';

export function ResizeHandle({ onResize, direction = 'horizontal', onResizeStart, onResizeEnd }: {
  onResize: (delta: number) => void;
  direction?: ResizeDirection;
  /** Fired on drag start/end — e.g. to suspend a width transition mid-drag. */
  onResizeStart?: () => void;
  onResizeEnd?: () => void;
}) {
  const horizontal = direction === 'horizontal';
  const cursor = horizontal ? 'col-resize' : 'row-resize';
  const dragging = useRef(false);
  const cb = useRef(onResize);
  cb.current = onResize;
  const endCb = useRef(onResizeEnd);
  endCb.current = onResizeEnd;
  const [active, setActive] = useState(false);
  const [hover, setHover] = useState(false);

  useEffect(() => {
    const move = (e: MouseEvent) => { if (dragging.current) cb.current(horizontal ? e.movementX : e.movementY); };
    const up = () => {
      if (!dragging.current) return;
      dragging.current = false;
      setActive(false);
      document.body.style.cursor = '';
      document.body.style.userSelect = '';
      endCb.current?.();
    };
    window.addEventListener('mousemove', move);
    window.addEventListener('mouseup', up);
    return () => { window.removeEventListener('mousemove', move); window.removeEventListener('mouseup', up); };
  }, [horizontal]);

  const lit = active || hover;
  return (
    <div
      onMouseDown={() => {
        dragging.current = true;
        setActive(true);
        document.body.style.cursor = cursor;
        document.body.style.userSelect = 'none';
        onResizeStart?.();
      }}
      onMouseEnter={() => setHover(true)}
      onMouseLeave={() => setHover(false)}
      title="drag to resize"
      style={{
        flexShrink: 0, cursor, alignSelf: 'stretch', display: 'flex',
        justifyContent: 'center', alignItems: 'stretch',
        ...(horizontal ? { width: 8 } : { height: 8, flexDirection: 'column' }),
      }}>
      <div style={{
        background: lit ? T.green : T.border, transition: 'background .12s, width .12s, height .12s',
        ...(horizontal ? { width: lit ? 2 : 1 } : { height: lit ? 2 : 1 }),
      }} />
    </div>
  );
}

interface ResizableOpts {
  min?: number;
  max?: number;
  /** Which pane the handle grows: 'left' grows as the divider moves right/down,
   * 'right' grows as it moves left/up. */
  side?: 'left' | 'right';
  direction?: ResizeDirection;
  /** When given, the size is also clamped so the OTHER pane keeps at least
   * `otherMin` px of the container's live size — neither pane can be dragged shut. */
  containerRef?: RefObject<HTMLElement | null>;
  otherMin?: number;
  /** Fired on drag start/end — e.g. to suspend a width transition mid-drag. */
  onDragStart?: () => void;
  onDragEnd?: () => void;
}

/**
 * useResizablePane — manages a draggable pane size (width for a horizontal split,
 * height for a vertical one) persisted under `storageKey`. Returns `[size, handle]`:
 * spread `size` onto the pane's `width`/`height` style and drop `handle` as the
 * pane's sibling on the side the divider should live. The size is clamped to
 * [min, max]; pass `containerRef` to also keep the opposite pane from collapsing.
 */
export function useResizablePane(storageKey: string, initial: number, opts?: ResizableOpts) {
  const min = opts?.min ?? 180;
  const max = opts?.max ?? 560;
  const side = opts?.side ?? 'left';
  const direction = opts?.direction ?? 'horizontal';
  const otherMin = opts?.otherMin ?? 200;
  const containerRef = opts?.containerRef;
  const [size, setSize] = useState(() => {
    const v = Number(localStorage.getItem(storageKey));
    return v >= min && v <= max ? v : initial;
  });
  useEffect(() => { localStorage.setItem(storageKey, String(size)); }, [storageKey, size]);
  const onResize = useCallback((delta: number) => setSize(w => {
    let next = side === 'left' ? w + delta : w - delta;
    next = Math.max(min, Math.min(next, max));
    const el = containerRef?.current;
    if (el) {
      const total = direction === 'horizontal' ? el.offsetWidth : el.offsetHeight;
      next = Math.max(min, Math.min(next, total - otherMin));
    }
    return next;
  }), [min, max, side, direction, otherMin, containerRef]);
  const handle = <ResizeHandle key="rh" onResize={onResize} direction={direction}
    onResizeStart={opts?.onDragStart} onResizeEnd={opts?.onDragEnd} />;
  return [size, handle] as const;
}

/**
 * useResizableWidth — the horizontal special case of {@link useResizablePane},
 * kept for the many master-detail rails that call it. Returns `[width, handle]`.
 */
export function useResizableWidth(
  storageKey: string,
  initial: number,
  opts?: { min?: number; max?: number; side?: 'left' | 'right' },
) {
  return useResizablePane(storageKey, initial, { ...opts, direction: 'horizontal' });
}

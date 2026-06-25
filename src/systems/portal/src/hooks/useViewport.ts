import { useEffect, useState } from 'react';

export interface Viewport {
  width: number;
  height: number;
}

/**
 * Tracks the window size so inline-styled components can be responsive without
 * CSS media queries. Updates on resize; SSR-safe defaults for the first render.
 */
export function useViewport(): Viewport {
  const [size, setSize] = useState<Viewport>(() => ({
    width: typeof window !== 'undefined' ? window.innerWidth : 1280,
    height: typeof window !== 'undefined' ? window.innerHeight : 800,
  }));

  useEffect(() => {
    const onResize = () => setSize({ width: window.innerWidth, height: window.innerHeight });
    onResize();
    window.addEventListener('resize', onResize);
    return () => window.removeEventListener('resize', onResize);
  }, []);

  return size;
}

/** Clamp a value to [min, max]. */
export const clamp = (n: number, min: number, max: number): number => Math.max(min, Math.min(n, max));

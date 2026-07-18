/**
 * Runs `load` on mount and again whenever the browser regains connectivity (the
 * window `online` event). One-shot option/catalog fetches that failed during an
 * outage otherwise leave a selector stuck with no options until a full page
 * reload; this lets them recover on their own once the connection is back.
 *
 * `load` is expected to keep the last-good value on failure (e.g. swallow the
 * error) rather than clearing it, so a failed retry never blanks a good list.
 * Pass `deps` exactly as you would to useEffect — the loader re-runs (and the
 * listener is refreshed) when they change.
 */
import { useEffect } from 'react';
import type { DependencyList } from 'react';

export function useReloadOnReconnect(load: () => void, deps: DependencyList): void {
  useEffect(() => {
    load();
    window.addEventListener('online', load);
    return () => window.removeEventListener('online', load);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);
}

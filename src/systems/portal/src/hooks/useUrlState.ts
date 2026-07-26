/**
 * useState-shaped hooks whose value lives in the URL query string, so a refresh
 * (and browser back/forward, and bookmarking/sharing a link) restores where you
 * were instead of resetting the page to its default view.
 *
 * Each module page mounts on its own path (/app/forge, /app/tickets, …) and the
 * sidebar navigates to absolute paths with no search string, so react-router
 * clears the query on a module switch — which means these hooks can use short,
 * shared keys ('tab', 'sel', …) without leaking state from one page to another.
 *
 * Writes use { replace: true } so navigating within a page (picking a tab, opening
 * an item) updates the current history entry rather than stacking a new one on
 * every click — a single Back still leaves the page.
 *
 * Each write is built from the LIVE window.location.search rather than the value
 * react-router closed over. react-router's setSearchParams reads its memoized
 * (committed) params, so two setters firing in the same tick would each compute
 * from the same stale base and the second would clobber the first's param. Because
 * history.pushState/replaceState update window.location synchronously, reading it
 * fresh each call lets several param updates in one handler compose correctly.
 */
import { useCallback } from 'react';
import { useSearchParams } from 'react-router-dom';

/**
 * A tab/enum selector backed by the URL. `fallback` is returned when the param is
 * absent, and setting the value back to `fallback` removes the param so the default
 * view keeps a clean URL.
 */
export function useUrlState<T extends string>(key: string, fallback: T): [T, (v: T) => void] {
  const [params, setParams] = useSearchParams();
  const value = (params.get(key) as T | null) ?? fallback;
  const set = useCallback(
    (v: T) => {
      const next = new URLSearchParams(window.location.search);
      if (v && v !== fallback) next.set(key, v);
      else next.delete(key);
      setParams(next, { replace: true });
    },
    [key, fallback, setParams],
  );
  return [value, set];
}

/**
 * A nullable id selector backed by the URL, shaped like useState<string | null>.
 * A null value (nothing selected) removes the param. Use this for "which row is
 * open" state so a refresh reopens the same item.
 */
export function useUrlParam(key: string): [string | null, (v: string | null) => void] {
  const [params, setParams] = useSearchParams();
  const value = params.get(key);
  const set = useCallback(
    (v: string | null) => {
      const next = new URLSearchParams(window.location.search);
      if (v) next.set(key, v);
      else next.delete(key);
      setParams(next, { replace: true });
    },
    [key, setParams],
  );
  return [value, set];
}

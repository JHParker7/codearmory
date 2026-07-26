/**
 * Form-draft persistence — the reusable form of the pattern the pipeline and ticket
 * editors use. Mirrors an in-progress form to localStorage so a refresh (or a
 * connection blip that bounces the app to sign-in) restores it instead of discarding
 * the user's work.
 *
 * It is two hooks because the seed must be read BEFORE the form's useState calls,
 * while the persist effect needs the live values AFTER them:
 *
 *   const baseline = useMemo(() => ({ title: '', body: '' }), [deps]);
 *   const { initial, restored } = useDraftSeed(key, baseline);   // read once, to seed state
 *   const [title, setTitle] = useState(initial.title); ...
 *   useDraftPersist(key, baseline, { title, body });             // save when dirty, clear when clean
 *
 * Clear the draft on a successful save with clearDraft(key) (re-exported here).
 *
 * SECURITY: never include secret/credential fields (tokens, private keys, secret
 * values) in the persisted object — plaintext in localStorage is readable by any XSS.
 * Keep those fields out of `baseline`/`current` so they are never written.
 *
 * Note both objects must list their keys in the same order: the dirty check compares
 * JSON.stringify output, and object key order is insertion order in JS. Build
 * `current` with the same field order as `baseline`.
 */
import { useEffect, useRef } from 'react';
import { loadDraft, saveDraft, clearDraft } from '../draftStorage';

export { clearDraft } from '../draftStorage';

/**
 * Read any persisted draft once, on first render. Returns the values to seed state
 * from (the draft when one that differs from the pristine baseline exists, else the
 * baseline) and whether a real draft was restored (for showing a restore banner).
 */
export function useDraftSeed<T extends Record<string, unknown>>(key: string, baseline: T): { initial: T; restored: boolean } {
  const seed = useRef<{ initial: T; restored: boolean } | null>(null);
  if (seed.current === null) {
    // An empty key disables persistence (e.g. a form reused in a context that already
    // has its own draft) — seed straight from the baseline, restore nothing.
    if (!key) {
      seed.current = { initial: baseline, restored: false };
    } else {
      const raw = loadDraft(key);
      let d: T | null = null;
      if (raw) {
        try {
          const p = JSON.parse(raw);
          if (p && typeof p === 'object') d = { ...baseline, ...(p as Partial<T>) };
        } catch { /* stale/corrupt draft — ignore */ }
      }
      const restored = !!d && JSON.stringify(d) !== JSON.stringify(baseline);
      seed.current = { initial: restored ? (d as T) : baseline, restored };
    }
  }
  return seed.current;
}

/**
 * Mirror the live form values to storage whenever they diverge from the pristine
 * baseline, and clear the draft when they match again (nothing worth restoring).
 */
export function useDraftPersist<T extends Record<string, unknown>>(key: string, baseline: T, current: T): void {
  const baseNorm = JSON.stringify(baseline);
  const curNorm = JSON.stringify(current);
  useEffect(() => {
    if (!key) return; // persistence disabled for this form instance
    if (curNorm !== baseNorm) saveDraft(key, curNorm);
    else clearDraft(key);
  }, [key, curNorm, baseNorm]);
}

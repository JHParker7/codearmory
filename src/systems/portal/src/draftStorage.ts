/**
 * Client-side draft persistence. In-progress work in the builders (a new/edited
 * pipeline, a new/edited service) lives only in component state, so a page reload
 * — or a connection blip that bounces the app back to the sign-in screen — used to
 * throw it away. These helpers mirror that work to localStorage under a per-entity
 * key so it survives a reload and can be restored when the user returns.
 *
 * Everything is best-effort: localStorage can be unavailable (private mode, quota,
 * disabled) and a stored draft can be stale/corrupt. All access is wrapped so a
 * failure degrades to "no draft" rather than crashing the builder.
 */

/** Read a persisted draft string, or null if none / storage is unavailable. */
export function loadDraft(key: string): string | null {
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}

/** Persist a draft string. Silently no-ops if storage is unavailable or full. */
export function saveDraft(key: string, value: string): void {
  try {
    localStorage.setItem(key, value);
  } catch {
    /* ignore — persistence is best-effort */
  }
}

/** Drop a persisted draft (e.g. after a successful save). */
export function clearDraft(key: string): void {
  try {
    localStorage.removeItem(key);
  } catch {
    /* ignore */
  }
}

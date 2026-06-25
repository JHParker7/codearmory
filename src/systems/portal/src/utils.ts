/**
 * Pure client-side helpers shared across the SPA: relative-time formatting,
 * password-strength scoring for the signup/setup forms, JWT payload decoding
 * (for UX only, never trust), and persistence of the blueprints workspace list.
 */

/** Compact fallback display for an unresolved id: first 8 chars + ellipsis. */
export function shortId(id: string): string {
  return `${id.slice(0, 8)}…`;
}

/** Run/step statuses that are still in flight (not terminal). */
export const RUN_ACTIVE = ['running', 'in_progress', 'pending', 'queued'];
/** Whether a run/step status is still in flight. */
export const isRunActive = (status: string): boolean => RUN_ACTIVE.includes(status);

/** Map a run/step status to a small badge tone (green=done, amber=in-flight, red=failed). */
export function statusTone(status: string): 'green' | 'amber' | 'red' | 'dim' {
  if (['completed', 'success'].includes(status)) return 'green';
  if (isRunActive(status)) return 'amber';
  if (['failed', 'error'].includes(status)) return 'red';
  return 'dim';
}

/** Compact human duration between two ISO timestamps; start→now when not ended. */
export function fmtDuration(startISO?: string | null, endISO?: string | null): string {
  if (!startISO) return '';
  const start = new Date(startISO).getTime();
  const end = endISO ? new Date(endISO).getTime() : Date.now();
  const s = Math.max(0, Math.round((end - start) / 1000));
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${s % 60}s`;
  return `${Math.floor(m / 60)}h ${m % 60}m`;
}

/** Format the elapsed time since an ISO timestamp as a compact `Ns`/`Nm`/`Nh`/`Nd` string. */
export function timeAgo(iso: string, now = Date.now()): string {
  const diff = now - new Date(iso).getTime();
  const s = Math.floor(diff / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h`;
  return `${Math.floor(h / 24)}d`;
}

export interface PasswordChecks {
  [key: string]: boolean;
  len: boolean;
  upper: boolean;
  lower: boolean;
  num: boolean;
  sym: boolean;
}

export interface PasswordScore {
  score: number;
  checks: PasswordChecks;
}

/** Score a password 0–5 against length/upper/lower/number/symbol checks, returning the score and the per-check booleans. */
export function passwordScore(pw: string): PasswordScore {
  if (!pw) return { score: 0, checks: { len: false, upper: false, lower: false, num: false, sym: false } };
  const checks: PasswordChecks = {
    len: pw.length >= 8,
    upper: /[A-Z]/.test(pw),
    lower: /[a-z]/.test(pw),
    num: /\d/.test(pw),
    sym: /[^A-Za-z0-9]/.test(pw),
  };
  return { score: Object.values(checks).filter(Boolean).length, checks };
}

/**
 * Decode a JWT's payload (base64url) without verifying the signature — for
 * client-side UX only (extracting sub/session_id); never a trust decision.
 * Returns null on any malformed token.
 */
export function decodeJwtPayload(token: string): Record<string, unknown> | null {
  try {
    return JSON.parse(atob(token.split('.')[1].replace(/-/g, '+').replace(/_/g, '/')));
  } catch {
    return null;
  }
}

/** Extract the user id (`sub` claim) from a JWT, or null if absent/malformed. */
export function decodeUserId(token: string): string | null {
  const payload = decodeJwtPayload(token);
  return typeof payload?.sub === 'string' ? payload.sub : null;
}

export interface WorkspaceEntry {
  path: string;
  label: string;
}

/** Parse the persisted workspace list from localStorage, returning [] on missing or malformed JSON. */
export function parseStoredWorkspaces(raw: string): WorkspaceEntry[] {
  try {
    const data = JSON.parse(raw);
    if (Array.isArray(data) && data.every((w) => typeof w?.path === 'string' && typeof w?.label === 'string')) {
      return data as WorkspaceEntry[];
    }
  } catch {}
  return [];
}

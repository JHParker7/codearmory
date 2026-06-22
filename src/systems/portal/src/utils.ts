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

// Decode a JWT's payload (base64url) without verifying the signature — for
// client-side UX only (extracting sub/session_id); never a trust decision.
export function decodeJwtPayload(token: string): Record<string, unknown> | null {
  try {
    return JSON.parse(atob(token.split('.')[1].replace(/-/g, '+').replace(/_/g, '/')));
  } catch {
    return null;
  }
}

export function decodeUserId(token: string): string | null {
  const payload = decodeJwtPayload(token);
  return typeof payload?.sub === 'string' ? payload.sub : null;
}

export interface WorkspaceEntry {
  path: string;
  label: string;
}

export function parseStoredWorkspaces(raw: string): WorkspaceEntry[] {
  try {
    const data = JSON.parse(raw);
    if (Array.isArray(data) && data.every((w) => typeof w?.path === 'string' && typeof w?.label === 'string')) {
      return data as WorkspaceEntry[];
    }
  } catch {}
  return [];
}

/**
 * In-memory TTL cache for normalized workspace-state views, keyed by
 * (workspace path, Authorization token) so one user's cached state is never
 * served to another. Backs the BFF's `/state/*` GET route; writes invalidate it.
 */

// TTL for cached workspace-state views. A non-numeric or non-positive
// STATE_CACHE_TTL_MS falls back to the default rather than yielding NaN — with a
// NaN TTL, expiresAt is NaN and `Date.now() > NaN` is always false, so entries
// would never expire.
function parseTtl(): number {
  const raw = process.env.STATE_CACHE_TTL_MS;
  if (raw === undefined) return 2000;
  const n = parseInt(raw, 10);
  return Number.isFinite(n) && n > 0 ? n : 2000;
}

const TTL_MS = parseTtl();
// How often set() opportunistically sweeps expired entries. Bounded below so a
// tiny TTL doesn't make every write do a full scan.
const SWEEP_INTERVAL_MS = Math.max(TTL_MS, 30_000);

interface Entry {
  value: unknown;
  expiresAt: number;
}

class StateCache {
  private readonly data = new Map<string, Map<string, Entry>>();
  private lastSweep = Date.now();

  /** Return the cached view for (wsPath, auth), or undefined if absent or expired (expired entries are evicted on read). */
  get(wsPath: string, auth: string): unknown | undefined {
    const byAuth = this.data.get(wsPath);
    if (!byAuth) return undefined;
    const entry = byAuth.get(auth);
    if (!entry) return undefined;
    if (Date.now() > entry.expiresAt) {
      byAuth.delete(auth);
      if (byAuth.size === 0) this.data.delete(wsPath);
      return undefined;
    }
    return entry.value;
  }

  /** Store a view for (wsPath, auth) with a fresh TTL, opportunistically sweeping expired entries first. */
  set(wsPath: string, auth: string, value: unknown): void {
    this.maybeSweep();
    let byAuth = this.data.get(wsPath);
    if (!byAuth) {
      byAuth = new Map();
      this.data.set(wsPath, byAuth);
    }
    byAuth.set(auth, { value, expiresAt: Date.now() + TTL_MS });
  }

  /** Drop all cached views for a workspace path (every token). Called after any write to that path. */
  invalidate(wsPath: string): void {
    this.data.delete(wsPath);
  }

  /** Empty the entire cache. */
  clear(): void {
    this.data.clear();
  }

  // maybeSweep drops expired entries (and emptied inner maps) at most once per
  // SWEEP_INTERVAL_MS. Without it, entries keyed by a now-rotated Authorization
  // token are only evicted when read again with that exact token — which never
  // happens for an old token — so the maps would grow without bound.
  private maybeSweep(): void {
    const now = Date.now();
    if (now - this.lastSweep < SWEEP_INTERVAL_MS) return;
    this.lastSweep = now;
    for (const [wsPath, byAuth] of this.data) {
      for (const [auth, entry] of byAuth) {
        if (now > entry.expiresAt) byAuth.delete(auth);
      }
      if (byAuth.size === 0) this.data.delete(wsPath);
    }
  }
}

export const stateCache = new StateCache();

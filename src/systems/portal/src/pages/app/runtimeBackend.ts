/**
 * Pure helpers for the forge runtime-backend admin tab. Kept out of the component
 * so the parsing/validation is unit-testable (and mirrors the CLI TUI's
 * parseKVLines/frMapToLines so both admin surfaces behave identically).
 */

/**
 * The runtime implementations a backend may target. Authoritatively enforced by
 * forge (validRuntimeTypes); duplicated here only to drive the type <select>.
 *   - docker / kubernetes: shared-kernel container backends.
 *   - kata / gvisor: kernel-isolated (microVM / userspace kernel) — require a
 *     `runtime_class` config key and are the only types that may run privileged.
 */
export const RUNTIME_TYPES = ['docker', 'kubernetes', 'kata', 'gvisor'] as const;
export type RuntimeType = (typeof RUNTIME_TYPES)[number];

/** kata/gvisor require a `runtime_class` config key (the Kubernetes RuntimeClass). */
export function requiresRuntimeClass(type: string): boolean {
  return type === 'kata' || type === 'gvisor';
}

/**
 * Parse a `key=value`-per-line textarea (config / secret_refs) into a map. Blank
 * lines are skipped; a line missing `=` or with an empty key is a hard error so a
 * typo surfaces before the request rather than silently dropping a setting. Only
 * the first `=` splits, so values may contain `=`. Matches the CLI TUI's parser.
 */
export function parseKVLines(text: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const raw of text.split('\n')) {
    const line = raw.trim();
    if (!line) continue;
    const eq = line.indexOf('=');
    if (eq < 0) throw new Error(`invalid line (expected key=value): ${line}`);
    const key = line.slice(0, eq).trim();
    if (!key) throw new Error(`invalid line (empty key): ${line}`);
    out[key] = line.slice(eq + 1).trim();
  }
  return out;
}

/** Render a map back to sorted `key=value` lines, to pre-fill the edit textareas. */
export function formatKVLines(m: Record<string, string> | null | undefined): string {
  if (!m) return '';
  return Object.keys(m).sort().map(k => `${k}=${m[k]}`).join('\n');
}

/**
 * Per-occurrence inputs editor shown inline on the selected step block in the
 * pipeline builder. It overrides the step's `with` config for THIS occurrence only
 * (merged over the step definition at run time) and — the point of it — lets each
 * input be wired to an earlier step's output via the ⚯ dropdown, which inserts a
 * ${steps.<name>.output[.KEY]} reference (for a forge step that's the env vars it
 * captures via output_env — stdout is never a step output). Only keys that differ
 * from the step definition are kept, so the override stays minimal.
 */
import { T } from '../../theme';

export type UpstreamOutput = { name: string; outputEnv: string[] };

// Step-defining `with` keys that are configured on the step definition (Steps tab),
// not wired per-occurrence in the pipeline builder.
const DEFINING = new Set(['image', 'run']);

const stop = (e: React.PointerEvent) => e.stopPropagation();
const fieldStyle: React.CSSProperties = {
  flex: 1, minWidth: 0, background: T.cardHi, border: `1px solid ${T.border}`, color: T.text,
  fontFamily: T.mono, fontSize: 11, padding: '3px 6px', outline: 'none',
};

function WireSelect({ refs, onPick }: { refs: string[]; onPick: (ref: string) => void }) {
  if (refs.length === 0) return null;
  return (
    <select onPointerDown={stop} value="" onChange={(e) => { if (e.target.value) onPick(e.target.value); }}
      title="wire to an earlier step's output"
      style={{ flexShrink: 0, maxWidth: 80, background: 'transparent', border: `1px solid ${T.border}`, color: T.green, fontFamily: T.mono, fontSize: 10, padding: '2px', cursor: 'pointer' }}>
      <option value="">⚯ wire</option>
      {refs.map((r) => <option key={r} value={r}>{r}</option>)}
    </select>
  );
}

export function StepInputsEditor({ defWith, override, upstream, onChange }: {
  defWith: Record<string, unknown>;
  override: Record<string, unknown>;
  upstream: UpstreamOutput[];
  onChange: (override: Record<string, unknown>) => void;
}) {
  const eff: Record<string, unknown> = { ...defWith, ...override };
  const refs: string[] = [];
  for (const u of upstream) {
    refs.push(`\${steps.${u.name}.output}`);
    for (const k of u.outputEnv) refs.push(`\${steps.${u.name}.output.${k}}`);
  }

  // Set a top-level key, dropping it from the override when it matches the default.
  const setKey = (key: string, value: unknown) => {
    const next = { ...override };
    if (JSON.stringify(value) === JSON.stringify(defWith[key])) delete next[key];
    else next[key] = value;
    onChange(next);
  };

  // The image and the run command define what the step IS — they belong to the step
  // definition (Steps tab), not per-occurrence wiring; so they're not editable here.
  const stringKeys = Object.keys(eff).filter((k) => k !== 'env' && !DEFINING.has(k) && typeof eff[k] === 'string');
  const env = (eff.env && typeof eff.env === 'object' && !Array.isArray(eff.env)) ? eff.env as Record<string, unknown> : null;
  const setEnv = (next: Record<string, unknown>) => setKey('env', next);

  const label: React.CSSProperties = { fontFamily: T.mono, fontSize: 10, color: T.faint, width: 64, flexShrink: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' };

  return (
    <div onPointerDown={stop} style={{ marginTop: 6, padding: 8, background: T.bg, border: `1px dashed ${T.border}`, borderLeft: `3px solid ${T.green}`, display: 'flex', flexDirection: 'column', gap: 6 }}>
      <div style={{ fontFamily: T.mono, fontSize: 9, color: T.green, letterSpacing: 1, textTransform: 'uppercase' }}>
        inputs · {refs.length > 0 ? 'wire ⚯ to an upstream output' : 'no upstream outputs yet'}
      </div>
      {stringKeys.length === 0 && !env && (
        <div style={{ fontFamily: T.mono, fontSize: 10, color: T.faint }}>this step has no editable inputs</div>
      )}
      {stringKeys.map((k) => (
        <div key={k} style={{ display: 'flex', alignItems: 'center', gap: 4 }}>
          <span style={label}>{k}</span>
          <input value={String(eff[k] ?? '')} onPointerDown={stop} onChange={(e) => setKey(k, e.target.value)} style={fieldStyle} />
          <WireSelect refs={refs} onPick={(ref) => setKey(k, `${String(eff[k] ?? '')}${eff[k] ? ' ' : ''}${ref}`)} />
        </div>
      ))}
      {env && (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
          <span style={{ fontFamily: T.mono, fontSize: 10, color: T.faint }}>env</span>
          {Object.entries(env).map(([ek, ev]) => (
            <div key={ek} style={{ display: 'flex', alignItems: 'center', gap: 4, paddingLeft: 8 }}>
              <span style={{ ...label, width: 56, color: T.amber }}>{ek}</span>
              <input value={String(ev ?? '')} onPointerDown={stop} onChange={(e) => setEnv({ ...env, [ek]: e.target.value })} style={fieldStyle} />
              <WireSelect refs={refs} onPick={(ref) => setEnv({ ...env, [ek]: ref })} />
              <button onPointerDown={stop} onClick={() => { const n = { ...env }; delete n[ek]; setEnv(n); }} title="remove env var"
                style={{ background: 'transparent', border: 'none', color: T.faint, cursor: 'pointer', fontFamily: T.mono, fontSize: 12, padding: 0 }}>✕</button>
            </div>
          ))}
          <AddEnvRow existing={env} onAdd={(k) => setEnv({ ...env, [k]: '' })} />
        </div>
      )}
    </div>
  );
}

function AddEnvRow({ existing, onAdd }: { existing: Record<string, unknown>; onAdd: (key: string) => void }) {
  return (
    <input placeholder="+ env var (KEY)" onPointerDown={stop}
      onKeyDown={(e) => {
        if (e.key === 'Enter') {
          const k = (e.target as HTMLInputElement).value.replace(/[^A-Za-z0-9_]/g, '');
          if (k && !(k in existing)) { onAdd(k); (e.target as HTMLInputElement).value = ''; }
        }
      }}
      style={{ ...fieldStyle, marginLeft: 8, fontSize: 10, color: T.faint }} />
  );
}

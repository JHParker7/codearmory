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
import {
  schemaForAction, actionSupportsGitRepo, gitRepoFromWith, withGitRepo,
  actionAttachesVolume, volumeAttachWith, volumeAttachFromWith, DEFAULT_MOUNT_PATH,
} from './stepSchema';
import { RepoSelect } from '../../components/RepoSelect';
import { BranchSelect } from '../../components/BranchSelect';
import type { GitRepo } from '../../api/bff';

export type UpstreamOutput = { name: string; outputEnv: string[] };

// The `with` keys that are NOT wired per-occurrence: an action's "config" fields
// (what the step IS — e.g. forge's image/run/runner) and its "output" declarations.
// Everything else is an input the pipeline connects here. Derived from the action
// schema so it stays in sync with the Steps form's config/inputs/outputs split.
function nonInputKeys(action: string): Set<string> {
  const keys = new Set<string>();
  for (const f of schemaForAction(action)) {
    // config/output belong to the step definition; pipeline fields (the volume) get a
    // dedicated selector below — none are handled by the generic input rows.
    if (f.config || f.output || f.pipeline) keys.add(f.key);
  }
  return keys;
}

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

export function StepInputsEditor({ action, defWith, override, upstream, upstreamVolumes, repos, token, onChange }: {
  action: string;
  defWith: Record<string, unknown>;
  override: Record<string, unknown>;
  upstream: UpstreamOutput[];
  // Names of workspace volumes created by upstream forge/create-volume steps, offered
  // as options in this step's volume selector (a step attaches a volume made earlier).
  upstreamVolumes: string[];
  repos: GitRepo[];
  token?: string;
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

  // The per-occurrence git repo lives in secret_refs.GIT_CLONE_URL. It is replaced as
  // a whole map (the backend merges `with` overrides at the top level), so build from
  // the EFFECTIVE secret_refs to preserve any other entries (e.g. a secret: ref).
  const showRepo = actionSupportsGitRepo(action);
  const repoVal = gitRepoFromWith(eff);
  const setRepo = (url: string) => {
    const sr = withGitRepo(eff.secret_refs, url);
    const next = { ...override };
    // Drop the override when the repo (with any sibling secret_refs) is empty or
    // matches the step definition, so the saved override stays minimal.
    if (sr === undefined || JSON.stringify(sr) === JSON.stringify(defWith.secret_refs)) delete next.secret_refs;
    else next.secret_refs = sr;
    onChange(next);
  };

  // Per-occurrence checkout: with a repo set, forge can clone it into the working
  // dir and cd in before the run (actions/checkout-style). Stored as with.checkout,
  // a passthrough object forge consumes; empty {} = defaults, {path} sets the dir.
  const checkoutSpec = (eff.checkout && typeof eff.checkout === 'object' && !Array.isArray(eff.checkout))
    ? eff.checkout as Record<string, unknown> : null;
  const checkoutPath = typeof checkoutSpec?.path === 'string' ? checkoutSpec.path : '';
  const checkoutRef = typeof checkoutSpec?.ref === 'string' ? checkoutSpec.ref : '';
  const setCheckout = (spec: Record<string, unknown> | null) => {
    const next = { ...override };
    if (spec === null) delete next.checkout;
    else next.checkout = spec;
    onChange(next);
  };
  // Update one field of the checkout spec, preserving the others (and any keys forge
  // understands beyond path/ref, e.g. depth). A blank value drops that key.
  const patchCheckout = (patch: { path?: string; ref?: string }) => {
    const next: Record<string, unknown> = { ...(checkoutSpec ?? {}) };
    for (const [k, v] of Object.entries(patch)) {
      const trimmed = (v ?? '').trim();
      if (trimmed) next[k] = trimmed; else delete next[k];
    }
    setCheckout(next);
  };

  // Workspace volume (per-occurrence): which shared volume this step attaches to
  // depends on the create-volume step in THIS pipeline, so it's wired here — as a
  // selector over the volumes upstream steps created — not baked into the step def.
  // eff.volumes is the run-scoped attach array; render it back to "name" / "name:/mount".
  const showVolume = actionAttachesVolume(action);
  const volSpec = volumeAttachFromWith(eff.volumes); // '' | 'name' | 'name:/mount'
  const volColon = volSpec.indexOf(':');
  const volName = volColon >= 0 ? volSpec.slice(0, volColon) : volSpec;
  const volMount = volColon >= 0 ? volSpec.slice(volColon + 1) : '';
  // Offer every upstream-created volume, plus the current selection if it isn't one of
  // them (e.g. a name typed before the create-volume step existed), so it's never lost.
  const volOptions = [...upstreamVolumes];
  if (volName && !volOptions.includes(volName)) volOptions.push(volName);
  const setVolume = (name: string, mount: string) => {
    const n = name.trim();
    if (!n) {
      // Detach. If the step DEFINITION bakes in a volume (a legacy step, since volumes
      // are now per-occurrence), an explicit empty array is needed to override it — a
      // dropped/undefined override would let the def's volume persist. Otherwise just
      // drop the key so the override stays minimal.
      setKey('volumes', Array.isArray(defWith.volumes) ? [] : undefined);
      return;
    }
    const m = mount.trim();
    setKey('volumes', volumeAttachWith(m && m !== DEFAULT_MOUNT_PATH ? `${n}:${m}` : n));
  };

  // Config fields (image/run/runner) and outputs define what the step IS — they
  // belong to the step definition, not per-occurrence wiring; so they're not
  // editable here. Everything else is an input the pipeline connects.
  const nonInput = nonInputKeys(action);
  const stringKeys = Object.keys(eff).filter((k) => k !== 'env' && !nonInput.has(k) && typeof eff[k] === 'string');
  const env = (eff.env && typeof eff.env === 'object' && !Array.isArray(eff.env)) ? eff.env as Record<string, unknown> : null;
  const setEnv = (next: Record<string, unknown>) => setKey('env', next);

  const label: React.CSSProperties = { fontFamily: T.mono, fontSize: 10, color: T.faint, width: 64, flexShrink: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' };

  return (
    <div onPointerDown={stop} style={{ marginTop: 6, padding: 8, background: T.bg, border: `1px dashed ${T.border}`, borderLeft: `3px solid ${T.green}`, display: 'flex', flexDirection: 'column', gap: 6 }}>
      <div style={{ fontFamily: T.mono, fontSize: 9, color: T.green, letterSpacing: 1, textTransform: 'uppercase' }}>
        inputs · {refs.length > 0 ? 'wire ⚯ to an upstream output' : 'no upstream outputs yet'}
      </div>
      {showRepo && (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 3 }}>
          <span style={{ fontFamily: T.mono, fontSize: 10, color: T.faint }}>git repo</span>
          <RepoSelect value={repoVal} onChange={setRepo} repos={repos} placeholder="select a repo, or ${inputs.REPO}" fontSize={11} />
          <span style={{ fontFamily: T.mono, fontSize: 9, color: T.faint, lineHeight: 1.4 }}>
            injected as $GIT_CLONE_URL · pick a repo for this step or reference a run input like {'${inputs.REPO}'}
          </span>
          {/* forge/git-clone always checks out (baked into the step). The branch/tag is
              picked here per-occurrence — like forge/run's checkout below — since the repo
              it enumerates is also per-occurrence. */}
          {repoVal && action === 'forge/git-clone' && (
            <div style={{ display: 'flex', flexDirection: 'column', gap: 3, marginTop: 2 }}>
              <span style={{ fontFamily: T.mono, fontSize: 10, color: T.faint }}>branch / tag</span>
              <div onPointerDown={stop}>
                <BranchSelect token={token ?? ''} repoUrl={repoVal} value={checkoutRef}
                  onChange={(v) => patchCheckout({ ref: v })} fontSize={10} />
              </div>
              <span style={{ fontFamily: T.mono, fontSize: 9, color: T.faint }}>blank = the remote's default branch</span>
            </div>
          )}
          {/* forge/run: an OPTIONAL checkout that clones into the working dir before the
              command — unlike forge/git-clone it runs in the step's own image, so that
              image must contain git. */}
          {repoVal && action !== 'forge/git-clone' && (
            <>
              <label style={{ display: 'flex', alignItems: 'center', gap: 6, cursor: 'pointer', fontFamily: T.mono, fontSize: 10, color: T.dim, marginTop: 2 }}>
                <input type="checkbox" checked={checkoutSpec !== null} onPointerDown={stop}
                  onChange={(e) => setCheckout(e.target.checked ? {} : null)} />
                check out into working dir <span style={{ color: T.faint }}>(clone + cd before the command · needs git in the image)</span>
              </label>
              {checkoutSpec !== null && (
                <div style={{ display: 'flex', flexDirection: 'column', gap: 4, marginLeft: 20 }}>
                  <div onPointerDown={stop}>
                    <BranchSelect token={token ?? ''} repoUrl={repoVal} value={checkoutRef}
                      onChange={(v) => patchCheckout({ ref: v })} fontSize={10} />
                  </div>
                  <input value={checkoutPath} onPointerDown={stop} placeholder="clone dir (optional, defaults to repo name)"
                    onChange={(e) => patchCheckout({ path: e.target.value })}
                    style={{ ...fieldStyle, flex: 'unset', fontSize: 10 }} />
                </div>
              )}
            </>
          )}
        </div>
      )}
      {showVolume && (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 3 }}>
          <span style={{ fontFamily: T.mono, fontSize: 10, color: T.faint }}>workspace volume</span>
          <select value={volName} onPointerDown={stop} onChange={(e) => setVolume(e.target.value, volMount)}
            title="attach a workspace created by an upstream forge/create-volume step"
            style={{ ...fieldStyle, cursor: 'pointer' }}>
            <option value="">— none —</option>
            {volOptions.map((n) => <option key={n} value={n}>{n}</option>)}
          </select>
          {volName && (
            <input value={volMount} onPointerDown={stop} placeholder="/workspace (default mount)"
              onChange={(e) => setVolume(volName, e.target.value)} style={{ ...fieldStyle, fontSize: 10 }} />
          )}
          <span style={{ fontFamily: T.mono, fontSize: 9, color: T.faint, lineHeight: 1.4 }}>
            {upstreamVolumes.length > 0
              ? 'the shared workspace this step mounts — pick one an upstream forge/create-volume step made'
              : 'no workspace created upstream yet — add a forge/create-volume step before this one'}
          </span>
        </div>
      )}
      {stringKeys.length === 0 && !env && !showRepo && !showVolume && (
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

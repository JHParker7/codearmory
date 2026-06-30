/**
 * Step-action input schemas — the portal twin of the CLI's steps_schema.go.
 *
 * Every workflow action consumes a flat `with` map that the workflows worker
 * turns into the target service's request body. Rather than make the user
 * hand-write that JSON, the create-step form renders a tailored set of inputs
 * per action and assembles the `with` map from them. Actions without a known
 * schema fall back to a single free-form With JSON field, so custom/unknown
 * catalog actions (and the `http` escape hatch) still work.
 *
 * Field keys are namespaced `with.<key>` (WITH_KEY_PREFIX) so they never collide
 * with the form's own step-level fields (name, action, timeout, desc).
 */

/** WITH_KEY_PREFIX namespaces schema field keys within the form. */
export const WITH_KEY_PREFIX = 'with.';

/** RAW_WITH_KEY is the key of the optional/advanced free-form With JSON field. */
export const RAW_WITH_KEY = '__raw';

/** Controls how a schema field's text value is parsed into the with map.
 * `list` splits a comma-separated string into a string array; `repo` wraps the
 * selected clone URL into a forge `secret_refs` entry (see GIT_CLONE_ENV). */
export type StepFieldKind = 'text' | 'int' | 'env' | 'json' | 'list' | 'repo';

/**
 * Names a catalog that backs a field with a picker instead of free text:
 * `image` (the forge image allowlist) and `repo` (the git broker's repo list).
 * The field degrades to a plain text input when its catalog is empty/unavailable.
 */
export type StepFieldCatalog = 'image' | 'repo';

/**
 * Env var a `repo`-kind field injects the minted clone URL as. Selecting a repo
 * sets `with.secret_refs = { GIT_CLONE_URL: "git:<clone-url>" }`; forge resolves
 * the ref at dispatch so the runner clones/pulls/pushes via `$GIT_CLONE_URL`.
 */
export const GIT_CLONE_ENV = 'GIT_CLONE_URL';

/**
 * One input within an action's schema. `key` is the with-map key it fills
 * (without the WITH_KEY_PREFIX the form adds). `catalog`, when set, renders the
 * field as a picker over that catalog.
 */
export interface StepField {
  key: string;
  label: string;
  placeholder: string;
  required?: boolean;
  kind?: StepFieldKind;
  multiline?: boolean;
  catalog?: StepFieldCatalog;
  /** Marks a field that configures the step's OUTPUT (not an input), so the form
   * groups it under a separate "outputs" section. */
  output?: boolean;
  /** Marks a "config" field that defines what the step IS (e.g. forge's image and
   * run command) rather than a wireable input. Config fields are set on the step
   * definition and are NOT wired per-occurrence in the pipeline builder. Fields
   * with neither `config` nor `output` are INPUTS: parameters the pipeline supplies
   * (the value set here is the default), grouped under the form's "inputs" section. */
  config?: boolean;
}

/**
 * Maps a catalog action to its ordered, tailored field set. Keep these in sync
 * with the target services' request bodies; fields not listed here can still be
 * supplied via the trailing advanced With JSON field.
 */
export const STEP_ACTION_SCHEMA: Record<string, StepField[]> = {
  'forge/run': [
    { key: 'image', label: 'Image', placeholder: 'ubuntu:22.04 (required)', required: true, catalog: 'image', config: true },
    { key: 'run', label: 'Run', placeholder: 'go test ./...', required: true, multiline: true, config: true },
    { key: 'runner_class', label: 'Runner', placeholder: 'runner class (optional, default standard)', config: true },
    { key: 'secret_refs', label: 'Git repo', placeholder: 'select a repo — clone creds injected as $GIT_CLONE_URL', catalog: 'repo', kind: 'repo', config: true },
    { key: 'env', label: 'Input variables', placeholder: 'REPO_URL= BRANCH=main', kind: 'env' },
    { key: 'output_env', label: 'Output variables', placeholder: 'BUILD_ID, VERSION', kind: 'list', output: true },
  ],
  'tickets/create': [
    { key: 'title', label: 'Title', placeholder: 'Build failed', required: true },
    { key: 'description', label: 'Description', placeholder: 'ticket body (optional)' },
    { key: 'priority', label: 'Priority', placeholder: 'low|medium|high|critical (optional)' },
    { key: 'status', label: 'Status', placeholder: 'open|in_progress|resolved|closed (optional)' },
  ],
  'tickets/update': [
    { key: 'id', label: 'Ticket ID', placeholder: 'ticket_id (required)', required: true },
    { key: 'title', label: 'Title', placeholder: 'ticket title (required)', required: true },
    { key: 'status', label: 'Status', placeholder: 'open|in_progress|resolved|closed (optional)' },
    { key: 'priority', label: 'Priority', placeholder: 'low|medium|high|critical (optional)' },
    { key: 'description', label: 'Description', placeholder: 'ticket body (optional)' },
  ],
  'tickets/delete': [
    { key: 'id', label: 'Ticket ID', placeholder: 'ticket_id (required)', required: true },
  ],
  'argo/sync': [
    { key: 'name', label: 'App', placeholder: 'argo app name (required)', required: true },
    { key: 'revision', label: 'Revision', placeholder: 'HEAD (optional)' },
    { key: 'outpost_id', label: 'Outpost ID', placeholder: 'outpost_id (optional)' },
  ],
  'chaos/run-experiment': [
    { key: 'outpost_id', label: 'Outpost ID', placeholder: 'outpost_id (required)', required: true },
    { key: 'experiment_type', label: 'Type', placeholder: 'pod-delete|pod-network-latency (required)', required: true },
    { key: 'target_app_ns', label: 'Namespace', placeholder: 'k8s namespace (required)', required: true },
    { key: 'target_app_label', label: 'Selector', placeholder: 'app=foo label selector (required)', required: true },
    { key: 'target_app_kind', label: 'Kind', placeholder: 'deployment (optional)' },
    { key: 'params', label: 'Params', placeholder: 'KEY=VALUE tuning (optional)', kind: 'env' },
  ],
  'blueprints/backend': [
    { key: 'workspace', label: 'Workspace', placeholder: 'workspace name (optional)' },
    { key: 'ttl_secs', label: 'TTL secs', placeholder: '14400 (optional)', kind: 'int' },
  ],
  // Built-in manual-approval gate: pauses the run until approved/rejected.
  'approval': [
    { key: 'message', label: 'Message', placeholder: 'Approve deploy to prod? (shown to approvers)', multiline: true },
    { key: 'approvers', label: 'Approvers', placeholder: 'alice, bob (usernames; empty = anyone with permission)', kind: 'list' },
  ],
};

/**
 * Buckets an action for change-detection: a known action keys to itself, every
 * unknown/custom action keys to '' (the generic single-With-field form).
 */
export function schemaKey(action: string): string {
  return action in STEP_ACTION_SCHEMA ? action : '';
}

/**
 * Returns the input fields for an action. Known actions get their tailored set
 * plus a trailing optional advanced-With field for extra keys; unknown/empty
 * actions fall back to a single required With JSON field.
 */
export function schemaForAction(action: string): StepField[] {
  const fields = STEP_ACTION_SCHEMA[action];
  if (fields) {
    return [
      ...fields,
      { key: RAW_WITH_KEY, label: 'With (advanced)', placeholder: '{"extra":"json"} (optional)', kind: 'json', multiline: true },
    ];
  }
  return [
    { key: RAW_WITH_KEY, label: 'With', placeholder: '{"key":"value"} JSON (required)', required: true, kind: 'json', multiline: true },
  ];
}

/**
 * Inverse of buildStepWith: turns a stored `with` map back into the form's
 * prefixed string field values, so an existing step can be loaded into the
 * create/edit form. Typed fields are serialised to the text shape their input
 * expects (env → "K=V K2=V2", int/json/other non-strings → JSON, strings as-is);
 * any keys not covered by a typed field are collected into the advanced With
 * JSON field. Round-trips with buildStepWith for every action schema.
 */
export function formValsFromWith(action: string, withMap: Record<string, unknown>): Record<string, string> {
  const vals: Record<string, string> = {};
  const fields = schemaForAction(action);
  const typedKeys = new Set<string>();

  for (const f of fields) {
    if (f.key === RAW_WITH_KEY) continue;
    if (f.kind === 'repo') {
      // Reverse the git: secret_ref into the bare clone URL — but only claim
      // secret_refs (so it leaves the advanced With JSON) when GIT_CLONE_URL is a
      // git: ref AND its sole key. Any other entries (e.g. a secret: ref) are left
      // for the advanced field to round-trip untouched.
      const v = withMap[f.key];
      const sr = (v && typeof v === 'object' && !Array.isArray(v)) ? v as Record<string, unknown> : null;
      const ref = sr && typeof sr[GIT_CLONE_ENV] === 'string' ? sr[GIT_CLONE_ENV] as string : '';
      if (sr && ref.startsWith('git:') && Object.keys(sr).length === 1) {
        typedKeys.add(f.key);
        vals[WITH_KEY_PREFIX + f.key] = ref.slice('git:'.length);
      }
      continue;
    }
    typedKeys.add(f.key);
    if (!(f.key in withMap)) continue;
    const v = withMap[f.key];
    if (f.kind === 'env' && v && typeof v === 'object' && !Array.isArray(v)) {
      vals[WITH_KEY_PREFIX + f.key] = Object.entries(v as Record<string, unknown>)
        .map(([k, val]) => `${k}=${val}`).join(' ');
    } else if (f.kind === 'list' && Array.isArray(v)) {
      vals[WITH_KEY_PREFIX + f.key] = (v as unknown[]).map(String).join(', ');
    } else {
      vals[WITH_KEY_PREFIX + f.key] = typeof v === 'string' ? v : JSON.stringify(v);
    }
  }

  // Keys the typed fields didn't claim go into the advanced With JSON field, the
  // same escape hatch buildStepWith reads them back from.
  const leftover: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(withMap)) {
    if (!typedKeys.has(k)) leftover[k] = v;
  }
  if (fields.some(f => f.key === RAW_WITH_KEY) && Object.keys(leftover).length > 0) {
    vals[WITH_KEY_PREFIX + RAW_WITH_KEY] = JSON.stringify(leftover, null, 2);
  }
  return vals;
}

/** Splits a whitespace-separated KEY=VALUE token string into a map. */
function parseEnvTokens(s: string, label: string): Record<string, string> {
  const env: Record<string, string> = {};
  for (const tok of s.split(/\s+/).filter(Boolean)) {
    const eq = tok.indexOf('=');
    if (eq <= 0) throw new Error(`invalid ${label} "${tok}": expected KEY=VALUE`);
    env[tok.slice(0, eq)] = tok.slice(eq + 1);
  }
  return env;
}

/**
 * Assembles the with map from an action's schema field values, validating
 * required fields. `valueOf` reads a form field by its prefixed key. Typed
 * fields are applied first; the advanced With JSON only fills keys the typed
 * fields didn't set, so it's a pure escape hatch for extra/uncommon keys.
 * Throws an Error with a user-facing message on any validation problem.
 */
export function buildStepWith(action: string, valueOf: (key: string) => string): Record<string, unknown> {
  const withMap: Record<string, unknown> = {};
  let base: Record<string, unknown> | undefined; // deferred from the advanced With JSON

  for (const f of schemaForAction(action)) {
    const raw = (valueOf(WITH_KEY_PREFIX + f.key) || '').trim();

    if (f.kind === 'json') {
      if (raw === '') {
        if (f.required) throw new Error(`${action} requires a With JSON object`);
        continue;
      }
      try {
        const parsed = JSON.parse(raw);
        if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) {
          throw new Error('With must be a JSON object');
        }
        base = parsed as Record<string, unknown>;
      } catch (e) {
        throw new Error(`With is not valid JSON: ${(e as Error).message}`);
      }
      continue;
    }

    if (raw === '') {
      if (f.required) throw new Error(`${f.label} is required`);
      continue;
    }

    switch (f.kind) {
      case 'int': {
        const n = Number(raw);
        if (!Number.isInteger(n) || n < 0) throw new Error(`${f.label} must be a non-negative integer`);
        withMap[f.key] = n;
        break;
      }
      case 'env':
        withMap[f.key] = parseEnvTokens(raw, f.label);
        break;
      case 'list':
        withMap[f.key] = raw.split(',').map(s => s.trim()).filter(Boolean);
        break;
      case 'repo':
        // Wrap the chosen clone URL as a forge git: secret_ref under GIT_CLONE_ENV.
        // Owns secret_refs when set; left empty, an advanced-With secret_refs flows.
        withMap.secret_refs = { [GIT_CLONE_ENV]: `git:${raw}` };
        break;
      default: // text
        withMap[f.key] = raw;
    }
  }

  // Overlay extra keys from the advanced With JSON without clobbering typed fields.
  if (base) {
    for (const [k, v] of Object.entries(base)) {
      if (!(k in withMap)) withMap[k] = v;
    }
  }
  return withMap;
}

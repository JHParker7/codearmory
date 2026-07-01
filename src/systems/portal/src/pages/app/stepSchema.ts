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
 * `list` splits a comma-separated string into a string array;
 * `volumeAttach` turns a compact "name[:/mount]" spec into a shared-volume attach. */
export type StepFieldKind = 'text' | 'int' | 'env' | 'json' | 'list' | 'volumeAttach';

/**
 * A shared workspace volume is scoped to the run: the workflows worker substitutes
 * ${run_id} at dispatch, so the create-volume step and every attach agree on one
 * per-run volume. DEFAULT_MOUNT_PATH is where an attach lands when no path is given.
 */
export const RUN_ID_REF = '${run_id}';
export const DEFAULT_MOUNT_PATH = '/workspace';
export const DEFAULT_VOLUME_NAME = 'workspace';

/** Env var (set via a secret_ref) a build reads its registry Docker config.json from;
 * the build-image form wires the chosen org secret to it. */
export const REGISTRY_AUTH_ENV = 'REGISTRY_AUTH';

/** The flat build-image form keys that nest under the `build` object of forge's
 * /executions body (volumes and runner_class stay top-level). */
const BUILD_IMAGE_FIELDS = ['destinations', 'dockerfile', 'context', 'build_args', 'target'];

/**
 * Turns a compact "name[:/mount]" attach spec into forge's `volumes` array: one
 * shared volume, scoped to the run (${run_id}), made the working directory. Returns
 * undefined for a blank spec so the key is dropped.
 */
export function volumeAttachWith(raw: string): Array<Record<string, unknown>> | undefined {
  const spec = raw.trim();
  if (spec === '') return undefined;
  const colon = spec.indexOf(':');
  const name = (colon >= 0 ? spec.slice(0, colon) : spec).trim() || DEFAULT_VOLUME_NAME;
  const mount = (colon >= 0 ? spec.slice(colon + 1) : '').trim() || DEFAULT_MOUNT_PATH;
  return [{ workflow_id: RUN_ID_REF, name, mount_path: mount, workdir: true }];
}

/**
 * Reverses volumeAttachWith for the edit form: renders the first attached volume back
 * to "name" or "name:/mount". Returns '' when the value isn't a single-volume attach,
 * leaving it to the advanced With field.
 */
export function volumeAttachFromWith(value: unknown): string {
  if (!Array.isArray(value) || value.length !== 1) return '';
  const m = value[0] as Record<string, unknown>;
  const name = typeof m?.name === 'string' ? m.name : '';
  if (name === '') return '';
  const mount = typeof m?.mount_path === 'string' ? m.mount_path : '';
  return mount === '' || mount === DEFAULT_MOUNT_PATH ? name : `${name}:${mount}`;
}

/**
 * Names a catalog that backs a field with a picker instead of free text:
 * `image` (the forge image allowlist). The field degrades to a plain text input
 * when its catalog is empty/unavailable.
 */
export type StepFieldCatalog = 'image';

/**
 * Env var the per-step git repo is injected as. Picking a repo for a forge step in
 * the pipeline builder sets that step-occurrence's
 * `with.secret_refs = { GIT_CLONE_URL: "git:<clone-url-or-${inputs.X}>" }`; forge
 * resolves the ref at dispatch so the runner clones/pulls/pushes via `$GIT_CLONE_URL`.
 * The repo is per-occurrence (chosen in the builder), not baked into the shared step,
 * so the same step (e.g. "unit tests") runs against whatever repo each pipeline names.
 */
export const GIT_CLONE_ENV = 'GIT_CLONE_URL';

/**
 * Whether an action's runner can clone a per-step git repo. Only forge steps consume
 * the `git:` secret_ref, so the repo picker shows only for them in the builder.
 */
export function actionSupportsGitRepo(action: string): boolean {
  return action.startsWith('forge/');
}

/**
 * Reads the bare repo reference (a clone URL or a `${inputs.X}` template) back out of
 * a step's `with.secret_refs.GIT_CLONE_URL`, stripping the `git:` scheme. Returns ''
 * when there is no git: ref, so the picker shows empty rather than a malformed value.
 */
export function gitRepoFromWith(withMap: Record<string, unknown>): string {
  const sr = withMap.secret_refs;
  if (!sr || typeof sr !== 'object' || Array.isArray(sr)) return '';
  const ref = (sr as Record<string, unknown>)[GIT_CLONE_ENV];
  return typeof ref === 'string' && ref.startsWith('git:') ? ref.slice('git:'.length) : '';
}

/**
 * Returns the secret_refs map with the git repo set to `repo` (or GIT_CLONE_URL
 * removed when `repo` is blank), preserving any other secret_refs entries. Returns
 * undefined when nothing remains, so callers can drop the key entirely.
 */
export function withGitRepo(secretRefs: unknown, repo: string): Record<string, unknown> | undefined {
  const sr: Record<string, unknown> = (secretRefs && typeof secretRefs === 'object' && !Array.isArray(secretRefs))
    ? { ...(secretRefs as Record<string, unknown>) } : {};
  const v = repo.trim();
  if (v === '') delete sr[GIT_CLONE_ENV];
  else sr[GIT_CLONE_ENV] = `git:${v}`;
  return Object.keys(sr).length > 0 ? sr : undefined;
}

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
    { key: 'volumes', label: 'Attach volume', placeholder: 'workspace or workspace:/src (optional)', kind: 'volumeAttach', config: true },
    { key: 'env', label: 'Input variables', placeholder: 'REPO_URL= BRANCH=main', kind: 'env' },
    { key: 'output_env', label: 'Output variables', placeholder: 'BUILD_ID, VERSION', kind: 'list', output: true },
  ],
  'forge/create-volume': [
    { key: 'name', label: 'Volume name', placeholder: 'workspace (default)', config: true },
    { key: 'size_mb', label: 'Size MB', placeholder: '1024 (optional)', kind: 'int', config: true },
    { key: 'medium', label: 'Medium', placeholder: 'memory (default) | disk', config: true },
    { key: 'mount_path', label: 'Mount path', placeholder: '/workspace (default)', config: true },
  ],
  'forge/build-image': [
    { key: 'destinations', label: 'Push to', placeholder: 'reg.io/acme/app:1.0, reg.io/acme/app:latest (required)', required: true, kind: 'list', config: true },
    { key: 'dockerfile', label: 'Dockerfile', placeholder: 'Dockerfile (default, relative to context)', config: true },
    { key: 'context', label: 'Context', placeholder: '/workspace (default)', config: true },
    { key: 'build_args', label: 'Build args', placeholder: 'VERSION=1.0 COMMIT=abc', kind: 'env', config: true },
    { key: 'target', label: 'Target stage', placeholder: 'multi-stage target (optional)', config: true },
    { key: 'registry_secret', label: 'Registry secret', placeholder: 'org secret holding a docker config.json (to push)', config: true },
    { key: 'volumes', label: 'Source volume', placeholder: 'workspace or workspace:/src (attach the checkout)', kind: 'volumeAttach', config: true },
    { key: 'runner_class', label: 'Build runner', placeholder: 'privileged kata/gvisor class (required)', required: true, config: true },
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
  // Flatten nested action shapes (build-image's `build` object + registry secret_ref)
  // back to the flat form fields the schema names.
  const wm = flattenStepWith(action, withMap);

  for (const f of fields) {
    if (f.key === RAW_WITH_KEY) continue;
    typedKeys.add(f.key);
    if (!(f.key in wm)) continue;
    const v = wm[f.key];
    if (f.kind === 'env' && v && typeof v === 'object' && !Array.isArray(v)) {
      vals[WITH_KEY_PREFIX + f.key] = Object.entries(v as Record<string, unknown>)
        .map(([k, val]) => `${k}=${val}`).join(' ');
    } else if (f.kind === 'list' && Array.isArray(v)) {
      vals[WITH_KEY_PREFIX + f.key] = (v as unknown[]).map(String).join(', ');
    } else if (f.kind === 'volumeAttach') {
      vals[WITH_KEY_PREFIX + f.key] = volumeAttachFromWith(v);
    } else {
      vals[WITH_KEY_PREFIX + f.key] = typeof v === 'string' ? v : JSON.stringify(v);
    }
  }

  // A create-volume step's workflow_id defaults to ${run_id} and has no field of its
  // own; don't spill that default into the advanced With JSON on edit.
  if (action === 'forge/create-volume' && wm.workflow_id === RUN_ID_REF) {
    typedKeys.add('workflow_id');
  }

  // Keys the typed fields didn't claim go into the advanced With JSON field, the
  // same escape hatch buildStepWith reads them back from.
  const leftover: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(wm)) {
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
      case 'volumeAttach': {
        const attach = volumeAttachWith(raw);
        if (attach) withMap[f.key] = attach;
        break;
      }
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

  // A create-volume step is always scoped to the run: default its workflow_id to
  // ${run_id} so the user never types it (an explicit value still wins).
  if (action === 'forge/create-volume' && !('workflow_id' in withMap)) {
    withMap.workflow_id = RUN_ID_REF;
  }
  if (action === 'forge/build-image') nestForgeBuild(withMap);
  return withMap;
}

/**
 * Restructures the flat build-image form fields into forge's nested request: the
 * build.* keys under a `build` object, and the chosen registry secret into
 * secret_refs[REGISTRY_AUTH]. volumes and runner_class stay top-level.
 */
function nestForgeBuild(withMap: Record<string, unknown>): void {
  const build: Record<string, unknown> = {};
  for (const k of BUILD_IMAGE_FIELDS) {
    if (k in withMap) { build[k] = withMap[k]; delete withMap[k]; }
  }
  if (Object.keys(build).length > 0) withMap.build = build;

  const rs = withMap.registry_secret;
  if (typeof rs === 'string') {
    delete withMap.registry_secret;
    if (rs !== '') {
      const sr: Record<string, unknown> = (withMap.secret_refs && typeof withMap.secret_refs === 'object' && !Array.isArray(withMap.secret_refs))
        ? { ...(withMap.secret_refs as Record<string, unknown>) } : {};
      sr[REGISTRY_AUTH_ENV] = `secret:${rs}`;
      withMap.secret_refs = sr;
    }
  }
}

/**
 * Inverse of nestForgeBuild: returns a shallow copy of `withMap` with build-image's
 * `build` object and registry secret_ref lifted back to the flat form fields, so the
 * edit form pre-populates and round-trips. Other actions pass through unchanged.
 */
export function flattenStepWith(action: string, withMap: Record<string, unknown>): Record<string, unknown> {
  const wm: Record<string, unknown> = { ...withMap };
  if (action !== 'forge/build-image') return wm;

  if (wm.build && typeof wm.build === 'object' && !Array.isArray(wm.build)) {
    Object.assign(wm, wm.build as Record<string, unknown>);
    delete wm.build;
  }
  const sr = wm.secret_refs;
  if (sr && typeof sr === 'object' && !Array.isArray(sr)) {
    const refs = sr as Record<string, unknown>;
    const ref = refs[REGISTRY_AUTH_ENV];
    if (typeof ref === 'string') {
      wm.registry_secret = ref.startsWith('secret:') ? ref.slice('secret:'.length) : ref;
      const rest = Object.fromEntries(Object.entries(refs).filter(([k]) => k !== REGISTRY_AUTH_ENV));
      if (Object.keys(rest).length > 0) wm.secret_refs = rest; else delete wm.secret_refs;
    }
  }
  return wm;
}

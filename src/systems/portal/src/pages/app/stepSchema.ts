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

/** The flat git-clone form keys that nest under the `checkout` object of forge's
 * /executions body (image and volumes stay top-level). */
const CHECKOUT_FIELDS = ['path', 'ref', 'depth'];

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
 * `image` (the forge image allowlist) or `pipeline` (the workflows list, for the
 * workflows/trigger target). The field degrades to a plain text input when its
 * catalog is empty/unavailable.
 */
export type StepFieldCatalog = 'image' | 'pipeline';

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
 * Whether an action clones a per-step git repo — i.e. whether the builder shows a
 * repo picker on its block. Only two forge actions actually consume the repo:
 * `forge/git-clone` clones it into a shared volume, and `forge/run` can optionally
 * check it out into the working dir. `forge/create-volume` (only provisions a PVC)
 * and `forge/build-image` (builds from an already-populated volume and authenticates
 * with a registry secret, not GIT_CLONE_URL) do NOT clone, so a repo picker on them
 * just misleads — attaching a repo there does nothing. Keep this list tight.
 */
export function actionSupportsGitRepo(action: string): boolean {
  return action === 'forge/run' || action === 'forge/git-clone';
}

/**
 * Whether an action ATTACHES a shared workspace volume (so the block shows a volume
 * selector wired to an upstream forge/create-volume step). forge/run mounts one to
 * share a checkout/artifacts, forge/git-clone clones into one, and forge/build-image
 * builds from one. forge/create-volume itself creates rather than attaches.
 */
export function actionAttachesVolume(action: string): boolean {
  return action === 'forge/run' || action === 'forge/build-image' || action === 'forge/git-clone';
}

/** Whether an action CREATES a shared workspace volume — the source of the names the
 * attach selector offers to downstream steps. Only forge/create-volume does. */
export function actionCreatesVolume(action: string): boolean {
  return action === 'forge/create-volume';
}

/** The volume name a forge/create-volume step provisions: its `with.name`, or the
 * default "workspace" when unset (matching the create-volume schema default). */
export function createdVolumeName(withMap: Record<string, unknown>): string {
  const n = withMap?.name;
  return typeof n === 'string' && n.trim() !== '' ? n.trim() : DEFAULT_VOLUME_NAME;
}

/**
 * Splits a step's `with` map into its DEFINITION part (config + input defaults) and
 * its PER-OCCURRENCE pipeline part — the keys the action schema marks `pipeline`
 * (e.g. the attached `volumes`), whose value depends on other steps in the pipeline.
 *
 * Inline steps mirror the def/override split of a stored-step reference: the
 * definition lives in `inline.with` (edited by the step-definition form) and the
 * pipeline part lives in the block override (`block.with`, edited by the block's
 * inputs editor). Keeping pipeline fields OUT of the definition layer is essential —
 * the step-definition form rebuilds `inline.with` via buildStepWith, which drops
 * pipeline fields, so a volume left in the definition would be silently removed the
 * next time the inline step's definition is edited.
 */
export function splitPipelineWith(action: string, withMap: Record<string, unknown>): { def: Record<string, unknown>; pipeline: Record<string, unknown> } {
  const pipelineKeys = new Set(schemaForAction(action).filter(f => f.pipeline).map(f => f.key));
  const def: Record<string, unknown> = {};
  const pipeline: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(withMap)) {
    if (pipelineKeys.has(k)) pipeline[k] = v; else def[k] = v;
  }
  return { def, pipeline };
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
 * Reasons a step block's configuration is still incomplete (a required field is
 * unset), so the pipeline builder can flag it — a red border + hint — before the
 * user saves the workflow. An empty array means the block is ready to run.
 *
 * `eff` is the block's EFFECTIVE `with` (the step definition merged with the
 * per-occurrence override, in forge's nested body shape); `repo` is the block's
 * per-occurrence git repo (secret_refs.GIT_CLONE_URL), which the schema field list
 * does not itself cover. The forge actions are checked against their real nested
 * shape; every other action falls back to its schema's `required` flags.
 */
export function stepConfigIssues(action: string, eff: Record<string, unknown>, repo: string): string[] {
  const issues: string[] = [];
  const str = (v: unknown) => typeof v === 'string' && v.trim() !== '';
  const arr = (v: unknown) => Array.isArray(v) && v.length > 0;
  switch (action) {
    case 'forge/git-clone':
      if (!arr(eff.volumes)) issues.push('attach a volume to clone into');
      if (!str(repo)) issues.push('select a git repo');
      return issues;
    case 'forge/run':
      if (!str(eff.image)) issues.push('set an image');
      if (!str(eff.run)) issues.push('set a command to run');
      return issues;
    case 'forge/build-image': {
      const build = (eff.build && typeof eff.build === 'object' && !Array.isArray(eff.build)) ? eff.build as Record<string, unknown> : {};
      if (!arr(build.destinations) && !str(build.destinations)) issues.push('set a push destination');
      if (!str(eff.runner_class)) issues.push('set a privileged build runner');
      return issues;
    }
    case 'forge/create-volume':
      return issues; // name defaults to "workspace"; nothing is strictly required
  }
  // Generic fallback: every required, non-output field must have a non-empty value
  // at the top level of the built `with`.
  for (const f of schemaForAction(action)) {
    if (!f.required || f.output) continue;
    const v = eff[f.key];
    if (!str(v) && !arr(v) && typeof v !== 'number' && typeof v !== 'boolean') issues.push(`set ${f.label.toLowerCase()}`);
  }
  return issues;
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
  /** Marks a field configured PER-OCCURRENCE in the pipeline block (not the step
   * definition) because its value depends on other steps in that pipeline — e.g. the
   * workspace volume a forge step attaches to, which is created by an upstream
   * forge/create-volume step. Pipeline fields are hidden from the step-definition
   * form and surfaced (as a selector where possible) in the block's inputs editor. */
  pipeline?: boolean;
  /** A closed set of allowed values — rendered as a dropdown instead of free text.
   * A non-required field also offers a blank "(default)" choice. */
  options?: string[];
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
    { key: 'volumes', label: 'Attach volume', placeholder: 'workspace or workspace:/src (optional)', kind: 'volumeAttach', pipeline: true },
    { key: 'env', label: 'Input variables', placeholder: 'REPO_URL= BRANCH=main', kind: 'env' },
    { key: 'output_env', label: 'Output variables', placeholder: 'BUILD_ID, VERSION', kind: 'list', output: true },
  ],
  'forge/create-volume': [
    { key: 'name', label: 'Volume name', placeholder: 'workspace (default)', config: true },
    { key: 'size_mb', label: 'Size MB', placeholder: '1024 (optional)', kind: 'int', config: true },
    { key: 'medium', label: 'Medium', placeholder: 'memory (default)', options: ['memory', 'disk'], config: true },
    { key: 'mount_path', label: 'Mount path', placeholder: '/workspace (default)', config: true },
  ],
  'forge/build-image': [
    { key: 'destinations', label: 'Push to', placeholder: 'reg.io/acme/app:1.0, reg.io/acme/app:latest (required)', required: true, kind: 'list', config: true },
    { key: 'dockerfile', label: 'Dockerfile', placeholder: 'Dockerfile (default, relative to context)', config: true },
    { key: 'context', label: 'Context', placeholder: '/workspace (default)', config: true },
    { key: 'build_args', label: 'Build args', placeholder: 'VERSION=1.0 COMMIT=abc', kind: 'env', config: true },
    { key: 'target', label: 'Target stage', placeholder: 'multi-stage target (optional)', config: true },
    { key: 'registry_secret', label: 'Registry secret', placeholder: 'org secret holding a docker config.json (to push)', config: true },
    { key: 'volumes', label: 'Source volume', placeholder: 'workspace or workspace:/src (attach the checkout)', kind: 'volumeAttach', pipeline: true },
    { key: 'runner_class', label: 'Build runner', placeholder: 'privileged kata/gvisor class (required)', required: true, config: true },
  ],
  'forge/git-clone': [
    { key: 'volumes', label: 'Clone into volume', placeholder: 'workspace or workspace:/src (create with forge/create-volume)', required: true, kind: 'volumeAttach', pipeline: true },
    { key: 'path', label: 'Clone dir', placeholder: 'volume root (default; a subdir relative to the volume)', config: true },
    // Branch/tag (checkout.ref) is chosen per-occurrence on the block via a BranchSelect
    // (like forge/run's checkout), since the repo it enumerates is also per-occurrence —
    // so it is intentionally NOT a step-definition field here.
    { key: 'depth', label: 'Depth', placeholder: '1 (default shallow; 0 = full clone)', kind: 'int', config: true },
    { key: 'run', label: 'Post-clone command', placeholder: 'true (optional; runs in the checkout after clone)', config: true },
  ],
  // Runs another pipeline as a sub-run. `pipeline` names the target (name or id);
  // `inputs` feeds its declared inputs (wireable — an upstream ${steps.X.output.KEY}
  // or a ${matrix.item}). The step's output is the sub-run's resolved outputs map, so
  // downstream steps read ${steps.<triggerStep>.output.<KEY>}.
  'workflows/trigger': [
    { key: 'pipeline', label: 'Pipeline', placeholder: 'target pipeline name or id', required: true, config: true, catalog: 'pipeline' },
    { key: 'inputs', label: 'Inputs', placeholder: 'KEY=VALUE (wire to ${steps.X.output} or ${matrix.item})', kind: 'env' },
  ],
  'tickets/create': [
    { key: 'title', label: 'Title', placeholder: 'Build failed', required: true },
    { key: 'description', label: 'Description', placeholder: 'ticket body (optional)', multiline: true },
    { key: 'priority', label: 'Priority', placeholder: '(optional)', options: ['low', 'medium', 'high', 'critical'] },
    { key: 'status', label: 'Status', placeholder: '(optional)', options: ['open', 'in_progress', 'resolved', 'closed'] },
  ],
  'tickets/update': [
    { key: 'id', label: 'Ticket ID', placeholder: 'ticket_id (required)', required: true },
    { key: 'title', label: 'Title', placeholder: 'ticket title (required)', required: true },
    { key: 'status', label: 'Status', placeholder: '(optional)', options: ['open', 'in_progress', 'resolved', 'closed'] },
    { key: 'priority', label: 'Priority', placeholder: '(optional)', options: ['low', 'medium', 'high', 'critical'] },
    { key: 'description', label: 'Description', placeholder: 'ticket body (optional)', multiline: true },
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
    { key: 'target_app_kind', label: 'Kind', placeholder: 'deployment (default)', options: ['deployment', 'statefulset', 'daemonset', 'deploymentconfig', 'rollout'] },
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
    // Pipeline fields (e.g. the attached volume) are configured per-occurrence on the
    // block, not on the step definition, so the def form never renders them and they
    // are not built into the step's `with` here — the block override carries them.
    if (f.pipeline) continue;
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
  if (action === 'forge/git-clone') nestForgeCheckout(withMap);
  return withMap;
}

/**
 * Restructures the flat git-clone form fields into forge's request: the path/ref/depth
 * keys under a `checkout` object (always present so forge runs the clone prologue), and
 * a no-op `run` default so the execution is a valid shell command the checkout weaves
 * into. The repo itself is wired per-occurrence as secret_refs.GIT_CLONE_URL. image and
 * volumes stay top-level.
 */
function nestForgeCheckout(withMap: Record<string, unknown>): void {
  const checkout: Record<string, unknown> = {};
  for (const k of CHECKOUT_FIELDS) {
    if (k in withMap) { checkout[k] = withMap[k]; delete withMap[k]; }
  }
  withMap.checkout = checkout;
  if (!('run' in withMap)) withMap.run = 'true';
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

  if (action === 'forge/git-clone') {
    if (wm.checkout && typeof wm.checkout === 'object' && !Array.isArray(wm.checkout)) {
      Object.assign(wm, wm.checkout as Record<string, unknown>);
      delete wm.checkout;
    }
    return wm;
  }
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

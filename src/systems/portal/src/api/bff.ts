/**
 * Typed client for the portal BFF. Every function is a thin wrapper over {@link req}
 * that issues an HTTP call against `/api/*` (proxied to conductor) and returns the
 * decoded JSON. Grouped by backing service (gatekeeper, workflows, forge, …); the
 * interfaces mirror each service's response shape. The functions are intentionally
 * terse — the section banner and the verb+path in each call are the documentation.
 */

const BASE = '/api';

/**
 * Core fetch helper: issues `method BASE+path` with optional bearer auth and JSON
 * body, returns decoded JSON (or undefined for 204), and throws an Error with a
 * `.status` property on any non-2xx so callers can branch on the HTTP code.
 */
async function req<T>(method: string, path: string, token?: string, body?: unknown): Promise<T> {
  const res = await fetch(BASE + path, {
    method,
    headers: {
      'Content-Type': 'application/json',
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
    },
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  if (!res.ok) {
    const text = await res.text().catch(() => '');
    throw Object.assign(new Error(text || res.statusText), { status: res.status });
  }
  if (res.status === 204) return undefined as T;
  return res.json() as Promise<T>;
}

/**
 * Append a `project=<label>` view filter to a path's query string (choosing `?`
 * or `&` by what the path already carries), or return it unchanged when no
 * project is active. Mirrors the CLI's appendProjectParam.
 */
function withProject(path: string, project?: string): string {
  if (!project) return path;
  return `${path}${path.includes('?') ? '&' : '?'}project=${encodeURIComponent(project)}`;
}

// ── Auth ──────────────────────────────────────────────────────────────────────

export interface SignupPayload {
  email: string;
  username: string;
  password: string;
  firstname?: string;
  lastname?: string;
}

export interface SignupResult {
  user_id: string;
  email: string;
  username: string;
  firstname?: string;
  lastname?: string;
}

export interface LoginResult {
  token: string;
}

export function signup(payload: SignupPayload) {
  return req<SignupResult>('POST', '/gatekeeper/signup', undefined, payload);
}

export function login(email: string, password: string) {
  return req<LoginResult>('POST', '/gatekeeper/login', undefined, { email, password });
}

// ── First-run setup ─────────────────────────────────────────────────────────
// Public, unauthenticated. Reports whether the instance has been bootstrapped
// (has at least one user). The portal routes to the first-run setup page when
// initialized is false.

export interface SetupStatus {
  initialized: boolean;
}

/**
 * Fetch first-run setup status, time-bounded so a hung upstream can't trap the
 * SetupGate on the loading screen forever (the app's entire render is gated on
 * this resolving). On timeout the fetch aborts and rejects, which checkSetup
 * treats as a failed attempt.
 */
export async function getSetupStatus(timeoutMs = 4000): Promise<SetupStatus> {
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), timeoutMs);
  try {
    const res = await fetch(BASE + '/gatekeeper/setup/status', {
      method: 'GET',
      headers: { 'Content-Type': 'application/json' },
      signal: ctrl.signal,
    });
    if (!res.ok) throw Object.assign(new Error(res.statusText), { status: res.status });
    return (await res.json()) as SetupStatus;
  } finally {
    clearTimeout(timer);
  }
}

// ── Users ─────────────────────────────────────────────────────────────────────

export interface User {
  user_id: string;
  email: string;
  username: string;
  firstname?: string;
  lastname?: string;
  org_id?: string | null;
  team_id?: string | null;
  role_id?: string | null;
  created_at: string;
  updated_at: string;
  active: boolean;
}

export function listUsers(token: string) {
  return req<User[]>('GET', '/gatekeeper/users', token);
}

export function getUser(token: string, id: string) {
  return req<User>('GET', `/gatekeeper/users/${id}`, token);
}

export function updateUser(
  token: string,
  id: string,
  payload: { email: string; username: string; password?: string; firstname?: string; lastname?: string },
) {
  return req<User>('PUT', `/gatekeeper/users/${id}`, token, payload);
}

export function deleteUser(token: string, id: string) {
  return req<void>('DELETE', `/gatekeeper/users/${id}`, token);
}

// ── Orgs ──────────────────────────────────────────────────────────────────────

export interface Org {
  org_id: string;
  org_name: string;
  owner_id: string;
  created_at: string;
  updated_at: string;
  active: boolean;
}

export function createOrg(token: string, org_name: string) {
  return req<Org>('POST', '/gatekeeper/orgs', token, { org_name });
}

export function getOrg(token: string, id: string) {
  return req<Org>('GET', `/gatekeeper/orgs/${id}`, token);
}

// ── Teams ─────────────────────────────────────────────────────────────────────

export interface Team {
  team_id: string;
  team_name: string;
  role_id?: string | null;
  owner_id: string;
  org_id?: string | null;
  created_at: string;
  updated_at: string;
  active: boolean;
}

export function getTeam(token: string, id: string) {
  return req<Team>('GET', `/gatekeeper/teams/${id}`, token);
}

// ── Gatekeeper — roles, permissions, secrets ─────────────────────────────────

export interface Role {
  role_id: string;
  name?: string;
  permissions_ids: string[];
  org_id?: string | null;
  owner_id: string;
  created_at: string;
  updated_at: string;
  active: boolean;
}

export interface Permission {
  permissions_id: string;
  name: string;
  service: string;
  actions: string[];
  resources: string[];
  owner_id: string;
  org_id?: string | null;
  created_at: string;
  updated_at: string;
  active: boolean;
}

export interface Secret {
  secret_id: string;
  org_id: string;
  name: string;
  created_by: string;
  created_at: string;
  updated_at: string;
}

export function listRoles(token: string) {
  return req<Role[]>('GET', '/gatekeeper/roles', token);
}

export function listPermissions(token: string) {
  return req<Permission[]>('GET', '/gatekeeper/permissions', token);
}

export function listSecrets(token: string) {
  return req<Secret[]>('GET', '/gatekeeper/secrets', token);
}

export function createSecret(token: string, payload: { name: string; value: string }) {
  return req<Secret>('POST', '/gatekeeper/secrets', token, payload);
}

export function updateSecret(token: string, id: string, value: string) {
  return req<Secret>('PUT', `/gatekeeper/secrets/${id}`, token, { value });
}

export function deleteSecret(token: string, id: string) {
  return req<void>('DELETE', `/gatekeeper/secrets/${id}`, token);
}

// ── Secret provider (per-org secrets backend) ─────────────────────────────────
// Provider is one of: builtin, doppler, vault, aws_sm. The config object holds
// provider-specific credentials (e.g. { address } for vault) and is write-only —
// it is never returned by the API.

export type SecretProviderName = 'builtin' | 'doppler' | 'vault' | 'aws_sm';

export interface SecretProvider {
  org_id: string;
  provider: SecretProviderName | string;
  created_at: string;
  updated_at: string;
}

/** Fetch an org's secret-provider config, returning null when no provider row exists (org implicitly uses builtin). */
export async function getSecretProvider(token: string, orgId: string): Promise<SecretProvider | null> {
  try {
    return await req<SecretProvider>('GET', `/gatekeeper/orgs/${orgId}/secret-provider`, token);
  } catch (e) {
    if ((e as { status?: number }).status === 404) return null;
    throw e;
  }
}

export function setSecretProvider(
  token: string,
  orgId: string,
  provider: SecretProviderName | string,
  config?: Record<string, unknown>,
) {
  return req<SecretProvider>('PUT', `/gatekeeper/orgs/${orgId}/secret-provider`, token, { provider, config: config ?? null });
}

export function deleteSecretProvider(token: string, orgId: string) {
  return req<void>('DELETE', `/gatekeeper/orgs/${orgId}/secret-provider`, token);
}

// ── Workflows ─────────────────────────────────────────────────────────────────

/** Fans a step out into one execution per value in a list, binding
 * ${matrix.<var>} per execution. Mutually exclusive with parallel_group. */
export interface MatrixConfig {
  var: string;
  values?: string[];
  values_from?: string;
}

/** An inline manual-approval gate on a pipeline step ref — pauses the run with no
 * separate Step row. A ref carries either a step_id or an approval gate. */
export interface ApprovalGate {
  message?: string;
  approvers?: string[];
}

export interface WorkflowStepRef {
  step_id?: string;
  // Per-occurrence name override (so a reused step can have distinct names). On a
  // GET it is the effective name; on save send it only when it differs from the
  // step definition's name.
  name?: string;
  // Per-occurrence `with` overrides (input wiring). On a GET this is the effective
  // (merged) with; on save send only the keys that differ from the step definition.
  with?: Record<string, unknown> | null;
  parallel_group?: number | null;
  matrix?: MatrixConfig | null;
  approval?: ApprovalGate | null;
}

/** A run parameter a pipeline declares. `default` is applied when the trigger omits
 * the input; `required` makes the backend reject a trigger that leaves it unset. */
export interface WorkflowInputDef {
  name: string;
  default?: string;
  required?: boolean;
  description?: string;
}

/** A value a pipeline publishes on completion. `value` is a `${...}` template —
 * typically `${steps.STEP.output.KEY}` — resolved from step outputs at run end and
 * surfaced in WorkflowRun.outputs (and consumable by a parent workflows/trigger step). */
export interface WorkflowOutputDef {
  name: string;
  value: string;
}

export interface Workflow {
  workflow_id: string;
  name: string;
  description?: string | null;
  /** Free-text project (workspace) label this pipeline is tagged with — a view filter, not a permission. */
  project?: string;
  created_by: string;
  org_id?: string | null;
  active: boolean;
  steps: WorkflowStepRef[];
  /** Declared run parameters (defaults/required applied at trigger time). */
  inputs?: WorkflowInputDef[];
  /** Declared outputs published on completion (resolved into WorkflowRun.outputs). */
  outputs?: WorkflowOutputDef[];
  created_at: string;
  updated_at: string;
}

export interface WorkflowStepRun {
  step_run_id: string;
  run_id: string;
  step_index: number;
  step_name: string;
  status: string;
  output?: string | null;
  /** The step's execution log — the backing action's stdout (forge: the command's
   * stdout), captured on success for display. Distinct from `output`, which holds
   * the consumable captured outputs (output_env map). Absent on the failure path,
   * where stdout is folded into `output`. */
  logs?: string | null;
  started_at?: string | null;
  ended_at?: string | null;
}

export interface WorkflowRun {
  run_id: string;
  workflow_id: string;
  triggered_by: string;
  org_id?: string | null;
  status: string;
  current_step?: number | null;
  inputs?: Record<string, unknown> | null;
  /** Declared pipeline outputs resolved from step outputs at completion. */
  outputs?: Record<string, string> | null;
  step_runs?: WorkflowStepRun[];
  created_at: string;
  started_at?: string | null;
  ended_at?: string | null;
}

export function listWorkflows(token: string, project?: string) {
  return req<Workflow[]>('GET', withProject('/workflows/pipelines', project), token);
}

export function getWorkflow(token: string, id: string) {
  return req<Workflow>('GET', `/workflows/pipelines/${id}`, token);
}

export function deleteWorkflow(token: string, id: string) {
  return req<void>('DELETE', `/workflows/pipelines/${id}`, token);
}

export function listWorkflowRuns(token: string, workflowId: string) {
  return req<WorkflowRun[]>('GET', `/workflows/runs?workflow_id=${workflowId}`, token);
}

export function triggerWorkflow(token: string, workflowId: string, inputs?: Record<string, unknown>) {
  return req<WorkflowRun>('POST', `/workflows/pipelines/${workflowId}/runs`, token, inputs ? { inputs } : {});
}

export function listRuns(token: string) {
  return req<WorkflowRun[]>('GET', '/workflows/runs', token);
}

export function getRun(token: string, id: string) {
  return req<WorkflowRun>('GET', `/workflows/runs/${id}`, token);
}

export function cancelRun(token: string, id: string) {
  return req<void>('DELETE', `/workflows/runs/${id}`, token);
}

/** Approve a run paused on a manual-approval gate, resuming it. */
export function approveRun(token: string, id: string, comment?: string) {
  return req<WorkflowRun>('POST', `/workflows/runs/${id}/approve`, token, comment ? { comment } : {});
}

/** Reject a run paused on a manual-approval gate, failing it. */
export function rejectRun(token: string, id: string, comment?: string) {
  return req<WorkflowRun>('POST', `/workflows/runs/${id}/reject`, token, comment ? { comment } : {});
}

// ── Forge ─────────────────────────────────────────────────────────────────────

/**
 * actions/checkout-style clone config. When set on an execution, forge `git clone`s
 * the repo whose authenticated URL lives in `env` (default GIT_CLONE_URL, usually a
 * git:/gitea: secret_ref) into `path` and cd's into it before running the command.
 */
export interface CheckoutSpec {
  /** Env var holding the clone URL. Default GIT_CLONE_URL. */
  env?: string;
  /** Directory to clone into and cd into. Default: repo name derived from the ref, else "repo". */
  path?: string;
  /** Branch or tag to check out. Empty = the remote's default branch. */
  ref?: string;
  /** git clone --depth. Omit for a shallow depth-1 clone; 0 = full clone. */
  depth?: number;
}

export interface Execution {
  execution_id: string;
  user_id: string;
  image: string;
  command: string[];
  env?: Record<string, string> | null;
  timeout?: number | null;
  runner_class?: string | null;
  /** Credential references (target env var → "scheme:arg") resolved at dispatch, never the resolved values. */
  secret_refs?: Record<string, string> | null;
  /** actions/checkout-style clone config, if requested at submit. */
  checkout?: CheckoutSpec | null;
  /** Free-text project (workspace) label this execution is tagged with. */
  project?: string;
  status: string;
  exit_code?: number | null;
  stdout?: string | null;
  stderr?: string | null;
  created_at: string;
  started_at?: string | null;
  ended_at?: string | null;
}

export interface RunnerClass {
  name: string;
  memory_mb: number;
  cpu_millicores: number;
  pids_limit?: number | null;
  tmpfs_mb?: number | null;
  disk_gb?: number | null;
  /** Name of the RuntimeBackend this class runs on (resolve to its type for kata vs not). */
  backend?: string;
  enabled: boolean;
  /** Root + writable rootfs + privilege escalation; only meaningful on VM-isolated backends. */
  privileged?: boolean;
}

/** Admin runtime target. `type` is the runtime implementation: docker | kubernetes | kata | gvisor. */
export interface RuntimeBackend {
  name: string;
  type: string;
  enabled: boolean;
  /** Non-secret settings (e.g. `runtime_class` for kata/gvisor). Always an object from the API. */
  config?: Record<string, string>;
  /** Logical key → the NAME of an env var read via secret(); never the secret value. */
  secret_refs?: Record<string, string>;
  created_at?: string;
}

export function listExecutions(token: string, project?: string) {
  return req<Execution[]>('GET', withProject('/forge/executions', project), token);
}

export function getExecution(token: string, id: string) {
  return req<Execution>('GET', `/forge/executions/${id}`, token);
}

export function cancelExecution(token: string, id: string) {
  return req<void>('DELETE', `/forge/executions/${id}`, token);
}

export function listRunnerClasses(token: string) {
  return req<RunnerClass[]>('GET', '/forge/runner-classes', token);
}

export function listRuntimeBackends(token: string) {
  return req<RuntimeBackend[]>('GET', '/forge/runtime-backends', token);
}

/** The forge image allowlist (deduped, sorted). Executions may only use these. */
export function listForgeImages(token: string) {
  return req<string[]>('GET', '/forge/images', token);
}

// ── Tickets ───────────────────────────────────────────────────────────────────

export interface TicketComment {
  comment_id: string;
  ticket_id: string;
  author_id: string;
  body: string;
  created_at: string;
  updated_at: string;
}

export interface Ticket {
  ticket_id: string;
  title: string;
  description?: string | null;
  status: string;
  priority?: string | null;
  /** Free-text project (workspace) label this ticket is tagged with. */
  project?: string;
  /** The board this ticket belongs to. New tickets always have a board; null/absent only for legacy rows. */
  board_id?: string | null;
  created_by: string;
  org_id?: string | null;
  assignee_id?: string | null;
  workflow_id?: string | null;
  run_id?: string | null;
  forge_execution_id?: string | null;
  comments?: TicketComment[];
  created_at: string;
  updated_at: string;
}

export function listTickets(token: string, project?: string) {
  return req<Ticket[]>('GET', withProject('/tickets/tickets', project), token);
}

export function getTicket(token: string, id: string) {
  return req<Ticket>('GET', `/tickets/tickets/${id}`, token);
}

export function createTicket(token: string, payload: { title: string; description?: string; status?: string; priority?: string; project?: string; board_id?: string }) {
  return req<Ticket>('POST', '/tickets/tickets', token, payload);
}

// board_id: a string assigns the ticket to that board; "" re-homes it to the default board; omit to leave unchanged.
export function updateTicket(token: string, id: string, payload: Partial<{ title: string; description: string; status: string; priority: string; assignee_id: string; board_id: string }>) {
  return req<Ticket>('PUT', `/tickets/tickets/${id}`, token, payload);
}

export function deleteTicket(token: string, id: string) {
  return req<void>('DELETE', `/tickets/tickets/${id}`, token);
}

export function addComment(token: string, ticketId: string, body: string) {
  return req<TicketComment>('POST', `/tickets/tickets/${ticketId}/comments`, token, { body });
}

/** A configurable status/priority/timescale value — the kanban board's columns come from the `status` defs. */
export interface TicketFieldDef {
  field_def_id: string;
  kind: string;
  value: string;
  label: string;
  color?: string;
  position: number;
  /** Set on status defs that are owned by a specific board ("" = org/global). */
  board_id?: string;
}

// boardId scopes status columns to a single board; omit (or "") for the org/global set.
export function listTicketFieldDefs(token: string, kind: string, boardId?: string) {
  const q = boardId ? `&board_id=${encodeURIComponent(boardId)}` : '';
  return req<TicketFieldDef[]>('GET', `/tickets/field-defs?kind=${encodeURIComponent(kind)}${q}`, token);
}

export function createTicketFieldDef(token: string, payload: { kind: string; value: string; label: string; color?: string; position?: number; board_id?: string }) {
  return req<TicketFieldDef>('POST', '/tickets/field-defs', token, payload);
}

export function updateTicketFieldDef(token: string, id: string, payload: Partial<{ label: string; color: string; position: number }>) {
  return req<TicketFieldDef>('PUT', `/tickets/field-defs/${id}`, token, payload);
}

export function deleteTicketFieldDef(token: string, id: string) {
  return req<void>('DELETE', `/tickets/field-defs/${id}`, token);
}

/** A named kanban board — a first-class grouping of tickets owned by a user/org. */
export interface Board {
  board_id: string;
  name: string;
  description?: string;
  color?: string;
  position: number;
  created_by: string;
  org_id?: string | null;
  created_at: string;
  updated_at: string;
}

export function listBoards(token: string) {
  return req<Board[]>('GET', '/tickets/boards', token);
}

export function createBoard(token: string, payload: { name: string; description?: string; color?: string }) {
  return req<Board>('POST', '/tickets/boards', token, payload);
}

export function updateBoard(token: string, id: string, payload: Partial<{ name: string; description: string; color: string; position: number }>) {
  return req<Board>('PUT', `/tickets/boards/${id}`, token, payload);
}

// Deleting a board cascade-deletes its tickets, so the server requires the
// board's exact name echoed back as confirmation (?confirm=<name>).
export function deleteBoard(token: string, id: string, confirmName: string) {
  return req<void>('DELETE', `/tickets/boards/${id}?confirm=${encodeURIComponent(confirmName)}`, token);
}

// ── Hooks ─────────────────────────────────────────────────────────────────────

export interface PipelineRule {
  rule_id: string;
  name: string;
  repo: string;
  events: string[];
  ref_filter?: string | null;
  workflow_id: string;
  input_mapping?: Record<string, string> | null;
  created_by: string;
  org_id?: string | null;
  created_at: string;
  updated_at: string;
}

export interface HookTrigger {
  trigger_id: string;
  event_id: string;
  rule_id: string;
  workflow_id: string;
  run_id?: string | null;
  status: string;
  error?: string | null;
  created_at: string;
}

export interface HookEvent {
  event_id: string;
  repo: string;
  event_type: string;
  ref?: string | null;
  payload?: unknown;
  rules_matched: number;
  status: string;
  triggers?: HookTrigger[];
  created_at: string;
}

export function listRules(token: string) {
  return req<PipelineRule[]>('GET', '/hooks/rules', token);
}

export function getRule(token: string, id: string) {
  return req<PipelineRule>('GET', `/hooks/rules/${id}`, token);
}

export function createRule(token: string, payload: { name: string; repo: string; events: string[]; ref_filter?: string; workflow_id: string; input_mapping?: Record<string, string> }) {
  return req<PipelineRule>('POST', '/hooks/rules', token, payload);
}

export function updateRule(token: string, id: string, payload: Partial<{ name: string; repo: string; events: string[]; ref_filter: string; workflow_id: string; input_mapping: Record<string, string> }>) {
  return req<PipelineRule>('PUT', `/hooks/rules/${id}`, token, payload);
}

export function deleteRule(token: string, id: string) {
  return req<void>('DELETE', `/hooks/rules/${id}`, token);
}

export function listHookEvents(token: string) {
  return req<HookEvent[]>('GET', '/hooks/events', token);
}

export function getHookEvent(token: string, id: string) {
  return req<HookEvent>('GET', `/hooks/events/${id}`, token);
}

// ── Blueprints — BFF-normalized shape ────────────────────────────────────────
//
// The BFF absorbs conductor's 204/423/200 branching and pre-computes
// resource_types, so the client receives a single uniform shape at HTTP 200.

export interface WorkspaceLock {
  id: string;
  operation: string;
  who: string;
  info: string | null;
  version: string;
  created: string;
  path: string | null;
}

export interface WorkspaceStateData {
  serial: number;
  terraform_version: string;
  lineage: string;
  resource_count: number;
  resource_types: { type: string; count: number }[];
  outputs: Record<string, unknown> | null;
}

export interface WorkspaceView {
  isEmpty: boolean;
  locked: boolean;
  lock: WorkspaceLock | null;
  state: WorkspaceStateData | null;
}

export function getWorkspaceState(token: string, path: string) {
  return req<WorkspaceView>('GET', `/state/${path}`, token);
}

export function deleteWorkspaceState(token: string, path: string) {
  return req<void>('DELETE', `/state/${path}`, token);
}

// ── Audit ─────────────────────────────────────────────────────────────────────

export interface AuditLog {
  audit_log_id: string;
  actor_id: string;
  actor_type?: string | null; // "user" | "service"
  action: string;
  resource_id: string;
  org_id?: string | null; // actor's org at the time of the action
  detail?: string | null;
  created_at: string;
}

export function listAuditLogs(
  token: string,
  filters?: { actor_id?: string; action?: string; resource_id?: string; limit?: number; offset?: number },
) {
  const q = new URLSearchParams();
  if (filters?.actor_id) q.set('actor_id', filters.actor_id);
  if (filters?.action) q.set('action', filters.action);
  if (filters?.resource_id) q.set('resource_id', filters.resource_id);
  if (filters?.limit !== undefined) q.set('limit', String(filters.limit));
  if (filters?.offset !== undefined) q.set('offset', String(filters.offset));
  const qs = q.toString();
  return req<AuditLog[]>('GET', `/gatekeeper/audit-logs${qs ? '?' + qs : ''}`, token);
}

// ── Permission checks (access-decision audit log) ─────────────────────────────
// One row per permission evaluation (granted or denied), across every service.
// Admin-only — used to review access patterns and hunt suspicious usage. Unlike
// the mutation audit log, `resource` is the full scoped resource path used in the
// RBAC check, and `granted` records the decision.

export interface PermissionCheck {
  permissions_check_id: string;
  service: string;
  action: string;
  resource: string;
  user_id: string;
  org_id?: string | null;
  team_id?: string | null;
  granted: boolean;
  created_at: string;
}

export function listPermissionChecks(
  token: string,
  filters?: { user_id?: string; service?: string; action?: string; resource?: string; org_id?: string; granted?: boolean; limit?: number; offset?: number },
) {
  const q = new URLSearchParams();
  if (filters?.user_id) q.set('user_id', filters.user_id);
  if (filters?.service) q.set('service', filters.service);
  if (filters?.action) q.set('action', filters.action);
  if (filters?.resource) q.set('resource', filters.resource);
  if (filters?.org_id) q.set('org_id', filters.org_id);
  if (filters?.granted !== undefined) q.set('granted', String(filters.granted));
  if (filters?.limit !== undefined) q.set('limit', String(filters.limit));
  if (filters?.offset !== undefined) q.set('offset', String(filters.offset));
  const qs = q.toString();
  return req<PermissionCheck[]>('GET', `/gatekeeper/permission-checks${qs ? '?' + qs : ''}`, token);
}

// ── CI Steps ──────────────────────────────────────────────────────────────────

export interface Step {
  step_id: string;
  name: string;
  description?: string | null;
  action: string;
  with?: Record<string, unknown> | null;
  timeout?: number | null;
  created_by: string;
  org_id?: string | null;
  active: boolean;
  created_at: string;
  updated_at: string;
}

/** The gatekeeper permission triple an action requires to run. */
export interface ActionPermission {
  service: string;
  action: string;
  resource: string;
}

/** Rewrites a `with` key before the step payload is sent to the backend service. */
export interface ActionBodyTransform {
  from_key: string;
  to_key: string;
  wrap?: string[];
}

/** How a long-running action is polled to a terminal state. */
export interface ActionAsyncConfig {
  id_field: string;
  poll_path: string;
  poll_interval_secs: number;
  status_field: string;
  success_states?: string[];
  failure_states?: string[];
  cancel_states?: string[];
  output_field?: string;
  /** A response field holding an object that becomes the step's success output
   * (e.g. forge's captured output_env map). When set, stdout is never the output. */
  output_map_field?: string;
  error_fields?: string[];
}

/**
 * A callable action from the workflows catalog (mirrors the backend ActionDef).
 * summary/description are human-facing metadata; the remaining fields are the
 * action config — which backend service/route it hits, how the body is shaped,
 * the required permission, and any async polling spec.
 */
export interface WorkflowAction {
  name: string;
  summary?: string | null;
  description?: string | null;
  service_name?: string;
  service_url?: string;
  method?: string;
  path?: string;
  body_transforms?: ActionBodyTransform[] | null;
  async?: ActionAsyncConfig | null;
  required_permission?: ActionPermission | null;
}

export function listSteps(token: string) {
  return req<Step[]>('GET', '/workflows/steps', token);
}

export function getStep(token: string, id: string) {
  return req<Step>('GET', `/workflows/steps/${id}`, token);
}

export function createStep(
  token: string,
  payload: { name: string; description?: string; action: string; with?: Record<string, unknown>; timeout?: number },
) {
  return req<Step>('POST', '/workflows/steps', token, payload);
}

export function updateStep(
  token: string,
  id: string,
  payload: Partial<{ name: string; description: string; action: string; with: Record<string, unknown>; timeout: number }>,
) {
  return req<Step>('PUT', `/workflows/steps/${id}`, token, payload);
}

export function deleteStep(token: string, id: string) {
  return req<void>('DELETE', `/workflows/steps/${id}`, token);
}

export function listActions(token: string) {
  return req<WorkflowAction[]>('GET', '/workflows/actions', token);
}

// ── Workflow CRUD ─────────────────────────────────────────────────────────────

export function createWorkflow(
  token: string,
  payload: { name: string; description?: string; project?: string; steps: WorkflowStepRef[]; inputs?: WorkflowInputDef[]; outputs?: WorkflowOutputDef[] },
) {
  return req<Workflow>('POST', '/workflows/pipelines', token, payload);
}

export function updateWorkflow(
  token: string,
  id: string,
  payload: Partial<{ name: string; description: string; steps: WorkflowStepRef[]; inputs: WorkflowInputDef[]; outputs: WorkflowOutputDef[] }>,
) {
  return req<Workflow>('PUT', `/workflows/pipelines/${id}`, token, payload);
}

// ── Forge — create execution, manage runner classes ───────────────────────────

// Forge's POST /executions responds with only the new id — not a full Execution.
// secret_refs maps a target env var NAME to a "<scheme>:<arg>" credential reference
// (e.g. "git:https://github.com/acme/widgets.git") resolved at dispatch and injected
// into the runner env only — never persisted. The repo selector sets a git: ref.
export function createExecution(
  token: string,
  payload: { image: string; command: string[]; env?: Record<string, string>; timeout?: number; runner_class?: string; project?: string; secret_refs?: Record<string, string>; checkout?: CheckoutSpec },
) {
  return req<{ execution_id: string }>('POST', '/forge/executions', token, payload);
}

export function createRunnerClass(
  token: string,
  payload: { name: string; memory_mb: number; cpu_millicores: number; pids_limit?: number; tmpfs_mb?: number; enabled: boolean },
) {
  return req<RunnerClass>('POST', '/forge/runner-classes', token, payload);
}

export function getRunnerClass(token: string, name: string) {
  return req<RunnerClass>('GET', `/forge/runner-classes/${name}`, token);
}

export function updateRunnerClass(
  token: string,
  name: string,
  payload: Partial<{ memory_mb: number; cpu_millicores: number; pids_limit: number; tmpfs_mb: number; enabled: boolean }>,
) {
  return req<RunnerClass>('PUT', `/forge/runner-classes/${name}`, token, payload);
}

export function deleteRunnerClass(token: string, name: string) {
  return req<void>('DELETE', `/forge/runner-classes/${name}`, token);
}

// RuntimeBackend admin (docker/kubernetes/kata/gvisor). Create/update send the full
// object — forge re-validates type + the kata/gvisor runtime_class on every write,
// so partial updates would be rejected. secret_refs holds env-var NAMES, not values.
export type RuntimeBackendInput = { name: string; type: string; enabled: boolean; config?: Record<string, string>; secret_refs?: Record<string, string> };

export function createRuntimeBackend(token: string, payload: RuntimeBackendInput) {
  return req<RuntimeBackend>('POST', '/forge/runtime-backends', token, payload);
}

export function getRuntimeBackend(token: string, name: string) {
  return req<RuntimeBackend>('GET', `/forge/runtime-backends/${name}`, token);
}

export function updateRuntimeBackend(token: string, name: string, payload: Omit<RuntimeBackendInput, 'name'>) {
  return req<RuntimeBackend>('PUT', `/forge/runtime-backends/${name}`, token, payload);
}

export function deleteRuntimeBackend(token: string, name: string) {
  return req<void>('DELETE', `/forge/runtime-backends/${name}`, token);
}

// ── Containers ────────────────────────────────────────────────────────────────

export interface ContainerRepo {
  namespace: string;
  name: string;
  full_name: string;
  tags_count?: number | null;
  last_pushed?: string | null;
}

export interface ImageTag {
  name: string;
  digest: string;
  size?: number | null;
  pushed_at?: string | null;
}

export interface ImageManifest {
  digest: string;
  media_type?: string | null;
  size?: number | null;
  config?: unknown;
  layers?: Array<{ digest: string; size: number }> | null;
  created?: string | null;
}

export function listContainerRepos(token: string) {
  return req<ContainerRepo[]>('GET', '/containers/repositories', token);
}

export function listImageTags(token: string, namespace: string, image: string) {
  return req<ImageTag[]>('GET', `/containers/repositories/${namespace}/${image}/tags`, token);
}

export function getManifest(token: string, namespace: string, image: string, reference: string) {
  return req<ImageManifest>('GET', `/containers/repositories/${namespace}/${image}/manifests/${reference}`, token);
}

export function deleteManifest(token: string, namespace: string, image: string, digest: string) {
  return req<void>('DELETE', `/containers/repositories/${namespace}/${image}/manifests/${digest}`, token);
}

// ── Gitea integration ─────────────────────────────────────────────────────────

export interface GiteaAccount {
  username: string;
  url?: string | null;
  linked_at?: string | null;
}

export interface GiteaRepo {
  owner: string;
  name: string;
  full_name: string;
  description?: string | null;
  private: boolean;
  default_branch?: string | null;
  /** Free-text project (workspace) label this repo is tagged with. */
  project?: string;
  created_at?: string | null;
  updated_at?: string | null;
}

export interface Branch {
  name: string;
  commit_sha?: string | null;
}

export interface GitTag {
  name: string;
  commit_sha?: string | null;
}

export interface GitCommit {
  sha: string;
  message: string;
  author?: string | null;
  date?: string | null;
}

export interface PullRequest {
  index: number;
  title: string;
  head: string;
  base: string;
  status: string;
  body?: string | null;
  created_by?: string | null;
  created_at?: string | null;
  merged_at?: string | null;
}

export function getGiteaAccount(token: string) {
  return req<GiteaAccount>('GET', '/gitea_integration/account', token);
}

export function linkGiteaAccount(token: string, gitToken: string) {
  return req<GiteaAccount>('PUT', '/gitea_integration/account', token, { token: gitToken });
}

export function unlinkGiteaAccount(token: string) {
  return req<void>('DELETE', '/gitea_integration/account', token);
}

export function listGiteaRepos(token: string, project?: string) {
  return req<GiteaRepo[]>('GET', withProject('/gitea_integration/repos', project), token);
}

/** Assign a repo to a project (pass an empty string to clear it). Mirrors the CLI's `armory repos project`. */
export function setGiteaRepoProject(token: string, owner: string, name: string, project: string) {
  return req<GiteaRepo>('PUT', `/gitea_integration/repos/${owner}/${name}/project`, token, { project });
}

export function createGiteaRepo(token: string, payload: { name: string; description?: string; private?: boolean }) {
  return req<GiteaRepo>('POST', '/gitea_integration/repos', token, payload);
}

export function getGiteaRepo(token: string, owner: string, name: string) {
  return req<GiteaRepo>('GET', `/gitea_integration/repos/${owner}/${name}`, token);
}

export function deleteGiteaRepo(token: string, owner: string, name: string) {
  return req<void>('DELETE', `/gitea_integration/repos/${owner}/${name}`, token);
}

export function listBranches(token: string, owner: string, repo: string) {
  return req<Branch[]>('GET', `/gitea_integration/repos/${owner}/${repo}/branches`, token);
}

export function listGitTags(token: string, owner: string, repo: string) {
  return req<GitTag[]>('GET', `/gitea_integration/repos/${owner}/${repo}/tags`, token);
}

export function listCommits(token: string, owner: string, repo: string) {
  return req<GitCommit[]>('GET', `/gitea_integration/repos/${owner}/${repo}/commits`, token);
}

export function listPulls(token: string, owner: string, repo: string) {
  return req<PullRequest[]>('GET', `/gitea_integration/repos/${owner}/${repo}/pulls`, token);
}

export function createPull(
  token: string,
  owner: string,
  repo: string,
  payload: { title: string; head: string; base: string; body?: string },
) {
  return req<PullRequest>('POST', `/gitea_integration/repos/${owner}/${repo}/pulls`, token, payload);
}

export function getPull(token: string, owner: string, repo: string, index: number) {
  return req<PullRequest>('GET', `/gitea_integration/repos/${owner}/${repo}/pulls/${index}`, token);
}

export function mergePull(token: string, owner: string, repo: string, index: number) {
  return req<void>('POST', `/gitea_integration/repos/${owner}/${repo}/pulls/${index}/merge`, token);
}

// ── Git (credential broker) ─────────────────────────────────────────────────
// The `git` core service is a backend-agnostic git credential broker. It stores
// provider backends (github/gitlab/forgejo/generic) and mints short-lived clone
// credentials for a repo URL. Secrets are write-only: they are sent on
// create/update but NEVER returned by any read — the backend types below omit
// every secret field.

export type GitBackendType = 'github' | 'gitlab' | 'forgejo' | 'generic';

/**
 * A registered git backend as returned by reads. Secret material (tokens, keys,
 * passwords) is never present — only the non-sensitive descriptor, including the
 * resolved `host` and the `auth_mode` in effect.
 */
export interface GitBackend {
  id: string;
  name: string;
  type: GitBackendType | string;
  base_url: string;
  host: string;
  auth_mode: string;
  created_at: string;
  updated_at: string;
}

/**
 * The write-only auth payload sent when creating/updating a backend. `mode`
 * selects the credential scheme for the chosen backend type; the remaining
 * fields are mode-specific and never read back. Numbers (app_id/installation_id)
 * are sent as ints.
 */
export interface GitBackendAuth {
  mode: string;
  // github "app"
  app_id?: number;
  installation_id?: number;
  private_key?: string;
  // github "pat" / gitlab "token" / forgejo "token"
  token?: string;
  username?: string;
  // gitlab "oauth"
  refresh_token?: string;
  client_id?: string;
  client_secret?: string;
  // forgejo "admin"
  admin_token?: string;
  // generic "basic"
  password?: string;
}

export interface GitBackendCreate {
  name: string;
  type: GitBackendType | string;
  base_url: string;
  auth: GitBackendAuth;
}

/** Result of probing a backend's stored credentials. */
export interface GitBackendTest {
  ok: boolean;
  backend_type: string;
  auth_mode: string;
  expires_at?: string | null;
}

/** A short-lived clone credential minted for a repo URL. `secret` is sensitive. */
export interface GitCredential {
  type: string;
  username: string;
  secret: string;
  clone_url: string;
  backend: string;
  backend_type: string;
  expires_at?: string | null;
}

export function listGitBackends(token: string) {
  return req<GitBackend[]>('GET', '/git/backends', token);
}

export function getGitBackend(token: string, id: string) {
  return req<GitBackend>('GET', `/git/backends/${id}`, token);
}

export function createGitBackend(token: string, payload: GitBackendCreate) {
  return req<GitBackend>('POST', '/git/backends', token, payload);
}

export function updateGitBackend(token: string, id: string, payload: Partial<GitBackendCreate>) {
  return req<GitBackend>('PUT', `/git/backends/${id}`, token, payload);
}

export function deleteGitBackend(token: string, id: string) {
  return req<void>('DELETE', `/git/backends/${id}`, token);
}

/** Probe a backend's stored credentials against its host, returning the broker's verdict. */
export function testGitBackend(token: string, id: string) {
  return req<GitBackendTest>('POST', `/git/backends/${id}/test`, token);
}

/** Mint a short-lived clone credential for a repo URL (the broker picks the matching backend by host). */
export function mintGitCredential(token: string, repoUrl: string) {
  return req<GitCredential>('POST', '/git/credentials', token, { repo_url: repoUrl });
}

/**
 * One entry in the repo selector. `url` is the HTTPS clone URL a forge `git:` ref
 * consumes. `source` is "enumerated" (discovered live from a linked backend's API)
 * or "manual" (pinned by the user); `id` is present only for manual repos (deletable).
 */
export interface GitRepo {
  id?: string;
  name: string;
  url: string;
  backend?: string;
  backend_type?: string;
  source: 'enumerated' | 'manual' | string;
}

/** List clone targets: repos enumerated across the caller's linked backends plus any pinned manually. */
export function listGitRepos(token: string) {
  return req<GitRepo[]>('GET', '/git/repos', token);
}

/** Pin a repo to the selector (for generic backends that can't be enumerated, or to surface extras). */
export function createGitRepo(token: string, payload: { url: string; name?: string }) {
  return req<GitRepo>('POST', '/git/repos', token, payload);
}

/** Remove a pinned (manual) repo. */
export function deleteGitRepo(token: string, id: string) {
  return req<void>('DELETE', `/git/repos/${id}`, token);
}

/** One branch of a repo; `default` flags the remote's default branch. */
export interface GitBranch {
  name: string;
  default?: boolean;
}

/**
 * List a repo's branches (for the checkout branch selector), enumerated via the
 * owning backend's API. Returns [] for generic/un-enumerable backends so the caller
 * falls back to a free-text ref. `cloneURL` is the repo's HTTPS clone URL.
 */
export function listGitBranches(token: string, cloneURL: string) {
  return req<GitBranch[]>('GET', `/git/repos/branches?url=${encodeURIComponent(cloneURL)}`, token);
}

// ── Invites ───────────────────────────────────────────────────────────────────

export interface Invite {
  invite_id: string;
  email: string;
  org_id?: string | null;
  team_id?: string | null;
  invited_by: string;
  status: string;
  created_at: string;
  updated_at: string;
}

export function listInvites(token: string) {
  return req<Invite[]>('GET', '/gatekeeper/invites', token);
}

export function getInvite(token: string, id: string) {
  return req<Invite>('GET', `/gatekeeper/invites/${id}`, token);
}

export function acceptInvite(token: string, id: string) {
  return req<void>('POST', `/gatekeeper/invites/${id}/accept`, token);
}

export function declineInvite(token: string, id: string) {
  return req<void>('POST', `/gatekeeper/invites/${id}/decline`, token);
}

export function deleteInvite(token: string, id: string) {
  return req<void>('DELETE', `/gatekeeper/invites/${id}`, token);
}

// ── Orgs extended ─────────────────────────────────────────────────────────────

export function listOrgs(token: string) {
  return req<Org[]>('GET', '/gatekeeper/orgs', token);
}

export function updateOrg(token: string, id: string, org_name: string) {
  return req<Org>('PUT', `/gatekeeper/orgs/${id}`, token, { org_name });
}

export function deleteOrg(token: string, id: string) {
  return req<void>('DELETE', `/gatekeeper/orgs/${id}`, token);
}

export function inviteToOrg(token: string, id: string, email: string) {
  return req<void>('POST', `/gatekeeper/orgs/${id}/invites`, token, { email });
}

// ── Permissions extended ──────────────────────────────────────────────────────

export function createPermission(
  token: string,
  payload: { name: string; service: string; actions: string[]; resources: string[] },
) {
  return req<Permission>('POST', '/gatekeeper/permissions', token, payload);
}

export function updatePermission(
  token: string,
  id: string,
  payload: Partial<{ name: string; service: string; actions: string[]; resources: string[] }>,
) {
  return req<Permission>('PUT', `/gatekeeper/permissions/${id}`, token, payload);
}

export function deletePermission(token: string, id: string) {
  return req<void>('DELETE', `/gatekeeper/permissions/${id}`, token);
}

// ── Roles extended ────────────────────────────────────────────────────────────

export function createRole(token: string, payload: { name?: string; permissions_ids: string[] }) {
  return req<Role>('POST', '/gatekeeper/roles', token, payload);
}

export function updateRole(token: string, id: string, payload: { name?: string; permissions_ids: string[] }) {
  return req<Role>('PUT', `/gatekeeper/roles/${id}`, token, payload);
}

export function deleteRole(token: string, id: string) {
  return req<void>('DELETE', `/gatekeeper/roles/${id}`, token);
}

// ── Service permission requests ───────────────────────────────────────────────

export interface ServiceRequest {
  request_id: string;
  service_name: string;
  requested_by: string;
  status: string;
  permissions?: string[] | null;
  org_id?: string | null;
  created_at: string;
  updated_at: string;
}

export function listServiceRequests(token: string, filters?: { service_name?: string; status?: string }) {
  const q = new URLSearchParams();
  if (filters?.service_name) q.set('service_name', filters.service_name);
  if (filters?.status) q.set('status', filters.status);
  const qs = q.toString();
  return req<ServiceRequest[]>('GET', `/gatekeeper/service-permission-requests${qs ? '?' + qs : ''}`, token);
}

export function getServiceRequest(token: string, id: string) {
  return req<ServiceRequest>('GET', `/gatekeeper/service-permission-requests/${id}`, token);
}

export function approveServiceRequest(token: string, id: string) {
  return req<void>('POST', `/gatekeeper/service-permission-requests/${id}/approve`, token);
}

export function declineServiceRequest(token: string, id: string) {
  return req<void>('POST', `/gatekeeper/service-permission-requests/${id}/decline`, token);
}

// ── Sessions ──────────────────────────────────────────────────────────────────

export interface Session {
  session_id: string;
  user_id: string;
  created_at: string;
  last_seen?: string | null;
  ip?: string | null;
  user_agent?: string | null;
}

export function getSession(token: string, id: string) {
  return req<Session>('GET', `/gatekeeper/sessions/${id}`, token);
}

export function deleteSession(token: string, id: string) {
  return req<void>('DELETE', `/gatekeeper/sessions/${id}`, token);
}

// ── Teams extended ────────────────────────────────────────────────────────────

export function listTeams(token: string) {
  return req<Team[]>('GET', '/gatekeeper/teams', token);
}

export function createTeam(token: string, payload: { team_name: string; role_id?: string }) {
  return req<Team>('POST', '/gatekeeper/teams', token, payload);
}

export function updateTeam(token: string, id: string, payload: { team_name: string }) {
  return req<Team>('PUT', `/gatekeeper/teams/${id}`, token, payload);
}

export function deleteTeam(token: string, id: string) {
  return req<void>('DELETE', `/gatekeeper/teams/${id}`, token);
}

export function inviteToTeam(token: string, id: string, email: string) {
  return req<void>('POST', `/gatekeeper/teams/${id}/invites`, token, { email });
}

// ── Ticket comments extended ──────────────────────────────────────────────────

export function deleteComment(token: string, ticketId: string, commentId: string) {
  return req<void>('DELETE', `/tickets/tickets/${ticketId}/comments/${commentId}`, token);
}

// ── Permissions check ─────────────────────────────────────────────────────────

export function checkPermission(token: string, service: string, action: string, resource: string) {
  return req<{ authorized: boolean }>('POST', '/gatekeeper/check_permissions', token, { service, action, resource });
}

// ── Builder — global service control plane (system admin) ─────────────────────
// Builder is a SYSTEM-ADMIN-only control plane over the single global service
// baseline (addressed by the literal id "default"). The admin page lets the admin
// toggle, configure, and register platform services for the whole instance; there
// are no per-org overrides. `core` services (gatekeeper, conductor, registry,
// builder) are always enabled and cannot be configured. Reads are granted to all
// users (read-only); only the system admin may write.

export interface OrgService {
  service: string;
  enabled: boolean;
  kind: string;   // "platform" | "custom"
  source: string; // "catalog" | "default" | "override" | "custom" | "core"
  config?: Record<string, unknown>;
  image?: string;
  port?: number;
  description?: string;
  core?: boolean;
  // coming_soon flags a spun-off service whose source is not in this repo: it can't be
  // deployed yet, so builder forces it disabled and the UI shows a "coming soon" badge
  // in place of the enable/configure controls.
  coming_soon?: boolean;
  db_configured?: boolean;
  db_host?: string;
}

// SetOrgServiceBody is the PUT payload. `db_url` and `secrets` are write-only — they
// are encrypted on receipt and never read back (only the redacted db_host is returned).
// `secrets` carries sensitive config keyed by env var (e.g. REDIS_URL, GITEA_ADMIN_TOKEN)
// that a service declares in its secretConfig; non-sensitive config goes in `config`.
export interface SetOrgServiceBody {
  enabled?: boolean;
  kind?: string;
  config?: Record<string, unknown>;
  image?: string;
  port?: number;
  description?: string;
  db_url?: string;
  secrets?: Record<string, string>;
}

export function listServices(token: string) {
  return req<OrgService[]>('GET', '/builder/services', token);
}

export function setService(token: string, service: string, body: SetOrgServiceBody) {
  return req<OrgService>('PUT', `/builder/services/${encodeURIComponent(service)}`, token, body);
}

export function deleteService(token: string, service: string) {
  return req<void>('DELETE', `/builder/services/${encodeURIComponent(service)}`, token);
}

// ── Registered services (routing availability) ────────────────────────────────
// Conductor's live routing table: every service currently registered/routable,
// whether it registered from the manifest at startup (core / compose / Helm) or
// was deployed-and-registered by builder at runtime when the system admin enabled
// it. The sidebar uses this to show only services that are actually up — a service
// builder later disables is unregistered and drops out. Public read on conductor;
// no permissions required.
export interface RegisteredService {
  name: string;
  description?: string;
  // Path under the service's route prefix that serves its embedded mini-portal
  // (e.g. "/ui"). Present only for services that ship a UI; the shell renders an
  // iframe page for any registered, non-bundled service that advertises one.
  ui_path?: string;
}

/** Fetch conductor's live routing table (the services currently registered/routable), flattened to the bare array. */
export async function listRegisteredServices(token: string): Promise<RegisteredService[]> {
  const res = await req<{ services: RegisteredService[] }>('GET', '/services', token);
  return res.services ?? [];
}

// ── Outposts ──────────────────────────────────────────────────────────────────

export interface Outpost {
  outpost_id: string;
  org_id?: string;
  name: string;
  modules: string;
  status: 'pending' | 'connected' | 'stale' | string;
  last_seen_at?: string | null;
  created_at: string;
  updated_at?: string;
}

export interface CreateOutpostResponse extends Outpost {
  // Single-use enrollment token, returned only on creation.
  enrollment_token: string;
}

export function listOutposts(token: string) {
  return req<Outpost[]>('GET', '/outpost-gateway/outposts', token);
}

export function getOutpost(token: string, id: string) {
  return req<Outpost>('GET', `/outpost-gateway/outposts/${id}`, token);
}

export function createOutpost(token: string, payload: { name: string; modules: string[] }) {
  return req<CreateOutpostResponse>('POST', '/outpost-gateway/outposts', token, payload);
}

export function deleteOutpost(token: string, id: string) {
  return req<void>('DELETE', `/outpost-gateway/outposts/${id}`, token);
}

// ── Chaos ─────────────────────────────────────────────────────────────────────

export interface Experiment {
  experiment_id: string;
  user_id?: string;
  org_id?: string;
  outpost_id: string;
  experiment_type: string;
  target_app_ns: string;
  target_app_label: string;
  target_app_kind: string;
  engine_name?: string;
  params?: Record<string, string>;
  status: 'pending' | 'running' | 'Pass' | 'Fail' | 'Error' | 'stopped' | string;
  verdict?: string;
  fail_step?: string;
  probe_success?: string;
  created_at: string;
  started_at?: string | null;
  ended_at?: string | null;
}

export interface ExperimentType {
  name: string;
  description: string;
  params: Record<string, string>;
}

export function listExperiments(token: string) {
  return req<Experiment[]>('GET', '/chaos/experiments', token);
}

export function getExperiment(token: string, id: string) {
  return req<Experiment>('GET', `/chaos/experiments/${id}`, token);
}

export function listExperimentTypes(token: string) {
  return req<ExperimentType[]>('GET', '/chaos/experiment-types', token);
}

export function createExperiment(
  token: string,
  payload: {
    outpost_id: string;
    experiment_type: string;
    target_app_ns: string;
    target_app_label: string;
    target_app_kind?: string;
    params?: Record<string, string>;
  },
) {
  return req<Experiment>('POST', '/chaos/experiments', token, payload);
}

export function deleteExperiment(token: string, id: string) {
  return req<void>('DELETE', `/chaos/experiments/${id}`, token);
}

// ── Argo ──────────────────────────────────────────────────────────────────────

export interface ArgoApp {
  app_id: string;
  org_id?: string;
  outpost_id: string;
  name: string;
  sync_status?: string;
  health_status?: string;
  revision?: string;
  operation_phase?: string;
  created_at: string;
  updated_at?: string;
}

export interface ArgoSync {
  sync_id: string;
  app_name: string;
  revision?: string;
  status: 'pending' | 'running' | 'Synced' | 'Failed' | string;
  message?: string;
  created_at: string;
}

export function listArgoApps(token: string) {
  return req<ArgoApp[]>('GET', '/argo/apps', token);
}

export function getArgoApp(token: string, name: string) {
  return req<ArgoApp>('GET', `/argo/apps/${encodeURIComponent(name)}`, token);
}

export function syncArgoApp(token: string, name: string, payload?: { outpost_id?: string; revision?: string }) {
  return req<ArgoSync>('POST', `/argo/apps/${encodeURIComponent(name)}/sync`, token, payload ?? {});
}

export function getArgoSync(token: string, id: string) {
  return req<ArgoSync>('GET', `/argo/syncs/${id}`, token);
}

// ── Projects (workspaces) ─────────────────────────────────────────────────────
// A project is a free-text label attached to pipelines, executions, tickets and
// repos — a view filter, not a permission boundary. There is no project registry
// endpoint: the in-use labels are derived by scanning those resources' lists.

/**
 * Aggregate the distinct, sorted project labels currently in use across the
 * caller's pipelines, executions, tickets and repos — the source list for the
 * project switcher. Best-effort: an endpoint the user can't reach (or that
 * errors) is skipped, never fatal, mirroring the CLI's fetchKnownProjects.
 */
export async function fetchProjectLabels(token: string): Promise<string[]> {
  const settled = await Promise.allSettled<{ project?: string }[]>([
    listWorkflows(token),
    listExecutions(token),
    listTickets(token),
    listGiteaRepos(token),
  ]);
  const labels = new Set<string>();
  for (const r of settled) {
    if (r.status !== 'fulfilled') continue;
    for (const item of r.value) {
      if (item.project) labels.add(item.project);
    }
  }
  return [...labels].sort();
}

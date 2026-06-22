const BASE = '/api';

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

export function getSetupStatus() {
  return req<SetupStatus>('GET', '/gatekeeper/setup/status');
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

// Returns null when no provider row exists (org implicitly uses builtin).
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

export interface Workflow {
  workflow_id: string;
  name: string;
  description?: string | null;
  created_by: string;
  org_id?: string | null;
  active: boolean;
  steps: Array<{ step_id: string; parallel_group?: string | null }>;
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
  step_runs?: WorkflowStepRun[];
  created_at: string;
  started_at?: string | null;
  ended_at?: string | null;
}

export function listWorkflows(token: string) {
  return req<Workflow[]>('GET', '/workflows/pipelines', token);
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

// ── Forge ─────────────────────────────────────────────────────────────────────

export interface Execution {
  execution_id: string;
  user_id: string;
  image: string;
  command: string[];
  env?: Record<string, string> | null;
  timeout?: number | null;
  runner_class?: string | null;
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
  enabled: boolean;
}

export function listExecutions(token: string) {
  return req<Execution[]>('GET', '/forge/executions', token);
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

export function listTickets(token: string) {
  return req<Ticket[]>('GET', '/tickets/tickets', token);
}

export function getTicket(token: string, id: string) {
  return req<Ticket>('GET', `/tickets/tickets/${id}`, token);
}

export function createTicket(token: string, payload: { title: string; description?: string; priority?: string }) {
  return req<Ticket>('POST', '/tickets/tickets', token, payload);
}

export function updateTicket(token: string, id: string, payload: Partial<{ title: string; description: string; status: string; priority: string; assignee_id: string }>) {
  return req<Ticket>('PUT', `/tickets/tickets/${id}`, token, payload);
}

export function deleteTicket(token: string, id: string) {
  return req<void>('DELETE', `/tickets/tickets/${id}`, token);
}

export function addComment(token: string, ticketId: string, body: string) {
  return req<TicketComment>('POST', `/tickets/tickets/${ticketId}/comments`, token, { body });
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
  audit_id: string;
  actor_id: string;
  action: string;
  resource_id: string;
  resource_type?: string | null;
  org_id?: string | null;
  meta?: Record<string, unknown> | null;
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

// ── CI Steps ──────────────────────────────────────────────────────────────────

export interface Step {
  step_id: string;
  name: string;
  description?: string | null;
  action: string;
  with?: Record<string, string> | null;
  timeout?: number | null;
  created_by: string;
  org_id?: string | null;
  active: boolean;
  created_at: string;
  updated_at: string;
}

export interface WorkflowAction {
  name: string;
  description?: string | null;
  inputs?: Record<string, unknown> | null;
}

export function listSteps(token: string) {
  return req<Step[]>('GET', '/workflows/steps', token);
}

export function getStep(token: string, id: string) {
  return req<Step>('GET', `/workflows/steps/${id}`, token);
}

export function createStep(
  token: string,
  payload: { name: string; description?: string; action: string; with?: Record<string, string>; timeout?: number },
) {
  return req<Step>('POST', '/workflows/steps', token, payload);
}

export function updateStep(
  token: string,
  id: string,
  payload: Partial<{ name: string; description: string; action: string; with: Record<string, string>; timeout: number }>,
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
  payload: { name: string; description?: string; steps: Array<{ step_id: string; parallel_group?: string }> },
) {
  return req<Workflow>('POST', '/workflows/pipelines', token, payload);
}

export function updateWorkflow(
  token: string,
  id: string,
  payload: Partial<{ name: string; description: string; steps: Array<{ step_id: string; parallel_group?: string }> }>,
) {
  return req<Workflow>('PUT', `/workflows/pipelines/${id}`, token, payload);
}

// ── Forge — create execution, manage runner classes ───────────────────────────

export function createExecution(
  token: string,
  payload: { image: string; command: string[]; env?: Record<string, string>; timeout?: number; runner_class?: string },
) {
  return req<Execution>('POST', '/forge/executions', token, payload);
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

export function listGiteaRepos(token: string) {
  return req<GiteaRepo[]>('GET', '/gitea_integration/repos', token);
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

export function createRole(token: string, payload: { permissions_ids: string[] }) {
  return req<Role>('POST', '/gatekeeper/roles', token, payload);
}

export function updateRole(token: string, id: string, payload: { permissions_ids: string[] }) {
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

// ── Builder — per-org service control plane ───────────────────────────────────
// The builder control plane reports the effective state of every platform
// service for an org, overlaying the live catalog, the "default" baseline, and
// the org's own overrides. The sidebar uses the list to hide services an org has
// turned off; the builder admin page lets admins toggle, configure, and register
// services. `core` services (gatekeeper, conductor, registry, builder) are always
// enabled and cannot be configured. Use the literal org id "default" to address
// the baseline every org inherits (platform-admin only).

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
  db_configured?: boolean;
  db_host?: string;
}

// SetOrgServiceBody is the PUT payload. `db_url` is write-only — it is encrypted
// on receipt and never read back (only the redacted db_host is returned).
export interface SetOrgServiceBody {
  enabled?: boolean;
  kind?: string;
  config?: Record<string, unknown>;
  image?: string;
  port?: number;
  description?: string;
  db_url?: string;
}

export function listOrgServices(token: string, orgId: string) {
  return req<OrgService[]>('GET', `/builder/orgs/${orgId}/services`, token);
}

export function setOrgService(token: string, orgId: string, service: string, body: SetOrgServiceBody) {
  return req<OrgService>('PUT', `/builder/orgs/${orgId}/services/${encodeURIComponent(service)}`, token, body);
}

export function deleteOrgService(token: string, orgId: string, service: string) {
  return req<void>('DELETE', `/builder/orgs/${orgId}/services/${encodeURIComponent(service)}`, token);
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

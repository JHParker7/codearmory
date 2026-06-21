const BASE = '/api';

function authHeaders(token: string) {
  return { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' };
}

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
  return req<SignupResult>('POST', '/signup', undefined, payload);
}

export function login(email: string, password: string) {
  return req<LoginResult>('POST', '/login', undefined, { email, password });
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

export function getUser(token: string, id: string) {
  return req<User>('GET', `/users/${id}`, token);
}

export function updateUser(
  token: string,
  id: string,
  payload: { email: string; username: string; password?: string; firstname?: string; lastname?: string },
) {
  return req<User>('PUT', `/users/${id}`, token, payload);
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
  return req<Org>('POST', '/orgs', token, { org_name });
}

export function getOrg(token: string, id: string) {
  return req<Org>('GET', `/orgs/${id}`, token);
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
  return req<Team>('GET', `/teams/${id}`, token);
}

// ── Blueprints ────────────────────────────────────────────────────────────────

export interface TerraformState {
  version: number;
  terraform_version: string;
  serial: number;
  lineage: string;
  outputs?: Record<string, unknown>;
  resources?: Array<{ type: string; name: string; module?: string; [k: string]: unknown }>;
}

export interface LockInfo {
  ID: string;
  Operation: string;
  Who: string;
  Info?: string;
  Version: string;
  Created: string;
  Path?: string;
}

export interface WorkspaceState {
  state: TerraformState | null;
  lock: LockInfo | null;
}

export async function getWorkspaceState(
  token: string,
  path: string,
): Promise<WorkspaceState> {
  const res = await fetch(`${BASE}/state/${path}`, {
    headers: authHeaders(token),
  });
  if (res.status === 204) return { state: null, lock: null };
  if (res.status === 423) {
    const lock: LockInfo = await res.json();
    return { state: null, lock };
  }
  if (!res.ok) {
    const text = await res.text().catch(() => '');
    throw Object.assign(new Error(text || res.statusText), { status: res.status });
  }
  const state: TerraformState = await res.json();
  return { state, lock: null };
}

export async function deleteWorkspaceState(token: string, path: string) {
  await req<void>('DELETE', `/state/${path}`, token);
}

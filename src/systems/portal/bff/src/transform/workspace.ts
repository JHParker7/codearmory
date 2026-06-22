interface TerraformResource {
  type: string;
  [k: string]: unknown;
}

interface TerraformState {
  terraform_version: string;
  serial: number;
  lineage: string;
  outputs?: Record<string, unknown>;
  resources?: TerraformResource[];
}

interface LockInfo {
  ID: string;
  Operation: string;
  Who: string;
  Info?: string;
  Version: string;
  Created: string;
  Path?: string;
}

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

export function fromEmpty(): WorkspaceView {
  return { isEmpty: true, locked: false, lock: null, state: null };
}

export function fromLocked(raw: LockInfo): WorkspaceView {
  return {
    isEmpty: false,
    locked: true,
    lock: {
      id: raw.ID,
      operation: raw.Operation,
      who: raw.Who,
      info: raw.Info ?? null,
      version: raw.Version,
      created: raw.Created,
      path: raw.Path ?? null,
    },
    state: null,
  };
}

export function fromState(raw: TerraformState): WorkspaceView {
  const resources = raw.resources ?? [];
  const counts: Record<string, number> = {};
  for (const r of resources) {
    counts[r.type] = (counts[r.type] ?? 0) + 1;
  }
  const resource_types = Object.entries(counts)
    .sort((a, b) => b[1] - a[1])
    .map(([type, count]) => ({ type, count }));

  return {
    isEmpty: false,
    locked: false,
    lock: null,
    state: {
      serial: raw.serial,
      terraform_version: raw.terraform_version,
      lineage: raw.lineage,
      resource_count: resources.length,
      resource_types,
      outputs: raw.outputs ?? null,
    },
  };
}

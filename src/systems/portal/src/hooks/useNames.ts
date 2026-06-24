/**
 * Name-resolution hooks — turn foreign-key UUIDs (user ids, workflow ids, org ids)
 * into human-readable names for display.
 *
 * Each hook fetches its catalog once on mount and returns an `id → name` map.
 * Resolution is BEST-EFFORT: if the caller lacks the listing permission (or the
 * request fails) the map stays empty and call sites fall back to the raw id
 * (typically via {@link shortId}). Pages render `map[id] ?? shortId(id)`.
 */
import { useEffect, useState } from 'react';
import { listUsers, listWorkflows, listOrgs, listPermissions, listSecrets } from '../api/bff';

/** Fetch a catalog once and build an id→name map via `pick`; empty on permission/network error. */
function useNameMap<T>(
  token: string,
  fetcher: (t: string) => Promise<T[]>,
  pick: (x: T) => [string, string],
): Record<string, string> {
  const [map, setMap] = useState<Record<string, string>>({});
  useEffect(() => {
    let active = true;
    fetcher(token)
      .then(items => { if (active) setMap(Object.fromEntries(items.map(pick))); })
      .catch(() => {});
    return () => { active = false; };
  }, [token]); // eslint-disable-line react-hooks/exhaustive-deps
  return map;
}

/** user_id → username. */
export const useUserNames = (token: string) =>
  useNameMap(token, listUsers, u => [u.user_id, u.username]);

/** workflow_id → workflow name. */
export const useWorkflowNames = (token: string) =>
  useNameMap(token, listWorkflows, w => [w.workflow_id, w.name]);

/** org_id → org name. */
export const useOrgNames = (token: string) =>
  useNameMap(token, listOrgs, o => [o.org_id, o.org_name]);

/** permissions_id → permission name. */
export const usePermissionNames = (token: string) =>
  useNameMap(token, listPermissions, p => [p.permissions_id, p.name]);

/** secret_id → secret name. */
export const useSecretNames = (token: string) =>
  useNameMap(token, listSecrets, s => [s.secret_id, s.name]);

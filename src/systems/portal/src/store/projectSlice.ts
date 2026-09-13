/**
 * Project slice — the sticky "current project" scope plus the set of projects the
 * caller can reach.
 *
 * A project is a first-class gatekeeper RBAC scope (slug, namespace, three tier
 * roles), NOT a free-text label. Selecting one filters every list view to that
 * project's resources and tags newly-created resources with its slug; membership
 * in the project's tier role is what actually authorizes access. `current` holds
 * the selected project's *slug* — the same value every resource carries in its
 * `project` field and that `withProject()` sends as `?project=` — so the pages
 * that read `current` need no change.
 *
 * `chain` is the selected project's slug plus every ANCESTOR's slug (root last),
 * from GET /projects/{id}/ancestors. It is the effective DISPLAY scope: a child
 * project inherits its parents' resources (view + run, never edit — that cap is
 * enforced by gatekeeper's use-only grants), so list views filter by
 * `chain.includes(resource.project)` rather than the exact current slug. Newly
 * created resources are still tagged with `current` (the child), never the chain.
 *
 * `current` is persisted to localStorage so the choice survives reloads, and is
 * cleared on logout / session expiry so the next user on the same browser starts
 * unscoped. `known` is the caller's accessible projects, re-fetched per session
 * and whenever the switcher opens.
 */
import { createSlice, createAsyncThunk } from '@reduxjs/toolkit';
import type { PayloadAction } from '@reduxjs/toolkit';
import { listAccessibleProjects, getProjectAncestors } from '../api/bff';
import type { Project } from '../api/bff';
import { logout, hydrateUser } from './authSlice';

const CURRENT_KEY = 'ca_current_project';

interface ProjectState {
  /** The selected project's slug (the resource `project` value), or null for "all projects". */
  current: string | null;
  /** The current project's slug + its ancestors' slugs (root last) — the effective display scope. */
  chain: string[];
  /** The projects the caller can reach (owned or member), each tagged with the caller's tier. */
  known: Project[];
  knownStatus: 'idle' | 'loading' | 'succeeded' | 'failed';
}

const stored = (localStorage.getItem(CURRENT_KEY) ?? '').trim();
const initialState: ProjectState = {
  current: stored || null,
  // Seed the chain with just the current slug; the ancestor fetch (fetchAncestorChain)
  // widens it once known projects have loaded and the id is resolvable.
  chain: stored ? [stored] : [],
  known: [],
  knownStatus: 'idle',
};

/**
 * The projects the caller can reach — owned or member — as the source list for
 * the switcher. Each carries the caller's tier. This is the real RBAC scope, not
 * a scan of free-text labels.
 */
export const fetchKnownProjects = createAsyncThunk(
  'project/fetchKnown',
  (token: string) => listAccessibleProjects(token),
);

/**
 * Resolve the selected project's ancestor chain (self + parents, root last) so
 * list views can show inherited resources. Keyed by project_id (the /ancestors
 * route is by id); the caller maps the current slug → id via `known`.
 */
export const fetchAncestorChain = createAsyncThunk(
  'project/fetchAncestors',
  ({ token, projectId }: { token: string; projectId: string }) =>
    getProjectAncestors(token, projectId),
);

const projectSlice = createSlice({
  name: 'project',
  initialState,
  reducers: {
    /** Set (or clear, with null/empty) the sticky current project slug and persist it. */
    setCurrentProject(state, action: PayloadAction<string | null>) {
      const next = action.payload?.trim() || null;
      state.current = next;
      // Reset the chain to just the child; fetchAncestorChain widens it. On clear,
      // empty chain = "all projects" (list views fall back to no filtering).
      state.chain = next ? [next] : [];
      if (next) localStorage.setItem(CURRENT_KEY, next);
      else localStorage.removeItem(CURRENT_KEY);
    },
  },
  extraReducers(builder) {
    builder
      .addCase(fetchKnownProjects.pending, (state) => { state.knownStatus = 'loading'; })
      .addCase(fetchKnownProjects.fulfilled, (state, action) => {
        state.known = [...action.payload].sort((a, b) => (a.name || a.slug).localeCompare(b.name || b.slug));
        state.knownStatus = 'succeeded';
      })
      .addCase(fetchKnownProjects.rejected, (state) => { state.knownStatus = 'failed'; })

      // Widen the display scope to the selected project's ancestor chain. Guard against
      // a stale response from a previous selection: only apply when the returned self
      // (first entry, the child) still matches the current slug.
      .addCase(fetchAncestorChain.fulfilled, (state, action) => {
        const slugs = action.payload.map((p) => p.slug);
        if (slugs.length && state.current && slugs[0] === state.current) {
          state.chain = slugs;
        }
      })

      // Clear the current project + known list whenever the session ends —
      // explicit logout or token expiry — so a later user on the same browser
      // doesn't inherit the previous user's project scope.
      .addCase(logout, (state) => {
        state.current = null;
        state.chain = [];
        state.known = [];
        state.knownStatus = 'idle';
        localStorage.removeItem(CURRENT_KEY);
      })
      .addCase(hydrateUser.rejected, (state) => {
        state.current = null;
        state.chain = [];
        state.known = [];
        state.knownStatus = 'idle';
        localStorage.removeItem(CURRENT_KEY);
      });
  },
});

export const { setCurrentProject } = projectSlice.actions;
export const projectReducer = projectSlice.reducer;

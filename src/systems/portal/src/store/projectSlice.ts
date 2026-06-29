/**
 * Project (workspace) slice — the sticky "current project" view filter plus the
 * set of project labels currently in use.
 *
 * A project is a free-text label attached to pipelines, forge executions,
 * tickets and repos. It is purely a view filter, never a permission boundary:
 * selecting one filters every list view to it and tags newly-created resources,
 * mirroring the CLI's `armory project use`.
 *
 * `current` is persisted to localStorage so the choice survives reloads, and is
 * cleared on logout / session expiry so the next user on the same browser starts
 * unfiltered. `known` is the distinct labels aggregated (best-effort) across the
 * list endpoints; it is re-fetched per session and whenever the switcher opens.
 */
import { createSlice, createAsyncThunk } from '@reduxjs/toolkit';
import type { PayloadAction } from '@reduxjs/toolkit';
import { fetchProjectLabels } from '../api/bff';
import { logout, hydrateUser } from './authSlice';

const CURRENT_KEY = 'ca_current_project';

interface ProjectState {
  current: string | null;
  known: string[];
  knownStatus: 'idle' | 'loading' | 'succeeded' | 'failed';
}

const stored = (localStorage.getItem(CURRENT_KEY) ?? '').trim();
const initialState: ProjectState = {
  current: stored || null,
  known: [],
  knownStatus: 'idle',
};

/**
 * Aggregate the distinct project labels in use across the caller's accessible
 * resources — the source list for the switcher. Best-effort: an endpoint the
 * user can't reach (or that errors) is skipped, never fatal.
 */
export const fetchKnownProjects = createAsyncThunk(
  'project/fetchKnown',
  (token: string) => fetchProjectLabels(token),
);

const projectSlice = createSlice({
  name: 'project',
  initialState,
  reducers: {
    /** Set (or clear, with null/empty) the sticky current project and persist it. */
    setCurrentProject(state, action: PayloadAction<string | null>) {
      const next = action.payload?.trim() || null;
      state.current = next;
      if (next) localStorage.setItem(CURRENT_KEY, next);
      else localStorage.removeItem(CURRENT_KEY);
      // Surface a freshly-typed label right away so the switcher lists it before
      // any resource has actually been tagged with it.
      if (next && !state.known.includes(next)) state.known = [...state.known, next].sort();
    },
  },
  extraReducers(builder) {
    builder
      .addCase(fetchKnownProjects.pending, (state) => { state.knownStatus = 'loading'; })
      .addCase(fetchKnownProjects.fulfilled, (state, action) => {
        // Keep the active label visible even if no resource carries it yet.
        const known = new Set(action.payload);
        if (state.current) known.add(state.current);
        state.known = [...known].sort();
        state.knownStatus = 'succeeded';
      })
      .addCase(fetchKnownProjects.rejected, (state) => { state.knownStatus = 'failed'; })

      // Clear the current project + known list whenever the session ends —
      // explicit logout or token expiry — so a later user on the same browser
      // doesn't inherit the previous user's project filter or label names.
      .addCase(logout, (state) => {
        state.current = null;
        state.known = [];
        state.knownStatus = 'idle';
        localStorage.removeItem(CURRENT_KEY);
      })
      .addCase(hydrateUser.rejected, (state) => {
        state.current = null;
        state.known = [];
        state.knownStatus = 'idle';
        localStorage.removeItem(CURRENT_KEY);
      });
  },
});

export const { setCurrentProject } = projectSlice.actions;
export const projectReducer = projectSlice.reducer;

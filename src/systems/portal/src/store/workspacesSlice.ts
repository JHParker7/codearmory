/**
 * Workspaces slice — the user's pinned blueprints workspaces and their fetched
 * state views. The entry list is persisted to localStorage (and cleared on logout
 * or session expiry so the next user can't see another's workspace paths); each
 * workspace's normalized state is fetched lazily and tracked per-path under
 * `details`.
 */
import { createSlice, createAsyncThunk } from '@reduxjs/toolkit';
import type { PayloadAction } from '@reduxjs/toolkit';
import { getWorkspaceState, deleteWorkspaceState } from '../api/bff';
import type { WorkspaceView } from '../api/bff';
import { parseStoredWorkspaces } from '../utils';
import type { WorkspaceEntry } from '../utils';
import { logout, hydrateUser } from './authSlice';

const WS_KEY = 'ca_workspaces';

export interface WorkspaceDetail {
  data: WorkspaceView | null;
  status: 'idle' | 'loading' | 'succeeded' | 'failed';
  error: string | null;
  lastFetched: number | null;
}

interface WorkspacesState {
  entries: WorkspaceEntry[];
  details: Record<string, WorkspaceDetail>;
  selected: string | null;
}

const initialState: WorkspacesState = {
  entries: parseStoredWorkspaces(localStorage.getItem(WS_KEY) ?? '[]'),
  details: {},
  selected: null,
};

// ── Thunks ────────────────────────────────────────────────────────────────────

/** Fetch and store one workspace's normalized state view, keyed by path. Rejects with {path, message} so the reducer can mark just that entry failed. */
export const fetchWorkspace = createAsyncThunk(
  'workspaces/fetchOne',
  async ({ token, path }: { token: string; path: string }, { rejectWithValue: reject }) => {
    try {
      const data = await getWorkspaceState(token, path);
      return { path, data };
    } catch (err: unknown) {
      const e = err as { message?: string };
      return reject({ path, message: e.message ?? 'failed to fetch state' });
    }
  },
);

/** Delete a workspace's remote state, then drop it from the local list. A missing remote state is non-fatal — the entry is removed regardless. */
export const removeWorkspaceAndCleanup = createAsyncThunk(
  'workspaces/remove',
  async ({ token, path }: { token: string; path: string }) => {
    try {
      await deleteWorkspaceState(token, path);
    } catch {
      // Non-fatal: the state file may not exist; still remove from local list.
    }
    return path;
  },
);

// ── Slice ─────────────────────────────────────────────────────────────────────

const workspacesSlice = createSlice({
  name: 'workspaces',
  initialState,
  reducers: {
    /** Pin a workspace (dedup by path), persist the list, and select it. */
    addWorkspace(state, action: PayloadAction<WorkspaceEntry>) {
      if (state.entries.some(e => e.path === action.payload.path)) return;
      state.entries.push(action.payload);
      localStorage.setItem(WS_KEY, JSON.stringify(state.entries));
      state.selected = action.payload.path;
    },
    /** Unpin a workspace locally (without touching remote state), re-selecting the first remaining entry if it was selected. */
    removeWorkspace(state, action: PayloadAction<string>) {
      state.entries = state.entries.filter(e => e.path !== action.payload);
      delete state.details[action.payload];
      localStorage.setItem(WS_KEY, JSON.stringify(state.entries));
      if (state.selected === action.payload) {
        state.selected = state.entries[0]?.path ?? null;
      }
    },
    /** Set the currently focused workspace path (or null). */
    setSelected(state, action: PayloadAction<string | null>) {
      state.selected = action.payload;
    },
  },
  extraReducers(builder) {
    builder
      .addCase(fetchWorkspace.pending, (state, action) => {
        const { path } = action.meta.arg;
        state.details[path] = {
          data: state.details[path]?.data ?? null,
          status: 'loading',
          error: null,
          lastFetched: state.details[path]?.lastFetched ?? null,
        };
      })
      .addCase(fetchWorkspace.fulfilled, (state, action) => {
        const { path, data } = action.payload;
        state.details[path] = { data, status: 'succeeded', error: null, lastFetched: Date.now() };
      })
      .addCase(fetchWorkspace.rejected, (state, action) => {
        const { path, message } = action.payload as { path: string; message: string };
        state.details[path] = { data: null, status: 'failed', error: message, lastFetched: null };
      })

      .addCase(removeWorkspaceAndCleanup.fulfilled, (state, action) => {
        const path = action.payload;
        state.entries = state.entries.filter(e => e.path !== path);
        delete state.details[path];
        localStorage.setItem(WS_KEY, JSON.stringify(state.entries));
        if (state.selected === path) {
          state.selected = state.entries[0]?.path ?? null;
        }
      })

      // Clear workspace list whenever the session ends — explicit logout or
      // token expiry — so a subsequent user on the same browser cannot see
      // workspace paths they don't own.
      .addCase(logout, (state) => {
        state.entries = [];
        state.details = {};
        state.selected = null;
        localStorage.removeItem(WS_KEY);
      })
      .addCase(hydrateUser.rejected, (state) => {
        state.entries = [];
        state.details = {};
        state.selected = null;
        localStorage.removeItem(WS_KEY);
      });
  },
});

export const { addWorkspace, removeWorkspace, setSelected } = workspacesSlice.actions;
export const workspacesReducer = workspacesSlice.reducer;

/**
 * Setup slice — tracks whether the instance has been bootstrapped (has its first
 * user). The SetupGate router guard gates the entire app on `initialized`, which
 * stays null until {@link checkSetup} resolves so no page flashes before routing
 * is decided.
 */
import { createSlice, createAsyncThunk } from '@reduxjs/toolkit';
import { getSetupStatus } from '../api/bff';

export interface SetupState {
  // null while the first-run check is in flight or unresolved — the SetupGate
  // waits on this rather than flashing a page before we know which way to route.
  initialized: boolean | null;
  // Whether registration is invite-only. Surfaced to the public signup page so it
  // can show invite-only messaging. Defaults false until the check resolves.
  inviteOnly: boolean;
}

const initialState: SetupState = { initialized: null, inviteOnly: false };

/**
 * Resolve whether the instance has any users yet. Retries a few times (each attempt
 * time-bounded) so a transient failure doesn't decide routing prematurely — most
 * importantly the brief window on a fresh deploy before conductor has polled the
 * /setup/status route into its table, during which a fail-open-to-true would route the
 * first operator to /login (where no account exists) instead of /setup.
 *
 * After the retries are exhausted it fails OPEN: any persistent error resolves to
 * initialized=true, so a misconfiguration never traps users on the setup page — the
 * normal login flow stays reachable, and a reload re-checks once upstream recovers.
 */
const SETUP_CHECK_ATTEMPTS = 8;
const SETUP_RETRY_DELAY_MS = 2000;

export const checkSetup = createAsyncThunk('setup/check', async () => {
  for (let i = 0; i < SETUP_CHECK_ATTEMPTS; i++) {
    try {
      const { initialized, invite_only } = await getSetupStatus();
      return { initialized, inviteOnly: !!invite_only };
    } catch {
      if (i < SETUP_CHECK_ATTEMPTS - 1) {
        await new Promise((r) => setTimeout(r, SETUP_RETRY_DELAY_MS));
      }
    }
  }
  return { initialized: true, inviteOnly: false };
});

const setupSlice = createSlice({
  name: 'setup',
  initialState,
  reducers: {
    /** Flip to initialized after a successful first-run signup so the gate releases immediately without waiting for a re-fetch. */
    markInitialized(state) {
      state.initialized = true;
    },
  },
  extraReducers(builder) {
    builder
      .addCase(checkSetup.fulfilled, (state, action) => {
        state.initialized = action.payload.initialized;
        state.inviteOnly = action.payload.inviteOnly;
      })
      .addCase(checkSetup.rejected, (state) => {
        state.initialized = true;
      });
  },
});

export const { markInitialized } = setupSlice.actions;
export const setupReducer = setupSlice.reducer;

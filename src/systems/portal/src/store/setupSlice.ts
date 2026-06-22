import { createSlice, createAsyncThunk } from '@reduxjs/toolkit';
import { getSetupStatus } from '../api/bff';

export interface SetupState {
  // null while the first-run check is in flight or unresolved — the SetupGate
  // waits on this rather than flashing a page before we know which way to route.
  initialized: boolean | null;
}

const initialState: SetupState = { initialized: null };

// Resolves whether the instance has any users yet. Fails OPEN: any error (the
// status route not yet known to conductor, a network blip, an unexpected status)
// resolves to initialized=true, so a misconfiguration never traps users on the
// setup page — the normal login flow stays reachable.
export const checkSetup = createAsyncThunk('setup/check', async () => {
  try {
    const { initialized } = await getSetupStatus();
    return initialized;
  } catch {
    return true;
  }
});

const setupSlice = createSlice({
  name: 'setup',
  initialState,
  reducers: {
    // Flipped after a successful first-run signup so the gate releases immediately
    // without waiting for a re-fetch.
    markInitialized(state) {
      state.initialized = true;
    },
  },
  extraReducers(builder) {
    builder
      .addCase(checkSetup.fulfilled, (state, action) => {
        state.initialized = action.payload;
      })
      .addCase(checkSetup.rejected, (state) => {
        state.initialized = true;
      });
  },
});

export const { markInitialized } = setupSlice.actions;
export const setupReducer = setupSlice.reducer;

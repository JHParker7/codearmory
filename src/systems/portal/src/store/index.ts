/**
 * Redux store wiring. Combines the three feature slices — auth (session, user,
 * permission gates, registered services), workspaces (blueprints state list), and
 * setup (first-run status) — and exports the inferred RootState/AppDispatch types
 * the typed hooks build on.
 */
import { configureStore } from '@reduxjs/toolkit';
import { authReducer } from './authSlice';
import { workspacesReducer } from './workspacesSlice';
import { setupReducer } from './setupSlice';

export const store = configureStore({
  reducer: {
    auth: authReducer,
    workspaces: workspacesReducer,
    setup: setupReducer,
  },
});

export type RootState = ReturnType<typeof store.getState>;
export type AppDispatch = typeof store.dispatch;

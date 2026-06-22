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

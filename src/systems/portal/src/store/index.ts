import { configureStore } from '@reduxjs/toolkit';
import { authReducer } from './authSlice';
import { workspacesReducer } from './workspacesSlice';

export const store = configureStore({
  reducer: {
    auth: authReducer,
    workspaces: workspacesReducer,
  },
});

export type RootState = ReturnType<typeof store.getState>;
export type AppDispatch = typeof store.dispatch;

/**
 * SPA entrypoint. Applies the persisted theme before first paint, kicks off the
 * two startup checks the router gates on — first-run setup status and (if a
 * stored session exists) user hydration — then mounts the app inside the Redux
 * Provider.
 */
import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import './nerdfont.css';
import { Provider } from 'react-redux';
import { store } from './store';
import { hydrateUser } from './store/authSlice';
import { checkSetup } from './store/setupSlice';
import { App } from './App';
import { applyTheme, getStoredTheme } from './theme';

// Apply the persisted theme to :root before first paint so there is no flash.
applyTheme(getStoredTheme());

// Resolve first-run setup state before routing — when the instance has no users
// the SetupGate funnels everything to /setup. Dispatched unconditionally (the
// check is public and cheap) and in parallel with user hydration below.
store.dispatch(checkSetup());

// Kick off user hydration if a stored session exists.
// authSlice initialState reads localStorage synchronously; status will be
// 'loading' when a token is present, so we dispatch here before first render.
if (store.getState().auth.status === 'loading') {
  store.dispatch(hydrateUser());
}

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <Provider store={store}>
      <App />
    </Provider>
  </StrictMode>,
);

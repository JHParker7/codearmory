import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { Provider } from 'react-redux';
import { store } from './store';
import { hydrateUser } from './store/authSlice';
import { App } from './App';
import { applyTheme, getStoredTheme } from './theme';

// Apply the persisted theme to :root before first paint so there is no flash.
applyTheme(getStoredTheme());

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

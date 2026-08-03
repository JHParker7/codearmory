/**
 * Auth slice — the session source of truth for the SPA. Holds the user id, the
 * hydrated user object, and two derived gating layers the UI reads: `permissions`
 * (per-action allow map for admin nav) and `registeredServices` (which modules are
 * routable).
 *
 * ## Where the credential lives
 *
 * NOT in localStorage. Gatekeeper issues the session as an HttpOnly, Secure,
 * SameSite=Strict `armory_session` cookie at login, and conductor accepts that cookie
 * on every non-public route (it converts it to a bearer before forwarding). The cookie
 * is therefore the credential, and JavaScript cannot read it — which is the entire
 * point: the SPA previously mirrored the same JWT into `localStorage.ca_token`, so any
 * XSS could lift a full session token good against every service and use it from
 * anywhere. Persisting it there forfeited the protection the HttpOnly flag exists to
 * give.
 *
 * `token` below is an in-memory copy, kept only for the tab that performed the login
 * (it is "" after a reload). It is never required: `req` simply omits the Authorization
 * header when it is empty and the cookie authenticates the call. It is retained because
 * dozens of call sites pass it through, and passing "" is harmless.
 *
 * ## What survives a reload
 *
 * Only the USER ID (`ca_uid`), which is not a credential — it already appears in the
 * path of most API calls. On startup it seeds `userId` and the app fetches the user
 * with the cookie; a 401 means the session is genuinely gone and the app falls back to
 * the login screen. Nothing an attacker could replay is written to storage.
 */
import { createSlice, createAsyncThunk } from '@reduxjs/toolkit';
import { getUser, updateUser, login as apiLogin, logout as apiLogout, signup as apiSignup, checkPermission, listRegisteredServices } from '../api/bff';
import type { User, SignupPayload } from '../api/bff';
import { decodeUserId } from '../utils';

/**
 * The persisted user id. Deliberately not the token: a user id is an identifier, not a
 * credential — holding it grants nothing without the HttpOnly cookie.
 */
const USER_ID_KEY = 'ca_uid';

/** Legacy key. Removed on sight so a token persisted by an older build does not linger. */
const LEGACY_TOKEN_KEY = 'ca_token';

export interface AuthState {
  /**
   * In-memory bearer for the tab that logged in; "" after a reload, when the
   * HttpOnly cookie is the only credential. Never null, so the many call sites that
   * forward it keep their `string` type — `req` omits the header when it is empty.
   * Do NOT treat this as "is the user signed in": use `userId` for that.
   */
  token: string;
  userId: string | null;
  user: User | null;
  status: 'idle' | 'loading' | 'succeeded' | 'failed';
  error: string | null;
  permissions: Record<string, boolean> | null;
  // Names of platform services currently registered/routable in conductor (the
  // live routing table — manifest- and builder-registered alike). The sidebar
  // shows a module only when its backing service appears here. null = unresolved
  // (error or in flight); combined with servicesResolved this fails OPEN — a
  // module is hidden only once we hold a resolved, non-null list that omits it.
  registeredServices: string[] | null;
  // name → ui_path for registered services that advertise an embedded mini-portal.
  // Drives the shell's iframe nav/routes; empty for services that ship no UI.
  serviceUiPaths: Record<string, string>;
  servicesResolved: boolean;
}

/**
 * Seed userId from storage so a reload can resume the session on the cookie alone.
 *
 * `status: 'loading'` matters: it is what stops the route guard bouncing to /login
 * before the session has been checked. The id alone proves nothing — hydrateUser
 * immediately verifies it against the server, and a 401 clears it.
 */
function readStoredSession() {
  // Drop any token left by a build that persisted one. Doing this unconditionally,
  // and on every start, is what makes the migration self-cleaning rather than leaving
  // a live credential in the storage of everyone who upgraded.
  localStorage.removeItem(LEGACY_TOKEN_KEY);

  const userId = localStorage.getItem(USER_ID_KEY);
  if (!userId) return { token: '', userId: null, status: 'idle' as const };
  return { token: '', userId, status: 'loading' as const };
}

const stored = readStoredSession();

const initialState: AuthState = {
  ...stored,
  user: null,
  error: null,
  permissions: null,
  registeredServices: null,
  serviceUiPaths: {},
  servicesResolved: false,
};

// ── Thunks ────────────────────────────────────────────────────────────────────

/** Hydrate the user object for a stored session at startup; rejects (clearing the token) if the session is gone. */
export const hydrateUser = createAsyncThunk(
  'auth/hydrateUser',
  async (_, { getState, rejectWithValue }) => {
    const { token, userId } = (getState() as { auth: AuthState }).auth;
    // Gated on userId ALONE. After a reload there is no in-memory token and the
    // cookie is the credential, so requiring one here would reject every resumed
    // session and log the user out on every refresh.
    if (!userId) return rejectWithValue({ transient: false });
    try {
      return await getUser(token, userId);
    } catch (err: unknown) {
      // Only a genuine auth rejection (401/403 — the token is gone/expired) ends
      // the session. Anything else — offline, request timeout, a 5xx from an
      // upstream blip — is transient: we keep the session so a dropped connection
      // doesn't dump the user (and their in-progress work) back to the sign-in
      // screen. AppLayout re-hydrates once the connection returns.
      const status = (err as { status?: number }).status;
      const transient = status !== 401 && status !== 403;
      return rejectWithValue({ transient });
    }
  },
);

/** Log in with email/password, persist the returned token, and fetch the user in one round trip. Rejects with {status, message}. */
export const loginAndFetch = createAsyncThunk(
  'auth/loginAndFetch',
  async ({ email, password }: { email: string; password: string }, { rejectWithValue }) => {
    try {
      const { token } = await apiLogin(email, password);
      const userId = decodeUserId(token);
      if (!userId) return rejectWithValue({ status: 0, message: 'invalid token' });
      // The id, never the token: login already set the HttpOnly cookie, which is the
      // credential from here on.
      localStorage.setItem(USER_ID_KEY, userId);
      const user = await getUser(token, userId);
      return { token, userId, user };
    } catch (err: unknown) {
      const e = err as { status?: number; message?: string };
      return rejectWithValue({ status: e.status ?? 0, message: e.message ?? 'login failed' });
    }
  },
);

/**
 * Sign up, then immediately log in. If the account is created but the follow-up
 * login or user-fetch fails, the distinct rejection status lets the caller tell
 * "signup failed" from "created but couldn't sign you in" apart.
 */
export const signupAndLogin = createAsyncThunk(
  'auth/signupAndLogin',
  async (payload: SignupPayload, { rejectWithValue }) => {
    try {
      await apiSignup(payload);
    } catch (err: unknown) {
      const e = err as { status?: number; message?: string };
      return rejectWithValue({ status: e.status ?? 0, message: e.message ?? 'signup failed' });
    }
    try {
      const { token } = await apiLogin(payload.email, payload.password);
      const userId = decodeUserId(token);
      if (!userId) return rejectWithValue({ status: 0, message: 'invalid token' });
      localStorage.setItem(USER_ID_KEY, userId);
      let user: User | null = null;
      try {
        user = await getUser(token, userId);
      } catch {
        // User-fetch failed but the token is valid — proceed without user data.
        // AppLayout will dispatch hydrateUser to load it after navigation.
      }
      return { token, userId, user };
    } catch {
      // Login itself failed — account was created but we have no token.
      return rejectWithValue({ status: -1, message: 'auto-login failed' });
    }
  },
);

/**
 * The (service, action, resource) probes whose results drive which admin/privileged
 * UI affordances render. Each is checked once at login via {@link hydratePermissions}
 * and cached in `state.permissions` under a `service:action` key.
 */
const PERMISSION_GATES = [
  { service: 'gatekeeper', action: 'listAuditLog',       resource: 'gatekeeper/audit-logs' },
  { service: 'gatekeeper', action: 'listPermissionCheck', resource: 'gatekeeper/permission-checks' },
  { service: 'gatekeeper', action: 'listUser',            resource: 'gatekeeper/users' },
  { service: 'gatekeeper', action: 'listRole',            resource: 'gatekeeper/roles' },
  { service: 'gatekeeper', action: 'createPermission',    resource: 'gatekeeper/permissions' },
  { service: 'gatekeeper', action: 'listSecrets',         resource: 'gatekeeper/secrets' },
  { service: 'gatekeeper', action: 'listTeam',            resource: 'gatekeeper/teams' },
  { service: 'gatekeeper', action: 'listOrg',             resource: 'gatekeeper/orgs' },
  { service: 'gatekeeper', action: 'listInvite',          resource: 'gatekeeper/invites' },
  { service: 'gatekeeper', action: 'listSignupAllowlist', resource: 'gatekeeper/signup-allowlist' },
  { service: 'gatekeeper', action: 'listSPR',             resource: 'gatekeeper/service-permission-requests' },
  { service: 'forge',      action: 'createRunnerClass',   resource: 'forge/runner-classes' },
  { service: 'forge',      action: 'createRuntimeBackend', resource: 'forge/runtime-backends' },
  { service: 'containers', action: 'deleteManifest',      resource: 'containers/repositories/*' },
] as const;

/** Resolve every {@link PERMISSION_GATES} probe (plus the builder admin gate) into a `service:action → boolean` map; a failed check resolves to false. */
export const hydratePermissions = createAsyncThunk(
  'auth/hydratePermissions',
  async (_, { getState }) => {
    // No token guard: after a reload the cookie authenticates these probes, and
    // bailing out here would leave every admin affordance hidden until re-login.
    const { token } = (getState() as { auth: AuthState }).auth;
    // Builder is a system-admin-only global control plane: the configure grant is
    // checked against the single "default" baseline, never the caller's org. Only
    // the wildcard admin matches codearmory/builder/orgs/default, so this gate alone surfaces
    // builder/ for the system admin and hides it for everyone else.
    const gates: { service: string; action: string; resource: string }[] = [
      ...PERMISSION_GATES,
      { service: 'builder', action: 'configureOrgService', resource: 'codearmory/builder/orgs/default' },
    ];
    const results = await Promise.all(
      gates.map(async g => {
        const key = `${g.service}:${g.action}`;
        try {
          const { authorized } = await checkPermission(token, g.service, g.action, g.resource);
          return [key, authorized] as [string, boolean];
        } catch {
          return [key, false] as [string, boolean];
        }
      }),
    );
    return Object.fromEntries(results) as Record<string, boolean>;
  },
);

/**
 * Resolve the set of services currently registered/routable in conductor, so the
 * sidebar can show only modules that are actually deployed. Returns null on any
 * error so the UI fails OPEN (shows everything) rather than hiding a live module.
 */
export const hydrateRegisteredServices = createAsyncThunk(
  'auth/hydrateRegisteredServices',
  async (_, { getState }) => {
    const { token } = (getState() as { auth: AuthState }).auth;
    try {
      const services = await listRegisteredServices(token);
      const uiPaths: Record<string, string> = {};
      for (const s of services) {
        if (s.ui_path) uiPaths[s.name] = s.ui_path;
      }
      return { names: services.map(s => s.name), uiPaths };
    } catch {
      return null;
    }
  },
);

/** Persist edits to the current user's profile and replace the cached user on success. Rejects with the error message. */
export const saveUser = createAsyncThunk(
  'auth/saveUser',
  async (
    { token, userId, patch }: {
      token: string;
      userId: string;
      patch: { email: string; username: string; password?: string; firstname?: string; lastname?: string };
    },
    { rejectWithValue },
  ) => {
    try {
      return await updateUser(token, userId, patch);
    } catch (err: unknown) {
      const e = err as { message?: string };
      return rejectWithValue(e.message ?? 'update failed');
    }
  },
);

// ── Slice ─────────────────────────────────────────────────────────────────────

const authSlice = createSlice({
  name: 'auth',
  initialState,
  reducers: {
    /** Clear the session and all derived state (token, user, permissions, services) and drop the persisted token. */
    logout(state) {
      localStorage.removeItem(USER_ID_KEY);
      localStorage.removeItem(LEGACY_TOKEN_KEY);
      state.token = '';
      state.userId = null;
      state.user = null;
      state.status = 'idle';
      state.error = null;
      state.permissions = null;
      state.registeredServices = null;
      state.serviceUiPaths = {};
      state.servicesResolved = false;
    },
  },
  extraReducers(builder) {
    builder
      .addCase(hydrateUser.fulfilled, (state, action) => {
        state.user = action.payload;
        state.status = 'succeeded';
      })
      .addCase(hydrateUser.rejected, (state, action) => {
        if ((action.payload as { transient?: boolean } | undefined)?.transient) {
          // Transient failure (offline / timeout / 5xx): keep the token and let
          // the app render. AppLayout re-dispatches hydrateUser on mount and when
          // the browser comes back online, filling in the user once reachable.
          state.status = 'succeeded';
          return;
        }
        // A real 401/403: the session is gone, so drop the resumable id too.
        localStorage.removeItem(USER_ID_KEY);
        state.token = '';
        state.userId = null;
        state.status = 'idle';
      })

      .addCase(loginAndFetch.pending, (state) => {
        state.status = 'loading';
        state.error = null;
      })
      .addCase(loginAndFetch.fulfilled, (state, action) => {
        state.token = action.payload.token;
        state.userId = action.payload.userId;
        state.user = action.payload.user;
        state.status = 'succeeded';
        state.error = null;
      })
      .addCase(loginAndFetch.rejected, (state, action) => {
        state.status = 'failed';
        state.error = (action.payload as { message: string })?.message ?? 'login failed';
      })

      .addCase(signupAndLogin.pending, (state) => {
        state.status = 'loading';
        state.error = null;
      })
      .addCase(signupAndLogin.fulfilled, (state, action) => {
        state.token = action.payload.token;
        state.userId = action.payload.userId;
        state.user = action.payload.user;
        state.status = 'succeeded';
        state.error = null;
      })
      .addCase(signupAndLogin.rejected, (state, action) => {
        state.status = 'failed';
        state.error = (action.payload as { message: string })?.message ?? 'signup failed';
      })

      .addCase(saveUser.fulfilled, (state, action) => {
        state.user = action.payload;
      })
      .addCase(hydratePermissions.fulfilled, (state, action) => {
        state.permissions = action.payload;
      })
      .addCase(hydrateRegisteredServices.fulfilled, (state, action) => {
        // Resolved (success or handled error). A null payload (error) leaves the
        // sidebar failing open; a non-null list lets it hide unregistered modules
        // and render an iframe nav entry for each service advertising a ui_path.
        state.registeredServices = action.payload ? action.payload.names : null;
        state.serviceUiPaths = action.payload ? action.payload.uiPaths : {};
        state.servicesResolved = true;
      });
  },
});

export const { logout } = authSlice.actions;

/**
 * Log out properly: ask the server to revoke the session and expire its cookie, then
 * clear local state.
 *
 * The `logout` reducer on its own only forgets the session in this tab — the session
 * row stays active until it expires and the HttpOnly cookie stays in the browser, so
 * the credential still authenticates afterwards. Dispatch THIS from UI logout, not the
 * bare reducer.
 *
 * The server call is best-effort: if it fails (offline, already-expired token) the user
 * must still end up logged out locally, so the reducer runs either way.
 */
export const logoutSession = createAsyncThunk('auth/logoutSession', async (_arg: void, { getState, dispatch }) => {
  const { token } = (getState() as { auth: AuthState }).auth;
  try {
    await apiLogout(token ?? undefined);
  } catch {
    // Revocation is unavailable; clearing local state below is still correct.
  }
  dispatch(logout());
});
export const authReducer = authSlice.reducer;

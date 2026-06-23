import { createSlice, createAsyncThunk } from '@reduxjs/toolkit';
import { getUser, updateUser, login as apiLogin, signup as apiSignup, checkPermission, listRegisteredServices } from '../api/bff';
import type { User, SignupPayload } from '../api/bff';
import { decodeUserId } from '../utils';

const TOKEN_KEY = 'ca_token';

export interface AuthState {
  token: string | null;
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
  servicesResolved: boolean;
}

function readStoredToken() {
  const token = localStorage.getItem(TOKEN_KEY);
  if (!token) return { token: null, userId: null, status: 'idle' as const };
  const userId = decodeUserId(token);
  if (!userId) {
    localStorage.removeItem(TOKEN_KEY);
    return { token: null, userId: null, status: 'idle' as const };
  }
  return { token, userId, status: 'loading' as const };
}

const stored = readStoredToken();

const initialState: AuthState = {
  ...stored,
  user: null,
  error: null,
  permissions: null,
  registeredServices: null,
  servicesResolved: false,
};

// ── Thunks ────────────────────────────────────────────────────────────────────

// Called at startup when a stored token is found, to hydrate the user object.
export const hydrateUser = createAsyncThunk(
  'auth/hydrateUser',
  async (_, { getState, rejectWithValue }) => {
    const { token, userId } = (getState() as { auth: AuthState }).auth;
    if (!token || !userId) return rejectWithValue('no stored session');
    try {
      return await getUser(token, userId);
    } catch {
      return rejectWithValue('session expired');
    }
  },
);

export const loginAndFetch = createAsyncThunk(
  'auth/loginAndFetch',
  async ({ email, password }: { email: string; password: string }, { rejectWithValue }) => {
    try {
      const { token } = await apiLogin(email, password);
      const userId = decodeUserId(token);
      if (!userId) return rejectWithValue({ status: 0, message: 'invalid token' });
      localStorage.setItem(TOKEN_KEY, token);
      const user = await getUser(token, userId);
      return { token, userId, user };
    } catch (err: unknown) {
      const e = err as { status?: number; message?: string };
      return rejectWithValue({ status: e.status ?? 0, message: e.message ?? 'login failed' });
    }
  },
);

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
      localStorage.setItem(TOKEN_KEY, token);
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

const PERMISSION_GATES = [
  { service: 'gatekeeper', action: 'listAuditLog',       resource: 'gatekeeper/audit-logs' },
  { service: 'gatekeeper', action: 'listUser',            resource: 'gatekeeper/users' },
  { service: 'gatekeeper', action: 'listRole',            resource: 'gatekeeper/roles' },
  { service: 'gatekeeper', action: 'createPermission',    resource: 'gatekeeper/permissions' },
  { service: 'gatekeeper', action: 'listSecrets',         resource: 'gatekeeper/secrets' },
  { service: 'gatekeeper', action: 'listTeam',            resource: 'gatekeeper/teams' },
  { service: 'gatekeeper', action: 'listOrg',             resource: 'gatekeeper/orgs' },
  { service: 'gatekeeper', action: 'listInvite',          resource: 'gatekeeper/invites' },
  { service: 'gatekeeper', action: 'listSPR',             resource: 'gatekeeper/service-permission-requests' },
  { service: 'forge',      action: 'createRunnerClass',   resource: 'forge/runner-classes' },
  { service: 'containers', action: 'deleteManifest',      resource: 'containers/repositories/*' },
] as const;

export const hydratePermissions = createAsyncThunk(
  'auth/hydratePermissions',
  async (_, { getState }) => {
    const { token } = (getState() as { auth: AuthState }).auth;
    if (!token) return {};
    // Builder is a system-admin-only global control plane: the configure grant is
    // checked against the single "default" baseline, never the caller's org. Only
    // the wildcard admin matches builder/orgs/default, so this gate alone surfaces
    // builder/ for the system admin and hides it for everyone else.
    const gates: { service: string; action: string; resource: string }[] = [
      ...PERMISSION_GATES,
      { service: 'builder', action: 'configureOrgService', resource: 'builder/orgs/default' },
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

// Resolves the set of services currently registered/routable in conductor, so the
// sidebar can show only modules that are actually deployed. Returns null on any
// error so the UI fails OPEN (shows everything) rather than hiding a live module.
export const hydrateRegisteredServices = createAsyncThunk(
  'auth/hydrateRegisteredServices',
  async (_, { getState }) => {
    const { token } = (getState() as { auth: AuthState }).auth;
    if (!token) return null;
    try {
      const services = await listRegisteredServices(token);
      return services.map(s => s.name);
    } catch {
      return null;
    }
  },
);

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
    logout(state) {
      localStorage.removeItem(TOKEN_KEY);
      state.token = null;
      state.userId = null;
      state.user = null;
      state.status = 'idle';
      state.error = null;
      state.permissions = null;
      state.registeredServices = null;
      state.servicesResolved = false;
    },
  },
  extraReducers(builder) {
    builder
      .addCase(hydrateUser.fulfilled, (state, action) => {
        state.user = action.payload;
        state.status = 'succeeded';
      })
      .addCase(hydrateUser.rejected, (state) => {
        localStorage.removeItem(TOKEN_KEY);
        state.token = null;
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
        // sidebar failing open; a non-null list lets it hide unregistered modules.
        state.registeredServices = action.payload;
        state.servicesResolved = true;
      });
  },
});

export const { logout } = authSlice.actions;
export const authReducer = authSlice.reducer;

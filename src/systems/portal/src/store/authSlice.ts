import { createSlice, createAsyncThunk, type PayloadAction } from '@reduxjs/toolkit';
import { getUser, updateUser, login as apiLogin, signup as apiSignup, checkPermission, listOrgServices } from '../api/bff';
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
  // Names of platform services this org has disabled (from the builder control
  // plane). null = not yet resolved (or unresolvable) → show everything. The
  // sidebar hides any module whose service appears here. Fails open by design.
  disabledServices: string[] | null;
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
  disabledServices: null,
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
    const { token, user } = (getState() as { auth: AuthState }).auth;
    if (!token) return {};
    // The builder configure grant is scoped to the caller's own org, so its
    // resource is only knowable once the user (and org_id) is hydrated. Append it
    // dynamically; gatekeeper prepends the username and matches the org id.
    const gates: { service: string; action: string; resource: string }[] = [...PERMISSION_GATES];
    if (user?.org_id) {
      gates.push({ service: 'builder', action: 'configureOrgService', resource: `builder/orgs/${user.org_id}` });
    }
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

// Resolves which platform services the caller's org has disabled, so the sidebar
// can hide them. Fails open: no org, or any error, yields [] (show everything).
export const hydrateServices = createAsyncThunk(
  'auth/hydrateServices',
  async (_, { getState }) => {
    const { token, user } = (getState() as { auth: AuthState }).auth;
    const orgId = user?.org_id;
    if (!token || !orgId) return [] as string[];
    try {
      const services = await listOrgServices(token, orgId);
      return services.filter(s => !s.core && !s.enabled).map(s => s.service);
    } catch {
      return [] as string[];
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
      state.disabledServices = null;
    },
    // Replace the disabled-services set directly. Used by the builder page after a
    // mutation at the caller's own org scope, where it already holds the fresh list
    // and a re-fetch (hydrateServices) would be a redundant identical request.
    setDisabledServices(state, action: PayloadAction<string[]>) {
      state.disabledServices = action.payload;
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
      .addCase(hydrateServices.fulfilled, (state, action) => {
        state.disabledServices = action.payload;
      });
  },
});

export const { logout, setDisabledServices } = authSlice.actions;
export const authReducer = authSlice.reducer;

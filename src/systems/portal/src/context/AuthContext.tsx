import { createContext, useCallback, useContext, useEffect, useState } from 'react';
import type { ReactNode } from 'react';
import { getUser } from '../api/bff';
import type { User } from '../api/bff';
import { decodeUserId } from '../utils';

interface AuthState {
  token: string | null;
  userId: string | null;
  user: User | null;
  loading: boolean;
}

interface AuthCtx extends AuthState {
  login: (token: string) => Promise<void>;
  logout: () => void;
  refreshUser: () => Promise<void>;
}

const Ctx = createContext<AuthCtx | null>(null);

const TOKEN_KEY = 'ca_token';

export function AuthProvider({ children }: { children: ReactNode }) {
  const [state, setState] = useState<AuthState>({
    token: null,
    userId: null,
    user: null,
    loading: true,
  });

  const loadUser = useCallback(async (token: string, userId: string) => {
    try {
      const user = await getUser(token, userId);
      setState((s) => ({ ...s, user, loading: false }));
    } catch {
      localStorage.removeItem(TOKEN_KEY);
      setState({ token: null, userId: null, user: null, loading: false });
    }
  }, []);

  useEffect(() => {
    const stored = localStorage.getItem(TOKEN_KEY);
    if (!stored) {
      setState((s) => ({ ...s, loading: false }));
      return;
    }
    const userId = decodeUserId(stored);
    if (!userId) {
      localStorage.removeItem(TOKEN_KEY);
      setState((s) => ({ ...s, loading: false }));
      return;
    }
    setState((s) => ({ ...s, token: stored, userId }));
    loadUser(stored, userId);
  }, [loadUser]);

  const login = useCallback(async (token: string) => {
    const userId = decodeUserId(token);
    if (!userId) throw new Error('invalid token');
    localStorage.setItem(TOKEN_KEY, token);
    setState({ token, userId, user: null, loading: true });
    await loadUser(token, userId);
  }, [loadUser]);

  const logout = useCallback(() => {
    localStorage.removeItem(TOKEN_KEY);
    setState({ token: null, userId: null, user: null, loading: false });
  }, []);

  const refreshUser = useCallback(async () => {
    if (!state.token || !state.userId) return;
    await loadUser(state.token, state.userId);
  }, [state.token, state.userId, loadUser]);

  return (
    <Ctx.Provider value={{ ...state, login, logout, refreshUser }}>
      {children}
    </Ctx.Provider>
  );
}

export function useAuth() {
  const ctx = useContext(Ctx);
  if (!ctx) throw new Error('useAuth must be used inside AuthProvider');
  return ctx;
}

import {
  createContext,
  useCallback,
  useContext,
  useMemo,
  useState,
  type ReactNode,
} from 'react';
import client from '../api/client';
import {
  clearSession,
  readAccessToken,
  readRefreshToken,
  readRoles,
  readUser,
  saveIdentity,
  saveTokens,
  type SessionUser,
} from './session';

/**
 * Who the operator is, to this browser.
 *
 * The state is a mirror of what is in localStorage (see auth/session.ts), so a
 * reload, a second tab, and a deep link all start where the last request left
 * off. The server stays the authority: a stored token that the server refuses is
 * cleared by the client's renewal path, not by a guess made here about expiry.
 *
 * Renewal is deliberately absent from this surface. `api/client.ts` owns the
 * 401 → refresh → retry path, because the request that got the 401 is the one
 * that has to be replayed, and because a second caller of refresh — this hook
 * on a timer, or a second tab — would present a token the first one already
 * rotated and trip the server's reuse detection, which drops every live session
 * of the account. A background timer is a reuse-detection machine here.
 */

/** `POST /api/auth/login` answers the pair plus the account (api/auth.go Login). */
interface LoginResponse {
  access_token: string;
  refresh_token: string;
  token_type: string;
  expires_in: number;
  user: SessionUser;
}

export interface AuthValue {
  user: SessionUser | null;
  roles: string[];
  /** This browser holds a token. Not "the token is valid" — only the server knows that. */
  isAuthenticated: boolean;
  login(username: string, password: string): Promise<void>;
  logout(): Promise<void>;
  /** Adopt the pair the SSO callback arrived with, then ask the server who it is. */
  adoptTokens(accessToken: string, refreshToken: string): Promise<void>;
  hasRole(role: string): boolean;
  hasAnyRole(required: string[]): boolean;
}

const AuthContext = createContext<AuthValue | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<SessionUser | null>(readUser);
  const [roles, setRoles] = useState<string[]>(readRoles);
  const [isAuthenticated, setIsAuthenticated] = useState(readAccessToken() !== null);

  const login = useCallback(async (username: string, password: string) => {
    const { data } = await client.post<LoginResponse>('/auth/login', { username, password });
    saveTokens(data.access_token, data.refresh_token);
    // Roles live inside the user row (api/handlers.go userView). There is no
    // top-level `roles` on this response to read.
    saveIdentity(data.user, data.user.roles);
    setUser(data.user);
    setRoles(data.user.roles);
    setIsAuthenticated(true);
  }, []);

  const logout = useCallback(async () => {
    // Tell the server first. Clearing localStorage alone leaves a refresh token
    // valid for 14 days on a machine that still holds it; `POST /api/auth/logout`
    // with the token closes that one session (api/auth.go Logout).
    const refreshToken = readRefreshToken();
    try {
      await client.post('/auth/logout', refreshToken ? { refresh_token: refreshToken } : {});
    } catch {
      // The local session ends regardless. A console that stays signed in
      // because the server was unreachable would be lying about the door.
    }
    clearSession();
    setUser(null);
    setRoles([]);
    setIsAuthenticated(false);
  }, []);

  const adoptTokens = useCallback(async (accessToken: string, refreshToken: string) => {
    // The SSO handoff arrives as a fragment with tokens and no account
    // (api/sso_routes.go). `GET /api/auth/profile` is the route that answers
    // which account this is, roles included — it is what decides which tabs to
    // draw.
    saveTokens(accessToken, refreshToken);
    setIsAuthenticated(true);
    try {
      const { data } = await client.get<SessionUser>('/auth/profile');
      saveIdentity(data, data.roles);
      setUser(data);
      setRoles(data.roles);
    } catch {
      // The tokens were handed over by the operator's own identity provider, so
      // a failed profile read is a network blip, not a refusal. They are kept:
      // dropping them here would strand the handoff with nothing to retry with,
      // and the next request will re-answer who this is.
    }
  }, []);

  const value = useMemo<AuthValue>(
    () => ({
      user,
      roles,
      isAuthenticated,
      login,
      logout,
      adoptTokens,
      hasRole: (role: string) => roles.includes(role),
      hasAnyRole: (required: string[]) => required.some((r) => roles.includes(r)),
    }),
    [user, roles, isAuthenticated, login, logout, adoptTokens],
  );

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthValue {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error('useAuth must be used within AuthProvider');
  return ctx;
}

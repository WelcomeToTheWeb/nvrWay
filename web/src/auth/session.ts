/**
 * The session, and the only place its key names are written.
 *
 * Four keys, named by the plan and by nothing else: `nvr_access_token`,
 * `nvr_refresh_token`, `nvr_user`, `nvr_roles`. The client clears them when a
 * renewal fails, the auth context writes them on sign-in, and the SSO callback
 * writes them from a URL fragment — three writers, so the names live in one
 * module rather than in three string literals that can drift apart.
 *
 * The refresh token is kept because the server's is stateful: `POST
 * /api/auth/refresh` closes the token it is given and issues a replacement, and
 * presenting a closed one is reuse, which drops every live session of the
 * account. A client that forgets to update it is not lazy, it is a logout
 * machine.
 */

export const ACCESS_TOKEN_KEY = 'nvr_access_token';
export const REFRESH_TOKEN_KEY = 'nvr_refresh_token';
export const USER_KEY = 'nvr_user';
export const ROLES_KEY = 'nvr_roles';

/**
 * An account as the API answers it (api/handlers.go userView). `roles` is part
 * of the row, not a sibling field: `POST /api/auth/login` and `GET
 * /api/auth/profile` both answer with the user object, and the roles are inside
 * it. Times are RFC 3339 strings here rather than Date objects — the console
 * shows "last login" as the server wrote it, and parsing a timestamp into a
 * local Date is a decision some later surface makes, not this one.
 */
export interface SessionUser {
  id: string;
  username: string;
  name: string;
  email: string;
  provider: string;
  status: string;
  roles: string[];
  created_at: string;
  updated_at: string;
  last_login: string | null;
}

function read(key: string): string | null {
  try {
    return localStorage.getItem(key);
  } catch {
    // A browser that refuses localStorage (private mode with storage blocked)
    // is an unauthenticated browser, not a crash.
    return null;
  }
}

export function readAccessToken(): string | null {
  return read(ACCESS_TOKEN_KEY);
}

export function readRefreshToken(): string | null {
  return read(REFRESH_TOKEN_KEY);
}

/** The stored account, or null if it is absent, corrupt, or not an object. */
export function readUser(): SessionUser | null {
  const raw = read(USER_KEY);
  if (!raw) return null;
  try {
    const parsed: unknown = JSON.parse(raw);
    if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) return null;
    return parsed as SessionUser;
  } catch {
    return null;
  }
}

/** The stored roles, or an empty list. A non-list is treated as no roles. */
export function readRoles(): string[] {
  const raw = read(ROLES_KEY);
  if (!raw) return [];
  try {
    const parsed: unknown = JSON.parse(raw);
    if (!Array.isArray(parsed)) return [];
    return parsed.filter((r): r is string => typeof r === 'string');
  } catch {
    return [];
  }
}

/**
 * Store a renewed pair. Both halves rotate together on the server, so both
 * halves are written together here; a caller that passes only the access token
 * is asking for the reuse-detection failure described above, so it cannot.
 */
export function saveTokens(accessToken: string, refreshToken: string): void {
  localStorage.setItem(ACCESS_TOKEN_KEY, accessToken);
  localStorage.setItem(REFRESH_TOKEN_KEY, refreshToken);
}

export function saveIdentity(user: SessionUser, roles: string[]): void {
  localStorage.setItem(USER_KEY, JSON.stringify(user));
  localStorage.setItem(ROLES_KEY, JSON.stringify(roles));
}

export function clearSession(): void {
  localStorage.removeItem(ACCESS_TOKEN_KEY);
  localStorage.removeItem(REFRESH_TOKEN_KEY);
  localStorage.removeItem(USER_KEY);
  localStorage.removeItem(ROLES_KEY);
}

/** Whether this browser holds any session at all. */
export function hasSession(): boolean {
  return readAccessToken() !== null || readRefreshToken() !== null;
}

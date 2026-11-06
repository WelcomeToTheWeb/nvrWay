import axios, { type AxiosError, type InternalAxiosRequestConfig } from 'axios';
import {
  clearSession,
  hasSession,
  readAccessToken,
  readRefreshToken,
  saveTokens,
} from '../auth/session';

/**
 * The one HTTP client the console uses.
 *
 * It is rooted at `/api` and never names a host: the vite dev/preview proxy and
 * `docker/nginx.conf` both forward `/api` and `/ws`, so the same bundle works
 * against a laptop, a LAN box, or a reverse proxy without a rebuild.
 *
 * Everything below exists because of how the server answers. Three facts drive
 * it (all in server/api/auth.go and server/api/middleware.go):
 *
 *  1. An expired access token is a plain 401. The refresh token is good for 14
 *     days, so a 401 is a renewal opportunity, not an ending.
 *  2. `POST /api/auth/refresh` **rotates**: the token it is given is closed and
 *     the replacement is stored. Presenting a closed token is *reuse*, and reuse
 *     drops **every live session of the account** (401 `refresh token was
 *     already used`, `live_sessions_dropped: N`).
 *  3. A 403 from `RequireRole` is an answer about permissions, and a 401 from
 *     `POST /api/auth/login` is an answer about the credentials just typed.
 *     Neither is about this browser's session.
 *
 * Fact 2 is the one that shapes the design. THE WALL mounts and asks for
 * cameras, events, segments and storage at once; if every 401 posted its own
 * refresh, the first would rotate and the rest would present a closed token, and
 * the operator would be logged out of every device for opening a page. So
 * renewal is single-flight: concurrent 401s share one refresh, and every
 * request carries the rotated pair forward when it succeeds.
 */
const client = axios.create({
  baseURL: '/api',
  headers: { 'Content-Type': 'application/json' },
});

/**
 * Routes that answer about credentials or about the handshake, not about the
 * session this browser holds. A 401 from `POST /api/auth/login` is
 * `{"error":"invalid credentials"}` — the login page has to receive it to say
 * why the sign-in failed, and renewing there would rotate a healthy session for
 * nothing. The list mirrors the public group in api/handlers.go Register.
 */
export const AUTH_EXEMPT_PATHS = ['/auth/status', '/auth/login', '/auth/refresh'] as const;

interface RetriableConfig extends InternalAxiosRequestConfig {
  _retried?: boolean;
}

interface RefreshResponse {
  access_token: string;
  refresh_token: string;
  token_type: string;
  expires_in: number;
}

function isAuthExempt(config: RetriableConfig): boolean {
  const path = (config.url ?? '').split('?')[0];
  return (AUTH_EXEMPT_PATHS as readonly string[]).some((p) => path === p);
}

client.interceptors.request.use((config) => {
  const token = readAccessToken();
  if (token) {
    config.headers.Authorization = `Bearer ${token}`;
  }
  return config;
});

/**
 * The one in-flight renewal. Null when nobody is renewing.
 *
 * Resolves to the new access token, or null when the session cannot be renewed.
 * Callers that arrive while a renewal is running await the same promise, so a
 * page of six panels costs one rotation.
 */
let renewal: Promise<string | null> | null = null;

function renewSession(): Promise<string | null> {
  if (!renewal) {
    renewal = postRefresh().finally(() => {
      renewal = null;
    });
  }
  return renewal;
}

async function postRefresh(): Promise<string | null> {
  const refreshToken = readRefreshToken();
  if (!refreshToken) return null;
  try {
    // Through `client`, so the test harness and any future interceptor see it.
    // `/auth/refresh` is on the exempt list, so its own 401 cannot recurse.
    const { data } = await client.post<RefreshResponse>('/auth/refresh', {
      refresh_token: refreshToken,
    });
    // Both halves rotate on the server; both halves are stored. Keeping only
    // the access token would make the next renewal a replay of a closed token.
    saveTokens(data.access_token, data.refresh_token);
    return data.access_token;
  } catch {
    return null;
  }
}

/**
 * End the session locally and send the operator to the door.
 *
 * Guarded on there still being a session: when six panels discover the same dead
 * session, the first ends it and the other five find nothing left to end, rather
 * than firing six navigations out from under a page.
 */
function endSession(): void {
  if (!hasSession()) return;
  clearSession();
  if (window.location.pathname !== '/login') {
    window.location.replace('/login');
  }
}

client.interceptors.response.use(
  (response) => response,
  async (error: AxiosError) => {
    const config = error.config as RetriableConfig | undefined;
    const status = error.response?.status;

    if (status === 401 && config && !isAuthExempt(config)) {
      if (!config._retried) {
        config._retried = true;
        const access = await renewSession();
        if (access) {
          // Replayed through the client, so the request interceptor attaches
          // the token now in storage — the header is set in exactly one place.
          return client(config);
        }
        endSession();
      } else {
        // A fresh bearer was refused too. The session is over; a second attempt
        // would only be a loop with extra steps.
        endSession();
      }
    }

    return Promise.reject(error);
  },
);

export default client;

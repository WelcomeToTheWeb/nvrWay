import { beforeEach, describe, expect, it, vi } from 'vitest';
import axios, {
  AxiosError,
  type AxiosResponse,
  type InternalAxiosRequestConfig,
} from 'axios';
import client, { AUTH_EXEMPT_PATHS } from './client';
import {
  ACCESS_TOKEN_KEY,
  REFRESH_TOKEN_KEY,
  ROLES_KEY,
  USER_KEY,
} from '../auth/session';

/**
 * The harness is a real axios adapter, not a mock library.
 *
 * The thing under test is the interceptor chain — the request interceptor that
 * attaches the bearer and the response interceptor that renews the session and
 * retries. A mock of `client.request` would test a call nobody makes. A custom
 * adapter sits at the bottom of the real pipeline, so every assertion below is
 * about requests the interceptors actually produced, in the order axios
 * produced them.
 */
interface Scripted {
  status: number;
  data?: unknown;
}

interface Call {
  url: string;
  method: string;
  bearer: string | undefined;
  body: unknown;
}

function harness(script: (call: Call, attempt: number) => Scripted) {
  const calls: Call[] = [];

  const adapter = async (config: InternalAxiosRequestConfig): Promise<AxiosResponse> => {
    const call: Call = {
      url: config.url ?? '',
      method: (config.method ?? 'get').toLowerCase(),
      bearer: config.headers?.Authorization as string | undefined,
      body: config.data ? JSON.parse(String(config.data)) : undefined,
    };
    calls.push(call);
    const attempt = calls.filter((c) => c.url === call.url && c.method === call.method).length;
    const res = script(call, attempt);

    if (res.status >= 200 && res.status < 300) {
      return {
        status: res.status,
        statusText: 'OK',
        data: res.data ?? {},
        headers: {},
        config,
      };
    }
    throw new AxiosError(
      'request refused',
      'ERR_BAD_REQUEST',
      config,
      {},
      { status: res.status, statusText: '', data: res.data ?? {}, headers: {}, config } as AxiosResponse,
    );
  };

  return { calls, adapter };
}

/** The server's real refusal for an anonymous or expired call (api/middleware.go). */
const UNAUTHORIZED: Scripted = { status: 401, data: { error: 'access token is not valid' } };

function seedSession() {
  localStorage.setItem(ACCESS_TOKEN_KEY, 'old-access');
  localStorage.setItem(REFRESH_TOKEN_KEY, 'old-refresh');
  localStorage.setItem(USER_KEY, JSON.stringify({ id: 'u1', username: 'operator' }));
  localStorage.setItem(ROLES_KEY, JSON.stringify(['operator']));
}

function locationStub(pathname = '/events') {
  const replace = vi.fn();
  Object.defineProperty(window, 'location', {
    configurable: true,
    writable: true,
    value: { ...window.location, pathname, replace, href: `http://localhost:3000${pathname}` },
  });
  return replace;
}

beforeEach(() => {
  localStorage.clear();
});

describe('the bearer is attached where the server demands it', () => {
  it('puts the stored access token on every request as a bearer', async () => {
    const { calls, adapter } = harness(() => ({ status: 200, data: { cameras: [] } }));
    client.defaults.adapter = adapter;
    seedSession();

    await client.get('/v1/cameras');

    expect(calls).toHaveLength(1);
    expect(calls[0].bearer).toBe('Bearer old-access');
  });

  it('sends no Authorization header when there is no session', async () => {
    const { calls, adapter } = harness(() => ({ status: 200, data: { local_login: true } }));
    client.defaults.adapter = adapter;

    await client.get('/auth/status');

    expect(calls[0].bearer).toBeUndefined();
  });
});

describe('401 -> refresh once -> retry', () => {
  it('renews the session and replays the refused request with the new bearer', async () => {
    const { calls, adapter } = harness((call, attempt) => {
      if (call.url === '/auth/refresh') {
        return {
          status: 200,
          data: { access_token: 'new-access', refresh_token: 'new-refresh', token_type: 'bearer' },
        };
      }
      return attempt === 1 ? UNAUTHORIZED : { status: 200, data: { segments: [] } };
    });
    client.defaults.adapter = adapter;
    seedSession();

    const res = await client.get('/v1/segments');

    expect(res.status).toBe(200);
    expect(calls.map((c) => `${c.method} ${c.url}`)).toEqual([
      'get /v1/segments',
      'post /auth/refresh',
      'get /v1/segments',
    ]);
    // The refresh carries the refresh token the server asked for, in the body
    // name it binds (api/auth.go refreshRequest.RefreshToken).
    expect(calls[1].body).toEqual({ refresh_token: 'old-refresh' });
    // The retry carries the NEW access token, and the caller never sees the 401.
    expect(calls[2].bearer).toBe('Bearer new-access');
  });

  it('stores the ROTATED refresh token, not just the new access token', async () => {
    // The server closes the presented refresh token and names the replacement
    // in rotated_from (api/auth.go Refresh). A client that keeps the old one
    // presents a closed token next time, which is reuse: every live session of
    // the account is dropped. Keeping only the access token is not a
    // simplification, it is a logout machine.
    const { adapter } = harness((call, attempt) => {
      if (call.url === '/auth/refresh') {
        return { status: 200, data: { access_token: 'new-access', refresh_token: 'new-refresh' } };
      }
      return attempt === 1 ? UNAUTHORIZED : { status: 200, data: { segments: [] } };
    });
    client.defaults.adapter = adapter;
    seedSession();

    await client.get('/v1/segments');

    expect(localStorage.getItem(ACCESS_TOKEN_KEY)).toBe('new-access');
    expect(localStorage.getItem(REFRESH_TOKEN_KEY)).toBe('new-refresh');
  });

  it('renews once for a burst of concurrent 401s, not once per request', async () => {
    // THE WALL mounts and asks for cameras, events, segments and storage at
    // once. If each 401 posted its own refresh, the first would rotate and the
    // rest would present a closed token — the server's reuse detection then
    // drops EVERY live session of the account (401 "refresh token was already
    // used", live_sessions_dropped: N). One refresh is a correctness property
    // of this server's security model, not an optimisation.
    const { calls, adapter } = harness((call, attempt) => {
      if (call.url === '/auth/refresh') {
        return { status: 200, data: { access_token: 'new-access', refresh_token: 'new-refresh' } };
      }
      return attempt === 1 ? UNAUTHORIZED : { status: 200, data: {} };
    });
    client.defaults.adapter = adapter;
    seedSession();

    const results = await Promise.all([
      client.get('/v1/cameras'),
      client.get('/v1/events'),
      client.get('/v1/segments'),
    ]);

    expect(results.every((r) => r.status === 200)).toBe(true);
    const refreshes = calls.filter((c) => c.url === '/auth/refresh');
    expect(refreshes).toHaveLength(1);
    const retries = calls.filter((c) => c.method === 'get' && c.bearer === 'Bearer new-access');
    expect(retries).toHaveLength(3);
  });
});

describe('a refused refresh ends the session, it does not loop', () => {
  it('clears every session key and routes to /login', async () => {
    const replace = locationStub('/events');
    const { calls, adapter } = harness((call) =>
      call.url === '/auth/refresh'
        ? { status: 401, data: { error: 'refresh token was already used', live_sessions_dropped: 3 } }
        : UNAUTHORIZED,
    );
    client.defaults.adapter = adapter;
    seedSession();

    await expect(client.get('/v1/cameras')).rejects.toMatchObject({
      response: { status: 401 },
    });

    // One attempt, one refresh, no second attempt: the request is over.
    expect(calls.filter((c) => c.url === '/v1/cameras')).toHaveLength(1);
    expect(calls.filter((c) => c.url === '/auth/refresh')).toHaveLength(1);
    for (const key of [ACCESS_TOKEN_KEY, REFRESH_TOKEN_KEY, USER_KEY, ROLES_KEY]) {
      expect(localStorage.getItem(key)).toBeNull();
    }
    expect(replace).toHaveBeenCalledWith('/login');
  });

  it('shares one failed refresh across a concurrent burst, and logs out once', async () => {
    const replace = locationStub('/live');
    const { calls, adapter } = harness((call) =>
      call.url === '/auth/refresh' ? { status: 401, data: { error: 'refresh token has expired' } } : UNAUTHORIZED,
    );
    client.defaults.adapter = adapter;
    seedSession();

    const settled = await Promise.allSettled([
      client.get('/v1/cameras'),
      client.get('/v1/events'),
      client.get('/v1/segments'),
    ]);

    expect(settled.every((s) => s.status === 'rejected')).toBe(true);
    expect(calls.filter((c) => c.url === '/auth/refresh')).toHaveLength(1);
    expect(replace).toHaveBeenCalledTimes(1);
  });

  it('clears and routes on a 401 with no refresh token to renew with', async () => {
    const replace = locationStub('/audit');
    const { calls, adapter } = harness(() => UNAUTHORIZED);
    client.defaults.adapter = adapter;
    seedSession();
    localStorage.removeItem(REFRESH_TOKEN_KEY);

    await expect(client.get('/v1/cameras')).rejects.toBeTruthy();

    expect(calls.filter((c) => c.url === '/auth/refresh')).toHaveLength(0);
    expect(localStorage.getItem(ACCESS_TOKEN_KEY)).toBeNull();
    expect(replace).toHaveBeenCalledWith('/login');
  });

  it('does not navigate when already on /login', async () => {
    const replace = locationStub('/login');
    const { adapter } = harness(() => UNAUTHORIZED);
    client.defaults.adapter = adapter;
    seedSession();

    await expect(client.get('/v1/cameras')).rejects.toBeTruthy();

    expect(replace).not.toHaveBeenCalled();
  });
});

describe('a refusal that is not about this session stays a refusal', () => {
  it('does not burn a refresh token on a 403 role refusal', async () => {
    // RequireRole answers 403 {"error":"insufficient role"}. Refreshing there
    // would rotate a healthy session for nothing and show the operator a
    // logout they did not do.
    const { calls, adapter } = harness(() => ({ status: 403, data: { error: 'insufficient role' } }));
    client.defaults.adapter = adapter;
    seedSession();

    await expect(client.get('/api/roles')).rejects.toMatchObject({ response: { status: 403 } });

    expect(calls.filter((c) => c.url === '/auth/refresh')).toHaveLength(0);
    expect(localStorage.getItem(REFRESH_TOKEN_KEY)).toBe('old-refresh');
  });

  it('leaves a login 401 for the login page to name, and does not refresh', async () => {
    // POST /auth/login answers 401 {"error":"invalid credentials"}. That is an
    // answer about the credentials just typed, not about a session this browser
    // has; the LoginPage has to receive it to say why the sign-in failed.
    const replace = locationStub('/login');
    const { calls, adapter } = harness(() => ({ status: 401, data: { error: 'invalid credentials' } }));
    client.defaults.adapter = adapter;
    seedSession();

    await expect(client.post('/auth/login', { username: 'admin', password: 'nope' })).rejects.toMatchObject({
      response: { status: 401, data: { error: 'invalid credentials' } },
    });

    expect(calls.filter((c) => c.url === '/auth/refresh')).toHaveLength(0);
    expect(localStorage.getItem(ACCESS_TOKEN_KEY)).toBe('old-access');
    expect(replace).not.toHaveBeenCalled();
  });

  it('names the auth endpoints that answer about credentials, not about the session', () => {
    // The exemption list is a claim about the server's route table
    // (api/handlers.go Register: /api/auth/status, /login, /refresh are public).
    expect(AUTH_EXEMPT_PATHS).toEqual(['/auth/status', '/auth/login', '/auth/refresh']);
  });
});

describe('the session keys are the plan\'s, spelled exactly', () => {
  it('uses the four nvr_ names and nothing else', () => {
    expect([ACCESS_TOKEN_KEY, REFRESH_TOKEN_KEY, USER_KEY, ROLES_KEY]).toEqual([
      'nvr_access_token',
      'nvr_refresh_token',
      'nvr_user',
      'nvr_roles',
    ]);
  });
});

describe('the client is the one the console talks to', () => {
  it('is rooted at /api so the proxy owns the host', () => {
    // The console never names a server: relative /api and /ws are what the
    // vite proxy and docker/nginx.conf both forward (S8-01). The root is on
    // this instance, not on axios' shared default — a bare axios call from a
    // third-party library must not inherit the console's prefix.
    expect(client.defaults.baseURL).toBe('/api');
    expect(axios.defaults.baseURL ?? '').not.toBe('/api');
  });
});

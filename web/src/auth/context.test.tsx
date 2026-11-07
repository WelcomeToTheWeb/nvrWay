import { beforeEach, describe, expect, it } from 'vitest';
import { act, renderHook, waitFor } from '@testing-library/react';
import { createElement, type ReactNode } from 'react';
import axios from 'axios';
import client from '../api/client';
import { axiosHarness, type Call, type Script } from '../test/axiosHarness';
import { AuthProvider, useAuth } from './context';
import { ACCESS_TOKEN_KEY, REFRESH_TOKEN_KEY, ROLES_KEY, USER_KEY, type SessionUser } from './session';

/**
 * The shared axios harness (`src/test/axiosHarness.ts`): a real adapter at the
 * bottom of the pipeline, so the context is tested against requests the
 * interceptor chain actually made. This wrapper only exists because these
 * tests script calls without caring about the bearer or the attempt count —
 * it wires the adapter and hands back the call log.
 */
function harness(script: Script) {
  const h = axiosHarness(script);
  client.defaults.adapter = h.adapter;
  return h.calls;
}

const OPERATOR: SessionUser = {
  id: 'u-1',
  username: 'operator',
  name: 'Ops',
  email: 'ops@example.invalid',
  provider: 'local',
  status: 'active',
  roles: ['operator'],
  created_at: '2026-11-01T00:00:00Z',
  updated_at: '2026-11-01T00:00:00Z',
  last_login: null,
};

const wrapper = ({ children }: { children: ReactNode }) => createElement(AuthProvider, null, children);

beforeEach(() => {
  localStorage.clear();
});

describe('signing in', () => {
  it('stores the session under the four plan keys, roles read from the user row', async () => {
    // GET/POST shapes are the server's: POST /api/auth/login answers
    // {access_token, refresh_token, token_type, expires_in, user} and the user
    // row carries roles (api/handlers.go userView). There is no top-level
    // `roles` on that response to read.
    const calls = harness((call) => {
      if (call.url === '/auth/login') {
        return {
          status: 200,
          data: { access_token: 'a-1', refresh_token: 'r-1', token_type: 'bearer', expires_in: 1800, user: OPERATOR },
        };
      }
      return { status: 404, data: { error: 'no' } };
    });

    const { result } = renderHook(() => useAuth(), { wrapper });
    await act(async () => {
      await result.current.login('operator', 'correct horse battery');
    });

    expect(calls[0]).toEqual({
      url: '/auth/login',
      method: 'post',
      body: { username: 'operator', password: 'correct horse battery' },
    });
    expect(localStorage.getItem(ACCESS_TOKEN_KEY)).toBe('a-1');
    expect(localStorage.getItem(REFRESH_TOKEN_KEY)).toBe('r-1');
    expect(JSON.parse(String(localStorage.getItem(USER_KEY)))).toEqual(OPERATOR);
    expect(JSON.parse(String(localStorage.getItem(ROLES_KEY)))).toEqual(['operator']);

    expect(result.current.isAuthenticated).toBe(true);
    expect(result.current.user?.username).toBe('operator');
    expect(result.current.roles).toEqual(['operator']);
  });

  it('leaves a refused sign-in in place so the page can name it', async () => {
    harness(() => ({ status: 401, data: { error: 'invalid credentials' } }));
    const { result } = renderHook(() => useAuth(), { wrapper });

    await expect(
      act(async () => {
        await result.current.login('operator', 'wrong');
      }),
    ).rejects.toBeTruthy();

    expect(result.current.isAuthenticated).toBe(false);
    expect(localStorage.getItem(ACCESS_TOKEN_KEY)).toBeNull();
  });
});

describe('signing out', () => {
  it('closes the session on the server and clears all four keys', async () => {
    // A logout that only clears localStorage leaves a refresh token valid for
    // 14 days on a machine that still holds it. POST /api/auth/logout with the
    // refresh_token closes that one session (api/auth.go Logout).
    localStorage.setItem(ACCESS_TOKEN_KEY, 'a-1');
    localStorage.setItem(REFRESH_TOKEN_KEY, 'r-1');
    localStorage.setItem(USER_KEY, JSON.stringify(OPERATOR));
    localStorage.setItem(ROLES_KEY, JSON.stringify(['operator']));

    const calls = harness((call) =>
      call.url === '/auth/logout'
        ? { status: 200, data: { ok: true, live_sessions_dropped: 1 } }
        : { status: 404, data: { error: 'no' } },
    );

    const { result } = renderHook(() => useAuth(), { wrapper });
    await act(async () => {
      await result.current.logout();
    });

    expect(calls).toEqual([
      // The shared harness also records the bearer: logout is a session call,
      // so the stored access token must ride on it.
      { url: '/auth/logout', method: 'post', bearer: 'Bearer a-1', body: { refresh_token: 'r-1' } },
    ]);
    for (const key of [ACCESS_TOKEN_KEY, REFRESH_TOKEN_KEY, USER_KEY, ROLES_KEY]) {
      expect(localStorage.getItem(key)).toBeNull();
    }
    expect(result.current.isAuthenticated).toBe(false);
    expect(result.current.user).toBeNull();
    expect(result.current.roles).toEqual([]);
  });

  it('clears local state even when the server cannot be reached', async () => {
    localStorage.setItem(ACCESS_TOKEN_KEY, 'a-1');
    localStorage.setItem(REFRESH_TOKEN_KEY, 'r-1');
    const calls: Call[] = [];
    client.defaults.adapter = async (config) => {
      calls.push({ url: config.url ?? '', method: config.method ?? 'get', bearer: undefined, body: undefined });
      throw new axios.AxiosError('network down', 'ERR_NETWORK', config, {});
    };

    const { result } = renderHook(() => useAuth(), { wrapper });
    await act(async () => {
      await result.current.logout();
    });

    expect(localStorage.getItem(ACCESS_TOKEN_KEY)).toBeNull();
    expect(localStorage.getItem(REFRESH_TOKEN_KEY)).toBeNull();
    expect(result.current.isAuthenticated).toBe(false);
  });
});

describe('the SSO fragment handoff', () => {
  it('adopts tokens from the callback and then asks the server who it is', async () => {
    // The callback arrives with #access_token and #refresh_token and no user
    // row (api/sso_routes.go redirects the fragment only). GET /api/auth/profile
    // is the route that answers "which account is this", roles included — it is
    // what decides which tabs to draw.
    const calls = harness((call) => {
      if (call.url === '/auth/profile') return { status: 200, data: OPERATOR };
      return { status: 404, data: { error: 'no' } };
    });

    const { result } = renderHook(() => useAuth(), { wrapper });
    await act(async () => {
      await result.current.adoptTokens('a-sso', 'r-sso');
    });

    expect(localStorage.getItem(ACCESS_TOKEN_KEY)).toBe('a-sso');
    expect(localStorage.getItem(REFRESH_TOKEN_KEY)).toBe('r-sso');
    expect(calls.map((c) => c.url)).toContain('/auth/profile');
    await waitFor(() => expect(result.current.user?.username).toBe('operator'));
    expect(result.current.roles).toEqual(['operator']);
    expect(result.current.isAuthenticated).toBe(true);
  });

  it('keeps the tokens when the profile read fails, so a blip does not strand the handoff', async () => {
    // The pair came from the operator's own identity provider. Dropping it
    // because one read failed would leave nothing to retry with — the fragment
    // is gone once the URL is cleaned.
    harness(() => ({ status: 502, data: { error: 'bad gateway' } }));

    const { result } = renderHook(() => useAuth(), { wrapper });
    await act(async () => {
      await result.current.adoptTokens('a-sso', 'r-sso');
    });

    expect(localStorage.getItem(ACCESS_TOKEN_KEY)).toBe('a-sso');
    expect(localStorage.getItem(REFRESH_TOKEN_KEY)).toBe('r-sso');
    expect(result.current.user).toBeNull();
    expect(result.current.isAuthenticated).toBe(true);
  });
});

describe('a returning browser', () => {
  it('starts authenticated from the stored session, so a deep link survives reload', () => {
    localStorage.setItem(ACCESS_TOKEN_KEY, 'a-1');
    localStorage.setItem(REFRESH_TOKEN_KEY, 'r-1');
    localStorage.setItem(USER_KEY, JSON.stringify(OPERATOR));
    localStorage.setItem(ROLES_KEY, JSON.stringify(['admin']));

    const { result } = renderHook(() => useAuth(), { wrapper });

    expect(result.current.isAuthenticated).toBe(true);
    expect(result.current.user?.username).toBe('operator');
    expect(result.current.roles).toEqual(['admin']);
  });

  it('treats a corrupt stored user as no session, not as a crash', () => {
    localStorage.setItem(ACCESS_TOKEN_KEY, 'a-1');
    localStorage.setItem(USER_KEY, '{not json');
    localStorage.setItem(ROLES_KEY, '{not json');

    const { result } = renderHook(() => useAuth(), { wrapper });

    expect(result.current.user).toBeNull();
    expect(result.current.roles).toEqual([]);
    // The token is still there, so the server stays the authority on it.
    expect(result.current.isAuthenticated).toBe(true);
  });
});

describe('role checks drive the nav', () => {
  it('answers hasRole and hasAnyRole from the stored roles', () => {
    localStorage.setItem(ACCESS_TOKEN_KEY, 'a-1');
    localStorage.setItem(ROLES_KEY, JSON.stringify(['operator']));

    const { result } = renderHook(() => useAuth(), { wrapper });

    expect(result.current.hasRole('operator')).toBe(true);
    expect(result.current.hasRole('admin')).toBe(false);
    expect(result.current.hasAnyRole(['admin', 'operator'])).toBe(true);
    expect(result.current.hasAnyRole(['admin'])).toBe(false);
  });
});

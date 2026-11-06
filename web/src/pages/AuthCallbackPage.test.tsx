import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import { createElement, StrictMode } from 'react';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import client from '../api/client';
import { AuthProvider } from '../auth/context';
import { ACCESS_TOKEN_KEY, REFRESH_TOKEN_KEY, type SessionUser } from '../auth/session';
import { axiosHarness } from '../test/axiosHarness';
import AuthCallbackPage from './AuthCallbackPage';

const OPERATOR: SessionUser = {
  id: 'u-1', username: 'operator', name: '', email: 'ops@example.invalid', provider: 'okta',
  status: 'active', roles: ['operator'], created_at: '', updated_at: '', last_login: null,
};

function renderPage(entry = '/auth/callback', strict = false) {
  const tree = createElement(
    AuthProvider,
    null,
    createElement(
      MemoryRouter,
      { initialEntries: [entry] },
      createElement(
        Routes,
        null,
        createElement(Route, { path: '/auth/callback', element: createElement(AuthCallbackPage) }),
        createElement(Route, { path: '/live', element: createElement('div', null, 'THE WALL') }),
        createElement(Route, { path: '/login', element: createElement('div', null, 'THE DOOR') }),
      ),
    ),
  );
  return render(strict ? createElement(StrictMode, null, tree) : tree);
}

/** The fragment the server redirects here with (api/sso_routes.go). */
function withFragment(fragment: string) {
  window.location.hash = fragment;
}

beforeEach(() => {
  localStorage.clear();
  window.location.hash = '';
});

afterEach(() => {
  window.location.hash = '';
});

describe('a good handoff', () => {
  it('adopts the pair from the fragment and routes to the wall', async () => {
    // Success shape: `${WEB_URL}/auth/callback#access_token=…&refresh_token=…`.
    // A fragment is used on purpose — it is never sent to the server in a
    // request, so the tokens do not land in a proxy log or a Referer.
    const h = axiosHarness((call) =>
      call.url === '/auth/profile' ? { status: 200, data: OPERATOR } : { status: 404, data: {} },
    );
    client.defaults.adapter = h.adapter;
    withFragment('access_token=a-sso&refresh_token=r-sso');

    renderPage();

    expect(await screen.findByText('THE WALL')).toBeTruthy();
    expect(localStorage.getItem(ACCESS_TOKEN_KEY)).toBe('a-sso');
    expect(localStorage.getItem(REFRESH_TOKEN_KEY)).toBe('r-sso');
    // The fragment carries no account, so the page asks who it got.
    expect(h.calls.map((c) => c.url)).toContain('/auth/profile');
  });

  it('scrubs the tokens out of the address bar', async () => {
    client.defaults.adapter = axiosHarness((call) =>
      call.url === '/auth/profile' ? { status: 200, data: OPERATOR } : { status: 404, data: {} },
    ).adapter;
    withFragment('access_token=a-sso&refresh_token=r-sso');

    renderPage();
    await screen.findByText('THE WALL');

    // A token left in the address bar is a token in the browser history, on the
    // screen, and in anything that reads the URL.
    expect(window.location.hash).toBe('');
  });

  it('never reports a missing handoff while it is adopting one, even under StrictMode', async () => {
    // React 18 StrictMode mounts, unmounts and remounts the effect on the same
    // instance (probed: 2 runs against 1 without it). The first pass scrubs the
    // fragment, so a second pass reads an empty hash — and would report "no
    // handoff arrived" over a sign-in that is working. The navigation eventually
    // covers that up, so the assertion has to be made while the handoff is
    // still in flight: a profile read that never settles pins the page in
    // `adopting`, where a false error state is visible instead of transient.
    const urls: string[] = [];
    client.defaults.adapter = (config) => {
      urls.push(config.url ?? '');
      return new Promise(() => {});
    };
    withFragment('access_token=a-sso&refresh_token=r-sso');

    renderPage('/auth/callback', true);

    expect(await screen.findByText(/signing you in/i)).toBeTruthy();
    expect(screen.queryByText(/no sign-in handoff arrived/i)).toBeNull();
    expect(urls.filter((u) => u === '/auth/profile')).toHaveLength(1);
  });
});

describe('a refused exchange is a named state, never a spinner', () => {
  it('shows the reason and the provider, and does not route', async () => {
    // Refusal shape: `#error=<reason>&provider=<name>&error_description=<detail>`.
    const h = axiosHarness(() => ({ status: 404, data: {} }));
    client.defaults.adapter = h.adapter;
    withFragment('error=domain_not_allowed&provider=okta&error_description=the+domain+is+not+allowed');

    renderPage();

    // The operator's sentence and the server's own error_description are two
    // different lines on purpose: one explains, the other quotes.
    expect(await screen.findByText(/by the operator’s rules/i)).toBeTruthy();
    expect(screen.getByText(/domain_not_allowed/)).toBeTruthy();
    expect(screen.getByText(/okta/i)).toBeTruthy();
    // The server's own detail is quoted as its own line, not folded into the
    // explanation: when the two disagree, the operator needs to see both.
    expect(screen.getByText('the domain is not allowed')).toBeTruthy();
    expect(screen.queryByText('THE WALL')).toBeNull();
    // No session was handed over, so nothing is fetched with one.
    expect(h.calls.filter((c) => c.url === '/auth/profile')).toHaveLength(0);
  });

  it('says an administrator has to decide, and names the identity to approve', async () => {
    // `identity_pending` is the default link_policy outcome: an assertion whose
    // email matches an existing account becomes a PendingIdentity nobody merges
    // automatically. The operator on the other side of that screen needs to
    // know it is a decision waiting for someone, not a broken login.
    client.defaults.adapter = axiosHarness(() => ({ status: 404, data: {} })).adapter;
    withFragment('error=identity_pending&provider=okta&pending_identity=id-42&error_description=waiting');

    renderPage();

    expect(await screen.findByText(/has to approve this identity/i)).toBeTruthy();
    expect(screen.getByText(/id-42/)).toBeTruthy();
    expect(screen.queryByText('THE WALL')).toBeNull();
  });

  it('shows an unknown reason verbatim rather than hiding a fact it cannot interpret', async () => {
    client.defaults.adapter = axiosHarness(() => ({ status: 404, data: {} })).adapter;
    withFragment('error=id_token_userinfo_disagreement&provider=keycloak&error_description=two+subjects');

    renderPage();

    expect(await screen.findByText(/id_token_userinfo_disagreement/)).toBeTruthy();
  });

  it('offers the way back to the door', async () => {
    client.defaults.adapter = axiosHarness(() => ({ status: 404, data: {} })).adapter;
    withFragment('error=code_exchange_failed&provider=okta&error_description=x');

    renderPage();

    const back = await screen.findByRole('link', { name: /sign in/i });
    expect(back.getAttribute('href')).toBe('/login');
  });
});

describe('a callback with nothing in it', () => {
  it('names the empty handoff instead of hanging', async () => {
    // Reached by typing the URL, by reloading after the fragment was scrubbed,
    // or by a WEB_URL that points somewhere unexpected. The honest answer is
    // "nothing arrived", not a page that waits forever.
    client.defaults.adapter = axiosHarness(() => ({ status: 404, data: {} })).adapter;

    renderPage();

    expect(await screen.findByText(/no sign-in handoff arrived/i)).toBeTruthy();
    expect(screen.queryByText('THE WALL')).toBeNull();
  });

  it('names a half handoff as its own state', async () => {
    // access_token without refresh_token is not a session this console can
    // keep: it would die at the 30-minute mark with nothing to renew with.
    client.defaults.adapter = axiosHarness(() => ({ status: 404, data: {} })).adapter;
    withFragment('access_token=a-only');

    renderPage();

    expect(await screen.findByText(/incomplete/i)).toBeTruthy();
    expect(localStorage.getItem(ACCESS_TOKEN_KEY)).toBeNull();
    expect(screen.queryByText('THE WALL')).toBeNull();
  });
});

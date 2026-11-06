import { beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { createElement } from 'react';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import client from '../api/client';
import { AuthProvider } from '../auth/context';
import { ACCESS_TOKEN_KEY, REFRESH_TOKEN_KEY } from '../auth/session';
import { axiosHarness } from '../test/axiosHarness';
import LoginPage from './LoginPage';

/**
 * The pages are mounted inside a real router and a real AuthProvider, so
 * "routes to /live" is asserted by arriving at /live, not by spying on a
 * navigate function that the router may or may not have honoured.
 */
function renderPage(entry = '/login') {
  return render(
    createElement(
      AuthProvider,
      null,
      createElement(
        MemoryRouter,
        { initialEntries: [entry] },
        createElement(
          Routes,
          null,
          createElement(Route, { path: '/login', element: createElement(LoginPage) }),
          createElement(Route, { path: '/live', element: createElement('div', null, 'THE WALL') }),
        ),
      ),
    ),
  );
}

const STATUS = (local: boolean, providers: { provider: string; name: string; issuer: string }[] = []) => ({
  status: 200,
  data: { local_login: local, oidc: providers, registration: false },
});

beforeEach(() => {
  localStorage.clear();
});

describe('the local form follows what the installation actually has', () => {
  it('draws the form when a local account exists', async () => {
    client.defaults.adapter = axiosHarness((call) =>
      call.url === '/auth/status' ? STATUS(true) : { status: 404, data: {} },
    ).adapter;

    renderPage();

    await screen.findByRole('textbox', { name: /username/i });
    expect(screen.getByLabelText(/password/i)).toBeTruthy();
    expect(screen.getByRole('button', { name: /sign in/i })).toBeTruthy();
  });

  it('draws no local form when there is no local admin', async () => {
    // Exit criterion. `local_login` is `Users.CountLocalLogin() > 0`
    // (api/auth.go AuthStatus): a deployment seeded without
    // BOOTSTRAP_ADMIN_PASSWORD has no password to type, and a form that
    // invites one is a door that cannot open.
    client.defaults.adapter = axiosHarness((call) =>
      call.url === '/auth/status'
        ? STATUS(false, [{ provider: 'okta', name: 'Okta Work', issuer: 'https://id.example.com' }])
        : { status: 404, data: {} },
    ).adapter;

    renderPage();

    await screen.findByRole('link', { name: /Okta Work/i });
    expect(screen.queryByRole('textbox', { name: /username/i })).toBeNull();
    expect(screen.queryByLabelText(/password/i)).toBeNull();
  });

  it('says there is no way in, rather than showing an empty page', async () => {
    client.defaults.adapter = axiosHarness((call) =>
      call.url === '/auth/status' ? STATUS(false) : { status: 404, data: {} },
    ).adapter;

    renderPage();

    const text = await screen.findByText(/no way in/i);
    expect(text).toBeTruthy();
    expect(screen.queryByRole('textbox', { name: /username/i })).toBeNull();
  });
});

describe('the provider buttons are the server’s list, linked to the server’s route', () => {
  it('names each enabled provider and links to its authorize path', async () => {
    // GET /api/auth/status answers oidc[] as {provider, name, issuer}, and the
    // route is GET /api/auth/sso/:provider/authorize (api/sso_routes.go:172).
    // It is a top-level navigation, not a fetch: the IdP redirect chain cannot
    // happen inside an XHR, so the button is an <a href>.
    const h = axiosHarness((call) =>
      call.url === '/auth/status'
        ? STATUS(true, [
            { provider: 'okta', name: 'Okta Work', issuer: 'https://id.example.com' },
            { provider: 'keycloak', name: 'Home LAN', issuer: 'https://sso.lan.example' },
          ])
        : { status: 404, data: {} },
    );
    client.defaults.adapter = h.adapter;

    renderPage();

    const okta = await screen.findByRole('link', { name: /Okta Work/i });
    expect(okta.getAttribute('href')).toBe('/api/auth/sso/okta/authorize');
    const kc = screen.getByRole('link', { name: /Home LAN/i });
    expect(kc.getAttribute('href')).toBe('/api/auth/sso/keycloak/authorize');
    // The issuer is shown too: two providers with similar names are told apart
    // by who is asserting, and that is the one fact the operator cannot guess.
    expect(screen.getByText('https://id.example.com')).toBeTruthy();
  });
});

describe('signing in', () => {
  it('routes to the wall on a good password', async () => {
    const h = axiosHarness((call) => {
      if (call.url === '/auth/status') return STATUS(true);
      if (call.url === '/auth/login') {
        return {
          status: 200,
          data: {
            access_token: 'a-1',
            refresh_token: 'r-1',
            token_type: 'bearer',
            expires_in: 1800,
            user: {
              id: 'u-1', username: 'admin', name: '', email: '', provider: 'local',
              status: 'active', roles: ['admin'], created_at: '', updated_at: '', last_login: null,
            },
          },
        };
      }
      return { status: 404, data: {} };
    });
    client.defaults.adapter = h.adapter;

    renderPage();
    await screen.findByRole('textbox', { name: /username/i });

    fireEvent.change(screen.getByRole('textbox', { name: /username/i }), { target: { value: 'admin' } });
    fireEvent.change(screen.getByLabelText(/password/i), { target: { value: 'a-good-password' } });
    fireEvent.click(screen.getByRole('button', { name: /sign in/i }));

    expect(await screen.findByText('THE WALL')).toBeTruthy();
    expect(localStorage.getItem(ACCESS_TOKEN_KEY)).toBe('a-1');
    expect(localStorage.getItem(REFRESH_TOKEN_KEY)).toBe('r-1');
    expect(h.calls.find((c) => c.url === '/auth/login')?.body).toEqual({
      username: 'admin',
      password: 'a-good-password',
    });
  });

  it('shows the server’s reason for a refused sign-in and stays put', async () => {
    // 401 {"error":"invalid credentials"} is the answer the plan wants on
    // screen. The client passes a login 401 through untouched (AUTH_EXEMPT_PATHS)
    // precisely so this page can say why, instead of bouncing to a login page
    // that no longer explains anything.
    client.defaults.adapter = axiosHarness((call) =>
      call.url === '/auth/status'
        ? STATUS(true)
        : call.url === '/auth/login'
          ? { status: 401, data: { error: 'invalid credentials' } }
          : { status: 404, data: {} },
    ).adapter;

    renderPage();
    await screen.findByRole('textbox', { name: /username/i });

    fireEvent.change(screen.getByRole('textbox', { name: /username/i }), { target: { value: 'admin' } });
    fireEvent.change(screen.getByLabelText(/password/i), { target: { value: 'wrong' } });
    fireEvent.click(screen.getByRole('button', { name: /sign in/i }));

    expect(await screen.findByText('invalid credentials')).toBeTruthy();
    expect(screen.queryByText('THE WALL')).toBeNull();
    // The form is still there to correct — the operator is not sent away.
    expect(screen.getByRole('textbox', { name: /username/i })).toBeTruthy();
  });

  it('names a provider account instead of pretending the password was wrong', async () => {
    // 403 {"error":"this account signs in through its provider"} (api/auth.go
    // Login): the account exists and has no password to check.
    client.defaults.adapter = axiosHarness((call) =>
      call.url === '/auth/status'
        ? STATUS(true)
        : call.url === '/auth/login'
          ? { status: 403, data: { error: 'this account signs in through its provider' } }
          : { status: 404, data: {} },
    ).adapter;

    renderPage();
    await screen.findByRole('textbox', { name: /username/i });

    fireEvent.change(screen.getByRole('textbox', { name: /username/i }), { target: { value: 'ops' } });
    fireEvent.change(screen.getByLabelText(/password/i), { target: { value: 'whatever' } });
    fireEvent.click(screen.getByRole('button', { name: /sign in/i }));

    expect(await screen.findByText('this account signs in through its provider')).toBeTruthy();
  });
});

describe('the page states what it is doing, and what went wrong', () => {
  it('says what it is checking while the methods are unread', async () => {
    // "Nothing on screen is a spinner" is a binding rule of this product. An
    // unresolved read is named in words, so a slow box reads as a box that is
    // thinking about a specific question, not as a circle.
    client.defaults.adapter = () => new Promise(() => {});

    renderPage();

    expect(await screen.findByText(/checking how this installation allows sign-in/i)).toBeTruthy();
  });

  it('names a failed status read and offers to try again', async () => {
    const h = axiosHarness((call) =>
      call.url === '/auth/status' ? { status: 500, data: { error: 'accounts are unreadable' } } : { status: 404, data: {} },
    );
    client.defaults.adapter = h.adapter;

    renderPage();

    expect(await screen.findByText(/could not read the sign-in methods/i)).toBeTruthy();
    expect(screen.getByText('accounts are unreadable')).toBeTruthy();

    fireEvent.click(screen.getByRole('button', { name: /try again/i }));
    await waitFor(() => expect(h.calls.filter((c) => c.url === '/auth/status')).toHaveLength(2));
  });
});

describe('a browser that is already signed in', () => {
  it('goes straight to the wall instead of offering the door again', async () => {
    localStorage.setItem(ACCESS_TOKEN_KEY, 'a-1');
    localStorage.setItem(REFRESH_TOKEN_KEY, 'r-1');
    client.defaults.adapter = axiosHarness(() => STATUS(true)).adapter;

    renderPage();

    expect(await screen.findByText('THE WALL')).toBeTruthy();
  });
});

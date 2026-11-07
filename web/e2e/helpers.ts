import { expect, type Page } from '@playwright/test';

import { ADMIN_PASSWORD, ADMIN_USER, API_URL, WORKER_TOKEN } from './playwright.config';

// The API side of the specs. The console is what the specs observe, but the
// fixtures are API calls: a spec that seeds through the UI is testing the same
// code path it claims to verify, and a viewer account does not exist in the UI
// at all (registration is 403 by design).

export interface Tokens {
  access: string;
  refresh: string;
}

export async function apiLogin(username: string, password: string): Promise<Tokens & { user: { id: string; roles: string[] } }> {
  const res = await fetch(`${API_URL}/api/auth/login`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ username, password }),
  });
  if (!res.ok) throw new Error(`e2e fixture: login as ${username} answered ${res.status}: ${await res.text()}`);
  const body = (await res.json()) as { access_token: string; refresh_token: string; user: { id: string; roles: string[] } };
  return { access: body.access_token, refresh: body.refresh_token, user: body.user };
}

export const adminTokens = () => apiLogin(ADMIN_USER, ADMIN_PASSWORD);

export async function apiAs<T>(tokens: Tokens, method: string, path: string, body?: unknown): Promise<{ status: number; json: T }> {
  const res = await fetch(`${API_URL}${path}`, {
    method,
    headers: { authorization: `Bearer ${tokens.access}`, 'content-type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await res.text();
  let json: unknown = null;
  try {
    json = text ? JSON.parse(text) : null;
  } catch {
    /* a non-JSON answer is still an answer the caller can assert on */
  }
  return { status: res.status, json: json as T };
}

export const apiAdmin = <T>(method: string, path: string, body?: unknown) =>
  adminTokens().then((t) => apiAs<T>(t, method, path, body));

/**
 * Sign the browser in without the login form: the auth context reads exactly
 * these keys (src/auth/session.ts), so this is the same state a login leaves
 * behind — the login itself is what auth.spec.ts tests, through the form.
 */
export async function signIn(page: Page, tokens: Tokens & { user: { id: string; roles: string[] } }) {
  await page.goto('/login');
  await page.evaluate(
    ([access, refresh, user]) => {
      localStorage.setItem('nvr_access_token', access);
      localStorage.setItem('nvr_refresh_token', refresh);
      localStorage.setItem('nvr_user', user);
      localStorage.setItem('nvr_roles', JSON.stringify((JSON.parse(user) as { roles?: string[] }).roles ?? []));
    },
    [tokens.access, tokens.refresh, JSON.stringify(tokens.user)],
  );
}

/** POST a worker signal batch — the same intake the detector uses (S5-10). */
export async function postSignals(signals: unknown[]) {
  const res = await fetch(`${API_URL}/api/internal/v1/signals`, {
    method: 'POST',
    headers: { authorization: `Bearer ${WORKER_TOKEN}`, 'content-type': 'application/json' },
    body: JSON.stringify({ signals }),
  });
  const text = await res.text();
  if (!res.ok) throw new Error(`e2e fixture: signals answered ${res.status}: ${text}`);
  const reply = JSON.parse(text) as { accepted: number; events?: { error?: string }[] };
  if (reply.accepted < signals.length) {
    throw new Error(`e2e fixture: signals refused (${reply.accepted}/${signals.length}): ${reply.events?.map((e) => e.error).join('; ')}`);
  }
  return reply;
}

export function signal(cameraId: string, eventKey: string, action: 'open' | 'extend' | 'close') {
  return {
    camera_id: cameraId,
    event_key: eventKey,
    action,
    rule: 'loiter',
    zone_id: 'e2e-zone',
    zone_name: 'E2E Zone',
    severity: 2,
    classes: ['person'],
    ts: new Date().toISOString(),
    objects: [{ class: 'person', confidence: 0.9, box: [10, 10, 100, 200], tracker_id: 1 }],
    record_mode: 'events',
  };
}

export async function waitFor<T>(what: string, probe: () => Promise<T | null>, ms = 90_000) {
  const deadline = Date.now() + ms;
  for (;;) {
    const got = await probe();
    if (got !== null && got !== undefined) return got;
    if (Date.now() > deadline) throw new Error(`e2e: timed out waiting for ${what}`);
    await new Promise((r) => setTimeout(r, 1000));
  }
}

export const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

export { expect };

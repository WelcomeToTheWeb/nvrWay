import { expect, test } from '@playwright/test';

import { adminTokens, apiAdmin, apiAs, apiLogin, signIn } from './helpers';
import { ADMIN_PASSWORD, ADMIN_USER } from './playwright.config';

// auth: local admin login -> console, viewer sees no Settings.
// The admin half goes through the form (that is the behavior under test); the
// viewer half seeds an account through the API — registration is 403 by
// design, so the admin API is the only way one exists.

test('local admin signs in through the form and lands on the console', async ({ page }) => {
  await page.goto('/login');
  await page.getByLabel('Username').fill(ADMIN_USER);
  await page.getByLabel('Password').fill(ADMIN_PASSWORD);
  await page.getByRole('button', { name: 'Sign in' }).click();

  await expect(page).toHaveURL(/\/live/);
  await expect(page.getByRole('navigation', { name: 'Console' })).toBeVisible();
  // The admin sees the admin tabs.
  await expect(page.getByRole('link', { name: 'Settings' })).toBeVisible();
  await expect(page.getByRole('link', { name: 'Audit' })).toBeVisible();
});

test('a wrong password is refused on the page, not by navigation', async ({ page }) => {
  await page.goto('/login');
  await page.getByLabel('Username').fill(ADMIN_USER);
  await page.getByLabel('Password').fill('not-the-password');
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.locator('p.text-status-error')).toBeVisible();
  await expect(page).toHaveURL(/\/login/);
});

test('a viewer signs in and sees no Settings and no Audit', async ({ page }) => {
  const username = `e2e-viewer-${Date.now()}`;
  const password = 'e2e-viewer-pass-1';
  const created = await apiAdmin<{ id?: string; error?: string }>('POST', '/api/users', {
    username,
    password,
    name: 'E2E Viewer',
    roles: ['viewer'],
  });
  expect(created.status, `viewer account created: ${JSON.stringify(created.json)}`).toBeLessThan(400);

  // The viewer signs in through the same form — the role gate is about what
  // the console renders for a real session, not about injected tokens.
  await page.goto('/login');
  await page.getByLabel('Username').fill(username);
  await page.getByLabel('Password').fill(password);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page).toHaveURL(/\/live/);

  const nav = page.getByRole('navigation', { name: 'Console' });
  await expect(nav.getByRole('link', { name: 'Live' })).toBeVisible();
  await expect(nav.getByRole('link', { name: 'Events' })).toBeVisible();
  await expect(nav.getByRole('link', { name: 'Cameras' })).toBeVisible();
  await expect(nav.getByRole('link', { name: 'Settings' })).toHaveCount(0);
  await expect(nav.getByRole('link', { name: 'Audit' })).toHaveCount(0);

  // And the gate is not decoration: the viewer's own token is refused where
  // the admin's is accepted.
  const viewer = await apiLogin(username, password);
  const denied = await apiAs(viewer, 'GET', '/api/storage/backends');
  expect(denied.status).toBe(403);
});

test('an unauthenticated browser is bounced to /login', async ({ page }) => {
  await page.goto('/events');
  await expect(page).toHaveURL(/\/login/);
});

test('a session token in storage is a session: no form needed', async ({ page }) => {
  const tokens = await adminTokens();
  await signIn(page, tokens);
  await page.goto('/');
  await expect(page).toHaveURL(/\/live/);
  await expect(page.getByRole('navigation', { name: 'Console' })).toBeVisible();
});

import { expect, test } from '@playwright/test';

import { RUN_ID } from './playwright.config';
import { adminTokens, apiAdmin, signIn, waitFor } from './helpers';

// storage: add fs backend, test-connection renders ok.
// The root path is the run's own clips directory — it exists (e2e-stack.sh
// made it), so `test: ok` is the server proving it can write where it says it
// can write, not a form accepting a fiction.

const name = `e2e-fs-${Date.now()}`;
const rootPath = `/tmp/nvr-e2e/${RUN_ID}/clips`;

test('an admin adds an fs backend and the test renders ok', async ({ page }) => {
  const tokens = await adminTokens();
  await signIn(page, tokens);
  await page.goto('/settings');

  await page.getByRole('button', { name: 'add a backend' }).click();
  await page.getByRole('combobox').first().selectOption('fs');
  await page.getByLabel(/^name$/i).fill(name);
  await page.getByLabel(/root_path/i).fill(rootPath);
  await page.getByRole('button', { name: 'add backend' }).click();

  const row = page.locator('li, [class*=card]').filter({ hasText: name }).first();
  // exact: the name span contains "fs" too; the kind span is exactly "fs".
  await expect(row.getByText('fs', { exact: true })).toBeVisible();

  await row.getByRole('button', { name: 'test connection' }).click();
  await expect(row.getByText('test: ok')).toBeVisible({ timeout: 30_000 });
});

test('the backend the spec added is gone when the spec is done', async () => {
  const { json } = await apiAdmin<{ backends?: { id: string; name: string }[] }>('GET', '/api/storage/backends');
  for (const b of json.backends ?? []) {
    if (b.name === name) {
      const del = await apiAdmin('DELETE', `/api/storage/backends/${b.id}`);
      expect(del.status).toBeLessThan(400);
      await waitFor(
        'the delete to land',
        async () => {
          const after = await apiAdmin<{ backends?: { name: string }[] }>('GET', '/api/storage/backends');
          return (after.json.backends ?? []).some((x) => x.name === name) ? null : 'gone';
        },
        15_000,
      );
    }
  }
});

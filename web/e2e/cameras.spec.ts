import { expect, test } from '@playwright/test';

import { adminTokens, apiAdmin, signIn, waitFor } from './helpers';

// cameras: create + draw a 4-vertex zone, reload, vertices persisted.
// The camera is created through the form (the behavior under test); the zone
// is drawn on the canvas the way an operator draws one — `new zone`, four
// clicks, Enter — and the persistence check is a real reload plus the zone
// row's own vertex count, backed by the server's row.

const slug = `e2e-zone-${Date.now()}`;

test('an operator adds a camera through the form', async ({ page }) => {
  const tokens = await adminTokens();
  await signIn(page, tokens);
  await page.goto('/cameras');

  await page.getByRole('heading', { name: 'Add a camera' }).waitFor();
  await page.getByRole('textbox', { name: 'slug' }).fill(slug);
  await page.getByRole('textbox', { name: 'name' }).fill('E2E Zone Cam');
  await page
    .getByPlaceholder('rtsp://user:pass@192.168.1.20:554/Streaming/Channels/101')
    .fill('rtsp://127.0.0.1:18554/does-not-exist');
  await page.getByRole('button', { name: 'add camera' }).click();

  // The row appears with the slug the server stored.
  await expect(page.getByRole('link', { name: new RegExp(slug) })).toBeVisible();
});

test('a 4-vertex zone survives a reload', async ({ page }) => {
  const tokens = await adminTokens();
  await signIn(page, tokens);
  await page.goto(`/cameras`);
  await page.getByRole('link', { name: new RegExp(slug) }).click();

  // The zone editor is embedded in the editor, not behind a button.
  await expect(page.getByRole('heading', { name: 'Detection zones' })).toBeVisible();
  await expect(page.getByText('No zones on this camera')).toBeVisible();

  await page.getByRole('button', { name: 'new zone' }).click();
  const canvas = page.getByLabel(/zone editor over the .* snapshot/);
  const box = await canvas.boundingBox();
  expect(box, 'the zone canvas has a box to click inside').not.toBeNull();
  // Four clicks, four corners of a quadrilateral — each click appends exactly
  // one vertex; Enter closes the shape.
  const pts = [
    [0.2, 0.2],
    [0.8, 0.25],
    [0.75, 0.8],
    [0.25, 0.7],
  ];
  for (const [fx, fy] of pts) {
    await canvas.click({ position: { x: box!.width * fx, y: box!.height * fy } });
  }
  await expect(page.getByText('4 points')).toBeVisible();
  await page.getByRole('button', { name: 'close shape' }).click();

  // The zone editor is a <section> of its own; the camera row's save button
  // reads "saved" whenever the row is clean, so an unscoped 'saved' locator
  // is two elements. Scoping by `filter({ has: heading })` is NOT enough —
  // the camera editor's own <section> contains the heading too, so the
  // filter matches the ancestor as well and the strict-mode violation comes
  // back whenever the row happens to be clean at this instant (cold stacks
  // hit it; warm ones raced past it). The nearest ancestor section of the
  // heading is the zone editor itself, always.
  const zoneSection = page.getByRole('heading', { name: 'Detection zones' }).locator('xpath=ancestor::section[1]');
  await zoneSection.getByRole('button', { name: 'save zones' }).click();
  await expect(zoneSection.getByRole('button', { name: 'saved' })).toBeVisible();

  // The reload is the persistence: the row re-reads from the server.
  await page.reload();
  await expect(page.getByRole('heading', { name: 'Detection zones' })).toBeVisible();
  await expect(page.getByRole('button', { name: /select / })).toContainText('4 vertices');

  // And the server's row agrees — the console is not the only witness.
  const { json } = await apiAdmin<{ zones?: { polygon: number[][] }[] }>('GET', '/api/v1/cameras');
  const cam = ((json as { cameras?: { slug: string; zones?: { polygon: number[][] }[] }[] }).cameras ?? []).find(
    (c) => c.slug === slug,
  );
  expect(cam?.zones?.[0]?.polygon).toHaveLength(4);
});

test('the camera the specs created is gone when the specs are done with it', async () => {
  // Cleanup rides in this suite because it created the camera; the throwaway
  // stack is wiped by e2e-stack.sh stop anyway, but a camera left behind
  // mid-suite would show up in live.spec's tile count.
  // Non-vacuous on purpose: a cleanup that silently finds nothing is a
  // cleanup that never ran, and that is exactly how a stale tile poisoned a
  // later spec once already.
  const list = await apiAdmin<{ cameras?: { id: string; slug: string }[] }>('GET', '/api/v1/cameras');
  expect(list.status).toBe(200);
  const mine = (list.json?.cameras ?? []).filter((c) => c.slug === slug);
  expect(mine.length, `the created camera is findable for cleanup (saw ${(list.json?.cameras ?? []).map((c) => c.slug).join(', ')})`).toBe(1);
  const del = await apiAdmin('DELETE', `/api/v1/cameras/${mine[0].id}`);
  expect(del.status).toBeLessThan(300);
  await waitFor(
    'the delete to land',
    async () => {
      const after = await apiAdmin<{ cameras?: { slug: string }[] }>('GET', '/api/v1/cameras');
      return (after.json?.cameras ?? []).some((c) => c.slug === slug) ? null : 'gone';
    },
    15_000,
  );
});

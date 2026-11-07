import { readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { expect, test } from '@playwright/test';

import { CAMERA_ID, CAMERA_SLUG } from './playwright.config';
import { adminTokens, apiAdmin, postSignals, signIn, signal, sleep, waitFor } from './helpers';

// events: seeded event in timeline, drawer, clip plays to currentTime > 0.
// "Seeded" is honest here: the stack runs no detector, so the spec posts the
// worker's own signals (S5-10 intake, worker token) — the same bytes the
// detector would send. Recording is switched on for the seeded camera through
// the API first: a clip is cut from segments, and segments only exist while
// the recorder runs.

const here = path.dirname(fileURLToPath(import.meta.url));
const snapshotB64 = readFileSync(path.join(here, '..', '..', 'testdata', 'bus.jpg')).toString('base64');
const eventKey = `e2e-${Date.now()}`;
let eventRowId = '';

test('the seeded camera records: segments appear once record_mode flips on', async () => {
  // detection_enabled too: the seed camera ships with detection off, and the
  // signal intake drops signals for such a camera ("detection disabled").
  const flipped = await apiAdmin('PUT', `/api/v1/cameras/${CAMERA_ID}`, {
    record_mode: 'continuous',
    detection_enabled: true,
  });
  expect(flipped.status, `record_mode flip: ${JSON.stringify(flipped.json)}`).toBeLessThan(300);

  const from = new Date(Date.now() - 60_000).toISOString();
  await waitFor(
    'segments from the recorder',
    async () => {
      const { json } = await apiAdmin<{ segments?: unknown[] }>(
        'GET',
        `/api/v1/segments?camera_id=${CAMERA_ID}&from=${encodeURIComponent(from)}&limit=50`,
      );
      return (json.segments?.length ?? 0) >= 1 ? json.segments!.length : null;
    },
    120_000,
  );
});

test('signals open an event, close builds the clip', async () => {
  const opened = await postSignals([{ ...signal(CAMERA_ID, eventKey, 'open'), snapshot_base64: snapshotB64 }]);
  expect(opened.accepted).toBe(1);
  await sleep(3000);
  await postSignals([signal(CAMERA_ID, eventKey, 'extend')]);
  await sleep(3000);
  const closed = await postSignals([signal(CAMERA_ID, eventKey, 'close')]);
  expect(closed.accepted).toBe(1);

  // The clip is cut at close (S6): poll the row until the server says it has one.
  await waitFor(
    'the clip to be built at close',
    async () => {
      const { json } = await apiAdmin<{ events?: { id: string; event_key: string; has_clip?: boolean }[] }>(
        'GET',
        `/api/v1/events?camera_id=${CAMERA_ID}&limit=20`,
      );
      const ev = (json.events ?? []).find((e) => e.event_key === eventKey && e.has_clip);
      if (ev) {
        eventRowId = ev.id;
        return ev.id;
      }
      return null;
    },
    120_000,
  );
});

test('the event is on the timeline, the drawer shows it, and the clip plays', async ({ page }) => {
  const tokens = await adminTokens();
  expect(eventRowId, 'the previous test recorded the row id').not.toBe('');

  await signIn(page, tokens);
  await page.goto('/events');

  const card = page.getByRole('button', { name: `open the detail for the event on ${CAMERA_SLUG}` });
  await expect(card.first()).toBeVisible();
  await card.first().click();
  await expect(page).toHaveURL(new RegExp(`/events/${eventRowId}`));

  const drawer = page.getByRole('dialog');
  await expect(drawer.getByRole('heading', { name: CAMERA_SLUG })).toBeVisible();
  await expect(drawer.getByText(/severity/)).toBeVisible();
  // The snapshot the open signal carried is the frame the drawer shows.
  await expect(drawer.getByAltText(/the frame this event was detected on/)).toBeVisible();

  const clip = drawer.locator('video').first();
  await expect(clip).toBeVisible({ timeout: 30_000 });
  await clip.evaluate((el) => {
    const v = el as HTMLVideoElement;
    v.muted = true;
    return v.play();
  });
  await expect
    .poll(() => clip.evaluate((el) => (el as HTMLVideoElement).currentTime).catch(() => 0), {
      message: 'the clip never played',
      timeout: 30_000,
    })
    .toBeGreaterThan(0);

  // Closing the drawer is the backdrop or the close button; Escape is the app's.
  await page.keyboard.press('Escape');
  await expect(page.getByRole('dialog')).toHaveCount(0);
});

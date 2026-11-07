import { expect, test } from '@playwright/test';

import { CAMERA_SLUG, CONSOLE_URL } from './playwright.config';
import { adminTokens, signIn } from './helpers';

// live: player mounts, video readyState >= 1.
// The tile's <video> is hidden until go2rtc's WebRTC track arrives (the page
// has no other connected-state text), so "the video is visible" is the
// playing state, and readyState >= 1 is the card's own bar: the browser has
// metadata for a stream it received.

test('the seeded camera streams: the tile plays and the video has metadata', async ({ page }) => {
  const tokens = await adminTokens();
  await signIn(page, tokens);
  await page.goto('/live');

  // Slug-exact: any other camera whose name starts with "E2E" (the cameras
  // spec's own camera did) must never be mistaken for the seeded one.
  const tile = page.locator('section').filter({ has: page.getByText(CAMERA_SLUG, { exact: true }) });
  await expect(tile.locator('span', { hasText: CAMERA_SLUG })).toBeVisible();

  const video = tile.locator('video').first();
  await expect(video).toBeVisible({ timeout: 30_000 });
  await video.waitFor({ state: 'attached' });
  await expect
    .poll(
      async () =>
        video.evaluate((el) => (el as HTMLVideoElement).readyState).catch(() => 0),
      { message: `the ${CAMERA_SLUG} video never reached readyState >= 1`, timeout: 30_000 },
    )
    .toBeGreaterThanOrEqual(1);
});

test('the live page names the media host it streams from — never a hard-coded one', async ({ page }) => {
  const tokens = await adminTokens();
  await signIn(page, tokens);
  await page.goto('/live');
  // The subtitle carries the public URL the server handed out (S8-04); the
  // console learned it, it did not know it.
  await expect(page.getByText(/streaming from http:\/\/127\.0\.0\.1:1985/)).toBeVisible();
  expect(CONSOLE_URL).toContain('127.0.0.1');
});

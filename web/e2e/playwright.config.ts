import { defineConfig } from '@playwright/test';

// The e2e suite runs against the throwaway stack scripts/e2e-stack.sh builds:
// sqlite + media under /tmp/nvr-e2e/<runid>, the server on :9090, a private
// go2rtc, and `vite preview` on :3113. Every address comes from the harness's
// environment — a spec that hard-codes a port is a spec that passes against
// yesterday's stack.
export const CONSOLE_URL = process.env.E2E_CONSOLE_URL ?? 'http://127.0.0.1:3113';
export const API_URL = process.env.E2E_API_URL ?? 'http://127.0.0.1:9090';
export const ADMIN_USER = process.env.BOOTSTRAP_ADMIN_USER ?? 'admin';
export const ADMIN_PASSWORD = process.env.BOOTSTRAP_ADMIN_PASSWORD ?? '';
export const WORKER_TOKEN = process.env.E2E_WORKER_TOKEN ?? '';
export const CAMERA_ID = process.env.E2E_CAMERA_ID ?? '';
export const CAMERA_SLUG = process.env.E2E_CAMERA_SLUG ?? '';
export const RUN_ID = process.env.NVR_E2E_RUN_ID ?? '';

export default defineConfig({
  testDir: '.',
  // One worker, serial files: the specs share the seeded camera (events.spec
  // flips its record_mode), and a parallel run would race on that mutation.
  fullyParallel: false,
  workers: 1,
  retries: 0,
  timeout: 180_000,
  expect: { timeout: 15_000 },
  // list for the terminal, html into web/playwright-report (gitignored) so a
  // failed run carries its own traces and screenshots for whoever reads it.
  reporter: [['list'], ['html', { open: 'never' }]],
  globalSetup: './global-setup.ts',
  use: {
    baseURL: CONSOLE_URL,
    headless: true,
    video: 'retain-on-failure',
  },
});

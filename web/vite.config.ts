import { defineConfig } from 'vitest/config';
import react from '@vitejs/plugin-react';

// API and WebSocket proxy targets. The console talks to relative /api and /ws
// paths, so whatever serves it — dev server, `vite preview`, or the e2e
// harness — must proxy both prefixes to the Go server. `dev` and `preview`
// share one object deliberately: the Playwright suite runs against
// `vite preview` on the built bundle, so the proxy it exercises is declared
// here. Production parity is docker/nginx.conf, which proxies the same two
// prefixes.
//
// NVR_PROXY_TARGET points the dev/preview proxy at a server on a non-default
// port (the e2e harness runs its own server instance against a throwaway
// database, scripts/e2e-stack.sh). It has no effect on the built bundle, which
// only ever uses the relative paths.
const proxyTarget = process.env.NVR_PROXY_TARGET ?? 'http://localhost:9090';

const proxy = {
  '/api': {
    target: proxyTarget,
    changeOrigin: true,
  },
  '/ws': {
    target: proxyTarget,
    ws: true,
    changeOrigin: true,
  },
};

export default defineConfig({
  plugins: [react()],
  base: '/',
  server: {
    port: 3000,
    proxy,
  },
  preview: {
    port: 3000,
    proxy,
  },
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: ['./src/test/setup.ts'],
    // Vitest owns the unit suite under src/ only. The Playwright suite in
    // e2e/ is a browser suite with its own runner (`npm run test:e2e`), and
    // collecting it here fails on import with no global setup in play.
    include: ['src/**/*.test.{ts,tsx}'],
    coverage: {
      // The unit-coverage denominator is src/ only. The v8 provider's default
      // include is every .ts in the package, so the Playwright specs in e2e/
      // counted as 0% here — the ratchet would have punished adding browser
      // tests, which is the opposite of what a floor is for.
      include: ['src/**'],
    },
  },
});

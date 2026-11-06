import '@testing-library/jest-dom/vitest';
import { afterEach } from 'vitest';
import { cleanup } from '@testing-library/react';

// Vitest's setup, wired in vite.config.ts. Kept to what the harness itself
// needs: unmount between tests so a component cannot leak into the next, and a
// cleared localStorage because the session keys (nvr_access_token and friends,
// S8-02) live there and a stale session would make an auth test pass for the
// wrong reason.
//
// Browser shims are added when a component actually needs one, not in
// anticipation — OurWay's setup carries a ResizeObserver stub for a virtualised
// list nvrWay has no reason to grow.
afterEach(() => {
  cleanup();
  localStorage.clear();
});

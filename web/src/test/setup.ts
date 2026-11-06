import '@testing-library/jest-dom/vitest';
import { afterEach } from 'vitest';
import { cleanup } from '@testing-library/react';

/**
 * localStorage, when the runtime does not supply a working one.
 *
 * Node 26 defines a `localStorage` global of its own (experimental web
 * storage). It is on globalThis before jsdom gets there, it shadows jsdom's
 * implementation, and without `--localstorage-file` it warns and returns
 * `undefined` — so `window.localStorage` is undefined in the suite even though
 * `window === globalThis` and jsdom is otherwise intact.
 *
 * The console's session lives in localStorage by design (auth/session.ts), so
 * the harness has to provide the storage a browser provides. This is not a
 * stand-in for behaviour under test: every assertion about what gets stored is
 * still made against a real Storage-shaped key/value store, with the spec's
 * string coercion. It is installed only when the host's own is unusable, so a
 * runtime that ships a working one keeps it.
 */
function installStorage(): void {
  const existing = (globalThis as { localStorage?: unknown }).localStorage as
    | { getItem?: unknown }
    | undefined;
  if (typeof existing?.getItem === 'function') return;

  class MemoryStorage implements Storage {
    private map = new Map<string, string>();

    get length(): number {
      return this.map.size;
    }

    key(index: number): string | null {
      return Array.from(this.map.keys())[index] ?? null;
    }

    getItem(key: string): string | null {
      return this.map.get(String(key)) ?? null;
    }

    setItem(key: string, value: string): void {
      this.map.set(String(key), String(value));
    }

    removeItem(key: string): void {
      this.map.delete(String(key));
    }

    clear(): void {
      this.map.clear();
    }
  }

  Object.defineProperty(globalThis, 'localStorage', {
    value: new MemoryStorage(),
    configurable: true,
    writable: true,
  });
}

installStorage();

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


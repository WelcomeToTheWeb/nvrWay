/**
 * The shell. What is pinned here is exactly what only the shell can get
 * right: the route table's pairings (a drawer and an editor that are URLs,
 * so deep links survive a reload), the nav that hides the doors a role will
 * be refused, the redirect that answers a signed-out deep link, and the live
 * feed whose lifecycle belongs to the session rather than to any one page.
 */
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import client from './api/client';
import type { EventView } from './api/events';
import type { CameraView } from './api/cameras';
import { AuthProvider } from './auth/context';
import { useRealtime } from './stores/realtime';
import { axiosHarness, type Call, type Scripted } from './test/axiosHarness';
import App from './App';

function event(over: Partial<EventView> = {}): EventView {
  return {
    id: 'e1',
    event_key: 'k1',
    camera_id: 'c1',
    camera_slug: 'porch',
    action: 'open',
    open: false,
    started_at: '2026-11-06T20:00:00Z',
    ended_at: '2026-11-06T20:00:30Z',
    seconds: 30,
    severity: 2,
    signal_count: 3,
    classes: ['person'],
    zones: ['driveway'],
    zone_ids: ['z1'],
    rules: ['intrusion'],
    objects: [],
    object_count: 4,
    has_snapshot: true,
    has_clip: true,
    clip_seconds: 44,
    snapshot_uploaded: false,
    clip_uploaded: false,
    created_at: '2026-11-06T20:00:01Z',
    ...over,
  };
}

function camera(over: Partial<CameraView> = {}): CameraView {
  return {
    id: 'c1',
    slug: 'porch',
    name: 'Porch',
    source_uri: 'rtsp://10.0.0.20:554/1',
    record_mode: 'events',
    detection_enabled: true,
    detection_fps: 2,
    detection_width: 640,
    detection_height: 360,
    sensitivity: 0.45,
    min_area_ratio: 0.004,
    zones: [],
    local_retention_days: 14,
    remote_retention_days: 90,
    active: true,
    created_at: '2026-11-01T00:00:00Z',
    updated_at: '2026-11-06T00:00:00Z',
    ...over,
  };
}

function attachHarness(script: (call: Call) => Scripted) {
  const { calls, adapter } = axiosHarness(script);
  const previous = client.defaults.adapter;
  client.defaults.adapter = adapter;
  return { calls, restore: () => (client.defaults.adapter = previous) };
}

/** The default server behind the shell: the event wall's reads, the camera
 * list, and the drawer's detail. Every test that navigates has its reads
 * answered here. */
function server(over: Partial<Record<string, Scripted>> = {}) {
  return (call: Call): Scripted => {
    const key = `${call.method} ${call.url.split('?')[0]}`;
    if (over[key]) return over[key] as Scripted;
    if (call.url.endsWith('/snapshot')) {
      return { status: 200, data: new Blob(['jpeg'], { type: 'image/jpeg' }) };
    }
    switch (key) {
      case 'get /v1/events':
        return {
          status: 200,
          data: { events: [event()], count: 1, truncated: false, limit: 50, offset: 0 },
        };
      case 'get /v1/events/e1':
        return { status: 200, data: event() };
      case 'get /v1/segments':
        return {
          status: 200,
          data: {
            segments: [],
            count: 0,
            from: '2026-11-06T20:00:00Z',
            to: '2026-11-06T20:00:30Z',
            truncated: false,
          },
        };
      case 'get /v1/cameras':
        return { status: 200, data: { cameras: [camera()], count: 1 } };
      case 'get /v1/cameras/c1':
        return { status: 200, data: camera() };
      case 'post /auth/logout':
        return { status: 200, data: { ok: true } };
      default:
        return { status: 404, data: { error: `unexpected ${call.method} ${call.url}` } };
    }
  };
}

function renderAt(entry: string, roles: string[] = ['admin']) {
  if (roles.length > 0) {
    localStorage.setItem('nvr_roles', JSON.stringify(roles));
    localStorage.setItem('nvr_access_token', 'a-token');
  }
  return render(
    <MemoryRouter initialEntries={[entry]}>
      <AuthProvider>
        <App />
      </AuthProvider>
    </MemoryRouter>,
  );
}

const heldObjectURL = URL.createObjectURL;

// The shell's feed-lifecycle spies. Every test runs against noops so no suite
// ever opens a real socket; the lifecycle tests read these back to count.
let connectSpy: ReturnType<typeof vi.fn>;
let disconnectSpy: ReturnType<typeof vi.fn>;

beforeEach(() => {
  localStorage.clear();
  URL.createObjectURL = vi.fn(() => 'blob:held') as unknown as typeof URL.createObjectURL;
  URL.revokeObjectURL = vi.fn() as unknown as typeof URL.revokeObjectURL;
  connectSpy = vi.fn();
  disconnectSpy = vi.fn();
  act(() =>
    useRealtime.setState({
      status: 'idle',
      stale: false,
      reason: null,
      events: {},
      cameras: {},
      recorders: {},
      storageQueue: null,
      connect: connectSpy,
      disconnect: disconnectSpy,
    }),
  );
});

afterEach(() => {
  URL.createObjectURL = heldObjectURL;
});

describe('App — the nav is role-aware', () => {
  it('an admin is offered every door', async () => {
    const h = attachHarness(server());
    renderAt('/events', ['admin']);

    const nav = await screen.findByRole('navigation', { name: 'Console' });
    expect(nav).toBeTruthy();
    for (const label of ['Live', 'Events', 'Cameras', 'Settings', 'Audit']) {
      expect(screen.getByRole('link', { name: label })).toBeTruthy();
    }
    h.restore();
  });

  it('a viewer is not offered Settings or Audit — the server would refuse them anyway', async () => {
    const h = attachHarness(server());
    renderAt('/events', ['viewer']);

    await screen.findByRole('navigation', { name: 'Console' });
    expect(screen.getByRole('link', { name: 'Events' })).toBeTruthy();
    expect(screen.queryByRole('link', { name: 'Settings' })).toBeNull();
    expect(screen.queryByRole('link', { name: 'Audit' })).toBeNull();
    h.restore();
  });

  it('a viewer who types /settings meets the page refusal, not a blank', async () => {
    const h = attachHarness(server());
    renderAt('/settings', ['viewer']);

    // The nav hides the tab; the page still refuses the deep link. Hiding is
    // courtesy, the refusal is the contract — and no settings request fires.
    expect(await screen.findByText(/Settings belong to an admin/)).toBeTruthy();
    expect(h.calls.filter((c) => c.url.startsWith('/storage') || c.url.startsWith('/settings'))).toEqual([]);
    h.restore();
  });
});

describe('App — the session decides what renders', () => {
  it('a signed-out deep link is answered by the login page, not the page behind it', async () => {
    const h = attachHarness(server());
    localStorage.clear(); // no token, no roles
    renderAt('/settings', []);

    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeTruthy();
    expect(screen.queryByRole('navigation', { name: 'Console' })).toBeNull();
    h.restore();
  });

  it('the callback stays reachable signed out — the SSO handoff arrives with no session', async () => {
    const h = attachHarness(server());
    localStorage.clear();
    renderAt('/auth/callback', []);

    // The callback's own page, answering its empty-handoff state — proof the
    // guard did not bounce the route before it could read its fragment.
    expect(await screen.findByText(/No sign-in handoff arrived/)).toBeTruthy();
    h.restore();
  });
});

describe('App — deep links survive a reload', () => {
  it('/events/:id renders the wall with the drawer open', async () => {
    const h = attachHarness(server());
    renderAt('/events/e1', ['viewer']);

    // A fresh render at the URL is what a reload is: the drawer is a route,
    // not modal state, so it comes back on its own.
    const dialog = await screen.findByRole('dialog');
    expect(dialog).toBeTruthy();
    // The wall is still behind it.
    expect(screen.getByText('4 boxes')).toBeTruthy();
    h.restore();
  });

  it('/cameras/:id renders the list with the editor open', async () => {
    const h = attachHarness(server());
    renderAt('/cameras/c1', ['operator']);

    expect(await screen.findByRole('button', { name: 'back to the list' })).toBeTruthy();
    h.restore();
  });
});

describe('App — the shell owns the feed', () => {
  it('opens the feed for a signed-in session and closes it when the session ends', async () => {
    const h = attachHarness(server());

    const view = renderAt('/events', ['viewer']);
    await screen.findByRole('navigation', { name: 'Console' });
    expect(connectSpy).toHaveBeenCalled();

    fireEvent.click(screen.getByRole('button', { name: 'sign out' }));
    await waitFor(() => expect(disconnectSpy).toHaveBeenCalled());
    // Signed out, the shell is the login page and the nav is gone.
    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeTruthy();

    view.unmount();
    h.restore();
  });

  it('unmounting the shell closes the feed', async () => {
    const h = attachHarness(server());

    const view = renderAt('/events', ['viewer']);
    await screen.findByRole('navigation', { name: 'Console' });
    view.unmount();
    expect(disconnectSpy).toHaveBeenCalled();
    h.restore();
  });

  it('a stale feed says so above every page, and a current feed says nothing', async () => {
    const h = attachHarness(server());
    renderAt('/events', ['viewer']);
    await screen.findByRole('navigation', { name: 'Console' });
    expect(screen.queryByRole('status')).toBeNull();

    act(() =>
      useRealtime.setState({
        status: 'stale',
        stale: true,
        reason: 'the live feed dropped; retrying',
      }),
    );
    expect(await screen.findByRole('status')).toBeTruthy();
    expect(screen.getByRole('status').textContent).toContain('the live feed dropped; retrying');

    act(() =>
      useRealtime.setState({
        status: 'refused',
        reason: 'the live feed has stopped opening; check the server address',
      }),
    );
    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('the live feed has stopped opening');
    h.restore();
  });
});

// The shell mounts the pages under one router; this guards the test harness
// itself: if someone re-renders `App` inside a bare `<Routes>`, the nav's
// `NavLink`s would silently render outside any route and pass nothing.
describe('App — harness sanity', () => {
  it('renders nothing outside a router by construction', () => {
    expect(() =>
      render(
        <Routes>
          <Route path="*" element={<App />} />
        </Routes>,
      ),
    ).toThrow();
  });
});

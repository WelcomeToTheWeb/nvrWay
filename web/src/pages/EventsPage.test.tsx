/**
 * The wall of events. What is pinned here is the page's two contracts with the
 * operator: the address bar *is* the filter state (in both directions), and a
 * card says what is true about its media instead of showing a broken box.
 *
 * The feed is driven through the real store (`useRealtime.setState`), because the
 * page's promise is about a frame arriving — mocking the store would test the
 * mock. Media is fetched through the real client with the shared axios harness, so
 * the bearer rule and the 404's body are the ones the server actually sends.
 */
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import client from '../api/client';
import type { EventView } from '../api/events';
import { __resetMediaCache } from '../api/media';
import { useRealtime } from '../stores/realtime';
import { axiosHarness, UNAUTHORIZED, type Call, type Scripted } from '../test/axiosHarness';
import EventsPage from './EventsPage';

const CAMERAS = { cameras: [{ id: 'c1', slug: 'porch', name: 'Porch' }] };

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
    det_width: 640,
    det_height: 360,
    has_snapshot: true,
    has_clip: true,
    clip_seconds: 44,
    snapshot_uploaded: false,
    clip_uploaded: false,
    created_at: '2026-11-06T20:00:01Z',
    ...over,
  };
}

function attachHarness(script: (call: Call) => Scripted) {
  const { calls, adapter } = axiosHarness(script);
  const previous = client.defaults.adapter;
  client.defaults.adapter = adapter;
  return { calls, restore: () => (client.defaults.adapter = previous) };
}

/** The default server: a page of events, the one camera, and a real JPEG for the
 * picture. `over` replaces one of those answers per test. */
function server(events: EventView[], over: Partial<Record<string, Scripted>> = {}) {
  return (call: Call): Scripted => {
    if (call.url.endsWith('/snapshot')) {
      return over.snapshot ?? { status: 200, data: new Blob(['jpeg'], { type: 'image/jpeg' }) };
    }
    if (call.url.startsWith('/v1/cameras')) return { status: 200, data: CAMERAS };
    if (call.url.startsWith('/v1/events')) {
      return {
        status: 200,
        data: { events, count: events.length, truncated: false, limit: 50, offset: 0 },
      };
    }
    return { status: 404, data: { error: `unexpected ${call.url}` } };
  };
}

function renderAt(entry: string) {
  return render(
    <MemoryRouter initialEntries={[entry]}>
      <Routes>
        <Route path="/events" element={<EventsPage />} />
        <Route path="/events/:id" element={<EventsPage />} />
      </Routes>
    </MemoryRouter>,
  );
}

const eventCalls = (calls: Call[]) =>
  calls.filter((c) => c.url === '/v1/events' || c.url.startsWith('/v1/events?'));

// jsdom ships no object URLs, and the media hook makes them from fetched bytes.
const heldObjectURL = URL.createObjectURL;

beforeEach(() => {
  URL.createObjectURL = vi.fn(() => 'blob:held') as unknown as typeof URL.createObjectURL;
  URL.revokeObjectURL = vi.fn() as unknown as typeof URL.revokeObjectURL;
  __resetMediaCache();
  act(() => useRealtime.setState({ events: {}, recorders: {} }));
});

afterEach(() => {
  URL.createObjectURL = heldObjectURL;
});

describe('EventsPage — the address bar is the filter state', () => {
  it('asks for exactly the filters the address bar names', async () => {
    const h = attachHarness(server([event()]));
    renderAt('/events?severity=2&class=person&camera_id=c1');

    await waitFor(() => expect(eventCalls(h.calls).length).toBeGreaterThan(0));
    const url = eventCalls(h.calls)[0].url;
    expect(url).toContain('severity=2');
    expect(url).toContain('class=person');
    expect(url).toContain('camera_id=c1');
    h.restore();
  });

  it('sends no filter the address bar did not name', async () => {
    const h = attachHarness(server([event()]));
    renderAt('/events');
    await waitFor(() => expect(eventCalls(h.calls).length).toBeGreaterThan(0));
    expect(eventCalls(h.calls)[0].url).toBe('/v1/events');
    h.restore();
  });

  it('writes a card\'s class into the address bar and asks again for it', async () => {
    const h = attachHarness(server([event()]));
    renderAt('/events');
    await screen.findByText('person');

    fireEvent.click(screen.getByRole('button', { name: 'person' }));

    await waitFor(() => expect(eventCalls(h.calls)).toHaveLength(2));
    expect(eventCalls(h.calls)[1].url).toContain('class=person');
    expect(await screen.findByText('class: person')).toBeTruthy();
    h.restore();
  });

  it('takes a filter out of the address bar when its chip is cleared', async () => {
    const h = attachHarness(server([event()]));
    renderAt('/events?severity=3');
    await screen.findByText('severity: 3');

    fireEvent.click(screen.getByRole('button', { name: 'clear the severity filter' }));

    await waitFor(() => expect(eventCalls(h.calls)).toHaveLength(2));
    expect(eventCalls(h.calls)[1].url).toBe('/v1/events');
    expect(screen.queryByText('severity: 3')).toBeNull();
    h.restore();
  });

  it('names a filtered camera by its slug, because an operator chose a name', async () => {
    const h = attachHarness(server([event()]));
    renderAt('/events?camera_id=c1');
    expect(await screen.findByText('camera: porch')).toBeTruthy();
    h.restore();
  });

  it('keeps the id when the camera list cannot say what it means', async () => {
    const h = attachHarness(
      (call) =>
        call.url.startsWith('/v1/cameras')
          ? { status: 401, data: { error: 'access token is not valid' } }
          : server([event()])(call),
    );
    renderAt('/events?camera_id=7f3a-no-such-camera');
    expect(await screen.findByText('camera: 7f3a-no-such-camera')).toBeTruthy();
    // The wall still shows its events: an unreadable camera list is not a reason
    // to hide the events that arrived.
    expect(screen.getByText('4 boxes')).toBeTruthy();
    h.restore();
  });

  it('round-trips a search through the address bar', async () => {
    const h = attachHarness(server([event()]));
    renderAt('/events');
    await waitFor(() => expect(eventCalls(h.calls).length).toBeGreaterThan(0));

    fireEvent.change(screen.getByLabelText('search events'), { target: { value: 'driveway' } });
    fireEvent.click(screen.getByRole('button', { name: 'search' }));

    await waitFor(() => expect(eventCalls(h.calls)).toHaveLength(2));
    expect(eventCalls(h.calls)[1].url).toContain('q=driveway');
    expect(await screen.findByText('search: driveway')).toBeTruthy();
    h.restore();
  });
});

describe('EventsPage — a card is the proof', () => {
  it('shows the frame the event came with', async () => {
    const h = attachHarness(server([event()]));
    const { container } = renderAt('/events');
    await waitFor(() => expect(container.querySelectorAll('img')).toHaveLength(1));
    expect(container.querySelector('img')?.getAttribute('src')).toBe('blob:held');
    h.restore();
  });

  it('says no snapshot yet when the row never had a picture, and asks for none', async () => {
    const h = attachHarness(server([event({ has_snapshot: false })]));
    const { container } = renderAt('/events');
    expect(await screen.findByText('no snapshot yet')).toBeTruthy();
    expect(container.querySelectorAll('img')).toHaveLength(0);
    expect(h.calls.some((c) => c.url.endsWith('/snapshot'))).toBe(false);
    h.restore();
  });

  it('repeats the route\'s own sentence when the picture is gone', async () => {
    const body = new Blob([JSON.stringify({ error: 'snapshot not available' })], {
      type: 'application/json',
    });
    const h = attachHarness(
      server([event()], { snapshot: { status: 404, data: body } }),
    );
    const { container } = renderAt('/events');
    expect(await screen.findByText('snapshot not available')).toBeTruthy();
    expect(container.querySelectorAll('img')).toHaveLength(0);
    h.restore();
  });

  it('names the camera, the duration, the severity and what was counted', async () => {
    const h = attachHarness(server([event()]));
    const { container } = renderAt('/events');
    await waitFor(() => expect(container.querySelectorAll('article')).toHaveLength(1));
    const card = within(container.querySelector('article') as HTMLElement);
    expect(card.getByText('porch')).toBeTruthy();
    expect(card.getByText('30s')).toBeTruthy();
    expect(card.getByTitle('severity 2')).toBeTruthy();
    expect(card.getByText('4 boxes')).toBeTruthy();
    expect(card.getByText('driveway')).toBeTruthy();
    h.restore();
  });

  it('says so when an event has no clip, rather than offering one', async () => {
    const h = attachHarness(server([event({ has_clip: false })]));
    renderAt('/events');
    expect(await screen.findByText('no clip')).toBeTruthy();
    h.restore();
  });

  it('says when the page was cut short, because a quiet-looking wall may be a clipped one', async () => {
    const h = attachHarness((call) =>
      call.url.startsWith('/v1/events')
        ? {
            status: 200,
            data: { events: [event()], count: 1, truncated: true, limit: 50, offset: 0 },
          }
        : server([event()])(call),
    );
    renderAt('/events');
    expect(await screen.findByText(/cut short/)).toBeTruthy();
    h.restore();
  });

  it('says nothing matched, which is not the same as nothing happened', async () => {
    const h = attachHarness(server([]));
    renderAt('/events?class=bear');
    expect(await screen.findByText(/nothing matches these filters/i)).toBeTruthy();
    h.restore();
  });

  it('names a refused list instead of showing an empty wall', async () => {
    const h = attachHarness((call) => (call.url.startsWith('/v1/events') ? UNAUTHORIZED : server([])(call)));
    renderAt('/events');
    expect(await screen.findByText(/access token is not valid/i)).toBeTruthy();
    h.restore();
  });
});

describe('EventsPage — the feed keeps the cards honest', () => {
  it('shows live for an open event', async () => {
    const h = attachHarness(server([event({ open: true, ended_at: '' })]));
    renderAt('/events');
    expect(await screen.findByText('live')).toBeTruthy();
    h.restore();
  });

  it('moves a card from an event.update frame, with no request to the server', async () => {
    const h = attachHarness(server([event({ open: true, ended_at: '' })]));
    renderAt('/events');
    await screen.findByText('live');
    const before = h.calls.length;

    act(() =>
      useRealtime.setState({
        events: { e1: { id: 'e1', open: true, seconds: 99, signal_count: 7 } },
      }),
    );

    expect(await screen.findByText('1m 39s')).toBeTruthy();
    expect(h.calls).toHaveLength(before);
    h.restore();
  });

  it('takes live away when the close frame says the event is over', async () => {
    const h = attachHarness(server([event({ open: true, ended_at: '' })]));
    renderAt('/events');
    await screen.findByText('live');

    act(() => useRealtime.setState({ events: { e1: { id: 'e1', open: false } } }));

    await waitFor(() => expect(screen.queryByText('live')).toBeNull());
    h.restore();
  });

  it('counts the open ones, because that is the number an operator is scanning for', async () => {
    const h = attachHarness(
      server([event({ id: 'e1', open: true, ended_at: '' }), event({ id: 'e2', open: false })]),
    );
    renderAt('/events');
    expect(await screen.findByText(/2 events, 1 open/)).toBeTruthy();
    h.restore();
  });
});

describe('EventsPage — the drawer is the address bar too', () => {
  it('puts the event in the address bar when a card is opened, and asks for its detail', async () => {
    const h = attachHarness((call) =>
      call.url.match(/\/v1\/events\/e1$/)
        ? { status: 200, data: event() }
        : call.url.includes('/v1/segments')
          ? { status: 200, data: { segments: [], count: 0, from: '2026-11-06T20:00:00Z', to: '2026-11-06T20:00:30Z', truncated: false } }
          : call.url.match(/\/v1\/cameras\/c1$/)
            ? { status: 200, data: { id: 'c1', slug: 'porch', name: 'Porch', detection_width: 640, detection_height: 360, zones: [] } }
            : server([event()])(call),
    );
    renderAt('/events');
    await screen.findByText('4 boxes');

    fireEvent.click(screen.getByRole('button', { name: /open the detail for the event on porch/ }));

    await waitFor(() =>
      expect(h.calls.some((c) => c.url === '/v1/events/e1')).toBe(true),
    );
    // The wall is still there behind the drawer, filters and all.
    expect(screen.getByText('4 boxes')).toBeTruthy();
    h.restore();
  });
});

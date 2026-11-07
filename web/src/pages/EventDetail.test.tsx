/**
 * The drawer. Three things are pinned here, and each is a place where the easy
 * implementation would show an operator something untrue:
 *
 * a clip that does not exist must say `clip not available` — the server's own
 * sentence — and must not mount a player to be disappointed in;
 *
 * a row's `has_clip: true` is a claim, so the route's answer is what decides;
 *
 * a recorded box is a pixel in a frame the camera row may never have named, and
 * a box drawn at a guessed position looks exactly like evidence.
 */
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import client from '../api/client';
import type { CameraRefs, EventView, SegmentView } from '../api/events';
import { __resetMediaCache } from '../api/media';
import { useRealtime } from '../stores/realtime';
import { axiosHarness, type Call, type Scripted } from '../test/axiosHarness';
import EventDetail from './EventDetail';

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
    objects: [
      { class: 'person', conf: 0.9, box: [100, 50, 200, 150] },
      { class: 'car', conf: 0.7, box: [0, 0, 640, 360] },
    ],
    object_count: 2,
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

function camera(over: Partial<CameraRefs> = {}): CameraRefs {
  return {
    id: 'c1',
    slug: 'porch',
    name: 'Porch',
    detection_width: 640,
    detection_height: 360,
    zones: [
      {
        id: 'z1',
        name: 'driveway',
        polygon: [
          [0.1, 0.1],
          [0.6, 0.1],
          [0.6, 0.8],
        ],
        rule: 'intrusion',
        enabled: true,
      },
    ],
    ...over,
  };
}

function segment(over: Partial<SegmentView> = {}): SegmentView {
  return {
    id: 's1',
    camera_id: 'c1',
    slug: 'porch',
    generation: 1,
    start_ts: '2026-11-06T20:00:00Z',
    end_ts: '2026-11-06T20:00:05Z',
    seconds: 5,
    bytes: 20480,
    kind: 'continuous',
    uploaded: false,
    ...over,
  };
}

const jsonBlob = (body: unknown) => new Blob([JSON.stringify(body)], { type: 'application/json' });

function attachHarness(script: (call: Call) => Scripted) {
  const { calls, adapter } = axiosHarness(script);
  const previous = client.defaults.adapter;
  client.defaults.adapter = adapter;
  return { calls, restore: () => (client.defaults.adapter = previous) };
}

function server(over: Partial<Record<string, Scripted>> = {}) {
  const detail = over.detail ?? { status: 200, data: event() };
  const cam = over.camera ?? { status: 200, data: camera() };
  const clip = over.clip ?? { status: 200, data: new Blob(['mp4'], { type: 'video/mp4' }) };
  const segments = over.segments ?? {
    status: 200,
    data: {
      segments: [segment()],
      count: 1,
      from: '2026-11-06T20:00:00Z',
      to: '2026-11-06T20:00:30Z',
      truncated: false,
    },
  };

  return (call: Call): Scripted => {
    if (call.url.endsWith('/snapshot')) {
      return over.snapshot ?? { status: 200, data: new Blob(['jpeg'], { type: 'image/jpeg' }) };
    }
    if (call.url.endsWith('/clip')) return clip;
    if (call.url === '/v1/events/e1') return detail;
    if (call.url.startsWith('/v1/cameras/')) return cam;
    if (call.url.startsWith('/v1/segments')) return segments;
    return { status: 404, data: { error: `unexpected ${call.url}` } };
  };
}

function open(over: Partial<Record<string, Scripted>> = {}) {
  const h = attachHarness(server(over));
  const onClose = vi.fn();
  const view = render(<EventDetail id="e1" onClose={onClose} />);
  return { ...view, h, onClose };
}

const heldObjectURL = URL.createObjectURL;

beforeEach(() => {
  URL.createObjectURL = vi.fn(() => 'blob:held') as unknown as typeof URL.createObjectURL;
  URL.revokeObjectURL = vi.fn() as unknown as typeof URL.revokeObjectURL;
  __resetMediaCache();
  act(() => useRealtime.setState({ events: {} }));
});

afterEach(() => {
  URL.createObjectURL = heldObjectURL;
});

describe('EventDetail — the clip', () => {
  it('says clip not available when the row never had one, and asks the route for nothing', async () => {
    const { container, h } = open({ detail: { status: 200, data: event({ has_clip: false }) } });
    expect(await screen.findByText('clip not available')).toBeTruthy();
    expect(container.querySelectorAll('video')).toHaveLength(0);
    expect(h.calls.some((c) => c.url.endsWith('/clip'))).toBe(false);
    h.restore();
  });

  it('says clip not available when the flag is true and the route disagrees', async () => {
    // The row believes there is a clip; the sweeper took the file. The route is
    // the truth, and its sentence is the one the operator reads.
    const { container, h } = open({
      clip: { status: 404, data: jsonBlob({ error: 'clip not available' }) },
    });
    expect(await screen.findByText('clip not available')).toBeTruthy();
    expect(container.querySelectorAll('video')).toHaveLength(0);
    h.restore();
  });

  it('mounts a player when the bytes actually arrive', async () => {
    const { container, h } = open();
    await waitFor(() => expect(container.querySelectorAll('video')).toHaveLength(1));
    expect(container.querySelector('video')?.getAttribute('src')).toBe('blob:held');
    h.restore();
  });

  it('names an unreachable tier instead of showing a dead player', async () => {
    const { container, h } = open({
      clip: { status: 502, data: jsonBlob({ error: 'the storage tier is not reachable' }) },
    });
    expect(await screen.findByText('the storage tier is not reachable')).toBeTruthy();
    expect(container.querySelectorAll('video')).toHaveLength(0);
    h.restore();
  });

  it('states the clip it found, in the length the server measured', async () => {
    const { h } = open();
    expect(await screen.findByText('44s of footage')).toBeTruthy();
    h.restore();
  });
});

describe('EventDetail — the boxes and the frame they belong to', () => {
  it('draws every recorded box on the frame the event names', async () => {
    const { container, h } = open();
    await waitFor(() => expect(container.querySelectorAll('rect')).toHaveLength(2));
    // The worker's pixels are `[x1, y1, x2, y2]` in the 640×360 frame the
    // event row carries (P-10); the overlay canvas is 0..1000, so the
    // person's left edge is 100/640 of the way across.
    const first = container.querySelector('rect') as SVGRectElement;
    expect(Number(first.getAttribute('x'))).toBeCloseTo(156.25, 2);
    expect(Number(first.getAttribute('width'))).toBeCloseTo(156.25, 2);
    expect(screen.getByText(/drawn on a 640×360 frame/)).toBeTruthy();
    h.restore();
  });

  it('draws them on a default camera row — the frame travels on the event, not the row', async () => {
    // P-10's whole point: the camera row's detection_width/height stay at 0
    // and the boxes still land, because the intake stored the tap size with
    // the event.
    const { container, h } = open({
      camera: { status: 200, data: camera({ detection_width: 0, detection_height: 0 }) },
    });
    await waitFor(() => expect(container.querySelectorAll('rect')).toHaveLength(2));
    expect(screen.getByText(/drawn on a 640×360 frame/)).toBeTruthy();
    h.restore();
  });

  it('refuses to draw them when the event itself names no frame', async () => {
    // The rows this refusal is for were written before the frame was
    // recorded; the count survives — what is withheld is the position, not
    // the fact.
    const { container, h } = open({
      detail: { status: 200, data: event({ det_width: 0, det_height: 0 }) },
    });
    await screen.findByText(/2 boxes recorded/);
    expect(container.querySelectorAll('rect')).toHaveLength(0);
    expect(screen.getByText(/will not guess/)).toBeTruthy();
    expect(screen.getByText(/2 boxes recorded/).textContent).toContain('2 boxes recorded');
    h.restore();
  });

  it('draws the zone polygon without a frame, because a polygon is stored normalised', async () => {
    const { container, h } = open({
      detail: { status: 200, data: event({ det_width: 0, det_height: 0 }) },
    });
    await waitFor(() => expect(container.querySelectorAll('polygon')).toHaveLength(1));
    expect(container.querySelector('polygon')?.getAttribute('points')).toBe('100,100 600,100 600,800');
    h.restore();
  });

  it('still places the boxes when the camera row itself is unreadable', async () => {
    // P-10 moved the frame onto the event, so a drawer that cannot read the
    // camera row has lost the polygons, not the proof.
    const { container, h } = open({
      camera: { status: 403, data: { error: 'you are not allowed to read cameras' } },
    });
    expect(await screen.findByText(/2 boxes recorded/)).toBeTruthy();
    await waitFor(() => expect(container.querySelectorAll('rect')).toHaveLength(2));
    // The event stands: a drawer that cannot read the row is not a drawer
    // that has lost the event.
    expect(screen.getByText('porch')).toBeTruthy();
    h.restore();
  });
});

describe('EventDetail — the footage the event sits in', () => {
  it('draws the manifest over the window the server answered', async () => {
    const { container, h } = open();
    await waitFor(() => expect(container.querySelectorAll('[title*="porch"]')).toHaveLength(1));
    expect(screen.getByText(/1 segments between/)).toBeTruthy();
    expect(screen.getByText(/the clip may reach past this window/)).toBeTruthy();
    h.restore();
  });

  it('asks for the event window, and no end for an event that has not ended', async () => {
    const { h } = open({ detail: { status: 200, data: event({ open: true, ended_at: '' }) } });
    await waitFor(() => expect(h.calls.some((c) => c.url.startsWith('/v1/segments'))).toBe(true));
    const asked = h.calls.find((c) => c.url.startsWith('/v1/segments'))!.url;
    expect(asked).toContain('from=2026-11-06T20%3A00%3A00Z');
    expect(asked).toContain('camera_id=c1');
    expect(asked).not.toContain('to=');
    h.restore();
  });

  it('says there is no footage to cut a clip from, which is why the clip is missing', async () => {
    const { h } = open({
      segments: {
        status: 200,
        data: {
          segments: [],
          count: 0,
          from: '2026-11-06T20:00:00Z',
          to: '2026-11-06T20:00:30Z',
          truncated: false,
        },
      },
    });
    expect(await screen.findByText(/no segments in this window/)).toBeTruthy();
    h.restore();
  });
});

describe('EventDetail — the live event', () => {
  it('says LIVE, and moves when the feed moves', async () => {
    const { h } = open({ detail: { status: 200, data: event({ open: true, ended_at: '' }) } });
    await screen.findByText(/LIVE/);

    act(() => useRealtime.setState({ events: { e1: { id: 'e1', open: true, seconds: 125 } } }));

    expect(await screen.findByText(/2m 5s/)).toBeTruthy();
    h.restore();
  });

  it('takes LIVE away when the close frame arrives', async () => {
    const { h } = open({ detail: { status: 200, data: event({ open: true, ended_at: '' }) } });
    await screen.findByText(/LIVE/);

    act(() => useRealtime.setState({ events: { e1: { id: 'e1', open: false } } }));

    await waitFor(() => expect(screen.queryByText(/LIVE/)).toBeNull());
    h.restore();
  });
});

describe('EventDetail — getting out, and getting refused', () => {
  it('closes from the button and from Escape', async () => {
    const { h, onClose } = open();
    await screen.findByText('porch');

    fireEvent.click(screen.getByRole('button', { name: 'close' }));
    expect(onClose).toHaveBeenCalledTimes(1);

    fireEvent.keyDown(window, { key: 'Escape' });
    expect(onClose).toHaveBeenCalledTimes(2);
    h.restore();
  });

  it('closes the backdrop too, because that is where the cursor already is', async () => {
    const { h, onClose } = open();
    await screen.findByText('porch');
    fireEvent.click(screen.getByRole('button', { name: 'close the detail' }));
    expect(onClose).toHaveBeenCalledTimes(1);
    h.restore();
  });

  it('repeats the server\'s sentence when the event is not there', async () => {
    const { h } = open({ detail: { status: 404, data: { error: 'event not found' } } });
    expect(await screen.findByText('event not found')).toBeTruthy();
    h.restore();
  });
});

/**
 * The zone editor. What is pinned here is the two things the card names — the
 * `0..1` round trip and the `PUT` payload — plus the one promise the console
 * makes about refusals: the server's sentence is what the operator reads.
 *
 * The canvas is *not* under test. It is a painting surface, and a headless
 * runner has no 2d context to paint into, so `getContext` answers `null` the way
 * a platform without one would. That is only possible because no vertex lives in
 * the canvas: the geometry is React state, and every pixel is derived from it —
 * which is exactly the property the round-trip test is about.
 */
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import client from '../api/client';
import type { CameraView, Zone } from '../api/cameras';
import { __resetMediaCache } from '../api/media';
import { axiosHarness, type Call, type Scripted } from '../test/axiosHarness';
import ZoneEditor from './ZoneEditor';

/** The picture's box, in CSS pixels. Every click below is stated in these
 * coordinates and asserted in normalised ones, so the mapping is the thing under
 * test rather than an assumption of it. */
const BOX = { left: 0, top: 0, width: 800, height: 450, x: 0, y: 0, right: 800, bottom: 450 };

function zone(over: Partial<Zone> = {}): Zone {
  return {
    id: 'z1',
    name: 'driveway',
    polygon: [
      [0.25, 0.25],
      [0.75, 0.25],
      [0.75, 0.75],
    ],
    classes: ['person'],
    rule: 'intrusion',
    dwell_seconds: 30,
    severity: 2,
    min_confidence: 0,
    min_area_ratio: 0,
    enabled: true,
    ...over,
  };
}

function camera(zones: Zone[]): CameraView {
  return {
    id: 'c1',
    slug: 'porch',
    name: 'Porch',
    source_uri: 'rtsp://10.0.0.20/1',
    record_mode: 'continuous',
    detection_enabled: true,
    detection_fps: 2,
    detection_width: 640,
    detection_height: 360,
    sensitivity: 0.45,
    min_area_ratio: 0.004,
    zones,
    local_retention_days: 14,
    remote_retention_days: 90,
    active: true,
    created_at: '2026-11-01T00:00:00Z',
    updated_at: '2026-11-06T00:00:00Z',
  };
}

function attachHarness(script: (call: Call) => Scripted) {
  const { calls, adapter } = axiosHarness(script);
  const previous = client.defaults.adapter;
  client.defaults.adapter = adapter;
  return { calls, restore: () => (client.defaults.adapter = previous) };
}

/** The default server: a JPEG for the backdrop, and a zone write that answers
 * with what it was given — which is what a real one does after normalising it. */
function server(over: Partial<Record<string, Scripted>> = {}) {
  return (call: Call): Scripted => {
    if (call.url.startsWith('/v1/cameras/c1/snapshot')) {
      return { status: 200, data: new Blob(['jpeg'], { type: 'image/jpeg' }) };
    }
    if (call.method === 'put' && call.url === '/v1/cameras/c1/zones') {
      return (
        over.zonesWrite ?? {
          status: 200,
          data: { camera_id: 'c1', slug: 'porch', zones: call.body },
        }
      );
    }
    return { status: 404, data: { error: `unexpected ${call.method} ${call.url}` } };
  };
}

function renderEditor(
  props: { zones?: Zone[]; canWrite?: boolean; onSaved?(zones: Zone[]): void } = {},
) {
  const zones = props.zones ?? [];
  return render(
    <ZoneEditor
      camera={camera(zones)}
      canWrite={props.canWrite ?? true}
      onSaved={props.onSaved ?? (() => undefined)}
    />,
  );
}

const canvas = () => document.querySelector('canvas') as HTMLCanvasElement;
const putBodies = (calls: Call[]) =>
  calls.filter((c) => c.method === 'put' && c.url === '/v1/cameras/c1/zones').map((c) => c.body);

function click(x: number, y: number) {
  fireEvent.pointerDown(canvas(), { clientX: x, clientY: y, pointerId: 1 });
}

const heldGetContext = HTMLCanvasElement.prototype.getContext;
const heldRect = Element.prototype.getBoundingClientRect;
const heldObjectURL = URL.createObjectURL;

beforeEach(() => {
  HTMLCanvasElement.prototype.getContext = vi.fn(() => null) as unknown as typeof HTMLCanvasElement.prototype.getContext;
  Element.prototype.getBoundingClientRect = vi.fn(() => BOX as DOMRect);
  URL.createObjectURL = vi.fn(() => 'blob:held') as unknown as typeof URL.createObjectURL;
  URL.revokeObjectURL = vi.fn() as unknown as typeof URL.revokeObjectURL;
  __resetMediaCache();
});

afterEach(() => {
  HTMLCanvasElement.prototype.getContext = heldGetContext;
  Element.prototype.getBoundingClientRect = heldRect;
  URL.createObjectURL = heldObjectURL;
});

describe('ZoneEditor — the pointer is a number, not a pixel', () => {
  it('turns clicks into normalised vertices and writes those', async () => {
    const h = attachHarness(server());
    renderEditor();
    await screen.findByText('new zone');
    fireEvent.click(screen.getByRole('button', { name: 'new zone' }));

    click(200, 112.5); // 0.25, 0.25
    click(600, 112.5); // 0.75, 0.25
    click(600, 337.5); // 0.75, 0.75
    fireEvent.click(screen.getByRole('button', { name: 'close shape' }));
    fireEvent.click(screen.getByRole('button', { name: 'save zones' }));

    await waitFor(() => expect(putBodies(h.calls)).toHaveLength(1));
    const [written] = putBodies(h.calls) as Zone[][];
    expect(written).toHaveLength(1);
    expect(written[0].polygon).toEqual([
      [0.25, 0.25],
      [0.75, 0.25],
      [0.75, 0.75],
    ]);
    h.restore();
  });

  it('draws a stored vertex back at the pixel it came from', async () => {
    const h = attachHarness(server());
    renderEditor({ zones: [zone({ polygon: [[0.25, 0.75], [0.5, 0.25], [0.75, 0.75]] })] });

    const badge = await screen.findByRole('button', { name: 'driveway · intrusion' });
    // The same number, read the other way: 0.25 of the box is 25 % of it. A rule
    // that drifted on resize would show up here as a badge off its corner.
    expect(badge.style.left).toBe('25%');
    expect(badge.style.top).toBe('75%');
    h.restore();
  });

  it('keeps a vertex inside 0..1 when the pointer leaves the picture', async () => {
    const h = attachHarness(server());
    renderEditor();
    await screen.findByText('new zone');
    fireEvent.click(screen.getByRole('button', { name: 'new zone' }));

    click(-120, 600);
    click(400, 100);
    click(600, 300);
    fireEvent.click(screen.getByRole('button', { name: 'close shape' }));
    fireEvent.click(screen.getByRole('button', { name: 'save zones' }));

    await waitFor(() => expect(putBodies(h.calls)).toHaveLength(1));
    const [written] = putBodies(h.calls) as Zone[][];
    expect(written[0].polygon[0]).toEqual([0, 1]);
    h.restore();
  });

  it('moves one vertex on a drag and leaves the others alone', async () => {
    const h = attachHarness(server());
    renderEditor({ zones: [zone()] });
    await screen.findByText('driveway · intrusion');

    // The pointer lands on vertex 0 (0.25, 0.25 → 200, 112.5) and drags it.
    fireEvent.pointerDown(canvas(), { clientX: 200, clientY: 112.5, pointerId: 1 });
    fireEvent.pointerMove(canvas(), { clientX: 80, clientY: 90, pointerId: 1 });
    fireEvent.pointerUp(canvas(), { clientX: 80, clientY: 90, pointerId: 1 });

    fireEvent.click(screen.getByRole('button', { name: 'save zones' }));
    await waitFor(() => expect(putBodies(h.calls)).toHaveLength(1));
    const [written] = putBodies(h.calls) as Zone[][];
    expect(written[0].polygon).toEqual([
      [0.1, 0.2],
      [0.75, 0.25],
      [0.75, 0.75],
    ]);
    h.restore();
  });
});

describe('ZoneEditor — the keys the plan names', () => {
  it('Enter closes the shape, and the closed zone is on the board', async () => {
    const h = attachHarness(server());
    renderEditor();
    await screen.findByText('new zone');
    fireEvent.click(screen.getByRole('button', { name: 'new zone' }));

    click(100, 100);
    click(500, 100);
    click(500, 400);
    fireEvent.keyDown(canvas(), { key: 'Enter' });

    expect(await screen.findByText(/· intrusion/)).toBeTruthy();
    // Nothing has been written yet: closing a shape is not saving it.
    expect(putBodies(h.calls)).toHaveLength(0);
    h.restore();
  });

  it('Escape throws away an unfinished shape', async () => {
    const h = attachHarness(server());
    renderEditor();
    await screen.findByText('new zone');
    fireEvent.click(screen.getByRole('button', { name: 'new zone' }));

    click(100, 100);
    click(500, 100);
    fireEvent.keyDown(canvas(), { key: 'Escape' });

    expect(screen.queryByText(/· intrusion/)).toBeNull();
    expect(screen.getByRole('button', { name: 'new zone' })).toBeTruthy();
    h.restore();
  });

  it('Delete removes the selected zone, and saving writes the smaller set', async () => {
    const h = attachHarness(server());
    renderEditor({ zones: [zone()] });
    await screen.findByText('driveway · intrusion');

    fireEvent.click(screen.getByRole('button', { name: 'driveway · intrusion' }));
    fireEvent.keyDown(canvas(), { key: 'Delete' });

    expect(screen.queryByText('driveway · intrusion')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'save zones' }));
    await waitFor(() => expect(putBodies(h.calls)).toHaveLength(1));
    expect(putBodies(h.calls)[0]).toEqual([]);
    h.restore();
  });

  it('leaves a key typed in a field to that field', async () => {
    const h = attachHarness(server());
    renderEditor({ zones: [zone()] });
    const name = await screen.findByDisplayValue('driveway');

    fireEvent.change(name, { target: { value: 'driveway east' } });
    fireEvent.keyDown(name, { key: 'Enter' });

    // Enter in the name box is the end of a name, not a second polygon.
    expect(screen.getAllByText(/driveway east/)).toHaveLength(1);
    h.restore();
  });
});

describe('ZoneEditor — the write is the server\'s shape, and the refusal is its words', () => {
  it('sends exactly the fields the zone row has, named as the server names them', async () => {
    const h = attachHarness(server());
    renderEditor({ zones: [zone()] });
    await screen.findByText('driveway · intrusion');

    fireEvent.click(screen.getByRole('button', { name: /remove driveway/ }));
    fireEvent.click(screen.getByRole('button', { name: 'new zone' }));
    click(100, 100);
    click(500, 100);
    click(500, 400);
    fireEvent.click(screen.getByRole('button', { name: 'close shape' }));
    fireEvent.click(screen.getByRole('button', { name: 'save zones' }));

    await waitFor(() => expect(putBodies(h.calls)).toHaveLength(1));
    const [written] = putBodies(h.calls) as Zone[][];
    expect(Object.keys(written[0]).sort()).toEqual([
      'classes',
      'dwell_seconds',
      'enabled',
      'id',
      'min_area_ratio',
      'min_confidence',
      'name',
      'polygon',
      'rule',
      'severity',
    ]);
    expect(written[0].classes).toEqual(['person']);
    expect(written[0].rule).toBe('intrusion');
    expect(written[0].enabled).toBe(true);
    h.restore();
  });

  it('refuses to be the authority: a two-vertex shape is sent, and the 422 is what is read', async () => {
    const detail =
      'zone z-1: polygon needs 3-32 vertices, got 2 (an open path is closed implicitly, so 3 points are a triangle)';
    const h = attachHarness(
      server({
        zonesWrite: {
          status: 422,
          data: { error: 'zone polygon invalid', detail },
        },
      }),
    );
    renderEditor();
    await screen.findByText('new zone');
    fireEvent.click(screen.getByRole('button', { name: 'new zone' }));

    click(100, 100);
    click(500, 100);
    fireEvent.click(screen.getByRole('button', { name: 'close shape' }));
    // The editor warns in its own voice first — advice is free, and the operator
    // has not lost anything yet.
    expect(await screen.findByText(/the server wants 3–32/)).toBeTruthy();

    fireEvent.click(screen.getByRole('button', { name: 'save zones' }));
    await waitFor(() => expect(putBodies(h.calls)).toHaveLength(1));
    expect(await screen.findByRole('alert')).toHaveTextContent('zone polygon invalid');
    expect(screen.getByRole('alert')).toHaveTextContent(detail);
    // And the drawing survives the refusal: the operator fixes it, they do not
    // start over.
    expect(screen.getByText('2 vertices')).toBeTruthy();
    expect(screen.getByText('the server will refuse this shape')).toBeTruthy();
    h.restore();
  });

  it('adopts the array the server answered with, because it is the one that was stored', async () => {
    const stored = [zone({ id: 'z-kept', name: 'porch edge', polygon: [[0.1, 0.1], [0.9, 0.1], [0.9, 0.9]] })];
    const h = attachHarness(
      server({ zonesWrite: { status: 200, data: { camera_id: 'c1', slug: 'porch', zones: stored } } }),
    );
    renderEditor();
    await screen.findByText('new zone');
    fireEvent.click(screen.getByRole('button', { name: 'new zone' }));

    click(100, 100);
    click(500, 100);
    click(500, 400);
    fireEvent.click(screen.getByRole('button', { name: 'close shape' }));
    fireEvent.click(screen.getByRole('button', { name: 'save zones' }));

    expect(await screen.findByText('porch edge · intrusion')).toBeTruthy();
    // The board now matches the row the server kept, so "saved" is true.
    await waitFor(() => expect(screen.getByRole('button', { name: 'saved' })).toBeDisabled());
    h.restore();
  });

  it('writes nothing until something changed', async () => {
    const h = attachHarness(server());
    renderEditor({ zones: [zone()] });
    await screen.findByText('driveway · intrusion');

    expect(screen.getByRole('button', { name: 'saved' })).toBeDisabled();
    expect(putBodies(h.calls)).toHaveLength(0);
    h.restore();
  });
});

describe('ZoneEditor — a role that cannot write is told so', () => {
  it('shows the rules and says what this role cannot do', async () => {
    const h = attachHarness(server());
    renderEditor({ zones: [zone()], canWrite: false });

    expect(await screen.findByText('your role can read zones, not change them')).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'new zone' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'save zones' })).toBeNull();
    // Read is still read: the rule set is on screen.
    expect(screen.getByText('driveway · intrusion')).toBeTruthy();
    h.restore();
  });
});

describe('ZoneEditor — a picture is a backdrop, not a requirement', () => {
  it('repeats the route\'s sentence when the camera is not producing, and still lets the operator draw', async () => {
    const h = attachHarness((call) =>
      call.url.startsWith('/v1/cameras/c1/snapshot')
        ? { status: 404, data: new Blob([JSON.stringify({ error: 'no snapshot from porch: stream is not producing' })], { type: 'application/json' }) }
        : server()(call),
    );
    renderEditor();

    expect(await screen.findByText(/drawing without a picture/)).toHaveTextContent(
      'no snapshot from porch: stream is not producing',
    );
    // The canvas is still there and still takes points: an operator knows where
    // their driveway is.
    expect(canvas()).toBeTruthy();
    h.restore();
  });
});

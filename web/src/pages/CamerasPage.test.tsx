/**
 * The cameras page. What is pinned here is the page's own two promises: the
 * editor is a URL, and the row's write is *only* the row's.
 *
 * The second one is the interesting one. `cameraInput` in `server/api/cameras.go`
 * accepts `zones`, so a form that spread the whole row into its body would write
 * the rule set through the route that has no zone audit and no polygon
 * normalisation, and the ledger would say the frame rate changed. The test fails
 * the moment someone "simplifies" the body into a spread.
 */
import { MemoryRouter, Route, Routes, useLocation } from 'react-router-dom';
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import client from '../api/client';
import type { CameraDeleteSummary, CameraView, Zone } from '../api/cameras';
import { AuthProvider } from '../auth/context';
import { __resetMediaCache } from '../api/media';
import { useRealtime } from '../stores/realtime';
import { axiosHarness, type Call, type Scripted } from '../test/axiosHarness';
import CamerasPage from './CamerasPage';

function zone(over: Partial<Zone> = {}): Zone {
  return {
    id: 'z1',
    name: 'driveway',
    polygon: [
      [0.1, 0.3],
      [0.9, 0.3],
      [0.9, 0.9],
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
    zones: [zone()],
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

function server(cameras: CameraView[], over: Partial<Record<string, Scripted>> = {}) {
  return (call: Call): Scripted => {
    if (call.url.startsWith('/v1/cameras/c1/snapshot')) {
      return { status: 200, data: new Blob(['jpeg'], { type: 'image/jpeg' }) };
    }
    if (call.method === 'get' && call.url === '/v1/cameras') {
      return { status: 200, data: { cameras } };
    }
    if (call.method === 'put' && call.url === '/v1/cameras/c1') {
      return over.rowWrite ?? { status: 200, data: camera() };
    }
    if (call.method === 'post' && call.url === '/v1/cameras') {
      return over.create ?? { status: 201, data: camera({ id: 'c2', slug: 'gate' }) };
    }
    if (call.method === 'get' && call.url === '/v1/cameras/c1/delete-summary') {
      return over.deleteSummary ?? { status: 200, data: deleteSummary() };
    }
    if (call.method === 'delete' && call.url === '/v1/cameras/c1') {
      return over.delete ?? { status: 200, data: { deleted: 'c1', slug: 'porch' } };
    }
    return { status: 404, data: { error: `unexpected ${call.method} ${call.url}` } };
  };
}

/** The server's own counts for the confirmation dialog (S8-13): the delete's
 * arithmetic, answered before the fact by `GET /:id/delete-summary`. */
function deleteSummary(over: Partial<CameraDeleteSummary> = {}): CameraDeleteSummary {
  return {
    camera_id: 'c1',
    slug: 'porch',
    segments: 3,
    events: 2,
    local_files: 11,
    remote_jobs: 4,
    storage_tier: true,
    ...over,
  };
}

/** The roles live in localStorage (auth/session.ts), which is what the provider
 * reads on mount — the same way a reload starts where the last request left off. */
function renderAt(entry: string, roles: string[] = ['operator']) {
  localStorage.setItem('nvr_roles', JSON.stringify(roles));
  localStorage.setItem('nvr_access_token', 'a-token');
  return render(
    <MemoryRouter initialEntries={[entry]}>
      <AuthProvider>
        <LocationProbe />
        <Routes>
          <Route path="/cameras" element={<CamerasPage />} />
          <Route path="/cameras/:id" element={<CamerasPage />} />
        </Routes>
      </AuthProvider>
    </MemoryRouter>,
  );
}

/** The address bar, as a test can read it: a delete that closes the editor
 * but leaves `/cameras/c1` in the bar lands the next reload on a dead URL. */
function LocationProbe() {
  const { pathname } = useLocation();
  return <p data-testid="location">{pathname}</p>;
}

beforeEach(() => {
  // jsdom ships no object URLs, and the editor's backdrop is made from fetched
  // bytes through api/media.ts.
  URL.createObjectURL = vi.fn(() => 'blob:held') as unknown as typeof URL.createObjectURL;
  URL.revokeObjectURL = vi.fn() as unknown as typeof URL.revokeObjectURL;
  __resetMediaCache();
  act(() => useRealtime.setState({ cameras: {}, recorders: {}, events: {} }));
});

describe('CamerasPage — the inventory', () => {
  it('says what each camera records, watches and holds', async () => {
    const h = attachHarness(server([camera()]));
    renderAt('/cameras');

    expect(await screen.findByText('Porch')).toBeTruthy();
    // Scoped to the row: the add-a-camera form below also carries the words
    // `events`, as one of the modes an operator can choose.
    const row = within(screen.getByRole('list'));
    expect(row.getByText('porch')).toBeTruthy();
    expect(row.getByText('events')).toBeTruthy();
    expect(row.getByText('detection on')).toBeTruthy();
    expect(row.getByText('1 zone')).toBeTruthy();
    h.restore();
  });

  it('takes online and offline from the feed, because a row always says it exists', async () => {
    const h = attachHarness(server([camera()]));
    renderAt('/cameras');
    expect(await screen.findByText('no status yet')).toBeTruthy();

    act(() =>
      useRealtime.setState({
        cameras: { c1: { camera_id: 'c1', slug: 'porch', online: false, producers: [] } },
      }),
    );
    expect(await screen.findByText('offline')).toBeTruthy();
    h.restore();
  });

  it('says an installation has no cameras, which is not the same as a failed read', async () => {
    const h = attachHarness(server([]));
    renderAt('/cameras');
    expect(await screen.findByText(/No cameras yet/)).toBeTruthy();
    h.restore();
  });

  it('names a refused list instead of drawing an empty one', async () => {
    const h = attachHarness(() => ({ status: 500, data: { error: 'the database is not reachable' } }));
    renderAt('/cameras');
    expect(await screen.findByText('the database is not reachable')).toBeTruthy();
    h.restore();
  });
});

describe('CamerasPage — the editor is a URL', () => {
  it('opens a camera from the address bar, with no second request', async () => {
    const h = attachHarness(server([camera()]));
    renderAt('/cameras/c1');

    expect(await screen.findByText('Detection zones')).toBeTruthy();
    // The list already carries the whole row, zones included, so a deep link is
    // one request — and it survives a reload because the URL is the state.
    expect(h.calls.filter((c) => c.url === '/v1/cameras')).toHaveLength(1);
    expect(h.calls.some((c) => c.url === '/v1/cameras/c1')).toBe(false);
    h.restore();
  });

  it('writes the row through the row route, and never touches the zones route', async () => {
    const h = attachHarness(server([camera()]));
    renderAt('/cameras/c1');

    const name = await screen.findByDisplayValue('Porch');
    fireEvent.change(name, { target: { value: 'Porch east' } });
    fireEvent.click(screen.getByRole('button', { name: 'save row' }));

    await waitFor(() =>
      expect(h.calls.some((c) => c.method === 'put' && c.url === '/v1/cameras/c1')).toBe(true),
    );
    expect(h.calls.some((c) => c.url === '/v1/cameras/c1/zones')).toBe(false);
    h.restore();
  });

  it('sends the row without zones, because the zone route owns the rule set', async () => {
    const h = attachHarness(server([camera()]));
    renderAt('/cameras/c1');

    const name = await screen.findByDisplayValue('Porch');
    fireEvent.change(name, { target: { value: 'Porch east' } });
    fireEvent.click(screen.getByRole('button', { name: 'save row' }));

    await waitFor(() =>
      expect(h.calls.some((c) => c.method === 'put' && c.url === '/v1/cameras/c1')).toBe(true),
    );
    const body = h.calls.find((c) => c.method === 'put' && c.url === '/v1/cameras/c1')
      ?.body as Record<string, unknown>;
    expect(body.name).toBe('Porch east');
    expect(body).not.toHaveProperty('zones');
    expect(body).not.toHaveProperty('slug');
    expect(body).not.toHaveProperty('id');
    h.restore();
  });

  it('shows the slug and does not offer it as a field', async () => {
    const h = attachHarness(server([camera()]));
    renderAt('/cameras/c1');

    expect(await screen.findByText(/porch · detection frame 640×360/)).toBeTruthy();
    // Renaming a camera moves every key it has ever written; that is a migration,
    // not a text input.
    expect(screen.queryByDisplayValue('porch')).toBeNull();
    h.restore();
  });

  it('repeats the server\'s sentence when the row is refused', async () => {
    const h = attachHarness(
      server([camera()], {
        rowWrite: { status: 422, data: { error: 'detection_fps must be 0.1..30, got 0' } },
      }),
    );
    renderAt('/cameras/c1');

    const fps = await screen.findByLabelText('detection fps');
    fireEvent.change(fps, { target: { value: '0' } });
    fireEvent.click(screen.getByRole('button', { name: 'save row' }));

    expect(await screen.findByText('detection_fps must be 0.1..30, got 0')).toBeTruthy();
    h.restore();
  });
});

describe('CamerasPage — adding a camera', () => {
  it('sends the four things the server cannot invent', async () => {
    const h = attachHarness(server([camera()]));
    renderAt('/cameras');

    fireEvent.change(screen.getByPlaceholderText('porch'), { target: { value: 'gate' } });
    fireEvent.change(screen.getByPlaceholderText('Porch'), { target: { value: 'Gate' } });
    fireEvent.change(screen.getByPlaceholderText(/rtsp:/), {
      target: { value: 'rtsp://10.0.0.21/1' },
    });
    fireEvent.click(screen.getByRole('button', { name: 'add camera' }));

    await waitFor(() => expect(h.calls.some((c) => c.method === 'post')).toBe(true));
    const body = h.calls.find((c) => c.method === 'post')?.body as Record<string, unknown>;
    expect(body).toEqual({
      slug: 'gate',
      name: 'Gate',
      source_uri: 'rtsp://10.0.0.21/1',
      record_mode: 'continuous',
    });
    h.restore();
  });

  it('says so when the slug is taken, in the server\'s words', async () => {
    const h = attachHarness(
      server([camera()], {
        create: { status: 409, data: { error: 'slug taken: porch' } },
      }),
    );
    renderAt('/cameras');

    fireEvent.change(screen.getByPlaceholderText('porch'), { target: { value: 'porch' } });
    fireEvent.change(screen.getByPlaceholderText(/rtsp:/), {
      target: { value: 'rtsp://10.0.0.22/1' },
    });
    fireEvent.click(screen.getByRole('button', { name: 'add camera' }));

    expect(await screen.findByText('slug taken: porch')).toBeTruthy();
    h.restore();
  });
});

describe('CamerasPage — a viewer reads', () => {
  it('shows the inventory and offers no way to change it', async () => {
    const h = attachHarness(server([camera()]));
    renderAt('/cameras', ['viewer']);

    expect(await screen.findByText('Porch')).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'add camera' })).toBeNull();
    expect(screen.queryByPlaceholderText('porch')).toBeNull();
    h.restore();
  });

  it('says what this role cannot do, on the row it cannot change', async () => {
    const h = attachHarness(server([camera()]));
    renderAt('/cameras/c1', ['viewer']);

    // Two sentences, because there are two things not to change: the row, and
    // the rule set drawn over its picture.
    expect(await screen.findByText('your role can read cameras, not change them')).toBeTruthy();
    expect(screen.getByText('your role can read zones, not change them')).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'save row' })).toBeNull();
    h.restore();
  });
});

describe('CamerasPage — the delete is admin’s, and the confirmation counts', () => {
  it('an operator is not offered the delete the route would refuse', async () => {
    const h = attachHarness(server([camera()]));
    renderAt('/cameras/c1', ['operator']);
    await screen.findByRole('button', { name: 'back to the list' });

    // The route answers 403 for operator; a destructive control shown to a
    // role the server refuses teaches that the console's buttons lie.
    expect(screen.queryByRole('button', { name: 'delete this camera' })).toBeNull();
    expect(h.calls.some((c) => c.url.includes('delete-summary'))).toBe(false);
    h.restore();
  });

  it('the confirmation names the counts the server counted — never a bare are-you-sure', async () => {
    const h = attachHarness(server([camera()]));
    renderAt('/cameras/c1', ['admin']);

    fireEvent.click(await screen.findByRole('button', { name: 'delete this camera' }));
    const dialog = await screen.findByRole('alertdialog');
    // The counts are the server's, from its own read — not the page's guess.
    expect(h.calls.some((c) => c.url === '/v1/cameras/c1/delete-summary')).toBe(true);
    expect(dialog.textContent).toContain('3 segments');
    expect(dialog.textContent).toContain('2 events');
    expect(dialog.textContent).toContain('11 local files');
    expect(dialog.textContent).toContain('4 remote purge jobs');
    h.restore();
  });

  it('no tier says nothing remote to purge, instead of a bare zero', async () => {
    const h = attachHarness(
      server([camera()], {
        deleteSummary: { status: 200, data: deleteSummary({ storage_tier: false, remote_jobs: 0 }) },
      }),
    );
    renderAt('/cameras/c1', ['admin']);

    fireEvent.click(await screen.findByRole('button', { name: 'delete this camera' }));
    const dialog = await screen.findByRole('alertdialog');
    expect(dialog.textContent).toContain('No storage tier is configured');
    h.restore();
  });

  it('the refusal’s own sentence is what shows when the server says no', async () => {
    const h = attachHarness(
      server([camera()], {
        delete: { status: 403, data: { error: 'your role cannot delete cameras' } },
      }),
    );
    renderAt('/cameras/c1', ['admin']);

    fireEvent.click(await screen.findByRole('button', { name: 'delete this camera' }));
    const dialog = await screen.findByRole('alertdialog');
    fireEvent.click(within(dialog).getByRole('button', { name: 'delete porch' }));

    expect(await screen.findByRole('alert')).toBeTruthy();
    expect(screen.getByRole('alert').textContent).toContain('your role cannot delete cameras');
    // A refused delete changes nothing: the editor is still on screen.
    expect(screen.getByRole('button', { name: 'back to the list' })).toBeTruthy();
    h.restore();
  });

  it('a deleted camera leaves no row in the list and no editor on screen', async () => {
    const cams = [camera()];
    const h = attachHarness((call) => {
      if (call.method === 'delete' && call.url === '/v1/cameras/c1') {
        cams.length = 0; // the row is gone from the server's world now
        return { status: 200, data: { deleted: 'c1', slug: 'porch' } };
      }
      return server(cams)(call);
    });
    renderAt('/cameras/c1', ['admin']);

    fireEvent.click(await screen.findByRole('button', { name: 'delete this camera' }));
    const dialog = await screen.findByRole('alertdialog');
    fireEvent.click(within(dialog).getByRole('button', { name: 'delete porch' }));

    // The editor closes — there is nothing left to edit — the address bar
    // stops naming a camera that no longer exists, and the reloaded list no
    // longer carries the row.
    await waitFor(() => expect(screen.queryByRole('button', { name: 'back to the list' })).toBeNull());
    expect(screen.getByTestId('location').textContent).toBe('/cameras');
    expect(await screen.findByRole('heading', { name: 'Cameras' })).toBeTruthy();
    expect(screen.queryByText('Porch')).toBeNull();
    h.restore();
  });
});

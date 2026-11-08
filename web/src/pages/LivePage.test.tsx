/**
 * LivePage renders the wall: one tile per camera, each tile saying what it
 * knows about the media and what the recorder last closed. The player itself is
 * mocked — its wire contract is `go2rtc.test.ts`'s job. What is pinned here is
 * the page's promise to the operator: the media host comes from the server, every
 * state is a sentence, and a camera that cannot show anything says why instead of
 * showing a black box.
 */
import { act, render, screen, waitFor } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import client from '../api/client';
import { axiosHarness, UNAUTHORIZED, type Call } from '../test/axiosHarness';
import LivePage from './LivePage';
import { connectLive } from '../live/go2rtc';
import { useRealtime, type RecorderStatus } from '../stores/realtime';

vi.mock('../live/go2rtc', () => ({
  connectLive: vi.fn(),
  liveWsUrl: vi.fn((base: string, slug: string) => `${base}/api/ws?src=${slug}`),
}));

const connectMock = vi.mocked(connectLive);

interface PlayerCall {
  publicBaseUrl: string;
  slug: string;
  onState(state: { kind: string; reason?: string }): void;
}

const MEDIA_HOST = 'http://media.example:1984';

const started: PlayerCall[] = [];
const closed: string[] = [];

function attachHarness(script: Parameters<typeof axiosHarness>[0]) {
  const { calls, adapter } = axiosHarness(script);
  const previous = client.defaults.adapter;
  client.defaults.adapter = adapter;
  return { calls, restore: () => (client.defaults.adapter = previous) };
}

const CAMERAS = {
  cameras: [
    { id: 'c1', slug: 'front-door', name: 'Front door' },
    { id: 'c2', slug: 'porch', name: 'Porch' },
  ],
};

function ok(call: Call) {
  if (call.url.includes('settings/live')) {
    return { status: 200, data: { go2rtc_public_url: MEDIA_HOST } };
  }
  if (call.url.includes('cameras')) return { status: 200, data: CAMERAS };
  return { status: 404, data: { error: `unexpected ${call.url}` } };
}

beforeEach(() => {
  started.length = 0;
  closed.length = 0;
  // The feed is a module singleton; a frame left in it by one test would be a
  // segment the next test never announced.
  act(() => useRealtime.setState({ recorders: {} }));
  connectMock.mockImplementation((options) => {
    const call = options as unknown as PlayerCall;
    started.push(call);
    return { close: () => closed.push(call.slug) };
  });
});

/** A `recorder.status` frame as `server/media/segments.go` sends it. */
function status(cameraId: string, lastSegmentAt: string, over: Partial<RecorderStatus> = {}): RecorderStatus {
  return {
    camera_id: cameraId,
    mode: 'continuous',
    segment_seconds: 5,
    last_segment_at: lastSegmentAt,
    ...over,
  };
}

/** The same time the console renders it: the server's UTC instant, in the clock
 * the operator reads. Pinned here so the assertion is about the line showing the
 * feed's value, not about a timezone this suite happens to run in. */
function clock(rfc3339: string): string {
  return new Intl.DateTimeFormat(undefined, {
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  }).format(new Date(rfc3339));
}

function tiles(container: HTMLElement): HTMLElement[] {
  return Array.from(container.querySelectorAll('section')) as HTMLElement[];
}

describe('LivePage', () => {
  it('asks the server for the media host, the cameras, and today\'s count', async () => {
    const h = attachHarness(ok);
    render(<LivePage />);
    await waitFor(() => expect(started).toHaveLength(2));
    expect(h.calls.map((c) => c.url).sort()).toEqual(['/v1/cameras', '/v1/events', '/v1/settings/live']);
    h.restore();
  });

  it('hands the player the media host the server named, never a built-in one', async () => {
    const h = attachHarness(ok);
    render(<LivePage />);
    await waitFor(() => expect(started).toHaveLength(2));
    expect(started.map((s) => s.publicBaseUrl)).toEqual([MEDIA_HOST, MEDIA_HOST]);
    expect(started.map((s) => s.slug)).toEqual(['front-door', 'porch']);
    h.restore();
  });

  it('names the camera on its tile, so a wall of two is not a wall of black boxes', async () => {
    const h = attachHarness(ok);
    render(<LivePage />);
    expect(await screen.findByText('Front door')).toBeTruthy();
    expect(screen.getByText('Porch')).toBeTruthy();
    h.restore();
  });

  it('says offline, in go2rtc\'s words, when a camera has no producer', async () => {
    const h = attachHarness(ok);
    render(<LivePage />);
    await waitFor(() => expect(started).toHaveLength(2));

    await act(async () => {
      started[0].onState({ kind: 'offline', reason: 'webrtc/offer: stream not found' });
    });

    expect(screen.getByText('offline')).toBeTruthy();
    // The tile states the condition in its own sentence; go2rtc's raw words
    // ride on the sentence as its tooltip — present for the operator who
    // wants the diagnosis, not printed over the field.
    expect(screen.getByText('offline').getAttribute('title')).toBe(
      'webrtc/offer: stream not found',
    );
    h.restore();
  });

  it('distinguishes a media host it cannot reach from a camera with nothing to show', async () => {
    const h = attachHarness(ok);
    render(<LivePage />);
    await waitFor(() => expect(started).toHaveLength(2));

    await act(async () => {
      started[0].onState({ kind: 'unreachable', reason: 'the media host closed the connection' });
    });

    expect(screen.getByText('unreachable')).toBeTruthy();
    expect(screen.queryByText('offline')).toBeNull();
    h.restore();
  });

  it('shows the picture only when a track arrived', async () => {
    const h = attachHarness(ok);
    const { container } = render(<LivePage />);
    await waitFor(() => expect(started).toHaveLength(2));
    const videos = () => Array.from(container.querySelectorAll('video'));
    // No track yet, so nothing is showing a picture.
    expect(videos().every((v) => v.hidden)).toBe(true);

    await act(async () => {
      started[0].onState({ kind: 'playing' });
    });

    expect(videos().filter((v) => !v.hidden)).toHaveLength(1);
    h.restore();
  });

  it('says the problem and the first move when the installation has no media host, with the server\'s words on the sentence', async () => {
    const refusal = {
      error: 'GO2RTC_PUBLIC_URL is not configured, so this installation has no media host to name',
    };
    const h = attachHarness((call) =>
      call.url.includes('settings/live') ? { status: 503, data: refusal } : ok(call),
    );
    render(<LivePage />);

    const sentence = await screen.findByText(/record could not be read/);
    expect(sentence.getAttribute('title')).toBe(refusal.error);
    // Without a media host there is nothing to stream from, so no player starts.
    expect(started).toHaveLength(0);
    h.restore();
  });

  it('says so when there are no cameras yet, rather than rendering an empty wall', async () => {
    const h = attachHarness((call) =>
      call.url.includes('cameras') ? { status: 200, data: { cameras: [] } } : ok(call),
    );
    render(<LivePage />);
    expect(await screen.findByText(/no cameras/i)).toBeTruthy();
    expect(started).toHaveLength(0);
    h.restore();
  });

  it('names a refused camera list instead of showing nothing', async () => {
    const h = attachHarness((call) => (call.url.includes('cameras') ? UNAUTHORIZED : ok(call)));
    render(<LivePage />);
    const sentence = await screen.findByText(/record could not be read/);
    expect(sentence.getAttribute('title')).toMatch(/access token is not valid/i);
    h.restore();
  });

  it('closes every player when the wall is left', async () => {
    const h = attachHarness(ok);
    const { unmount } = render(<LivePage />);
    await waitFor(() => expect(started).toHaveLength(2));

    unmount();
    expect(closed.sort()).toEqual(['front-door', 'porch']);
    h.restore();
  });
});

/**
 * The recorder's line (card S4-06). A different fact from the media state above
 * it: the wall can show a dead camera live, and a recording camera not at all.
 * What is pinned here is where the line's value comes from — the feed, keyed by
 * camera id — and that reading it costs the server no request.
 */
describe('LivePage — the recorder line', () => {
  it('says there is no segment yet, rather than implying a recording it has not seen', async () => {
    const h = attachHarness(ok);
    render(<LivePage />);
    expect(await screen.findAllByText(/no segment yet/)).toHaveLength(2);
    h.restore();
  });

  it('puts the segment on the tile the feed named, and on no other', async () => {
    const h = attachHarness(ok);
    const { container } = render(<LivePage />);
    await screen.findAllByText(/no segment yet/);

    act(() => useRealtime.setState({ recorders: { c1: status('c1', '2026-11-06T20:46:12Z') } }));

    const [front, porch] = tiles(container);
    expect(front.textContent).toContain(`last segment ${clock('2026-11-06T20:46:12Z')}`);
    expect(porch.textContent).toContain('no segment yet');
    h.restore();
  });

  it('says what the recorder said: the mode, the length it cuts, and when the last one closed', async () => {
    const h = attachHarness(ok);
    const { container } = render(<LivePage />);
    await screen.findAllByText(/no segment yet/);

    act(() =>
      useRealtime.setState({
        recorders: { c1: status('c1', '2026-11-06T20:46:12Z', { mode: 'events', segment_seconds: 15 }) },
      }),
    );

    const line = tiles(container)[0].textContent ?? '';
    expect(line).toContain(`last segment ${clock('2026-11-06T20:46:12Z')}`);
    expect(line).toContain('events');
    expect(line).toContain('15s segments');
    h.restore();
  });

  it('moves when the feed moves, and asks the server for nothing to do it', async () => {
    const h = attachHarness(ok);
    const { container } = render(<LivePage />);
    await waitFor(() => expect(h.calls).toHaveLength(3));

    act(() => useRealtime.setState({ recorders: { c1: status('c1', '2026-11-06T20:46:12Z') } }));
    const first = tiles(container)[0].textContent ?? '';

    act(() => useRealtime.setState({ recorders: { c1: status('c1', '2026-11-06T20:46:17Z') } }));
    const second = tiles(container)[0].textContent ?? '';

    expect(first).toContain(clock('2026-11-06T20:46:12Z'));
    expect(second).toContain(clock('2026-11-06T20:46:17Z'));
    expect(second).not.toContain(clock('2026-11-06T20:46:12Z'));
    // The status line is the feed's, so the wall still made exactly the three
    // requests it makes on its own. This is the card's "no polling".
    expect(h.calls.map((c) => c.url).sort()).toEqual(['/v1/cameras', '/v1/events', '/v1/settings/live']);
    h.restore();
  });

  it('keeps reporting closed segments when the wall cannot show a picture', async () => {
    const h = attachHarness(ok);
    const { container } = render(<LivePage />);
    await waitFor(() => expect(started).toHaveLength(2));

    act(() => {
      started[0].onState({ kind: 'offline', reason: 'webrtc/offer: stream not found' });
      useRealtime.setState({ recorders: { c1: status('c1', '2026-11-06T20:46:12Z') } });
    });

    const line = tiles(container)[0].textContent ?? '';
    expect(line).toContain('offline');
    expect(line).toContain(`last segment ${clock('2026-11-06T20:46:12Z')}`);
    h.restore();
  });

  it('shows a timestamp it cannot read as it arrived, not as Invalid Date', async () => {
    const h = attachHarness(ok);
    const { container } = render(<LivePage />);
    await screen.findAllByText(/no segment yet/);

    act(() => useRealtime.setState({ recorders: { c1: status('c1', 'not a timestamp') } }));

    const line = tiles(container)[0].textContent ?? '';
    expect(line).toContain('last segment not a timestamp');
    expect(line).not.toContain('Invalid Date');
    h.restore();
  });
});

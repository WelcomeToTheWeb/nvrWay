/**
 * LivePage renders the wall: one tile per camera, each tile saying what it
 * knows about the media. The player itself is mocked — its wire contract is
 * `go2rtc.test.ts`'s job. What is pinned here is the page's promise to the
 * operator: the media host comes from the server, every state is a sentence, and
 * a camera that cannot show anything says why instead of showing a black box.
 */
import { act, render, screen, waitFor } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import client from '../api/client';
import { axiosHarness, UNAUTHORIZED, type Call } from '../test/axiosHarness';
import LivePage from './LivePage';
import { connectLive } from '../live/go2rtc';

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
  connectMock.mockImplementation((options) => {
    const call = options as unknown as PlayerCall;
    started.push(call);
    return { close: () => closed.push(call.slug) };
  });
});

describe('LivePage', () => {
  it('asks the server for the media host and for the cameras', async () => {
    const h = attachHarness(ok);
    render(<LivePage />);
    await waitFor(() => expect(started).toHaveLength(2));
    expect(h.calls.map((c) => c.url).sort()).toEqual(['/v1/cameras', '/v1/settings/live']);
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
    expect(screen.getByText('webrtc/offer: stream not found')).toBeTruthy();
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

  it('repeats the server\'s own sentence when the installation has no media host', async () => {
    const refusal = {
      error: 'GO2RTC_PUBLIC_URL is not configured, so this installation has no media host to name',
    };
    const h = attachHarness((call) =>
      call.url.includes('settings/live') ? { status: 503, data: refusal } : ok(call),
    );
    render(<LivePage />);

    expect(await screen.findByText(refusal.error)).toBeTruthy();
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
    expect(await screen.findByText(/access token is not valid/i)).toBeTruthy();
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

/**
 * The wall. Every camera, live, on one screen.
 *
 * Two promises shape this file. The media host is **named by the server**
 * (`GET /api/v1/settings/live`), never baked into the bundle, so the same build
 * serves any installation. And nothing here is a spinner: every state the page
 * or a tile can be in is a sentence the operator can act on, because a wall of
 * black boxes tells you nothing and "offline" tells you where to look.
 */
import { useCallback, useEffect, useRef, useState } from 'react';

import axios from 'axios';
import client from '../api/client';
import { connectLive, type LiveState } from '../live/go2rtc';

/** The part of a camera row this page renders. The wall does not need the
 * detection tuning, and asking for a narrower view is the server's problem. */
interface Camera {
  id: string;
  slug: string;
  name: string;
}

type PageState =
  | { kind: 'reading' }
  | { kind: 'failed'; reason: string }
  | { kind: 'ready'; base: string; cameras: Camera[] };

/** The server's own words when it has them. A status code is not a sentence,
 * and "something went wrong" is not either. */
function serverWords(error: unknown, fallback: string): string {
  if (axios.isAxiosError(error)) {
    const body = error.response?.data as { error?: string } | undefined;
    if (body?.error) return body.error;
    if (error.response) return `the server answered ${error.response.status}`;
  }
  return fallback;
}

/** Only three states carry a reason, and the type says which. Narrowing with
 * `"reason" in state` does not satisfy the compiler across a discriminated
 * union, and casting would hide a state that gains one later. */
function reasonOf(state: LiveState): string | undefined {
  return state.kind === 'offline' || state.kind === 'unreachable' || state.kind === 'unsupported'
    ? state.reason
    : undefined;
}

/** What a tile says when it has no picture, and why. */
const TILE_STATE: Record<Exclude<LiveState['kind'], 'playing'>, string> = {
  connecting: 'connecting',
  ended: 'ended',
  offline: 'offline',
  unreachable: 'unreachable',
  unsupported: 'unsupported',
};

function LiveTile({ base, camera }: { base: string; camera: Camera }) {
  const videoRef = useRef<HTMLVideoElement>(null);
  const [state, setState] = useState<LiveState>({ kind: 'connecting' });

  useEffect(() => {
    const video = videoRef.current;
    if (!video) return;
    const handle = connectLive({
      publicBaseUrl: base,
      slug: camera.slug,
      video,
      onState: setState,
    });
    return () => {
      handle.close();
      // A stream left attached keeps its producer counted on the media host.
      video.srcObject = null;
    };
  }, [base, camera.slug]);

  const playing = state.kind === 'playing';

  return (
    <section className="border border-border bg-surface p-3">
      <header className="flex items-baseline justify-between gap-3">
        <h2 className="font-cap text-sm uppercase tracking-cap text-text-primary">{camera.name}</h2>
        <span className="font-mono text-xs text-text-secondary">{camera.slug}</span>
      </header>

      {/* The element is always there — attaching a track to a fresh one would
          drop the picture — but it is hidden until a track exists. A hidden
          video is not a black box; the sentence below is what is on screen. */}
      <video
        ref={videoRef}
        hidden={!playing}
        autoPlay
        playsInline
        muted
        className="mt-2 aspect-video w-full bg-black"
      />

      {!playing && (
        <p className="mt-2 text-sm">
          <span
            className={
              state.kind === 'connecting'
                ? 'text-text-secondary'
                : 'text-danger uppercase tracking-cap font-cap text-xs'
            }
          >
            {TILE_STATE[state.kind]}
          </span>
          {reasonOf(state) ? (
            <span className="ml-2 text-text-secondary">{reasonOf(state)}</span>
          ) : null}
        </p>
      )}
    </section>
  );
}

export default function LivePage() {
  const [page, setPage] = useState<PageState>({ kind: 'reading' });

  const load = useCallback(async () => {
    // Both at once: the wall should be up in one round trip, and neither
    // answer depends on the other.
    const both = await Promise.all([
      client.get<{ go2rtc_public_url: string }>('/v1/settings/live'),
      client.get<{ cameras: Camera[] }>('/v1/cameras'),
    ]).then(
      ([media, cameras]) => ({ media, cameras, error: null as unknown }),
      (error: unknown) => ({ media: null, cameras: null, error }),
    );

    if (!both.media) {
      // One failure path, because there is one thing to say: the server's own
      // words. "GO2RTC_PUBLIC_URL is not configured, so this installation has no
      // media host to name" is already the diagnosis; branching on the status to
      // restate it would only add a way to be out of date.
      setPage({
        kind: 'failed',
        reason: serverWords(both.error, 'the media host or the camera list could not be read'),
      });
      return;
    }

    setPage({
      kind: 'ready',
      base: both.media.data.go2rtc_public_url,
      cameras: both.cameras?.data.cameras ?? [],
    });
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  if (page.kind === 'reading') {
    return (
      <main className="p-6">
        <h1 className="font-cap text-2xl uppercase tracking-cap text-text-primary">Live</h1>
        <p className="mt-4 text-sm text-text-secondary">Reading the camera list.</p>
      </main>
    );
  }

  if (page.kind !== 'ready') {
    return (
      <main className="p-6">
        <h1 className="font-cap text-2xl uppercase tracking-cap text-text-primary">Live</h1>
        <p className="mt-4 text-sm text-danger">{page.reason}</p>
      </main>
    );
  }

  if (page.cameras.length === 0) {
    return (
      <main className="p-6">
        <h1 className="font-cap text-2xl uppercase tracking-cap text-text-primary">Live</h1>
        <p className="mt-4 text-sm text-text-secondary">
          No cameras yet. Add one on the cameras page and it will appear here.
        </p>
      </main>
    );
  }

  return (
    <main className="p-6">
      <h1 className="font-cap text-2xl uppercase tracking-cap text-text-primary">Live</h1>
      <p className="mt-1 text-sm text-text-secondary">
        {page.cameras.length} cameras, streaming from {page.base}
      </p>
      <div className="mt-4 grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
        {page.cameras.map((camera) => (
          <LiveTile key={camera.id} base={page.base} camera={camera} />
        ))}
      </div>
    </main>
  );
}

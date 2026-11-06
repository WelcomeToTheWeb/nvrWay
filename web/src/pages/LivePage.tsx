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

import client, { serverWords } from '../api/client';
import { connectLive, type LiveState } from '../live/go2rtc';
import { useRealtime, type RecorderStatus } from '../stores/realtime';

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

/** The recorder's clock, in the operator's own clock: the value on the wire is
 * UTC because the server owns event time, and the wall is read by a person who
 * lives in a timezone. */
const SEGMENT_CLOCK = new Intl.DateTimeFormat(undefined, {
  hour: '2-digit',
  minute: '2-digit',
  second: '2-digit',
});

/** `last_segment_at`, as a time of day. A timestamp this build cannot read is
 * shown as it arrived rather than as `Invalid Date` — the server's oddity is
 * information, the formatter's complaint is not. */
function segmentClock(rfc3339: string): string {
  const at = new Date(rfc3339);
  return Number.isNaN(at.getTime()) ? rfc3339 : SEGMENT_CLOCK.format(at);
}

/** What the recorder last closed, in the recorder's own words.
 *
 * This is a different fact from the one above it. The media line says what the
 * *media host* is doing — a viewer can watch a dead camera live, and a recording
 * camera can be unwatchable. This line says a segment closed, and when, which is
 * the only proof of recording the console has: the recorder announces once per
 * closed segment (`server/media/segments.go`), never per frame, so this line
 * moves on the recorder's clock and needs no request of its own.
 *
 * It stops short of the word `recording`. The feed proves the last segment
 * closed; it does not prove the next one will, and a camera whose ffmpeg died
 * keeps showing its last closed segment until `camera.status` says offline —
 * which is the server's own announcement of that, on the line above.
 */
function RecorderLine({ status }: { status: RecorderStatus | undefined }) {
  if (!status) {
    return (
      <p className="mt-2 font-mono text-xs text-text-secondary">
        no segment yet — the recorder has not closed one for this camera
      </p>
    );
  }
  return (
    <p className="mt-2 font-mono text-xs text-text-secondary">
      <span>last segment {segmentClock(status.last_segment_at)}</span>
      <span className="ml-2 uppercase tracking-cap">{status.mode}</span>
      <span className="ml-2">{status.segment_seconds}s segments</span>
    </p>
  );
}

function LiveTile({ base, camera }: { base: string; camera: Camera }) {
  const videoRef = useRef<HTMLVideoElement>(null);
  const [state, setState] = useState<LiveState>({ kind: 'connecting' });
  // The recorder's line comes from the feed, keyed by camera id — the id, not the
  // slug, because a slug can be reused and an id cannot.
  const recorder = useRealtime((s) => s.recorders[camera.id]);

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
    <section className="border border-rule bg-bg-card p-3">
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
                : 'text-status-error uppercase tracking-cap font-cap text-xs'
            }
          >
            {TILE_STATE[state.kind]}
          </span>
          {reasonOf(state) ? (
            <span className="ml-2 text-text-secondary">{reasonOf(state)}</span>
          ) : null}
        </p>
      )}

      <RecorderLine status={recorder} />
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

  // The wall opens the live feed, and nothing here closes it. `useRealtime` is one
  // socket per tab, and the recorder's line is one of the things it carries; a
  // tile unmounting must not drop the feed the next page reads, so `disconnect()`
  // stays the shell's (S8-09) call, made when the session ends. `connect()` is
  // idempotent, so StrictMode's double-invoke costs nothing, and with no access
  // token it opens nothing and says why.
  useEffect(() => {
    useRealtime.getState().connect();
  }, []);

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
        <p className="mt-4 text-sm text-status-error">{page.reason}</p>
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

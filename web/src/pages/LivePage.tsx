/**
 * The wall. Every camera, live, on one screen — as a Metro mosaic: each
 * camera is a flat field whose header band states its condition in the
 * field's own colour (cyan live, amber connecting, red down), and the
 * signature move is the tile flip: when a new event lands for a camera,
 * its tile turns once to show the event, then turns back.
 *
 * Two promises shape this file. The media host is **named by the server**
 * (`GET /api/v1/settings/live`), never baked into the bundle, so the same build
 * serves any installation. And nothing here is a spinner: every state the page
 * or a tile can be in is a sentence the operator can act on, because a wall of
 * black boxes tells you nothing and "offline" tells you where to look.
 */
import { useCallback, useEffect, useRef, useState } from 'react';
import { Link } from 'react-router-dom';

import client, { serverWords } from '../api/client';
import type { EventView } from '../api/events';
import { connectLive, type LiveState } from '../live/go2rtc';
import { useRealtime, type RealtimeEvent, type RecorderStatus } from '../stores/realtime';

/** The part of a camera row this page renders. The wall does not need the
 * detection tuning, and asking for a narrower view is the server's problem. */
interface Camera {
  id: string;
  slug: string;
  name: string;
}

type PageState =
  | { kind: 'reading' }
  | { kind: 'failed'; reason: string; raw: string }
  | {
      kind: 'ready';
      base: string;
      cameras: Camera[];
      today: { events: EventView[]; count: number } | null;
    };

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
  // Freshness is a fact the recorder just proved: a segment closed inside
  // three of its own lengths. That fact earns the green field; an older one
  // is history and rides plain. The words never claim "recording" — the
  // close proves the last one, not the next.
  const fresh =
    Date.now() - new Date(status.last_segment_at).getTime() <
    status.segment_seconds * 3 * 1000;
  return (
    <p className="mt-2 font-mono text-xs text-text-secondary">
      {fresh ? (
        <span
          className="px-1.5 py-0.5"
          style={{ background: 'var(--field-green)', color: 'var(--field-green-ink)' }}
        >
          last segment {segmentClock(status.last_segment_at)}
        </span>
      ) : (
        <span>last segment {segmentClock(status.last_segment_at)}</span>
      )}
      <span className="ml-2 font-semibold">{status.mode}</span>
      <span className="ml-2">{status.segment_seconds}s segments</span>
    </p>
  );
}

/** The event's field, chosen by severity — the world's two trouble fields and
 * no third hue: amber for noteworthy, red from "call somebody" up. The
 * severity number rides the field so the scale survives the two-colour rule. */
function eventField(severity: number): { bg: string; ink: string } {
  if (severity >= 3) return { bg: 'var(--field-red)', ink: 'var(--field-red-ink)' };
  return { bg: 'var(--field-amber)', ink: 'var(--field-amber-ink)' };
}

/** The tile's header field, stated by the two truths the console has: the
 * media host's line (this tile's own stream) and the server's `camera.status`
 * broadcast. Red outranks amber because a camera the server calls offline is
 * a fact, and "connecting" to it is only an attempt. */
function headerField(
  media: LiveState['kind'],
  serverOnline: boolean | undefined,
): { bg: string; ink: string; word: string } {
  if (serverOnline === false) return { bg: 'var(--field-red)', ink: 'var(--field-red-ink)', word: 'down' };
  if (media === 'playing') return { bg: 'var(--field-cyan)', ink: 'var(--field-cyan-ink)', word: 'live' };
  if (media === 'connecting') return { bg: 'var(--field-amber)', ink: 'var(--field-amber-ink)', word: 'opening' };
  return { bg: 'var(--field-red)', ink: 'var(--field-red-ink)', word: 'down' };
}

function LiveTile({ base, camera }: { base: string; camera: Camera }) {
  const videoRef = useRef<HTMLVideoElement>(null);
  const [state, setState] = useState<LiveState>({ kind: 'connecting' });
  // The recorder's line comes from the feed, keyed by camera id — the id, not the
  // slug, because a slug can be reused and an id cannot.
  const recorder = useRealtime((s) => s.recorders[camera.id]);
  const serverStatus = useRealtime((s) => s.cameras[camera.id]);

  // The most recent event this camera has, straight from the feed. The store
  // hands out the event object itself, so the selector's identity is stable
  // while the camera's newest event does not change.
  const latest = useRealtime((s) => {
    let best: RealtimeEvent | undefined;
    for (const e of Object.values(s.events)) {
      if (e.camera_id === camera.id && (!best || String(e.started_at) > String(best.started_at))) {
        best = e;
      }
    }
    return best;
  });

  // The signature move. A tile that already had an event when it mounted does
  // not flip — the operator is arriving, not being alerted. From then on, each
  // new event id turns the tile once, and the tile turns back after the event
  // has been read.
  const [flipped, setFlipped] = useState(false);
  const seenEvent = useRef<string | undefined | null>(null);
  useEffect(() => {
    const id = latest?.id;
    if (seenEvent.current === null) {
      seenEvent.current = id;
      return;
    }
    if (id && id !== seenEvent.current) {
      seenEvent.current = id;
      setFlipped(true);
      const t = setTimeout(() => setFlipped(false), 2600);
      return () => clearTimeout(t);
    }
  }, [latest]);

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
  const field = headerField(state.kind, serverStatus?.online);
  // The name is struck through exactly when the band says the camera is
  // down — one truth, stated twice, never two conditions that can disagree.
  const down = field.word === 'down';

  return (
    <section className="flip-scene tile">
      <div className={`flip-inner${flipped ? ' flipped' : ''}`}>
        {/* Front: the camera, live. */}
        <div className="flip-face">
          <header
            className="flex items-baseline justify-between gap-3 px-3 py-2"
            style={{ background: field.bg, color: field.ink }}
          >
            <h2
              className={`text-base font-semibold tracking-cap${
                down ? ' line-through' : ''
              }`}
            >
              {camera.name}
            </h2>
            <span className="font-mono text-xs">{field.word}</span>
          </header>

          {/* The picture bleeds to the tile's edges — a Metro field has no
              mat around it. The element is always there — attaching a track
              to a fresh one would drop the picture — but it is hidden until
              a track exists. A hidden video is not a black box; the sentence
              below is what is on screen. */}
          <video
            ref={videoRef}
            hidden={!playing}
            autoPlay
            playsInline
            muted
            className="block aspect-video w-full bg-black"
          />

          <div className="px-3 pb-3">
            {!playing && (
              <p className="mt-2 text-sm">
                <span
                  className={
                    state.kind === 'connecting'
                      ? 'text-text-secondary'
                      : 'text-status-error'
                  }
                  title={reasonOf(state) || undefined}
                >
                  {TILE_STATE[state.kind]}
                </span>
              </p>
            )}

            <p className="mt-2 font-mono text-xs text-text-muted">{camera.slug}</p>

            <RecorderLine status={recorder} />
          </div>
        </div>

        {/* Back: the event that just landed. Tapping it opens the drawer. */}
        {latest ? (
          <Link
            to={`/events/${latest.id}`}
            className="flip-face flip-back block px-3 py-2"
            style={(() => {
              const f = eventField(Number(latest.severity ?? 1));
              return { background: f.bg, color: f.ink };
            })()}
          >
            <div className="flex items-baseline justify-between gap-3">
              <span className="text-sm font-semibold tracking-cap">{camera.name}</span>
              <span className="font-mono text-xs">{segmentClock(String(latest.started_at))}</span>
            </div>
            <div className="mt-1 text-5xl font-light leading-none">{Number(latest.severity ?? 1)}</div>
            <p className="mt-2 text-base font-semibold tracking-cap">
              {Array.isArray(latest.rules) && latest.rules.length
                ? String(latest.rules[0])
                : String(latest.action ?? 'event')}
              {latest.open ? ' · open' : ''}
            </p>
            <p className="mt-1 font-mono text-xs">tap to review</p>
          </Link>
        ) : null}
      </div>
    </section>
  );
}

export default function LivePage() {
  const [page, setPage] = useState<PageState>({ kind: 'reading' });

  const load = useCallback(async () => {
    // All at once: the wall should be up in one round trip, and none of the
    // answers depends on another. The today-count rides along and costs the
    // page nothing if it fails — a missing stat shows as an em dash, never
    // as a broken wall.
    const startOfToday = new Date();
    startOfToday.setHours(0, 0, 0, 0);
    const both = await Promise.all([
      client.get<{ go2rtc_public_url: string }>('/v1/settings/live'),
      client.get<{ cameras: Camera[] }>('/v1/cameras'),
      client
        .get<{ events: EventView[]; count: number }>('/v1/events', {
          params: { from: startOfToday.toISOString(), limit: 8 },
        })
        .catch(() => null),
    ]).then(
      ([media, cameras, today]) => ({ media, cameras, today, error: null as unknown }),
      (error: unknown) => ({ media: null, cameras: null, today: null, error }),
    );

    if (!both.media) {
      // The sentence names the problem and the first move; the server's own
      // words ride as the tooltip — the diagnosis is present for the operator
      // who wants it, not printed where the sentence belongs.
      setPage({
        kind: 'failed',
        reason: 'the record could not be read — is the server running?',
        raw: serverWords(both.error, 'no word from the server'),
      });
      return;
    }

    setPage({
      kind: 'ready',
      base: both.media.data.go2rtc_public_url,
      cameras: both.cameras?.data.cameras ?? [],
      today: both.today
        ? { events: both.today.data.events, count: both.today.data.count }
        : null,
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
        <h1 className="text-3xl font-light text-text-primary">Live</h1>
        <p className="mt-4 text-sm text-text-secondary">Reading the camera list.</p>
      </main>
    );
  }

  if (page.kind !== 'ready') {
    return (
      <main className="p-6">
        <h1 className="text-3xl font-light text-text-primary">Live</h1>
        <p className="mt-4 text-sm text-status-error" title={page.raw}>{page.reason}</p>
      </main>
    );
  }

  if (page.cameras.length === 0) {
    return (
      <main className="p-6">
        <h1 className="text-3xl font-light text-text-primary">Live</h1>
        <p className="mt-4 text-sm text-text-secondary">
          No cameras yet. Add one on the cameras page and it will appear here.
        </p>
      </main>
    );
  }

  return (
    <main className="p-6">
      <h1 className="text-3xl font-light text-text-primary">Live</h1>
      <p className="mt-1 text-sm text-text-secondary">
        {page.cameras.length} camera{page.cameras.length === 1 ? '' : 's'}, streaming from {page.base}
      </p>

      {/* The mosaic leads the first viewport — the card's viewport promise,
          verbatim. Counts and the day's record sit below it as the caption
          row; nothing above the wall but its name. The wall is two tiles
          wide from the phone up (Metro's own gesture: a wall is a grid, not
          a column) and four across on a wide desk, so eight feeds read as
          two rows — the shape PRODUCT.md promises at 8 feeds. */}
      <div className="mt-4 grid grid-cols-2 gap-1 md:grid-cols-2 xl:grid-cols-3 2xl:grid-cols-4">
        {page.cameras.map((camera) => (
          <LiveTile key={camera.id} base={page.base} camera={camera} />
        ))}
      </div>

      <p className="mt-4 font-mono text-sm text-text-secondary">
        <span className="text-text-primary">{page.today?.count ?? '—'}</span>{' '}
        {page.today?.count === 1 ? 'event' : 'events'} today
        {' · '}
        <span className="text-text-primary">{page.cameras.length}</span>{' '}
        camera{page.cameras.length === 1 ? '' : 's'}
      </p>

      {/* The day's strip: the record under the wall. Hairline-topped rows in
          the mosaic's own measure — time, camera, rule, severity — each a
          door to the drawer. */}
      {page.today && page.today.events.length > 0 ? (
        <div className="mt-2 border-t border-rule-strong">
          {page.today.events.map((event) => (
            <Link
              key={event.id}
              to={`/events/${event.id}`}
              className="grid grid-cols-[90px_140px_1fr_40px] items-baseline gap-3 border-b border-rule px-1 py-2 text-sm hover:bg-bg-secondary"
            >
              <span className="font-mono text-xs text-text-secondary">{segmentClock(event.started_at)}</span>
              <span className="truncate font-mono text-xs text-text-secondary">{event.camera_slug}</span>
              <span className="truncate text-text-primary">
                {event.rules.length ? event.rules.join(' · ') : event.action}
                {event.open ? <span className="ml-2 font-semibold text-accent">open</span> : null}
              </span>
              <span className="text-right font-mono text-text-primary">{event.severity}</span>
            </Link>
          ))}
        </div>
      ) : null}
    </main>
  );
}

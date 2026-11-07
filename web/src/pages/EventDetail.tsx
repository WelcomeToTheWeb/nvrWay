/**
 * The drawer: one event, and everything that proves it.
 *
 * The plan's sentence for this file is "the event card must be the frame it came
 * from, the rule that fired, the boxes it counted, and footage on both sides".
 * Four things follow, and each is a place where the easy version would lie:
 *
 * **The clip is fetched, not pointed at.** `server/api/handlers.go`: the bearer
 * header is the only REST credential, so a `<video src="/api/v1/…/clip">` is
 * answered 401 by its own server. `api/media.ts` fetches the bytes with the
 * bearer attached, which also means the 404's body is readable — so a missing
 * clip renders the server's sentence, `clip not available`, and never the
 * browser's broken-player icon.
 *
 * **The row's flags are claims; the routes are the truth.** `has_clip: true`
 * means the row believes a clip exists. The sweeper may have taken the file, a
 * volume may have come back without it. So the flag decides whether to *ask*, and
 * the answer decides what is on screen.
 *
 * **A recorded box is a pixel, and pixels need a frame.** The worker sends
 * `[x1, y1, x2, y2]` in the detection frame's pixels and the server stores them
 * verbatim; the event view carries no frame size. The camera row's
 * `detection_width`/`detection_height` name it — unless they are 0, which is the
 * default, in which case the installation fell back to a server-side default the
 * console is not told. Then the drawer shows the count and says it will not guess
 * where the boxes land. A zone polygon, by contrast, is stored normalised `0..1`,
 * so it always draws.
 *
 * **The strip is the event's own window, and says so.** The clip is cut from the
 * window *plus* the pre/post roll, and the console is not told those seconds, so
 * the strip draws the window it asked for — the server's echoed `from`/`to`, not
 * this browser's clock — and states the clip's measured length beside it.
 */
import { useEffect, useMemo, useState } from 'react';

import client, { serverWords } from '../api/client';
import {
  eventWindow,
  formatDuration,
  formatMoment,
  mergeLive,
  type CameraRefs,
  type EventView,
  type SegmentView,
} from '../api/events';
import { useMediaUrl } from '../api/media';
import { useRealtime } from '../stores/realtime';

interface Manifest {
  segments: SegmentView[];
  count: number;
  from: string;
  to: string;
  truncated: boolean;
}

type State =
  | { kind: 'reading' }
  | { kind: 'failed'; reason: string }
  | { kind: 'ready'; event: EventView; camera: CameraRefs | null };

/** The clip's four states, in the words that produced them. */
function Clip({ event }: { event: EventView }) {
  // A row that says there is no clip is not a row that failed to have one: a
  // camera in `record_mode=off` produces exactly this, and so does a window that
  // held no segments. Nothing is asked for, and the sentence is the one the route
  // would have answered — one sentence for both ways of having no clip, which is
  // how the server draws the distinction too.
  const media = useMediaUrl(event.has_clip ? `/v1/events/${event.id}/clip` : null);

  if (media.kind === 'ready') {
    return (
      <video controls className="aspect-video w-full bg-black" src={media.url}>
        <track kind="captions" />
      </video>
    );
  }

  const words =
    media.kind === 'none'
      ? 'clip not available'
      : media.kind === 'reading'
        ? 'reading the clip'
        : media.reason;

  return (
    <div className="flex aspect-video w-full items-center justify-center bg-bg-secondary px-4 text-center text-sm text-text-secondary">
      {words}
    </div>
  );
}

/**
 * The recorded boxes and the zone polygons, over the frame they belong to.
 *
 * One SVG, stretched to the picture (`preserveAspectRatio="none"`), on a
 * `0..1000` canvas: the polygons arrive normalised so they always land, and the
 * boxes are divided by the frame the event itself names — `det_width`/
 * `det_height` (P-10), the detection tap's size stored with the event, so the
 * picture does not depend on the camera row's knobs or on the installation's
 * current config. When the event names no frame (a row written before the
 * frame was recorded), the boxes are not drawn at all — a box placed by a
 * guess is worse than no box, because it looks like evidence.
 */
function Overlay({ event, camera }: { event: EventView; camera: CameraRefs | null }) {
  const frame =
    event.det_width > 0 && event.det_height > 0
      ? { w: event.det_width, h: event.det_height }
      : null;

  const boxes = frame ? event.objects.filter((o) => o.box.length === 4) : [];
  const polygons = (camera?.zones ?? []).filter((zone) => zone.polygon?.length >= 3);

  return (
    <div className="relative">
      <Snapshot event={event} />
      {polygons.length > 0 || boxes.length > 0 ? (
        <svg
          className="pointer-events-none absolute inset-0 h-full w-full"
          viewBox="0 0 1000 1000"
          preserveAspectRatio="none"
          aria-hidden="true"
        >
          {polygons.map((zone) => (
            <polygon
              key={zone.id}
              points={zone.polygon.map(([x, y]) => `${x * 1000},${y * 1000}`).join(' ')}
              style={{ fill: 'var(--accent)', fillOpacity: 0.2, stroke: 'var(--accent)' }}
              strokeWidth={2}
            />
          ))}
          {frame
            ? boxes.map((box, i) => (
                <rect
              key={`${box.class}-${i}`}
                x={(box.box[0] / frame.w) * 1000}
                y={(box.box[1] / frame.h) * 1000}
                width={((box.box[2] - box.box[0]) / frame.w) * 1000}
                height={((box.box[3] - box.box[1]) / frame.h) * 1000}
              style={{ stroke: 'var(--severity-3)' }}
                strokeWidth={2}
                fill="none"
              />
            ))
            : null}
        </svg>
      ) : null}

      <p className="mt-2 font-mono text-xs text-text-muted">
        {event.object_count} boxes recorded
        {frame
          ? ` · drawn on a ${frame.w}×${frame.h} frame`
          : ' · not drawn: this event was written before the frame its pixels belong to was recorded, and the drawer will not guess it'}
      </p>
    </div>
  );
}

function Snapshot({ event }: { event: EventView }) {
  const media = useMediaUrl(event.has_snapshot ? `/v1/events/${event.id}/snapshot` : null);

  if (media.kind === 'ready') {
    return (
      <img
        src={media.url}
        alt={`the frame this event was detected on, at ${formatMoment(event.started_at)}`}
        className="w-full"
      />
    );
  }

  const words =
    media.kind === 'none'
      ? 'no snapshot yet'
      : media.kind === 'reading'
        ? 'reading the frame'
        : media.reason;

  return (
    <div className="flex aspect-video w-full items-center justify-center bg-bg-secondary text-sm text-text-muted">
      {words}
    </div>
  );
}

/** The footage the event sits in, drawn on the window the server answered. */
function SegmentStrip({ segments, from, to, truncated }: Manifest) {
  const start = new Date(from).getTime();
  const end = new Date(to).getTime();
  const span = end - start;

  if (segments.length === 0) {
    return (
      <p className="font-mono text-xs text-text-muted">
        no segments in this window — there is no footage to cut a clip from
      </p>
    );
  }

  return (
    <div>
      <div className="relative h-6 w-full border border-rule bg-bg-secondary">
        {segments.map((segment) => {
          const left = span > 0 ? ((new Date(segment.start_ts).getTime() - start) / span) * 100 : 0;
          const width =
            span > 0 ? ((new Date(segment.end_ts).getTime() - new Date(segment.start_ts).getTime()) / span) * 100 : 100;
          return (
            <span
              key={segment.id}
              title={`${segment.slug} ${formatMoment(segment.start_ts)} · ${formatDuration(segment.seconds)}${
                segment.uploaded ? ' · on the storage tier' : ' · local only'
              }`}
              className={`absolute inset-y-0 border-r border-bg-card opacity-60 ${
                segment.uploaded ? 'bg-status-online' : 'bg-accent'
              }`}
              style={{ left: `${Math.max(0, left)}%`, width: `${Math.max(0.5, Math.min(width, 100))}%` }}
            />
          );
        })}
      </div>
      <p className="mt-1 font-mono text-xs text-text-muted">
        {segments.length} segments between {formatMoment(from)} and {formatMoment(to)}
        {truncated ? ' · the window held more' : ''} — the clip may reach past this window, because
        the server pads it by the roll settings and this console is not told them
      </p>
    </div>
  );
}

export default function EventDetail({ id, onClose }: { id: string; onClose: () => void }) {
  const [state, setState] = useState<State>({ kind: 'reading' });
  const [manifest, setManifest] = useState<Manifest | null>(null);
  const live = useRealtime((state) => state.events[id]);

  useEffect(() => {
    let alive = true;
    setState({ kind: 'reading' });
    setManifest(null);

    void (async () => {
      try {
        const { data } = await client.get<EventView>(`/v1/events/${id}`);
        if (!alive) return;

        // The camera row carries the zone polygons. Its failure is not the
        // drawer's failure: the event stands, and the boxes place themselves —
        // since P-10 the frame they need travels on the event, not the row.
        let camera: CameraRefs | null = null;
        try {
          const cam = await client.get<CameraRefs>(`/v1/cameras/${data.camera_id}`);
          camera = cam.data;
        } catch {
          camera = null;
        }

        if (alive) setState({ kind: 'ready', event: data, camera });

        const window = eventWindow(data);
        const params = new URLSearchParams({ camera_id: data.camera_id, from: window.from });
        if (window.to) params.set('to', window.to);
        try {
          const segments = await client.get<Manifest>(`/v1/segments?${params.toString()}`);
          if (alive) setManifest(segments.data);
        } catch {
          // A manifest that cannot be read leaves the strip unsaid, not wrong.
        }
      } catch (error) {
        if (alive) setState({ kind: 'failed', reason: serverWords(error, 'the event could not be read') });
      }
    })();

    return () => {
      alive = false;
    };
  }, [id]);

  // Esc closes a drawer. An operator with both hands on the keyboard should not
  // have to find the mouse to get back to the wall.
  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.key === 'Escape') onClose();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [onClose]);

  const event = useMemo(
    () => (state.kind === 'ready' ? mergeLive(state.event, live) : null),
    [state, live],
  );

  return (
    <div className="fixed inset-0 z-50 flex justify-end bg-black/60" role="dialog" aria-modal="true">
      <button type="button" aria-label="close the detail" className="flex-1" onClick={onClose} />
      <aside className="h-full w-full max-w-2xl overflow-y-auto border-l border-rule bg-bg-card p-5">
        {state.kind === 'reading' ? <p className="text-sm text-text-secondary">Reading the event.</p> : null}
        {state.kind === 'failed' ? <p className="text-sm text-status-error">{state.reason}</p> : null}

        {event && state.kind === 'ready' ? (
          <>
            <header className="flex items-baseline justify-between gap-3">
              <h2 className="font-cap text-xl uppercase tracking-cap text-text-primary">
                {event.camera_slug}
              </h2>
              <button
                type="button"
                onClick={onClose}
                className="border border-rule px-2 font-cap text-xs uppercase tracking-cap text-text-secondary hover:border-accent"
              >
                close
              </button>
            </header>

            <p className="mt-1 font-mono text-xs text-text-muted">
              {formatMoment(event.started_at)} · {formatDuration(event.seconds)} · severity{' '}
              {event.severity} · {event.signal_count} signals
              {event.open ? <span className="text-accent"> · LIVE</span> : null}
            </p>

            <div className="mt-3 flex flex-wrap gap-2">
              {event.rules.map((rule) => (
                <span key={rule} className="border border-rule px-1.5 font-mono text-xs text-text-secondary">
                  {rule}
                </span>
              ))}
              {event.zones.map((zone, i) => (
                <span
                  key={event.zone_ids[i] ?? zone}
                  className="border border-rule px-1.5 font-mono text-xs text-text-secondary"
                >
                  {zone}
                </span>
              ))}
              {event.classes.map((cls) => (
                <span key={cls} className="border border-rule px-1.5 font-mono text-xs text-text-secondary">
                  {cls}
                </span>
              ))}
            </div>

            <div className="mt-4">
              <Overlay event={event} camera={state.camera} />
            </div>

            <div className="mt-4">
              <h3 className="font-cap text-sm uppercase tracking-cap text-text-secondary">Clip</h3>
              <div className="mt-2">
                <Clip event={event} />
              </div>
              {event.has_clip && event.clip_seconds > 0 ? (
                <p className="mt-1 font-mono text-xs text-text-muted">
                  {formatDuration(event.clip_seconds)} of footage
                </p>
              ) : null}
            </div>

            <div className="mt-4">
              <h3 className="font-cap text-sm uppercase tracking-cap text-text-secondary">Footage</h3>
              <div className="mt-2">
                {manifest ? <SegmentStrip {...manifest} /> : <p className="text-sm text-text-secondary">Reading the manifest.</p>}
              </div>
            </div>
          </>
        ) : null}
      </aside>
    </div>
  );
}

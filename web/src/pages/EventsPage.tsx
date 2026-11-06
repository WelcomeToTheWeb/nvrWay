/**
 * The wall of events: what happened, on which camera, and the frame it came with.
 *
 * Three promises shape this file.
 *
 * **The address bar is the filter state.** The page keeps no filters of its own;
 * it reads the query string, asks `ListEvents` for exactly that, and writes the
 * query string when the operator narrows or widens. So a filter survives a
 * reload, a back button, and a paste into a colleague's chat — and a screenshot
 * of this page can be turned back into the query that produced it.
 *
 * **A card is the proof, not a row.** The mission's test is that an event card is
 * the frame it came from, the rule that fired, and the boxes it counted. So the
 * thumbnail is the first thing on the card, and when there is no thumbnail the
 * card says which kind of absence this is rather than showing a broken image.
 *
 * **The feed keeps the cards honest, and does not decide what is on the page.**
 * An open event shows `LIVE` and its numbers move from `event.update` alone. What
 * arrives after this page was read is not silently prepended: the page shows what
 * the server answered for these filters, and says when the answer was cut short.
 */
import { useCallback, useEffect, useMemo, useState } from 'react';

import { useMatch, useNavigate, useSearchParams } from 'react-router-dom';

import client, { serverWords } from '../api/client';
import {
  activeFilters,
  eventWindow,
  filtersFromParams,
  filtersToParams,
  formatDuration,
  formatMoment,
  mergeLive,
  type EventListResponse,
  type EventView,
  type FilterKey,
} from '../api/events';
import { useMediaUrl } from '../api/media';
import { useRealtime } from '../stores/realtime';
import EventDetail from './EventDetail';

interface CameraOption {
  id: string;
  slug: string;
  name: string;
}

type PageState =
  | { kind: 'reading' }
  | { kind: 'failed'; reason: string }
  | { kind: 'ready'; events: EventView[]; truncated: boolean; count: number };

/** The severity ring, spelled out in full.
 *
 * Written as literal class names rather than interpolated, because Tailwind's
 * scanner reads the source text: a `border-severity-${n}` it cannot see at build
 * time is a class it does not generate, and the ring arrives uncoloured — quietly,
 * on the one element whose whole job is to be seen. The values are the theme's
 * (`--severity-1…3`), and the bounds are the server's (`server/models/event.go`).
 */
const SEVERITY: Record<number, string> = {
  1: 'border-severity-1 text-severity-1',
  2: 'border-severity-2 text-severity-2',
  3: 'border-severity-3 text-severity-3',
};

function severityClass(severity: number): string {
  return SEVERITY[severity] ?? SEVERITY[1];
}

function Thumbnail({ event }: { event: EventView }) {
  const media = useMediaUrl(event.has_snapshot ? `/v1/events/${event.id}/snapshot` : null);

  if (media.kind === 'ready') {
    return (
      <img
        src={media.url}
        alt={`the frame ${event.camera_slug} caught`}
        className="aspect-video w-full object-cover"
      />
    );
  }

  // Every other state is a sentence in the same box, so a wall of events with no
  // pictures is still a wall an operator can read — and the three absences say
  // different things.
  const words =
    media.kind === 'none'
      ? 'no snapshot yet'
      : media.kind === 'reading'
        ? 'reading the frame'
        : media.kind === 'absent'
          ? media.reason
          : media.reason;

  return (
    <div
      className="flex aspect-video w-full items-center justify-center bg-bg-secondary px-3 text-center text-xs text-text-muted"
      data-testid="thumbnail-absent"
    >
      {words}
    </div>
  );
}

function EventCard({
  event,
  onOpen,
  onFilter,
}: {
  event: EventView;
  onOpen: () => void;
  onFilter: (key: FilterKey, value: string) => void;
}) {
  const window = eventWindow(event);

  return (
    <article className="border border-rule bg-bg-card">
      <button
        type="button"
        onClick={onOpen}
        className="block w-full text-left"
        aria-label={`open the detail for the event on ${event.camera_slug}`}
      >
        <Thumbnail event={event} />
        <div className="flex items-center justify-between gap-2 border-t border-rule px-3 py-2">
          <span className="font-mono text-xs text-text-secondary">{event.camera_slug}</span>
          <span className="font-mono text-xs text-text-muted">{formatMoment(window.from)}</span>
        </div>
      </button>

      <div className="flex flex-wrap items-center gap-2 px-3 pb-3">
        <span
          className={`flex h-6 w-6 items-center justify-center rounded-full border-2 font-mono text-xs ${severityClass(
            event.severity,
          )}`}
          title={`severity ${event.severity}`}
        >
          {event.severity}
        </span>

        {event.open ? (
          <span className="font-cap text-xs uppercase tracking-cap text-accent">LIVE</span>
        ) : null}

        <span className="font-mono text-xs text-text-secondary">
          {formatDuration(event.seconds)}
        </span>
        <span className="font-mono text-xs text-text-muted">{event.object_count} boxes</span>

        {event.classes.map((cls) => (
          <button
            key={cls}
            type="button"
            onClick={() => onFilter('class', cls)}
            className="border border-rule px-1.5 font-mono text-xs text-text-secondary hover:border-accent"
          >
            {cls}
          </button>
        ))}

        {event.zones.map((zone, i) => (
          <button
            key={event.zone_ids[i] ?? zone}
            type="button"
            onClick={() => onFilter('zone_id', event.zone_ids[i] ?? zone)}
            className="border border-rule px-1.5 font-mono text-xs text-text-secondary hover:border-accent"
          >
            {zone}
          </button>
        ))}

        {!event.has_clip ? (
          <span className="font-mono text-xs text-text-muted">no clip</span>
        ) : null}
      </div>
    </article>
  );
}

export default function EventsPage() {
  const [params, setParams] = useSearchParams();
  const navigate = useNavigate();
  // The serialised query is the dependency, not the object: react-router hands
  // back a fresh URLSearchParams on some renders, and an effect keyed on identity
  // would refetch the same page forever.
  const search = params.toString();
  const filters = useMemo(() => filtersFromParams(new URLSearchParams(search)), [search]);

  const detail = useMatch('/events/:id');
  const detailId = detail?.params.id ?? null;

  const [page, setPage] = useState<PageState>({ kind: 'reading' });
  const [cameras, setCameras] = useState<CameraOption[]>([]);
  const [searchDraft, setSearchDraft] = useState(filters.q ?? '');

  const liveEvents = useRealtime((state) => state.events);

  useEffect(() => {
    setSearchDraft(filters.q ?? '');
  }, [filters.q]);

  useEffect(() => {
    let live = true;
    setPage({ kind: 'reading' });
    const query = filtersToParams(filters).toString();
    void client
      .get<EventListResponse>(`/v1/events${query ? `?${query}` : ''}`)
      .then(
        (response) => {
          if (live) setPage({ kind: 'ready', ...response.data });
        },
        (error: unknown) => {
          if (live) setPage({ kind: 'failed', reason: serverWords(error, 'the events could not be read') });
        },
      );
    return () => {
      live = false;
    };
  }, [filters]);

  // The camera list is what turns `camera_id=7f3a…` into a name the operator
  // chose, both in the picker and on the chip.
  useEffect(() => {
    let live = true;
    void client
      .get<{ cameras: CameraOption[] }>('/v1/cameras')
      .then((response) => {
        if (live) setCameras(response.data.cameras ?? []);
      })
      .catch(() => {
        // A wall that cannot name its cameras still shows its events; the chip
        // falls back to the id, which is ugly and true.
      });
    return () => {
      live = false;
    };
  }, []);

  const write = useCallback(
    (change: Partial<Record<FilterKey, string | null>>) => {
      const next = { ...filters };
      for (const [key, value] of Object.entries(change) as [FilterKey, string | null][]) {
        if (value === null || value === '') delete next[key];
        else next[key] = value;
      }
      setParams(filtersToParams(next));
    },
    [filters, setParams],
  );

  const cameraNames = useMemo(
    () => Object.fromEntries(cameras.map((camera) => [camera.id, camera.slug])),
    [cameras],
  );
  const chips = activeFilters(filters, cameraNames);

  const rows =
    page.kind === 'ready' ? page.events.map((event) => mergeLive(event, liveEvents[event.id])) : [];
  const openCount = rows.filter((event) => event.open).length;

  return (
    <main className="p-6">
      <h1 className="font-cap text-2xl uppercase tracking-cap text-text-primary">Events</h1>

      <form
        className="mt-4 flex flex-wrap items-center gap-2"
        onSubmit={(event) => {
          event.preventDefault();
          write({ q: searchDraft });
        }}
      >
        <input
          value={searchDraft}
          onChange={(event) => setSearchDraft(event.target.value)}
          placeholder="search classes, rules, zone names"
          aria-label="search events"
          className="border border-rule bg-bg-secondary px-2 py-1 font-mono text-sm text-text-primary"
        />
        <button
          type="submit"
          className="border border-accent px-2 py-1 font-cap text-xs uppercase tracking-cap text-accent"
        >
          search
        </button>

        <span className="ml-2 font-cap text-xs uppercase tracking-cap text-text-muted">
          severity
        </span>
        {[1, 2, 3].map((level) => (
          <button
            key={level}
            type="button"
            onClick={() => write({ severity: filters.severity === String(level) ? null : String(level) })}
            className={`border px-2 py-0.5 font-mono text-xs ${
              filters.severity === String(level)
                ? 'border-accent text-accent'
                : 'border-rule text-text-secondary'
            }`}
          >
            {level}
          </button>
        ))}

        <select
          value={filters.camera_id ?? ''}
          onChange={(event) => write({ camera_id: event.target.value || null })}
          aria-label="filter by camera"
          className="border border-rule bg-bg-secondary px-2 py-1 font-mono text-sm text-text-primary"
        >
          <option value="">every camera</option>
          {cameras.map((camera) => (
            <option key={camera.id} value={camera.id}>
              {camera.slug}
            </option>
          ))}
        </select>
      </form>

      {chips.length > 0 ? (
        <div className="mt-3 flex flex-wrap items-center gap-2">
          {chips.map((chip) => (
            <span
              key={chip.key}
              className="flex items-center gap-1 border border-accent px-1.5 font-mono text-xs text-accent"
            >
              {chip.label}: {chip.value}
              <button
                type="button"
                onClick={() => write({ [chip.key]: null })}
                aria-label={`clear the ${chip.label} filter`}
                className="px-1 text-text-secondary hover:text-accent"
              >
                ×
              </button>
            </span>
          ))}
          <button
            type="button"
            onClick={() => setParams(new URLSearchParams())}
            className="font-mono text-xs text-text-secondary underline"
          >
            clear all
          </button>
        </div>
      ) : null}

      {page.kind === 'reading' ? (
        <p className="mt-4 text-sm text-text-secondary">Reading the event rows.</p>
      ) : null}

      {page.kind === 'failed' ? <p className="mt-4 text-sm text-status-error">{page.reason}</p> : null}

      {page.kind === 'ready' && page.count === 0 ? (
        <p className="mt-4 text-sm text-text-secondary">
          Nothing matches these filters. Widen them, or the property really was quiet.
        </p>
      ) : null}

      {page.kind === 'ready' && page.count > 0 ? (
        <>
          <p className="mt-4 font-mono text-xs text-text-muted">
            {page.count} events
            {openCount > 0 ? `, ${openCount} open` : ''}
            {page.truncated ? ' · the page was cut short, narrow the window to see the rest' : ''}
          </p>
          <div className="mt-3 grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
            {rows.map((event) => (
              <EventCard
                key={event.id}
                event={event}
                onOpen={() => navigate(`/events/${event.id}${search ? `?${search}` : ''}`)}
                onFilter={(key, value) => write({ [key]: value })}
              />
            ))}
          </div>
        </>
      ) : null}

      {detailId ? (
        <EventDetail
          id={detailId}
          onClose={() => navigate(`/events${search ? `?${search}` : ''}`)}
        />
      ) : null}
    </main>
  );
}

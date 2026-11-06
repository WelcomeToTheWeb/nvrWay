/**
 * The event surface, as the server speaks it — and the one mapping between the
 * address bar and the query.
 *
 * Two things live here because both pages (the wall of events and its drawer)
 * need them and neither should invent its own copy.
 *
 * **The shapes are the server's.** `eventView` is `server/api/events.go`,
 * `segmentView` is `server/api/segments.go`, and the filter names are the query
 * parameters `ListEvents` reads. Nothing here renames a field on the way in: a
 * field the server adds arrives instead of being dropped, and a field it renames
 * fails a test rather than drawing a blank column.
 *
 * **The URL is the filter state, in the server's names.** `?camera_id=&from=&to=
 * &class=&zone_id=&severity=&q=` is what `ListEvents` accepts, so the page keeps
 * no filter state of its own: it reads the query string, asks for exactly that,
 * and writes the query string when the operator narrows or widens. A filter that
 * is in the address bar survives a reload, a paste to a colleague, and a back
 * button — which is the only reason a filter is worth having.
 *
 * Severity stays a number. `server/models/event.go` says so outright — "A number,
 * not a word, so a filter is a comparison and the web hook payload says the same
 * thing the console draws" — so the console draws the number and a colour, and
 * invents no adjective for it.
 */

import type { RealtimeEvent } from '../stores/realtime';

/** One recorded box on an event. `box` is `[x1, y1, x2, y2]` in **pixels of the
 * detection frame** — the worker's `as_object()` sends them untransformed
 * (`detector/nvr_detector/model.py`), and the server stores them verbatim. That
 * is why the drawer needs the frame's size before it can place one. */
export interface EventObject {
  class: string;
  conf: number;
  box: number[];
}

/** `eventView` in `server/api/events.go`. The list answers `objects: []` and
 * `object_count`; only the detail answers the boxes — fifty events at two hundred
 * boxes each is a payload no wall draws. */
export interface EventView {
  id: string;
  event_key: string;
  camera_id: string;
  camera_slug: string;
  action: string;
  open: boolean;
  started_at: string;
  ended_at: string;
  seconds: number;
  severity: number;
  signal_count: number;
  classes: string[];
  zones: string[];
  zone_ids: string[];
  rules: string[];
  objects: EventObject[];
  object_count: number;
  /** The row's claim, not the disk's. A flag can be true and its route still
   * answer a named 404 — the route is the truth. */
  has_snapshot: boolean;
  has_clip: boolean;
  clip_seconds: number;
  snapshot_uploaded: boolean;
  clip_uploaded: boolean;
  created_at: string;
}

export interface EventListResponse {
  events: EventView[];
  count: number;
  truncated: boolean;
  limit: number;
  offset: number;
}

/** `segmentView` in `server/api/segments.go`. There is no `path` on purpose: it
 * is a name on the server's disk, and playback is server-proxied. */
export interface SegmentView {
  id: string;
  camera_id: string;
  slug: string;
  generation: number;
  start_ts: string;
  end_ts: string;
  seconds: number;
  bytes: number;
  kind: string;
  uploaded: boolean;
}

/** The camera row's parts the drawer needs: the frame the recorded boxes are
 * measured in, and the zone polygons to draw over the same picture. */
export interface CameraRefs {
  id: string;
  slug: string;
  name: string;
  detection_width: number;
  detection_height: number;
  zones: ZoneRef[];
}

export interface ZoneRef {
  id: string;
  name: string;
  /** Vertices as `[x, y]` pairs, normalised `0..1` — the write path validates
   * that range, so unlike a recorded box a polygon needs no frame to place it. */
  polygon: number[][];
  rule: string;
  enabled: boolean;
}

/** The seven filters `ListEvents` reads, in its own parameter names. */
export const FILTER_KEYS = [
  'camera_id',
  'from',
  'to',
  'class',
  'zone_id',
  'severity',
  'q',
] as const;

export type FilterKey = (typeof FILTER_KEYS)[number];

export type EventFilters = Partial<Record<FilterKey, string>>;

/** The query string as filters. A parameter the server would have to guess at is
 * not a filter, so an empty value is dropped rather than sent as `?class=`. */
export function filtersFromParams(params: URLSearchParams): EventFilters {
  const out: EventFilters = {};
  for (const key of FILTER_KEYS) {
    const value = (params.get(key) ?? '').trim();
    if (value !== '') out[key] = value;
  }
  return out;
}

/** Filters as a query string, in a fixed order so two operators who set the same
 * filters get the same address bar. */
export function filtersToParams(filters: EventFilters): URLSearchParams {
  const out = new URLSearchParams();
  for (const key of FILTER_KEYS) {
    const value = (filters[key] ?? '').trim();
    if (value !== '') out.set(key, value);
  }
  return out;
}

/** What a filter is called on a chip. The server's parameter names are for the
 * machine; an operator reading `zone_id=porch` is reading a database column. */
const CHIP_LABELS: Record<FilterKey, string> = {
  camera_id: 'camera',
  from: 'from',
  to: 'to',
  class: 'class',
  zone_id: 'zone',
  severity: 'severity',
  q: 'search',
};

export interface ActiveFilter {
  key: FilterKey;
  label: string;
  /** The value as the operator should read it — a slug when the page knows the
   * camera, the id when it does not. An id is ugly and true; a guess is neither. */
  value: string;
}

export function activeFilters(
  filters: EventFilters,
  cameraNames: Record<string, string> = {},
): ActiveFilter[] {
  const out: ActiveFilter[] = [];
  for (const key of FILTER_KEYS) {
    const value = filters[key];
    if (!value) continue;
    out.push({
      key,
      label: CHIP_LABELS[key],
      value: key === 'camera_id' ? (cameraNames[value] ?? value) : value,
    });
  }
  return out;
}

/** The event's own window, as the server wrote it: `ended_at` is absent while an
 * event is open, and an open event has no end to draw to. */
export function eventWindow(event: { started_at: string; ended_at: string; open: boolean }): {
  from: string;
  to: string | null;
} {
  return { from: event.started_at, to: event.open ? null : event.ended_at || null };
}

/** A duration an operator reads, not a float with six decimals. */
export function formatDuration(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) return '—';
  const whole = Math.round(seconds);
  if (whole < 60) return `${whole}s`;
  const minutes = Math.floor(whole / 60);
  const rest = whole % 60;
  if (minutes < 60) return `${minutes}m ${rest}s`;
  return `${Math.floor(minutes / 60)}h ${minutes % 60}m`;
}

/** A moment of the operator's day. The wire value is UTC because the server owns
 * event time; the wall is read by a person who lives in a timezone. */
const MOMENT = new Intl.DateTimeFormat(undefined, {
  month: 'short',
  day: 'numeric',
  hour: '2-digit',
  minute: '2-digit',
});

export function formatMoment(rfc3339: string): string {
  const at = new Date(rfc3339);
  return Number.isNaN(at.getTime()) ? rfc3339 : MOMENT.format(at);
}

/**
 * The feed's newer answer about a row a page already has.
 *
 * `realtime.ts` keeps the event payload loose on purpose and says so — it holds
 * `id` and `open` and "passes the rest through untouched for the event UI to
 * type". This is that typing, in one place, so the wall and the drawer cannot
 * disagree about what a frame means. It is sound because S6-08 sends the whole
 * `eventView` on all three frames: a frame is a newer answer about the same row,
 * not a partial patch of one.
 */
export function mergeLive(event: EventView, live: RealtimeEvent | undefined): EventView {
  if (!live) return event;
  return { ...event, ...(live as Partial<EventView>) };
}

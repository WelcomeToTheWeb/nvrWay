/**
 * The live feed: one WebSocket, the server's seven message types, and a store
 * the console renders from instead of polling.
 *
 * The types are the server's (`server/ws/hub.go`), not a choice made here, and
 * the frame is its `Envelope` — `{type, payload, ts}` — where `ts` is the server
 * clock. That clock is kept because the console compares a segment's end with an
 * event's start, and a browser clock that drifts across timezones cannot be the
 * measure of both.
 *
 * The store is a factory rather than a module-level singleton so a test can hand
 * it a socket and a clock. `useRealtime` below is the one the console uses.
 */
import { create, type StoreApi, type UseBoundStore } from 'zustand';

import { readAccessToken } from '../auth/session';

/** The seven types `server/ws/hub.go` broadcasts. Exported so the console can
 * assert it handles all of them rather than the ones someone remembered. */
export const WS_TYPES = [
  'event.new',
  'event.update',
  'event.close',
  'camera.status',
  'recorder.status',
  'storage.queue',
  'presence',
] as const;

export type WsType = (typeof WS_TYPES)[number];

/** An event as the feed sends it: the server's `eventView`. The store keeps the
 * fields it acts on (`id`, `open`) and passes the rest through untouched for the
 * event UI to type, so a new server field arrives instead of being dropped. */
export interface RealtimeEvent {
  id: string;
  open: boolean;
  [key: string]: unknown;
}

/** `broadcastCameraStatus` in `server/api/cameras.go`. */
export interface CameraStatus {
  camera_id: string;
  slug: string;
  online: boolean;
  producers: string[];
}

/** `recorderStatus` in `server/media/segments.go` — one per closed segment. */
export interface RecorderStatus {
  camera_id: string;
  mode: string;
  segment_seconds: number;
  last_segment_at: string;
}

/** The sweeper's `Report` (`server/sweeper/sweeper.go`). Typed loosely on
 * purpose: the store keeps what the sweep said, and the storage page (S8-08)
 * decides which numbers to show. `force_deleted` is the one that matters —
 * a non-zero number means an upload outage outlasted the protection window. */
export interface StorageQueueReport {
  [key: string]: unknown;
  force_deleted?: number;
  problems?: string[];
}

export type ConnectionStatus =
  /** Nothing is trying, because there is no token or the console stopped. */
  | 'idle'
  /** The first attempt is in flight. */
  | 'connecting'
  /** The socket is open and the feed is current. */
  | 'open'
  /** The socket dropped; a retry is scheduled. */
  | 'stale'
  /** The socket has refused to open repeatedly — see `REFUSAL_AFTER`. */
  | 'refused';

export interface RealtimeState {
  status: ConnectionStatus;
  /** The banner's condition: what is on screen is not current. */
  stale: boolean;
  /** Why, in a sentence the operator can act on. Null when nothing is wrong. */
  reason: string | null;
  /** Consecutive attempts that did not reach an open socket. */
  attempts: number;
  /** The envelope's `ts` from the last frame — the server clock, not this one. */
  lastFrameAt: string | null;

  events: Record<string, RealtimeEvent>;
  cameras: Record<string, CameraStatus>;
  recorders: Record<string, RecorderStatus>;
  storageQueue: StorageQueueReport | null;
  presence: number | null;
}

export interface RealtimeActions {
  connect(): void;
  disconnect(): void;
}

/** The store, as a hook. `UseBoundStore` rather than a bare `StoreApi` because the
 * pages subscribe with `useRealtime((s) => s.recorders[id])` — that call signature
 * is the documented way in, and a `StoreApi` return type erases it, which only
 * became visible when the first page consumed the feed (LivePage, card S4-06).
 * `getState()` / `setState()` / `subscribe()` still work for a caller that is not
 * rendering, so the suites that hand the store a socket are unchanged. */
export type RealtimeStore = UseBoundStore<StoreApi<RealtimeState & RealtimeActions>>;

export interface RealtimeOptions {
  /** Read fresh every attempt: a refresh rotates the access token, and a
   * reconnect holding the old one is a 401 that looks like a dead server. */
  token(): string | null;
  WebSocketImpl?: typeof WebSocket;
  /** Injectable so the backoff schedule can be asserted instead of waited for. */
  schedule?(fn: () => void, ms: number): () => void;
  location?: { protocol: string; host: string };
}

const BACKOFF_FIRST_MS = 5_000;
const BACKOFF_CAP_MS = 60_000;

/** A rejected origin is answered `403 origin is not allowed` **before** the
 * upgrade, so the browser sees only a socket that will not open — it cannot
 * read the server's sentence. Three attempts without ever opening is enough to
 * stop calling that "stale": the likely cause is a configuration the operator
 * can fix, and a console that retries it quietly for hours is the failure mode
 * this card names. The retry does not stop — the state says what to check. */
const REFUSAL_AFTER = 3;

const REFUSAL_REASON =
  'the live feed has refused to open three times. If this console is served from a host the ' +
  'server does not know, add it to WS_ORIGINS; a rejected origin is refused before the ' +
  'connection upgrades, so the browser cannot be told why.';

const EMPTY: RealtimeState = {
  status: 'idle',
  stale: false,
  reason: null,
  attempts: 0,
  lastFrameAt: null,
  events: {},
  cameras: {},
  recorders: {},
  storageQueue: null,
  presence: null,
};

export function createRealtime(options: RealtimeOptions): RealtimeStore {
  const WebSocketImpl = options.WebSocketImpl ?? globalThis.WebSocket;
  const location = options.location ?? globalThis.location;
  const schedule =
    options.schedule ??
    ((fn: () => void, ms: number) => {
      const timer = setTimeout(fn, ms);
      return () => clearTimeout(timer);
    });

  let socket: WebSocket | null = null;
  let cancelRetry: (() => void) | null = null;
  let stopped = false;

  return create<RealtimeState & RealtimeActions>()((set, get) => {
    const stopRetry = () => {
      cancelRetry?.();
      cancelRetry = null;
    };

    const url = (): string | null => {
      const token = options.token();
      if (!token) return null;
      // A browser WebSocket cannot set a header, so `?token=` is the one place
      // nvrWay accepts the access token outside Authorization (server/ws/hub.go).
      const scheme = location.protocol === 'https:' ? 'wss:' : 'ws:';
      return `${scheme}//${location.host}/ws?token=${encodeURIComponent(token)}`;
    };

    const backoff = (attempts: number) =>
      Math.min(BACKOFF_FIRST_MS * 2 ** (attempts - 1), BACKOFF_CAP_MS);

    const scheduleRetry = (attempts: number) => {
      cancelRetry = schedule(() => open(), backoff(attempts));
    };

    const handleFrame = (raw: string) => {
      let envelope: { type?: string; payload?: unknown; ts?: string };
      try {
        envelope = JSON.parse(raw);
      } catch {
        return; // Not a frame. Nothing to say about it.
      }
      const { type, payload } = envelope;
      if (!type) return;
      const ts = envelope.ts ?? null;

      switch (type as WsType) {
        case 'event.new':
        case 'event.update': {
          const event = payload as RealtimeEvent;
          if (!event?.id) return;
          set((state) => ({
            lastFrameAt: ts ?? state.lastFrameAt,
            events: { ...state.events, [event.id]: { ...state.events[event.id], ...event } },
          }));
          return;
        }
        case 'event.close': {
          // Kept, and marked closed. The console is showing a card for this
          // event; deleting the row makes it vanish under the operator's cursor.
          const event = payload as RealtimeEvent;
          if (!event?.id) return;
          set((state) => ({
            lastFrameAt: ts ?? state.lastFrameAt,
            events: { ...state.events, [event.id]: { ...state.events[event.id], ...event, open: false } },
          }));
          return;
        }
        case 'camera.status': {
          const status = payload as CameraStatus;
          if (!status?.camera_id) return;
          set((state) => ({
            lastFrameAt: ts ?? state.lastFrameAt,
            cameras: { ...state.cameras, [status.camera_id]: status },
          }));
          return;
        }
        case 'recorder.status': {
          const status = payload as RecorderStatus;
          if (!status?.camera_id) return;
          set((state) => ({
            lastFrameAt: ts ?? state.lastFrameAt,
            recorders: { ...state.recorders, [status.camera_id]: status },
          }));
          return;
        }
        case 'storage.queue': {
          set({ lastFrameAt: ts, storageQueue: (payload ?? {}) as StorageQueueReport });
          return;
        }
        case 'presence': {
          const { clients } = (payload ?? {}) as { clients?: number };
          set({ lastFrameAt: ts, presence: typeof clients === 'number' ? clients : null });
          return;
        }
        default:
          // A type this build does not know is a server ahead of the console,
          // not a fault. The frame loop keeps going.
          return;
      }
    };

    const open = () => {
      if (stopped) return;
      stopRetry();
      const target = url();
      if (!target) {
        set({
          ...EMPTY,
          reason: 'there is no access token to open the live feed with',
        });
        return;
      }

      const { attempts } = get();
      set({ status: attempts >= REFUSAL_AFTER ? 'refused' : 'connecting' });

      let next: WebSocket;
      try {
        next = new WebSocketImpl(target);
      } catch (error) {
        fail((error as Error).message);
        return;
      }
      socket = next;

      next.onopen = () => {
        if (socket !== next) return;
        set({ status: 'open', stale: false, reason: null, attempts: 0 });
      };

      next.onmessage = (event: MessageEvent) => handleFrame(String(event.data));

      next.onerror = () => {
        // `close` always follows; one state change per failure.
      };

      next.onclose = () => {
        if (socket !== next) return;
        socket = null;
        fail();
      };
    };

    const fail = (detail?: string) => {
      if (stopped) return;
      const attempts = get().attempts + 1;
      const refused = attempts >= REFUSAL_AFTER;
      set({
        status: refused ? 'refused' : 'stale',
        stale: true,
        attempts,
        reason: refused
          ? REFUSAL_REASON
          : detail
            ? `the live feed could not open: ${detail}`
            : 'the live feed dropped; retrying',
      });
      scheduleRetry(attempts);
    };

    return {
      ...EMPTY,
      connect: () => {
        stopped = false;
        if (socket) return;
        open();
      },
      disconnect: () => {
        stopped = true;
        stopRetry();
        const current = socket;
        socket = null;
        try {
          current?.close();
        } catch {
          /* already gone */
        }
        set({ ...EMPTY });
      },
    };
  });
}

/** The store the console uses. One socket per browser tab; the pages subscribe
 * to slices of it. */
export const useRealtime = createRealtime({ token: () => readAccessToken() });

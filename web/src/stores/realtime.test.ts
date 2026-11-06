/**
 * The live feed's store: one socket, seven message types, and a state the
 * console can render without polling.
 *
 * The seven types are the server's, not a preference — `server/ws/hub.go`
 * defines `event.new`, `event.update`, `event.close`, `camera.status`,
 * `recorder.status`, `storage.queue` and `presence`, and each test below names
 * the one it covers. The payloads are the shapes the server actually sends
 * (`broadcastCameraStatus`, `recorderStatus`, the sweeper's `Report`,
 * `Hub.Presence`, the event view), so a field renamed on either side shows up
 * here rather than as a blank column in the console.
 */
import { beforeEach, describe, expect, it } from 'vitest';

import { createRealtime, WS_TYPES, type RealtimeStore } from './realtime';

class FakeSocket {
  static instances: FakeSocket[] = [];
  static readonly OPEN = 1;
  static readonly CLOSED = 3;

  readyState = FakeSocket.OPEN;
  sent: string[] = [];

  onopen: (() => void) | null = null;
  onmessage: ((event: { data: string }) => void) | null = null;
  onclose: (() => void) | null = null;
  onerror: (() => void) | null = null;

  constructor(public url: string) {
    FakeSocket.instances.push(this);
  }

  send(data: string): void {
    this.sent.push(data);
  }

  close(): void {
    if (this.readyState === FakeSocket.CLOSED) return;
    this.readyState = FakeSocket.CLOSED;
    this.onclose?.();
  }

  // Test-side triggers.
  opened(): this {
    this.onopen?.();
    return this;
  }

  /** The server's own envelope (`server/ws/hub.go` `Envelope`): the payload is
   * nested under `payload`, and `ts` is the server clock, because a browser
   * clock drifts and "when did the event start" has to be comparable with
   * "when did the segment land". */
  frame(type: string, payload: unknown, ts = '2026-11-05T10:00:00Z'): this {
    this.onmessage?.({ data: JSON.stringify({ type, payload, ts }) });
    return this;
  }

  raw(data: string): this {
    this.onmessage?.({ data });
    return this;
  }
}

interface Scheduled {
  at: number;
  run(): void;
}

function harness(options?: { token?: () => string | null }) {
  FakeSocket.instances = [];
  const scheduled: Scheduled[] = [];
  let token = 'token-1';

  const store: RealtimeStore = createRealtime({
    token: options?.token ?? (() => token),
    WebSocketImpl: FakeSocket as unknown as typeof WebSocket,
    // Timers are captured, not run: the backoff schedule is one of the things
    // under test, and a wall clock cannot assert a cap.
    schedule: (fn: () => void, ms: number) => {
      scheduled.push({ at: ms, run: fn });
      return () => {
        const index = scheduled.findIndex((s) => s.run === fn);
        if (index >= 0) scheduled.splice(index, 1);
      };
    },
    location: { protocol: 'https:', host: 'nvr.example' },
  });

  const socket = (n = 0) => FakeSocket.instances[n];
  const nextRetry = () => scheduled.shift();

  return { store, socket, scheduled, nextRetry, setToken: (t: string) => (token = t) };
}

const EVENT = {
  id: 'e1',
  event_key: 'front-door:1',
  camera_id: 'c1',
  camera_slug: 'front-door',
  action: 'person_in_zone',
  open: true,
  started_at: '2026-11-05T10:00:00Z',
  seconds: 12,
  severity: 3,
  classes: ['person'],
};

beforeEach(() => {
  FakeSocket.instances = [];
});

describe('the socket', () => {
  it('opens /ws with the access token as a query parameter', () => {
    const h = harness();
    h.store.getState().connect();
    // A browser WebSocket cannot set a header, so ?token= is the one place the
    // access token travels outside Authorization (server/ws/hub.go).
    expect(h.socket().url).toBe('wss://nvr.example/ws?token=token-1');
  });

  it('reads the token again for every attempt, because a refresh rotates it', () => {
    const h = harness();
    h.store.getState().connect();
    h.socket().close();
    h.nextRetry()?.run();
    h.setToken('token-2');
    h.socket(1).close();
    h.nextRetry()?.run();
    expect(h.socket(2).url).toContain('token=token-2');
  });

  it('does not open a socket when there is no token to send', () => {
    const h = harness({ token: () => null });
    h.store.getState().connect();
    expect(FakeSocket.instances).toHaveLength(0);
    expect(h.store.getState().status).toBe('idle');
    expect(h.store.getState().reason).toBeTruthy();
  });

  it('stops retrying when the console disconnects', () => {
    const h = harness();
    h.store.getState().connect();
    h.socket().close();
    h.store.getState().disconnect();
    expect(h.scheduled).toHaveLength(0);
    expect(h.store.getState().status).toBe('idle');
  });
});

describe('the seven message types the server sends', () => {
  it('covers exactly the types server/ws/hub.go defines', () => {
    expect([...WS_TYPES].sort()).toEqual(
      [
        'camera.status',
        'event.close',
        'event.new',
        'event.update',
        'presence',
        'recorder.status',
        'storage.queue',
      ].sort(),
    );
  });

  it('event.new adds the event', () => {
    const h = harness();
    h.store.getState().connect();
    h.socket().opened().frame('event.new', EVENT);
    expect(h.store.getState().events.e1).toMatchObject({ camera_slug: 'front-door', open: true });
  });

  it('event.update replaces it, including the fields that only change later', () => {
    const h = harness();
    h.store.getState().connect();
    h.socket().opened().frame('event.new', EVENT);
    h.socket().frame('event.update', { ...EVENT, seconds: 90, severity: 5, signal_count: 7 });
    expect(h.store.getState().events.e1).toMatchObject({ seconds: 90, severity: 5, signal_count: 7 });
  });

  it('event.close keeps the event and marks it closed', () => {
    // The console is showing a card for this event. A close that deletes the
    // row makes the card vanish under the operator's cursor.
    const h = harness();
    h.store.getState().connect();
    h.socket().opened().frame('event.new', EVENT);
    h.socket().frame('event.close', { ...EVENT, open: false, ended_at: '2026-11-05T10:01:00Z' });
    expect(h.store.getState().events.e1).toMatchObject({ open: false });
  });

  it('camera.status is keyed by camera, so one camera cannot overwrite another', () => {
    const h = harness();
    h.store.getState().connect();
    h.socket()
      .opened()
      .frame('camera.status', { camera_id: 'c1', slug: 'front-door', online: true, producers: ['rtsp://a'] })
      .frame('camera.status', { camera_id: 'c2', slug: 'porch', online: false, producers: [] });
    expect(h.store.getState().cameras.c1).toMatchObject({ online: true });
    expect(h.store.getState().cameras.c2).toMatchObject({ online: false, slug: 'porch' });
  });

  it('recorder.status carries the segment clock the status line reads', () => {
    const h = harness();
    h.store.getState().connect();
    h.socket()
      .opened()
      .frame('recorder.status', {
        camera_id: 'c1',
        mode: 'continuous',
        segment_seconds: 10,
        last_segment_at: '2026-11-05T10:00:10Z',
      });
    expect(h.store.getState().recorders.c1).toMatchObject({
      mode: 'continuous',
      segment_seconds: 10,
      last_segment_at: '2026-11-05T10:00:10Z',
    });
  });

  it('storage.queue keeps the sweep report, including the number to watch', () => {
    const h = harness();
    h.store.getState().connect();
    h.socket()
      .opened()
      .frame('storage.queue', { force_deleted: 3, local_deleted: 12, problems: ['minio: timeout'] });
    expect(h.store.getState().storageQueue).toMatchObject({
      force_deleted: 3,
      local_deleted: 12,
      problems: ['minio: timeout'],
    });
  });

  it('presence counts the clients the server is serving', () => {
    const h = harness();
    h.store.getState().connect();
    h.socket().opened().frame('presence', { clients: 4 });
    expect(h.store.getState().presence).toBe(4);
  });

  it('keeps the envelope\'s server clock, which is the clock the console compares against', () => {
    const h = harness();
    h.store.getState().connect();
    h.socket().opened().frame('presence', { clients: 1 }, '2026-11-05T10:00:00Z');
    h.socket().frame('presence', { clients: 2 }, '2026-11-05T10:00:30Z');
    expect(h.store.getState().lastFrameAt).toBe('2026-11-05T10:00:30Z');
  });

  it('ignores a type it does not know instead of failing the frame loop', () => {
    const h = harness();
    h.store.getState().connect();
    h.socket().opened().frame('event.new', EVENT).frame('something.new', { id: 'x' });
    expect(Object.keys(h.store.getState().events)).toEqual(['e1']);
  });

  it('survives a frame that is not JSON', () => {
    const h = harness();
    h.store.getState().connect();
    h.socket().opened().raw('not json at all');
    expect(h.store.getState().status).toBe('open');
  });
});

describe('stale, and the failure that must not be silent', () => {
  it('is stale the moment the socket drops, and live again when it reopens', () => {
    const h = harness();
    h.store.getState().connect();
    h.socket().opened();
    expect(h.store.getState()).toMatchObject({ stale: false, status: 'open' });

    h.socket().close();
    expect(h.store.getState()).toMatchObject({ stale: true });

    h.nextRetry()?.run();
    h.socket(1).opened();
    expect(h.store.getState()).toMatchObject({ stale: false, status: 'open' });
  });

  it('waits five seconds for the first retry and doubles to a sixty second cap', () => {
    const h = harness();
    h.store.getState().connect();
    const waits: number[] = [];
    for (let attempt = 0; attempt < 6; attempt += 1) {
      h.socket(attempt).close();
      const retry = h.nextRetry();
      if (!retry) break;
      waits.push(retry.at);
      retry.run();
    }
    expect(waits).toEqual([5000, 10000, 20000, 40000, 60000, 60000]);
  });

  it('names a socket that never opens as a refusal, and says what to check', () => {
    // A rejected origin is answered 403 before the upgrade, so the browser
    // cannot read the server's sentence — it only sees a socket that will not
    // open. Retrying that in silence is the failure mode the card names: the
    // console looks briefly stale forever instead of saying the configuration
    // is wrong.
    const h = harness();
    h.store.getState().connect();
    for (let attempt = 0; attempt < 3; attempt += 1) {
      h.socket(attempt).close();
      h.nextRetry()?.run();
    }
    const state = h.store.getState();
    expect(state.status).toBe('refused');
    expect(state.stale).toBe(true);
    expect(state.reason).toMatch(/WS_ORIGINS/);
  });

  it('goes back to live after a refusal that stops happening', () => {
    const h = harness();
    h.store.getState().connect();
    for (let attempt = 0; attempt < 3; attempt += 1) {
      h.socket(attempt).close();
      h.nextRetry()?.run();
    }
    expect(h.store.getState().status).toBe('refused');
    h.socket(3).opened();
    expect(h.store.getState()).toMatchObject({ status: 'open', stale: false });
    expect(h.store.getState().reason).toBeNull();
  });
});

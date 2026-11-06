/**
 * The live player's wire contract, against fakes for WebSocket and
 * RTCPeerConnection.
 *
 * What is asserted here is the message shape go2rtc's `/api/ws` expects and
 * sends — the plan takes it from go2rtc's own `internal/api/ws/README.md` — plus
 * the states the tile renders when the media never arrives. The failure states
 * are not invented: they were observed against a real `alexxit/go2rtc:latest`
 * on 2026-11-05 (see the card's Notes). The one thing this file cannot prove is
 * that a browser's real `RTCPeerConnection` produces an SDP go2rtc accepts; that
 * is the Playwright card's job (S8-12).
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';

import { connectLive, liveWsUrl, type LiveState } from './go2rtc';

/** Everything both fakes write to, in the order it happened. Module scope,
 * because the module under test constructs them and passes only a URL. */
type Log = string[];
const log: Log = [];

class FakeWebSocket {
  static readonly OPEN = 1;
  static readonly CLOSED = 3;
  static instances: FakeWebSocket[] = [];

  instances: FakeWebSocket[] = [];

  readyState = FakeWebSocket.OPEN;
  sent: unknown[] = [];

  onopen: (() => void) | null = null;
  onmessage: ((event: { data: string }) => void) | null = null;
  onclose: (() => void) | null = null;
  onerror: (() => void) | null = null;

  constructor(public url: string) {
    FakeWebSocket.instances.push(this);
    log.push(`ws:connect ${url}`);
    // A real socket opens on a later task, never from inside the constructor.
    queueMicrotask(() => this.onopen?.());
  }

  send(data: string): void {
    this.sent.push(JSON.parse(data));
  }

  close(): void {
    if (this.readyState === FakeWebSocket.CLOSED) return;
    this.readyState = FakeWebSocket.CLOSED;
    log.push('ws:close');
    this.onclose?.();
  }

  // Test-side triggers for what the server puts on the wire.
  answer(sdp: string): void {
    this.onmessage?.({ data: JSON.stringify({ type: 'webrtc/answer', value: sdp }) });
  }

  candidate(candidate: string): void {
    this.onmessage?.({ data: JSON.stringify({ type: 'webrtc/candidate', value: candidate }) });
  }

  error(value: string): void {
    this.onmessage?.({ data: JSON.stringify({ type: 'error', value }) });
  }

  dropped(): void {
    this.readyState = FakeWebSocket.CLOSED;
    this.onclose?.();
  }
}

class FakePeerConnection {
  static instances: FakePeerConnection[] = [];

  transceivers: { kind: string; direction?: string }[] = [];
  remoteDescriptions: { type?: string; sdp?: string }[] = [];
  iceCandidates: { candidate?: string }[] = [];
  localDescription: { sdp: string } | null = null;
  closed = false;

  ontrack: ((event: { streams: MediaStream[] }) => void) | null = null;
  onicecandidate: ((event: { candidate: { candidate: string } | null }) => void) | null = null;

  constructor() {
    FakePeerConnection.instances.push(this);
  }

  addTransceiver(kind: string, init?: { direction?: string }): void {
    this.transceivers.push({ kind, direction: init?.direction });
    log.push(`pc:addTransceiver ${kind}:${init?.direction}`);
  }

  async createOffer(): Promise<{ type: 'offer'; sdp: string }> {
    log.push('pc:createOffer');
    return { type: 'offer', sdp: 'OFFER-SDP' };
  }

  async setLocalDescription(desc: { sdp: string }): Promise<void> {
    log.push('pc:setLocalDescription');
    this.localDescription = { sdp: desc.sdp };
  }

  async setRemoteDescription(desc: { type?: string; sdp?: string }): Promise<void> {
    log.push('pc:setRemoteDescription');
    this.remoteDescriptions.push(desc);
  }

  async addIceCandidate(candidate: { candidate?: string }): Promise<void> {
    log.push('pc:addIceCandidate');
    this.iceCandidates.push(candidate);
  }

  close(): void {
    this.closed = true;
    log.push('pc:close');
  }
}

function harness() {
  log.length = 0;
  FakeWebSocket.instances = [];
  FakePeerConnection.instances = [];
  const states: LiveState[] = [];

  const video = { srcObject: null as unknown } as { srcObject: unknown };

  const handle = connectLive({
    publicBaseUrl: 'http://127.0.0.1:1984',
    slug: 'front-door',
    video: video as unknown as HTMLVideoElement,
    WebSocketImpl: FakeWebSocket as unknown as typeof WebSocket,
    PeerConnectionImpl: FakePeerConnection as unknown as typeof RTCPeerConnection,
    onState: (state) => states.push(state),
  });

  const ws = () => FakeWebSocket.instances[0];
  // The module constructs its own peer connection, so the test reaches it the
  // same way it reaches the socket — through the registry, not a local.
  const pc = () => FakePeerConnection.instances[0];
  const lastState = (): LiveState => states[states.length - 1];

  return { log, states, ws, pc, video, handle, lastState };
}

describe('liveWsUrl', () => {
  it('is go2rtc\'s ws api with the slug as src', () => {
    expect(liveWsUrl('http://127.0.0.1:1984', 'front-door')).toBe(
      'ws://127.0.0.1:1984/api/ws?src=front-door',
    );
  });

  it('is wss when the media host is https', () => {
    expect(liveWsUrl('https://nvr.example', 'porch')).toBe('wss://nvr.example/api/ws?src=porch');
  });

  it('tolerates a trailing slash on the configured base', () => {
    expect(liveWsUrl('http://127.0.0.1:1984/', 'porch')).toBe(
      'ws://127.0.0.1:1984/api/ws?src=porch',
    );
  });

  it('refuses a slug that is not a stream name', () => {
    // The slug is the stream name and it goes into a query string. A slug with
    // a slash or a space would silently ask for a different stream.
    expect(() => liveWsUrl('http://h', 'front/door')).toThrow();
    expect(() => liveWsUrl('http://h', '')).toThrow();
  });
});

describe('connectLive', () => {
  let h: ReturnType<typeof harness>;

  beforeEach(() => {
    h = harness();
  });

  it('asks for video and audio, both recvonly', () => {
    expect(h.pc().transceivers).toEqual([
      { kind: 'video', direction: 'recvonly' },
      { kind: 'audio', direction: 'recvonly' },
    ]);
  });

  it('sends the offer as go2rtc\'s message shape, after setLocalDescription', async () => {
    await vi.waitFor(() => expect(h.ws().sent).toEqual([{ type: 'webrtc/offer', value: 'OFFER-SDP' }]));
    // The order is the protocol: an SDP that was never set locally is not an
    // offer, and go2rtc answers only the one it can find in pc.localDescription.
    expect(h.log).toEqual([
      'ws:connect ws://127.0.0.1:1984/api/ws?src=front-door',
      'pc:addTransceiver video:recvonly',
      'pc:addTransceiver audio:recvonly',
      'pc:createOffer',
      'pc:setLocalDescription',
    ]);
  });

  it('applies the answer as a remote description', async () => {
    h.ws().answer('ANSWER-SDP');
    await vi.waitFor(() =>
      expect(h.pc().remoteDescriptions).toEqual([{ type: 'answer', sdp: 'ANSWER-SDP' }]),
    );
  });

  it('adds a candidate from go2rtc only once the answer is in place', async () => {
    // addIceCandidate before the remote description throws in a real browser.
    h.ws().candidate('a=candidate:1 1 udp 2113937151 10.0.0.5 50000 typ host');
    expect(h.pc().iceCandidates).toEqual([]);

    h.ws().answer('ANSWER-SDP');
    await vi.waitFor(() =>
      expect(h.pc().iceCandidates).toEqual([
        { candidate: 'a=candidate:1 1 udp 2113937151 10.0.0.5 50000 typ host' },
      ]),
    );
  });

  it('sends a local candidate as webrtc/candidate', async () => {
    h.pc().onicecandidate?.({ candidate: { candidate: 'a=candidate:9 1 udp 1 1.2.3.4 443 typ srflx' } });
    expect(h.ws().sent).toContainEqual({
      type: 'webrtc/candidate',
      value: 'a=candidate:9 1 udp 1 1.2.3.4 443 typ srflx',
    });
  });

  it('sends nothing for the end-of-candidates marker', async () => {
    h.pc().onicecandidate?.({ candidate: null });
    expect(h.ws().sent.filter((m) => (m as { type: string }).type === 'webrtc/candidate')).toEqual([]);
  });

  it('puts the track on the video element and calls it playing', () => {
    const stream = { id: 'stream-1' } as unknown as MediaStream;
    h.pc().ontrack?.({ streams: [stream] });
    expect(h.video.srcObject).toBe(stream);
    expect(h.lastState().kind).toBe('playing');
  });
});

describe('the states a tile renders when no media arrives', () => {
  // Observed 2026-11-05 against `alexxit/go2rtc:latest`: a WS to a stream that
  // is not there answers `{"type":"error","value":"webrtc/offer: stream not
  // found"}` and keeps the socket open. Silence and a closed socket are
  // different failures and the tile says which one happened.
  it('names go2rtc\'s own words when it answers an error', async () => {
    const h = harness();
    h.ws().error('webrtc/offer: stream not found');
    expect(h.lastState()).toEqual({ kind: 'offline', reason: 'webrtc/offer: stream not found' });
    expect(h.video.srcObject).toBeNull();
  });

  it('names a socket that died before the answer as unreachable', () => {
    const h = harness();
    h.ws().dropped();
    // A reason is part of the state, not an extra: a tile that says only
    // "unreachable" leaves the operator guessing which host it means.
    expect(h.lastState()).toEqual({ kind: 'unreachable', reason: expect.any(String) });
  });

  it('does not call a dropped socket offline after media was flowing', () => {
    const h = harness();
    h.pc().ontrack?.({ streams: [{} as MediaStream] });
    h.ws().dropped();
    // A stream that ended is not the same sentence as one that never started.
    expect(h.lastState().kind).toBe('ended');
  });

  it('says so when the browser has no RTCPeerConnection, instead of throwing', () => {
    const states: LiveState[] = [];
    const handle = connectLive({
      publicBaseUrl: 'http://h',
      slug: 'porch',
      video: {} as HTMLVideoElement,
      WebSocketImpl: FakeWebSocket as unknown as typeof WebSocket,
      PeerConnectionImpl: undefined as unknown as typeof RTCPeerConnection,
      onState: (state) => states.push(state),
    });
    expect(states[states.length - 1].kind).toBe('unsupported');
    expect(() => handle.close()).not.toThrow();
  });

  it('tears the socket and the peer connection down on close, and survives a second call', async () => {
    const h = harness();
    h.handle.close();
    expect(h.ws().readyState).toBe(FakeWebSocket.CLOSED);
    expect(h.pc().closed).toBe(true);
    expect(() => h.handle.close()).not.toThrow();
  });
});

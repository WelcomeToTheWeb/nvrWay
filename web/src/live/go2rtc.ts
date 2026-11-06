/**
 * The live player: a native `RTCPeerConnection` talking to go2rtc's WebSocket
 * API. No dependency is added for this — the browser already has the client, and
 * a WebRTC library would be a second SDP state machine to trust.
 *
 * The protocol is go2rtc's own (`internal/api/ws/README.md`, quoted in
 * NVRWAY_V1_PLAN.md §Step 8): open `/api/ws?src=<slug>`, add recvonly
 * transceivers, send the local description as `webrtc/offer`, apply the
 * `webrtc/answer`, trickle candidates both ways, and take the stream from
 * `ontrack`. The server relays none of this — the browser talks to the media
 * host directly, which is why the base URL comes from the server rather than
 * from a build-time constant.
 *
 * The failure states are observed, not guessed. Against `alexxit/go2rtc:latest`
 * on 2026-11-05, a socket to a stream that is not there answers
 * `{"type":"error","value":"webrtc/offer: stream not found"}` and stays open; a
 * stream that is declared but whose source will not connect answers an error too
 * rather than an empty answer. So "offline" here means *go2rtc said so, in its
 * own words* — the console repeats them instead of inventing a diagnosis.
 */

export type LiveState =
  /** The offer is out or the answer has not landed yet. */
  | { kind: 'connecting' }
  /** A track arrived and is on the element. */
  | { kind: 'playing' }
  /** Media was flowing and the socket ended. Not the same sentence as one that never started. */
  | { kind: 'ended' }
  /** go2rtc answered `type:"error"` — no producer, no stream, or its own failure. */
  | { kind: 'offline'; reason: string }
  /** The socket died before any answer. The media host is not reachable from here. */
  | { kind: 'unreachable'; reason: string }
  /** This browser cannot do WebRTC over WebSocket at all. */
  | { kind: 'unsupported'; reason: string };

export interface LiveHandle {
  close(): void;
}

export interface LiveOptions {
  /** `go2rtc_public_url` as the server answered it — `http(s)://host[:port]`. */
  publicBaseUrl: string;
  /** The camera slug, which is also go2rtc's stream name. */
  slug: string;
  video: HTMLVideoElement;
  onState(state: LiveState): void;
  /** Injectable for tests; the browser's own globals in production. */
  WebSocketImpl?: typeof WebSocket;
  PeerConnectionImpl?: typeof RTCPeerConnection;
}

/**
 * The server's own slug rule (`server/models/camera.go`:
 * `^[a-z0-9][a-z0-9-]{1,31}$`). Checked here because the slug is the stream name
 * and it goes into a query string — a slug with a slash or a space would ask
 * go2rtc for a different stream and the tile would show someone else's picture.
 */
const SLUG = /^[a-z0-9][a-z0-9-]{1,31}$/;

export function liveWsUrl(publicBaseUrl: string, slug: string): string {
  if (!SLUG.test(slug)) {
    throw new Error(`"${slug}" is not a camera slug the server would have accepted`);
  }
  const base = publicBaseUrl.trim().replace(/\/+$/, '');
  if (!/^https?:\/\//.test(base)) {
    throw new Error(`"${publicBaseUrl}" is not an http(s) media host`);
  }
  return `${base.replace(/^http/, 'ws')}/api/ws?src=${encodeURIComponent(slug)}`;
}

export function connectLive(options: LiveOptions): LiveHandle {
  const { video, onState } = options;
  const WebSocketImpl = options.WebSocketImpl ?? globalThis.WebSocket;
  const PeerConnectionImpl = options.PeerConnectionImpl ?? globalThis.RTCPeerConnection;

  if (!PeerConnectionImpl) {
    onState({ kind: 'unsupported', reason: 'this browser has no RTCPeerConnection' });
    return { close() {} };
  }
  if (!WebSocketImpl) {
    onState({ kind: 'unsupported', reason: 'this browser has no WebSocket' });
    return { close() {} };
  }

  let url: string;
  try {
    url = liveWsUrl(options.publicBaseUrl, options.slug);
  } catch (error) {
    onState({ kind: 'unreachable', reason: (error as Error).message });
    return { close() {} };
  }

  let closed = false;
  let answered = false;
  let playing = false;
  /** Candidates that arrived before the answer. `addIceCandidate` before a
   * remote description throws in a real browser, so they wait here. */
  const pending: string[] = [];

  const pc = new PeerConnectionImpl();
  let ws: WebSocket | null = null;

  /** `WebSocket.OPEN`, spelled as a literal so the module does not depend on a
   * global the test environment may not ship. */
  const OPEN = 1;

  const send = (message: { type: string; value: string }) => {
    if (closed || !ws || ws.readyState !== OPEN) return;
    ws.send(JSON.stringify(message));
  };

  const addCandidate = (candidate: string) => {
    if (!candidate) return;
    if (!answered) {
      pending.push(candidate);
      return;
    }
    void pc.addIceCandidate({ candidate }).catch(() => {
      // A candidate go2rtc offered after the connection already picked a path
      // is noise, not a failure worth a red tile over.
    });
  };

  pc.onicecandidate = (event: RTCPeerConnectionIceEvent) => {
    // The null candidate marks the end of trickle. go2rtc accepts it, and it is
    // not a value worth sending as a candidate.
    const candidate = event.candidate?.candidate;
    if (candidate) send({ type: 'webrtc/candidate', value: candidate });
  };

  pc.ontrack = (event: RTCTrackEvent) => {
    if (closed) return;
    const [stream] = event.streams;
    if (!stream) return;
    playing = true;
    video.srcObject = stream;
    onState({ kind: 'playing' });
  };

  const teardown = () => {
    if (closed) return;
    closed = true;
    try {
      pc.close();
    } catch {
      /* already gone */
    }
    try {
      ws?.close();
    } catch {
      /* already gone */
    }
  };

  const handleAnswer = async (sdp: string) => {
    await pc.setRemoteDescription({ type: 'answer', sdp });
    if (closed) return;
    answered = true;
    const queued = pending.splice(0, pending.length);
    for (const candidate of queued) addCandidate(candidate);
  };

  const handleMessage = (raw: string) => {
    let message: { type?: string; value?: string };
    try {
      message = JSON.parse(raw);
    } catch {
      return; // Not a go2rtc frame. Nothing to say about it.
    }
    switch (message.type) {
      case 'webrtc/answer':
        if (message.value) void handleAnswer(message.value).catch(() => {});
        return;
      case 'webrtc/candidate':
        if (message.value) addCandidate(message.value);
        return;
      case 'error':
        // go2rtc keeps the socket open after this. There is nothing to wait for:
        // the answer will not come for this offer, so stop and say what it said.
        onState({ kind: 'offline', reason: message.value ?? 'go2rtc refused the offer' });
        teardown();
        return;
    }
  };

  const offer = async () => {
    const description = await pc.createOffer();
    await pc.setLocalDescription(description);
    if (closed || !pc.localDescription?.sdp) return;
    send({ type: 'webrtc/offer', value: pc.localDescription.sdp });
  };

  onState({ kind: 'connecting' });

  try {
    // The socket opens first: the handshake is the slow part of "all cameras,
    // under a second", and nothing below it has to wait for the network.
    ws = new WebSocketImpl(url);
  } catch (error) {
    onState({ kind: 'unreachable', reason: (error as Error).message });
    return { close: teardown };
  }

  pc.addTransceiver('video', { direction: 'recvonly' });
  pc.addTransceiver('audio', { direction: 'recvonly' });

  ws.onmessage = (event: MessageEvent) => handleMessage(String(event.data));
  ws.onerror = () => {
    // `close` always follows an error; the state is set there so a tile does not
    // flash two sentences for one failure.
  };
  ws.onclose = () => {
    if (closed) return;
    closed = true;
    try {
      pc.close();
    } catch {
      /* already gone */
    }
    if (playing) {
      onState({ kind: 'ended' });
      return;
    }
    onState({
      kind: 'unreachable',
      reason: answered
        ? 'the media host closed the connection'
        : `the media host at ${options.publicBaseUrl} closed the connection before answering the offer`,
    });
  };
  ws.onopen = () => {
    void offer().catch(() => {
      if (!closed) onState({ kind: 'unreachable', reason: 'the offer could not be created' });
    });
  };

  return { close: teardown };
}

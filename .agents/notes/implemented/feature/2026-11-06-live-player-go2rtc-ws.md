# Agent Note: live-player-go2rtc-ws

Status: implemented

## Problem

THE WALL is the product's first promise — all cameras, live, under a second, no spinner — and the interesting half of it is not the picture but the absence of one. A camera that is configured but whose source is dead, a camera that was never added to go2rtc, a media host the browser cannot reach, and a live stream that just dropped all look identical as a black box, and the plan forbids the spinner that would otherwise cover them. The card's criterion is "a camera with no producers shows offline, not a black box", which is a claim about a state the console cannot see directly: the console does not know whether a producer exists, only what go2rtc does with an offer.

The media host is the other half. `GO2RTC_PUBLIC_URL` differs per installation and the browser must reach it directly — the server relays no media in v1 — so a build-time constant would make one build serve one network.

## Decision

`web/src/live/go2rtc.ts` is the live player and `web/src/pages/LivePage.tsx` is the wall. No dependency is added: the player uses the browser's own `RTCPeerConnection` and `WebSocket`, both injectable for tests (`WebSocketImpl`, `PeerConnectionImpl`).

The protocol is go2rtc's `/api/ws`: open `${base→ws}/api/ws?src=<slug>`, `addTransceiver('video'|'audio', {direction:'recvonly'})`, `createOffer` → `setLocalDescription` → send `{type:'webrtc/offer', value: pc.localDescription.sdp}`, apply `{type:'webrtc/answer'}` as the remote description, exchange `{type:'webrtc/candidate'}` both ways, and take the stream from `ontrack` into `video.srcObject`. The socket is opened *before* the transceivers are added, because the handshake is the slow part of "all cameras, under a second". Candidates arriving before the answer are queued and flushed — `addIceCandidate` before a remote description throws in a real browser — and the null end-of-candidates marker is not sent.

`liveWsUrl` validates the slug against the server's own rule (`server/models/camera.go`: `^[a-z0-9][a-z0-9-]{1,31}# Agent Note: live-player-go2rtc-ws

Status: implemented

## Problem

THE WALL is the product's first promise — all cameras, live, under a second, no spinner — and the interesting half of it is not the picture but the absence of one. A camera that is configured but whose source is dead, a camera that was never added to go2rtc, a media host the browser cannot reach, and a live stream that just dropped all look identical as a black box, and the plan forbids the spinner that would otherwise cover them. The card's criterion is "a camera with no producers shows offline, not a black box", which is a claim about a state the console cannot see directly: the console does not know whether a producer exists, only what go2rtc does with an offer.

The media host is the other half. `GO2RTC_PUBLIC_URL` differs per installation and the browser must reach it directly — the server relays no media in v1 — so a build-time constant would make one build serve one network.

## Decision

), because the slug *is* the go2rtc stream name and it goes into a query string; a slug containing a slash would ask go2rtc for a different stream. `https` becomes `wss`.

The player reports a discriminated `LiveState`: `connecting | playing | ended | offline | unreachable | unsupported`. **`offline` means go2rtc answered `{"type":"error","value":…}` on an open socket, and the reason is go2rtc's own words, repeated verbatim** — measured against `alexxit/go2rtc:latest` on 2026-11-05, where an unknown src answers `webrtc/offer: stream not found` and a declared-but-unfed stream answers `webrtc/offer: streams: unknown error`, both with the socket left open. `unreachable` is reserved for a socket that died before any answer, and `ended` for a socket that died after media was flowing.

`LivePage` reads the media host from `GET /api/v1/settings/live` (S8-04) and the cameras from `GET /api/v1/cameras`, in parallel, and hands the server's answer to every tile — the bundle never contains a media host. There is one failure path for a failed load, and it repeats the server's error body. A tile's `<video>` is mounted from the first paint with `hidden` set, and unhidden only on `playing`, so a camera with nothing to show is a sentence, not a black box. `close()` on unmount tears down the peer connection, the socket, and `srcObject`.
## Alternatives considered

**Decide "offline" from go2rtc's `/api/streams`.** Rejected by measurement, not taste: a stream declared with a dead RTSP source still answers `{"porch":{"producers":[{"url":"rtsp://127.0.0.1:5541/unreachable"}]}}`, so a producer entry does not mean the camera is up. It would also put go2rtc's basic-auth API in front of the browser.

**A timeout heuristic** (no `ontrack` within N ms ⇒ offline). Rejected: go2rtc answers an `error` frame immediately and keeps the socket open, so a timer would only add latency and a second, weaker source of truth.

**`hls.js` as the fallback, behind a mode toggle.** Rejected for now, and the card's own criterion is what rejects it: the fallback is permitted *only if WebRTC fails in the target browser*, and WebRTC has not been run in a browser at all yet. Building the contingency now would fail the criterion it satisfies, and would add the console's only media dependency.

**Render the `<video>` element only once a track exists.** Rejected: the element has to exist before `connectLive` runs, because `ontrack` assigns `srcObject` to it, and attaching a stream to a freshly mounted element drops the picture. It is mounted from the first paint with `hidden` set instead.

**A WebRTC library (`werift`, `simple-peer`, `@koush`-style helpers).** Rejected by the plan and by the criterion "no new dependency": the browser already has `RTCPeerConnection`, and a library would be a second SDP state machine to trust between the console and go2rtc.
## Consequences

**Bought:** the wall's failure states are grounded in the media server's actual protocol rather than in a guess, so a camera with nothing to show says *why* in words an operator can act on, and the console does not need a second channel (a poll, or go2rtc's authenticated API) to know it. The media host is a server answer, so one build serves any installation. The wire contract is pinned by 16 tests that each fail on exactly the mutation that breaks them.

**Cost:** the offline mapping is coupled to go2rtc's current wording — it repeats `value` verbatim, which is the honest choice but means a future go2rtc that changes its error text changes what the tile says. The player is untested against a real browser's SDP: the probe used a stub offer, so the exact error string for an unfed stream under a real offer is unverified, and end-to-end WebRTC in chromium is S8-12's claim to make. `LivePage` is exported and tested but not mounted — routes and role-aware nav are S8-09's, so the wall is unreachable in a running console until then. Five tile states is one more than the plan listed; `ended` is a real distinction the plan did not have.

**Not done:** no HLS fallback and no `hls.js` dependency; no reconnect loop inside the player (a tile that ends stays ended until the wall is remounted — the socket lifecycle for the whole console is S8-10's); no `recorder.status` line on the wall, which is why S4-06 remains blocked, now on S8-10 rather than on this card.


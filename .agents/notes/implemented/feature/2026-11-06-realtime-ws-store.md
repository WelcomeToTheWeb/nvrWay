# Agent Note: realtime-ws-store

Status: implemented

## Problem

Every live surface the console still has to build — the event wall, the recorder status line, the storage queue, the camera tiles — needs the same socket, and each one owning its own connection would mean N sockets per tab, N backoff loops, and N places that can be quietly stale. The plan puts one store in front of them (`src/stores/realtime.ts`), and the card adds the two things that make a single socket trustworthy: the seven message types handled exactly, and a stale state the console can show rather than a feed that silently stops updating.

The hard part is the third criterion: "a rejected origin says so, silently reconnecting is the failure mode to avoid". A rejected origin is answered before the WebSocket upgrade, so the browser is told nothing — the console has to infer a configuration fault from a socket that never opens, without pretending to know more than that.

## Decision

`web/src/stores/realtime.ts` is a zustand store over the server's `/ws` endpoint, created by `createRealtime({token, WebSocketImpl?, schedule?, location?})`; `useRealtime` is the singleton wired to `readAccessToken()`. `token` is a **function**, re-read on every attempt, because a refresh rotates the access token.

The URL is `${wss|ws}//${location.host}/ws?token=…` — `/ws` is `config.WSPath`, a constant at the route root, deliberately not under `/api` (the axios client's baseURL is `/api`, so the socket URL cannot be built from it). `?token=` is the one place nvrWay accepts the access token outside `Authorization`, because a browser WebSocket cannot set a header.

Frames are the server's `Envelope` (`server/ws/hub.go`): `{type, payload, ts}`. **The payload is nested under `payload`**, and `ts` is the server clock in UTC, kept as `lastFrameAt` because the console compares a segment's end with an event's start and a browser clock cannot be the measure of both. `WS_TYPES` exports the seven types and a test asserts the set equals `hub.go`'s. `camera.status` and `recorder.status` are keyed by `camera_id`, never by slug. `event.close` keeps the event and sets `open: false` rather than deleting the row. An unknown type is ignored, not fatal.

Reconnect is exponential from 5 s, doubling, capped at 60 s: `5000, 10000, 20000, 40000, 60000, 60000`. `disconnect()` cancels the pending retry and resets to `idle`.

`status` is `idle | connecting | open | stale | refused`. After `REFUSAL_AFTER = 3` consecutive attempts that never reach an open socket, `status` becomes `refused` and `reason` names `WS_ORIGINS` — because `Hub.Handle` answers a rejected origin `403` **before the upgrade**, so the browser cannot read the server's sentence and can only observe a socket that will not open. The retry does not stop; the state says what to check. A socket that later opens returns to `open` with `reason: null`.
## Alternatives considered

**A module-level singleton store.** Rejected: the backoff schedule and the socket are the behaviour under test, and a singleton forces either fake timers or a wall clock. `createRealtime({token, WebSocketImpl, schedule, location})` returns a store; `useRealtime` is the singleton the console uses, built with the real `readAccessToken`. Timers are injected, so the schedule is asserted as data — `[5000, 10000, 20000, 40000, 60000, 60000]` — not waited for.

**A fixed five-second retry.** Rejected on the plan's own wording: "5 s reconnect backoff capped at 60 s" — a fixed interval needs no cap, so the cap only means something if the interval grows. It doubles and stops at sixty.

**Read the token once at connect and reuse it for reconnects.** Rejected: a refresh rotates the access token, and a reconnect carrying the old one is a 401 that looks exactly like a dead server. `token` is a function, and a test rotates it between attempts.

**Treat three failed attempts as "the origin is rejected" and state that as fact.** Rejected as overclaiming: a genuinely down network reads identically, and the browser cannot read the 403 body at all. The reason is written as a conditional and the state clears on the next successful open.

**Delete the event on `event.close`.** Rejected: the console is showing a card for that event, and deleting the row makes it vanish under the operator's cursor. It is kept with `open: false`.

**Poll the REST endpoints instead of holding a socket.** Rejected by the card's own criterion — "the LivePage status line reads from it (no polling)" — and by the plan's machine: a camera that says offline must be offline because the server said so, not because a poll happened to miss it.
## Consequences

**Bought:** the console has one place that knows what the live feed is doing, and every page can subscribe to a slice instead of owning a socket or polling. The seven types are asserted against the server's own set, so a type added in `hub.go` fails a test rather than arriving as a silently missing column. The server clock is carried through, so event and segment times are comparable. The failure that used to be invisible — a console served from a host the server does not know, retrying quietly forever — is now a named state with the fix in its sentence.

**Cost:** `refused` is an inference, not a diagnosis, and it will fire on a bad network too; the wording is conditional for exactly that reason, and anyone who later makes it assert a cause will be overclaiming. The store is coupled to the server's payload shapes by test, which catches a rename but also means a server field rename is a console test failure. Nothing calls `connect()` yet and no banner renders `stale`, so the store is currently dead weight in a built bundle until the routes card mounts it.

**Follow-ups this card names:** S8-06 (the event wall) and S4-06 (the recorder status line on the tile) both read from this store now. S4-06 was still labelled `blocked` after this card closed and was corrected in `d5c7123` — when a card closes, re-check the cards that named it as a blocker.

**Not done:** no banner component; no reconnect on token refresh (the socket simply drops and backs off, which is correct but not immediate); no per-page subscription helpers; no test against a real server socket — the frames here are the shapes the call sites send, read from `hub.go`, `cameras.go`, `segments.go` and `sweeper.go`, not captured off a live connection.


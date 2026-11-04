# nvrWay — Architecture

<!-- skeleton: S0-02 (2026-11-02). Headings, each naming the decision it must
     answer. The content is card S9-10, which requires a decision per section
     ("docs content that carries decisions, not filler"). -->

The shape of the system, the three rules it follows, and the gaps v1 leaves
open on purpose. Read `docs/configuration.md` for the knobs and `docs/api.md`
for the surface.

## Table of Contents

List the sections below once they have content.

## System Overview

The pipeline, drawn: `cameras →RTSP→ go2rtc →(RTSP)→ server/media recorder →
segments (mp4)`, the `<slug>_det` mjpeg tap → detector (YOLO + zones) →
internal API signals → events → clip assembly (pre-roll + post-roll concat)
→ WebSocket fan-out + per-camera webhooks → storage tier (async upload,
retention sweeps). Answer: **what is one process, what is a sidecar, and what
is a separate container.**

## Components

One section each for `server/` (config, models, store, auth, sso, api, ws,
media, go2rtc, storage, sweeper, webhooks), `detector/`, `web/`, go2rtc,
FFmpeg, Postgres. Answer per component: what it owns, what it may never do,
and which card implements it.

## Data Flow

### Recording flow

Segment naming, the `SEGMENT_SECONDS` window, the ffmpeg CSV that drives
segment rows, the Generation counter and what a restart does to it (S4-02,
S4-03).

### Detection flow

How a camera becomes a detection tap (`<slug>_det`, 640×360 @ 5 FPS), what
the worker polls for config, what a signal carries, and what the server does
with it (S5-08, S5-10).

### Event flow

open → extend → close on the server clock, `event_key` dedupe, the 10 s idle
close, `EVENT_MAX_SECONDS`, the 200-object cap, snapshot on open (S6-03).

### Clip flow

The window math (`PRE_ROLL_SECONDS` / `POST_ROLL_SECONDS`), concat over
segments, and **the empty-window rule** — what `clip_key == ""` means and
what gets logged (S6-01).

### Storage flow

Local-first, the upload queue (claim → upload → `uploaded`, exponential
retry, `failed` at 20 attempts, the enable flag), and the key layout
`nvr/<slug>/<YYYY>/<MM>/<DD>/<basename>.mp4`, `nvr/events/<id>/{clip,snapshot}`
(S7-07, S7-08).

## The clock rule

**The decision this section must state: the server clock owns event time, and
a worker's timestamp never reaches the database.** Why: host clock skew
cannot create events in the future, and an event timeline that two machines
disagree about is not a timeline. Say what the server does with a signal's
`ts` (nothing that reaches a row), and what the API asserts about it (S6-03).

## The single-instance rule

**The decision this section must state: one server process owns the media
tree — recording, clip assembly and the retention sweeper are single-writer,
which is why there is no Redis and no second instance.** Say what breaks if a
second server starts (two ffmpeg supervisors on one directory, two sweepers on
one queue), and what v2 must do before multi-instance is possible (media
ownership, S7-09).

## Playback and the remote seek gap

**Playback is proxied by the server. A clip that lives only on a remote tier
streams from byte 0 and cannot be scrubbed; seek-on-remote is a v2 item.**

### The proxy, and why there is no direct URL

The console asks for media by event id — `GET /api/v1/events/:id/clip`,
`GET /api/v1/events/:id/snapshot` — and never receives a path. Two reasons, both
load-bearing:

- A name on this server's disk is not a name a browser can dial. The console runs
  on a laptop, the media tree is on the server, and putting `CLIPS_DIR` in the
  page would make the deployment's directory layout part of the browser contract.
- The remote copy's address carries a credential. A tier URL would either hand a
  signed object URL to every viewer's browser or leak the tier's own credentials
  into the page, and the storage layer keeps those sealed in the database
  precisely so that neither happens.

The route is also where the role check lives. `viewer` may watch; that decision
belongs to the process that knows the session, not to the tier.

### Local first, then the tier

Both media routes resolve "where are the bytes" in the same order, so they cannot
contradict each other inside one drawer:

1. **The local file**, when the row names one and it lies inside `CLIPS_DIR` /
   `SNAPSHOTS_DIR`. Served by `http.ServeContent`.
2. **The tier**, when there is no readable local copy and the row's upload stamp
   is set (`clip_uploaded_at`, `snapshot_uploaded_at`). The installation's
   default backend is opened and the object at the row's key is streamed.
3. **A named 404** — `clip not available` / `snapshot not available` — when
   neither has them.

The row records the object's *key*, not which backend holds it, so changing the
default backend does not move old clips. That is a property of the data model, not
of the route.

### Local playback: Range, 206, conditional GET

`http.ServeContent` gives the local path three things for free, and the console
depends on all three:

- `Range: bytes=a-b` answers **206** with `Content-Range`. This is how a scrubber
  moves the play head without downloading the clip, and how a player fetches the
  tail (`bytes=-N`) to read an mp4's index.
- `If-Modified-Since` / `If-Range` answer **304** and fall back cleanly, which is
  what a drawer that redraws the same event sends without being clever about it.
- A malformed or unsatisfiable range answers **416**, which the browser handles by
  starting over.

The response always carries `Accept-Ranges: bytes` on both paths, and
`Cache-Control: private, max-age=3600` — `private` because the frame shows a
person's property to whoever holds a console session, `max-age` because an event's
media is written once and never rewritten.

### Remote playback: whole object, from byte 0, no seek

The storage interface is:

```go
Get(ctx context.Context, key string) (io.ReadCloser, error)
```

There is no offset and no length. A remote clip is therefore streamed whole, and a
request that carries `Range` gets the **entire object with a 200** — the fallback
HTTP sanctions when a server cannot satisfy a range, and what browsers do with it:
playback starts as soon as enough bytes have arrived, and the player buffers
forward. `Content-Length` is sent, so the player knows the clip's size and can
show a duration; what it cannot do is jump to a point it has not buffered.

Why the interface stops there:

- Every tier behind it *does* have a ranged read — minio's `GetObject` takes
  `Offset`/`Length`, WebDAV answers `Range`, `pkg/sftp`'s `FileReaderAt` seeks —
  so the gap is one method on one interface, not a limitation of the tiers.
- Answering `Range` correctly is not only the read. It means choosing 206 versus
  200 versus 416 per request, emitting `Content-Range`, and handling
  multi-range, which is a second contract on top of the first. Shipping a partial
  version of it against four drivers at once is how a media server ends up with
  four subtly different seek behaviors.
- The upload path never needs a range, and the retention sweep never does, so the
  method would exist for exactly one caller.

**What the console does with a remote clip.** It plays it as a progressive
download. The drawer should not offer a scrubber it cannot honour: the event view
carries `clip_uploaded` and `has_clip`, and the route is the truth about which
copy answered. A clip that is local-only-and-present seeks; a clip that came from
the tier plays forward. Until v2 adds the ranged read, that is the line.

The one thing already in place for remote seeking is on the encode side: clip
assembly passes `-movflags +faststart`, so the mp4's `moov` atom sits at the front
of the object and a browser can begin playback from the first bytes rather than
after a tail fetch. That is why a remote clip *starts* promptly today; only the
seek is missing.

### The two round trips, and the answers a tier gives

The remote path calls `Stat` before `Get`. That is one extra round trip per play,
bought for two things: a `Content-Length` that is true (a streamed object with no
length leaves the player unable to show progress or duration), and a 404 that
arrives **before** a 200's headers have been written — the difference between a
drawer that says "no clip" and one that shows a player that stopped halfway.

The tier's failures are named, in the response and in the log:

| Situation | Answer |
| --- | --- |
| No copy anywhere, or the object is gone from a tier that is there | `404 {"error":"clip not available"}` |
| No default backend configured at all | `404 {"error":"clip not available"}` |
| Tier configured and not usable (unreachable, wrong credentials, bucket or mount gone, host key changed) | `502 {"error":"…","tier":"<named state>"}` |
| Stored credential cannot be opened with this installation's key | `500` |

The 404 and the 502 are not interchangeable, and the fs driver is what forces the
distinction: `os.Stat` answers `ENOENT` both for an object that was never written
and for a volume that is not mounted. The driver checks the root once, on the miss
path, and reports `bucket missing` rather than "no such object" — otherwise an
unmounted media volume looks, for every clip at once, exactly like a house with no
recordings.

### What v2 adds

1. `GetRange(ctx, key, offset, length)` on `Backend`, implemented per tier
   (minio's options, WebDAV's `Range` header, sftp's `ReaderAt`), with the 206 /
   200 / 416 contract asserted in the shared driver test so all four agree.
2. The playback route answering `Range` on the remote path with it, and
   `Accept-Ranges` becoming a true statement rather than a promise.
3. A decision about whether the console should prefer the tier when the local copy
   has aged out but the clip is short enough to buffer whole.

## Live view

go2rtc WebRTC over its WebSocket API with a native `RTCPeerConnection`
(S8-05): what the console dials, what `GO2RTC_PUBLIC_URL` must be for the
browser to reach it, the LAN-only default, and the condition under which HLS
(`hls.js`) is added as the fallback.

## WebSocket hub

The local hub, token auth, the origin guard, the seven message types, the
fan-out throttling on `event.update` (S2-10, S6-08). Answer: why no Redis
adapter, and what a second instance would need.

## Authentication and tenancy

JWT access/refresh with rotation and reuse detection, the break-glass admin,
the OIDC authorization-code + PKCE + nonce flow, the `viewer` JIT floor, the
role matrix (viewer read / operator CRUD / admin) (S2-01, S2-03, S2-04,
S3-06).

## Detection

YOLOv8n ONNX on `CPUExecutionProvider` as the **verified baseline**;
letterbox preprocess, the `[1,84,8400]` decode, NMS thresholds, the greedy
IoU tracker (≥0.30, 6-frame expiry), the CPU motion gate (160 px grayscale
absdiff, 1%, continuous mode only). Answer: what is sized for 4 cameras on
4 vCPU, and what the sizing claim is based on.

## Inference host: CPU default, CUDA overlay

**The decision this section must state: CPU ONNX is the baseline that is
verified here; CUDA ships as `docker-compose.gpu.yml` and is documented, not
verified on a host without the NVIDIA container toolkit.** State which claims
were observed on which box, so a reader cannot mistake the GPU path for a
tested one (S9-06).

## Retention

The seven sweeps, the un-uploaded protection window (a storage outage never
destroys the only copy), and the `0 = keep forever` semantics of
`REMOTE_RETENTION_DAYS` (S7-09).

## Technology Choices

The table from the README with a reason per row: gin/gorm, hand-rolled OIDC,
go2rtc for ingest/WebRTC, FFmpeg for segments and clips, ONNX Runtime CPU,
sqlite for tests and Postgres for deployment, no Redis.

### Key Design Decisions

The list, each with the alternative it rejects: slug is the stream name;
camera writes re-render go2rtc's config; zones are normalized coordinates;
secrets sealed at rest and never echoed; registration disabled (`403`);
MIT license with detection assets downloaded at build time, never committed.

## Scalability Considerations

### Current limits

4–8 cameras, 5 FPS per camera, one server, one media tree, single-browser
WebSocket hub, the rate limiter's per-user budget.

### Scaling path

What a v2 site with 16 cameras would change first: media ownership, HLS,
detection worker placement, remote seek.

## Test seams

Where the design deliberately leaves a seam so it can be proven without
hardware: the `source_uri` override that lets a `lavfi` testsrc stand in for
an RTSP camera (S4-07), the demo-only `demo-doorbell` source rule in
`go2rtc/config.go` (S10-02), the in-repo test IdP (S2-05), the fixture
manifest (S10-01).

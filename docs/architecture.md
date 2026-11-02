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

**The decision this section must state: playback is proxied by the server; a
clip that lives only on a remote tier streams from byte 0 and cannot be
scrubbed — seek-on-remote is a v2 item.** Cover local Range/206 (S6-05), what
the console does with a remote clip, why Range cannot be answered against a
remote object, and what v2 would have to add.

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

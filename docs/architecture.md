# nvrWay — Architecture

The shape of the system, the three rules it follows, and the gaps v1 leaves
open on purpose. Read `docs/configuration.md` for the knobs and `docs/api.md`
for the surface.

## Table of Contents

- [System Overview](#system-overview)
- [Components](#components)
- [Data Flow](#data-flow) — [recording](#recording-flow), [detection](#detection-flow), [events](#event-flow), [clips](#clip-flow), [storage](#storage-flow)
- [The clock rule](#the-clock-rule)
- [The single-instance rule](#the-single-instance-rule)
- [Playback and the remote seek gap](#playback-and-the-remote-seek-gap)
- [Live view](#live-view)
- [WebSocket hub](#websocket-hub)
- [Authentication and tenancy](#authentication-and-tenancy)
- [Detection](#detection)
- [Inference host: CPU default, CUDA overlay](#inference-host-cpu-default-cuda-overlay)
- [Retention](#retention)
- [Technology Choices](#technology-choices)
- [Scalability Considerations](#scalability-considerations)
- [Gaps v1 leaves open, stated](#gaps-v1-leaves-open-stated)
- [Test seams](#test-seams)

## System Overview

```
cameras ──RTSP──▶ go2rtc ──RTSP──▶ server/media recorder ──ffmpeg──▶ segments (mp4)
  (or exec:)        │                                                    │
                    └──<slug>_det mjpeg 640×360@5──▶ detector (YOLO+zones)
                                                          │
                                                   internal API signals
                                                          ▼
                              events (open/extend/close) ──▶ clip assembly (pre+post concat)
                                                          │              │
                                              WS fan-out + webhooks   storage tier (async queue)
                                                                     retention sweeps (7, hourly)
```

Three deployment shapes, and the line between them is a decision, not an
accident:

- **One process**: the Go server (`server/`). API, auth, WS hub, recorder
  supervisor, clip assembly, upload queue, sweeper — all one binary, one
  process, because they share one truth: the media tree and the database. The
  recorder supervises ffmpeg children; the sweeper deletes what the recorder
  made. Splitting them means two processes agreeing about a directory, and
  [the single-instance rule](#the-single-instance-rule) is the statement that
  v1 does not ask them to.
- **A sidecar**: go2rtc. It owns camera connections and browser media
  (WebRTC), which the server must not own — the server talks RTSP to it and
  renders its config, and never touches a camera socket itself. It lives in
  the compose project beside the server because its config is the server's to
  write.
- **Separate containers**: the detector (Python + ONNX Runtime — a different
  runtime entirely, and it reaches the server only through the internal API),
  the console (a static bundle behind nginx), and Postgres.

The detector being *only* an API client is load-bearing: it holds no database
connection and no media path, so a worker can run on another host the day
someone wants that, with nothing to change in the server.

## Components

What each owns, and what it may **never** do — the never-list is the part a
maintainer should not be able to violate by accident.

### `server/` (one Go binary)

| Package | Owns | May never |
|---|---|---|
| `config` | every env read, with its default | let a zero mean absent (`REMOTE_RETENTION_DAYS=0` is a policy) |
| `models`, `store` | schema, migrations, the event state machine (`applySignalTo` is pure — no DB, no clock) | trust a caller's timestamp into a row |
| `auth`, `sso` | tokens, sessions, rotation + reuse detection, OIDC discovery/PKCE/nonce | store a password as anything but argon2id; echo a secret |
| `api` | the REST surface, role gates, the ledger, rate limiting | answer a media path to the browser |
| `ws` | the hub: auth, origin guard, seven message types, the `event.update` throttle | fan out unthrottled per signal |
| `media` | the recorder supervisor (one goroutine per camera), ffmpeg children, segment discovery, clip assembly | run two recorders on one camera, or trust ffmpeg's wall clock over the server's |
| `go2rtc` | the sidecar client: streams, `ApplyConfig`, probe, the rendered YAML | have its config hand-edited (the render is the source of truth) |
| `storage` | backends (fs/s3/webdav/sftp), sealing, the upload queue | lose a job on failure — retry, then mark `failed`, keep the row |
| `sweeper` | the seven retention sweeps | delete a local file whose upload has not landed |
| `webhooks` | per-camera delivery, HMAC signature, attempt history | block the event path on a slow receiver |

### `detector/` (Python 3.12, ONNX Runtime)

Owns: the tap poll (`<slug>_det`), preprocess, decode, NMS, the IoU tracker,
zone rules, the motion gate, event open/extend/close decisions, snapshot
encode, and the signal buffer that survives a server outage. May never:
connect to the database, touch the media tree, or stamp a signal with a time
the server must believe. Cards: S5-01…S5-10, S9-07 (heartbeat).

### `web/` (React 18 + Vite, served by nginx)

Owns: the console. Talks only REST, `/ws`, and the sidecar's WebRTC WebSocket.
May never: learn a filesystem path, or guess a roll window — the server tells
it (`GET /api/v1/settings/media`, P-11).

### go2rtc (sidecar image)

Owns: camera connections (RTSP/HTTP/exec sources), stream multiplexing,
WebRTC/RTSP serving, the detection tap transcode. Its config file may only be
written by the server's render or the boot seed (P-15/P-16).

### FFmpeg (children of the recorder, and of go2rtc)

Owns: segment encode (`-segment_time SEGMENT_SECONDS` + the CSV manifest the
recorder tails) and clip concat. May never: outlive its supervisor — every
child is killed on context cancel and its exit is a logged, status-bearing
event (S4-04).

### Postgres (or sqlite)

Owns: rows — cameras, events, segments, jobs, settings, sessions, audit.
sqlite is for tests and dev only, and the fallback is logged at boot, not
silent.

## Data Flow

### Recording flow

One goroutine per camera (`media/recorder.go`), each supervising one ffmpeg
child that pulls `rtsp://go2rtc:8554/<slug>` and writes
`<slug>-<generation>-<N>.mp4` with `-segment_time SEGMENT_SECONDS` (default
5) and a `-segment_list` CSV beside them. The recorder **tails the CSV**, not
the filenames: each new CSV line becomes a segment row, stamped with the
server's clock at discovery.

The **Generation counter** (S4-02) is a settings row bumped every time a
recorder (re)starts. It is why a restart cannot collide: ffmpeg restarts its
segment index at 0, and without the generation in the filename the second
run's `…-0.mp4` would overwrite the first run's.

`record_mode` decides which goroutines exist at all: `continuous` always
records; `events` records only while a window is open (the first signal of an
event opens it, later signals extend it, and the loop exits quietly when it
closes — silence there is by design, not a dead recorder); `off` never runs a
child.

### Detection flow

A camera with detection enabled gets a second go2rtc stream, `<slug>_det`: the
same source transcoded to mjpeg 640×360 at 5 FPS (`GO2RTC_MJPEG_*`). The
worker long-polls that tap, runs the motion gate first (a 160 px grayscale
absdiff; under 1% changed pixels and the frame skips the model — continuous
mode only), then YOLOv8n, NMS, the IoU tracker, and the camera's zone rules
as fetched from `GET /api/internal/v1/detector/config` (polled, so a zone
drawn in the browser reaches the worker within one poll without any push
path).

A signal is small on purpose: `event_key`, action (`open`/`extend`/`close`),
camera, zone, rule, severity, classes, the object boxes **in tap pixels**
with the tap's `det_width`/`det_height` beside them (P-10 — the server stores
the frame the boxes are pixels of, because a box without its frame is
unplaceable), and a base64 snapshot on `open` only, capped at 250 KB. The
server's answers are fixed: `200` with the event id, `410` for a closed event
(the worker stops retrying that key), and `500` only for a server-side
failure — which is the one answer the worker buffers and resends, safe
because `event_key` dedupes.

### Event flow

The state machine is `store/events.go:applySignalTo` — pure, no database, no
clock, which is what makes it exhaustively testable. `open` creates the row
(unique on `event_key`; a duplicate open is a no-op — a birth certificate
presented twice is one birth), `extend` unions classes/zones/rules and
appends objects, `close` ends it. The **worker** decides when an event is
over (10 s without a hit — `IDLE_CLOSE_SECONDS`) and the **server** enforces
the ceiling (`EVENT_MAX_SECONDS`, default 300: a forced close, so a stuck
worker cannot hold an event open forever). Objects are capped at 200 per
event, oldest trimmed first (`MaxEventObjects`). The snapshot arrives with
the open signal and is written once; over the cap it is stored as absent and
**the event survives** — the event is the fact, the picture is evidence.

Every timestamp on the row is the server's clock at receipt. See
[the clock rule](#the-clock-rule).

### Clip flow

When an event closes, the clip builder takes the window
`[started_at − PRE_ROLL_SECONDS, ended_at + POST_ROLL_SECONDS]` (8/15 by
default) and concats every segment overlapping it — `-c copy` concat,
`-movflags +faststart`, into `CLIPS_DIR`. The roll is not guessed at the
edges: the console is told the numbers (`GET /api/v1/settings/media`, P-11)
so the drawer's strip spans the padded window, not just the event.

**The empty-window rule**: if the window held no segments — an `events`
camera whose recorder lagged, a camera in `off` mode that still detects — the
event is still filed, the snapshot still exists, `clip_key` stays `""`, the
clip route answers the named 404, and the log carries the window it looked
at. An event without a clip is a stated state, never a silent one.

### Storage flow

Local-first: every object is written locally, and only then is an upload job
enqueued. The queue is claim → upload → stamp `uploaded`; failures retry with
exponential backoff on the tick (`UPLOADER_TICK_SECONDS`, default 30) and go
`failed` at **7 attempts** (`retryCapAttempts`) — the row stays, the console
shows it, and the requeue route restarts it. Key layout:
`nvr/<slug>/<YYYY>/<MM>/<DD>/<basename>.mp4` for segments,
`nvr/events/<id>/{clip,snapshot}` for event media.

**A job row is keyed by the object it owes** (P-08): a segment's job id is the
segment id, and an event's two objects are `<event_id>:clip` and
`<event_id>:snapshot`, named by the one helper `store.EventJobID` — the purge
routes, the clip builder and the snapshot writer all call it, and the uploader
parses the same suffix back. That id is a key, not a uuid, which is why
`storage_jobs.id` is text (migration v2); enqueue is idempotent because the
object's identity **is** the row's identity.

## The clock rule

**The server clock owns event time, and a worker's timestamp never reaches
the database.**

A detection box is computed on a machine this server does not control — its
clock may be wrong, may be set to the future, may jump when its NTP client
wakes up. If a signal's `ts` could file an event, a drifted worker could
create events hours from now, and an event timeline that two machines
disagree about is not a timeline. So `POST /api/internal/v1/signals` validates
that `ts` parses — a signal whose timestamp will not parse is refused, by
number — and then **throws it away**: every `started_at`, `ended_at`,
`updated_at` on the row is `time.Now()` in this process. The API documents the
same rule from the other side: the times it returns are server-clock times,
and the only client-visible use of a signal's `ts` is the worker's own
idle-close bookkeeping.

The cost is honest and stated: event time lags the detection by the
poll-and-post latency (sub-second on a LAN). That is a far cheaper error than
a timeline two machines disagree about.

## The single-instance rule

**One server process owns the media tree. Recording, clip assembly and the
retention sweeper are single-writer — which is why there is no Redis, no
leader election, and no second instance.**

What breaks if a second server starts against the same database and the same
`DATA_DIR`: two recorder supervisors spawn two ffmpeg children per camera,
both writing the same segment filenames; two sweepers race on the same files
(one deletes while the other stats); two uploaders claim the same queue rows
(the claim is atomic, the object writes are not coordinated); two hubs each
hold half the consoles, and an `event.close` published to one never reaches
the other. None of these fail loudly — they fail as duplicated footage and
half-deleted trees.

v1's answer is a rule, enforced by nothing but this document and the shape of
the code, and that is the honest size of it: **run one server.** What v2 must
do before a second instance is possible: media ownership (which server owns
which camera's recorder — a lease, not a flag), a shared hub (the Redis
adapter the plan names and defers), and queue claims that survive the
uploader dying mid-object. Until then, the single process is the design, not
a limitation of it.

## Live view

The console dials the **sidecar directly**, not the server: a tile opens a
WebSocket to `${GO2RTC_PUBLIC_URL}/api/ws`, exchanges an SDP offer/answer
with go2rtc, and hands the remote description to a native
`RTCPeerConnection` (S8-05). There is no server proxy for this, and there
will not be one: WebRTC media (8554/8555) reaches the sidecar from the
browser regardless of who proxies the control socket, so a proxy would add a
hop without adding a boundary.

Three consequences, each learned the hard way and now documented where they
belong:

- **`GO2RTC_PUBLIC_URL` is a browser-facing address.** `http://localhost:1984`
  is right on the host itself and wrong from every other machine on the LAN;
  a remote view needs it to be the address the *browser* can dial (LAN IP,
  reverse-proxied path, or Tailscale address).
- **The sidecar API is a trusted-LAN service (P-14).** Its `/api/ws` cannot
  carry basic auth — a browser cannot attach an `Authorization` header to a
  WebSocket — so the shipped posture is unauthenticated on a trusted network,
  with `GO2RTC_USERNAME`/`PASSWORD` documented as the thing that *breaks*
  live view unless a reverse proxy carries the auth. The server warns at boot
  when the credentials are set.
- **The sidecar's config is boot-time (P-15/P-16).** go2rtc reads its YAML
  once at start; the server's config pushes write the file for the *next*
  boot, and the container's supervisor reboots go2rtc when the file changes.
  A camera save therefore restarts the sidecar, and a live tile makes one
  attempt per mount — reload the page after saving a camera.

HLS (`hls.js`) is the planned fallback for networks where WebRTC ICE fails
(the condition is stated in the plan, not yet needed in practice); v1 ships
WebRTC only, and the tile's failure states (`connecting`/`unreachable`) name
which side failed.

## WebSocket hub

`/ws?token=<access_token>` — the one place a token rides a query string,
because browsers cannot set headers on a WebSocket; the guard is the same
HS256 verification plus an origin check (`WS_ORIGINS`, defaulting to
`WEB_URL`'s origin, so a page from elsewhere cannot read your camera feed).

Seven message types: `event.new`, `event.update`, `event.close`,
`camera.status`, `recorder.status`, `storage.queue`, `presence`. The hub is
in-process — a map of clients, a broadcast channel, no Redis. `event.update`
is the one type that is **throttled per event inside the hub** (one send per
event per window, latest wins): a busy camera can extend an event many times
a second, and the throttle lives where the fan-out lives, because a throttle
per caller would let N callers multiply the traffic it is meant to damp.

Why no Redis adapter: same answer as the single-instance rule — a Redis
pub/sub hub only pays for itself when a second server exists to publish to.
A second instance would need the hub shared *and* media ownership solved;
shipping the adapter first would invite exactly the split-brain the rule
forbids.

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

## Authentication and tenancy

Two token kinds, one rule: **the access token is a capability, the session is
the authority.** HS256 access tokens (30 min — `models.AccessTokenTTL`) carry
the account id and role snapshot; refresh tokens (14 days —
`RefreshTokenTTL`) are rows. Rotation is strict:
presenting a refresh token closes that row and mints its replacement naming
the old one in `rotated_from`; presenting a *spent* one is treated as theft —
every live session of that account is dropped and the response says how many
(`live_sessions_dropped`). A stolen token replaying is worse than an
inconvenience, so the account gets logged out everywhere.

The **break-glass admin** exists so OIDC misconfiguration cannot lock you out
of your own NVR: `BOOTSTRAP_ADMIN_USER`/`PASSWORD` seed one local admin on
first boot, its password door is never closed by a link or a role edit, and
deleting the last admin is refused.

SSO is authorization-code + PKCE + nonce, hand-rolled against discovery
(S2-04): the row carries what the IdP *said* (endpoints, algorithms, PKCE
support) and the login is built from that, not from guesses. A new SSO user
arrives at the **`viewer` JIT floor** — they watch, they do not change — and
role mapping (`group=role`) replaces roles on every login, so an IdP that
drops someone out of a group takes the role at the next sign-in. Mapping to
`viewer` is refused (422): the mapping replaces, the floor is the only thing
an identity arrives with, and a mapping that owns the floor locks out the
people who cannot ask for it back.

The role matrix is one table, enforced by middleware, audited on denial:

| | read (events, segments, live) | write (cameras, zones, storage, webhooks) | admin (users, roles, identities, audit, OIDC) |
|---|---|---|---|
| `viewer` | ✅ | ❌ | ❌ |
| `operator` | ✅ | ✅ | ❌ |
| `admin` | ✅ | ✅ | ✅ |

Tenancy is single-tenant: one installation, one family or one site. There is
no per-user media ACL — a `viewer` sees every camera's events. That is a
decision (the threat model is a shared household, not a hosting provider),
and it is the kind of decision that must not be discovered by a user who
wanted isolation.

## Detection

YOLOv8n ONNX on `CPUExecutionProvider` is the **verified baseline** — every
detection claim in this repo was observed on CPU, on the dev box, at the
sizing below. The pipeline: letterbox to 640×640 (aspect preserved, gray
padded), run the model, decode the `[1,84,8400]` output (80 COCO classes ×
8400 anchor-free boxes, confidence-gated), non-maximum suppression at
**IoU 0.45** keeping at most 100 boxes, then the tracker.

The tracker is deliberately not a Kalman filter: a greedy IoU matcher
(**≥ 0.30** between consecutive frames) with **six frames of grace** — at the
tap's 5 FPS, a person occluded by a van for a second resumes with the same
id, and someone who strolls back ten seconds later is a new track. That is
the whole identity model, and it is enough because the consumer is zone
rules, not forensics.

The CPU motion gate runs before the model on `continuous` cameras: downscale
to 160 px grayscale, absdiff against the previous frame, and if under **1%**
of pixels changed past threshold 17, the frame skips inference entirely. On a
quiet camera this is most frames, and it is what makes 4–8 cameras fit in the
sizing below. (`events`-mode cameras skip the gate — they only run while
something is already happening.)

**The sizing claim, stated as what it is:** 4–8 cameras at 5 FPS on ~4 vCPU,
measured on the dev box with the demo camera and the e2e stack — the CPU
detector sustains the tap with headroom, and the motion gate is what keeps
the idle cost low. It is a dev-box measurement, not a benchmark suite; a
deployment that outgrows it moves the worker to another host (it is only an
API client) or to the CUDA overlay below.

## Inference host: CPU default, CUDA overlay

**CPU ONNX is the default and the baseline every install gets; CUDA is a
compose overlay (`docker-compose.gpu.yml`) that is shipped and verified — on
a host with the NVIDIA container toolkit, which the dev box now is.**

The split is deliberate. The CPU image is what `docker compose up` gives
you: no driver story, no 2 GB of CUDA wheels, and a sizing claim measured on
hardware (the Detection section). The GPU image is the same worker, same
model, same entry point — stage 3 of `docker/Dockerfile.detector` swaps
`onnxruntime` for `onnxruntime-gpu` and adds the five CUDA libraries the
provider dlopens (`ldd` names them; missing one degrades the session to CPU
with a warning, which is exactly why the worker logs the **active** providers
after substitution, not the requested ones). The overlay adds the device
reservation and `DET_PROVIDERS=CUDAExecutionProvider,CPUExecutionProvider` —
CPU staying in the list so a worker that cannot open CUDA falls back and
warns instead of dying.

What was **observed** on this box (RTX 4070 Laptop, driver 615.78.08,
nvidia-container-toolkit 1.20.1, 2026-11-07): the worker recreated under the
overlay loads yolov8n.onnx with `CUDAExecutionProvider` active, holds 288 MiB
of GPU memory, and the container's CPU fell from ~3.4 cores to under 5% with
events still arriving seconds after boot. `scripts/gpu-probe.sh --cuda` is
the check that says whether a host can run this at all — it opens every
device node and runs a real CUDA compute, because NVML passing while CUDA
fails is this box's own documented history (`docs/gpu-on-lxc.md`).

The one claim that remains **not** verified: performance at scale. The GPU
path is proven to work, not proven to be needed — the CPU baseline's
4–8-camera sizing stands, and the overlay exists for the site that outgrows
it without buying another host.

## Retention

The sweeper runs **seven sweeps** every 15 minutes (`DefaultInterval`): local
segments past `LOCAL_RETENTION_DAYS` (14), event-window segments, event rows
past `EVENT_RETENTION_DAYS` (90) with their media, remote objects past
`REMOTE_RETENTION_DAYS` (30 — and **`0` means keep forever**, a real value,
not an absent one), then the three prunes: webhook delivery history, audit
rows, and terminal job rows.

The rule that makes the whole thing safe to run unattended: **a storage
outage never destroys the only copy — inside a stated window.** A local file
whose upload has not landed is protected for `max(3 × retention, 7 days)`
(`unuploadedMultiplier`, `unuploadedFloorDays`): a month-long S3 outage costs
disk, never footage, and the console shows the growing queue. Past the
protection window the sweep **does** force-delete, with a WARN naming every
byte — the alternative is a server that fills its disk and stops recording
everything to protect footage nobody has retrieved in three times the
retention period. Both halves of that sentence are load-bearing; the
protection is a window, not a promise of forever.

## Technology Choices

| Choice | Reason | Rejected alternative |
|---|---|---|
| gin + gorm | boring, hireable, complete; the router and the ORM are not where an NVR wins or loses | chi+sqlc: more control, more code, no v1 need it answers |
| hand-rolled OIDC (discovery + PKCE + nonce over `net/http`) | the OIDC libraries carry a decade of compatibility baggage; the flow v1 needs is one authorization code exchange, and the row that records what the IdP published is ours to shape | `go-oidc`: hides the discovery document we need to *show* in the console; `oauth2` proxy products: a second authn system, not a feature |
| go2rtc sidecar | camera protocols are someone else's full-time job: RTSP dialects, WebRTC ICE, tap transcoding — one binary, one config the server renders | ffmpeg-per-viewer fan-out: no WebRTC, no tap, no browser story |
| FFmpeg for segments and clips | `-segment_time` + CSV manifest is a recording API that predates every NVR; concat `-c copy` is instant and lossless | custom muxer: months of work to re-derive what ffmpeg already streams |
| ONNX Runtime, CPU default | runs everywhere Docker runs, no driver story, yolov8n fits the sizing | TensorRT/OpenVINO: per-hardware builds for a speed nobody measured a need for yet |
| sqlite for tests, Postgres for deployment | the whole suite runs with zero services; deployment gets the real engine; the fallback is logged, never silent | Postgres-only tests: every unit test needs a container |
| no Redis | one process owns the media tree; a pub/sub bus only pays for a second server that v1 forbids | Redis-backed hub + queue: the single-instance rule made moot before it is made true |

### Key Design Decisions

Each of these is a fork in the road with the road-not-taken named, because a
maintainer revisiting one deserves the original argument:

- **The slug *is* the go2rtc stream name.** One namespace, no mapping table;
  the cost is that renaming a camera moves every key it ever wrote, which is
  why the console shows the slug but refuses to edit it.
- **Camera writes re-render go2rtc's whole config** (S3-05), rather than
  calling the per-stream API: one render, golden-file tested, is a source of
  truth; incremental edits drift. The restart supervisor (P-16) pays the
  boot-time tax that this choice inherits.
- **Zones are normalized 0–1 coordinates** (S3-04): a zone survives a tap
  resolution change; pixel zones would silently move when someone edits
  `GO2RTC_MJPEG_HEIGHT`.
- **Secrets are sealed at rest and never echoed** (`secret_set: true` is the
  only read-back): the console cannot be tricked into re-displaying a
  credential it never received.
- **Registration is `403`** (S2-06): an NVR on a LAN with open signup is a
  camera peephole for the whole network; accounts are created by admins or
  arrive through OIDC at the `viewer` floor.
- **MIT, with detection assets downloaded at build time, never committed**:
  the weights' license and the code's license stay separate, and `git clone`
  stays small.

## Scalability Considerations

### Current limits

4–8 cameras at 5 FPS on ~4 vCPU (the sizing above, with its stated basis);
one server process, one media tree, one hub; the rate limiter's per-user
budget (`RATE_LIMIT_PER_MINUTE`, default 120 — a tenth as burst); the
200-object event cap and 200-row list page; sqlite's single-writer model is
fine at this scale and is not the deployment engine anyway.

### Scaling path

What a 16-camera v2 site changes, in the order the constraints bind:

1. **Media ownership** — a lease per camera so two servers can each run some
   recorders without colliding. Everything else multi-instance depends on
   this (the single-instance rule is the current answer).
2. **Detection worker placement** — already free: the worker is only an API
   client; point `NVR_SERVER_URL` at another box and the tap traffic leaves the
   server's CPU. This is the cheapest scaling win in the design and the only
   one v1 already supports.
3. **HLS fan-out** — past a handful of concurrent viewers, WebRTC per-tile
   stops being the cheap answer; go2rtc already serves HLS, the console
   needs the fallback logic.
4. **Remote seek** — `GetRange` on the storage interface (the Playback
   section states the contract); matters when the tier becomes the primary
   copy rather than the archive.

## Gaps v1 leaves open, stated

The intro promised the gaps would be stated, not discovered. Two, each
where it is decided, collected here so one read is enough:

- **Remote seek.** A clip that lives only on the storage tier streams from
  byte 0 and cannot be scrubbed; local clips seek fully. The contract, the
  reasoning, and what v2 adds: §
  [Playback and the remote seek gap](#playback-and-the-remote-seek-gap).
- **ONVIF events and presets.** v1's detection is the worker analyzing the
  video stream; the camera's own event bus (motion/alarm notifications) and
  PTZ presets are not surfaced at all. A camera that would rather tell the
  NVR than be analyzed still has to be analyzed. This is a v2 item (README
  roadmap), and the honest v1 statement is: nothing to configure, nothing to
  expect.

A third gap that **closed** (S9-06, 2026-11-07): GPU verification. The CUDA
overlay ships and was verified on the dev box with the NVIDIA container
toolkit — § [Inference host](#inference-host-cpu-default-cuda-overlay). What
remains open there is only the performance-at-scale claim: the GPU path is
proven to work, not proven to be needed.

## Test seams

Where the design deliberately leaves a seam so it can be proven without
hardware: the `source_uri` override that lets a `lavfi` testsrc stand in for
an RTSP camera (S4-07), the demo-only `demo-doorbell` source rule in
`go2rtc/config.go` (S10-02) — an `exec:` ffmpeg feeding `bus.jpg` as a real
stream, so the whole pipeline runs with no camera on the network — the
in-repo test IdP (S2-05), the fixture manifest (S10-01), and the internal
API's `WORKER_TOKEN` gate that lets the suite speak the worker's contract
without a Python process. Each seam is production code path, not a test
double: the demo camera records, detects, events and uploads exactly like a
real one, which is what makes verify.sh an acceptance gate rather than a
smoke test.

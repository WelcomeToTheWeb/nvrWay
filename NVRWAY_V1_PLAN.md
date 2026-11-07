# nvrWay v1 — self-hosted NVR with AI detection, OIDC login, remote storage

## Context

`/root/projects/nvrWay` is empty. Build v1 of a self-hosted NVR: RTSP camera ingest, **continuous and event-triggered** recording (per camera), YOLO AI detection with per-camera polygon zone rules, OIDC login with a break-glass local admin, a web console (live view, event timeline, camera setup, settings), and pluggable remote storage (S3, WebDAV, SFTP, local/NFS) with an async upload tier and retention sweeps. Target scale 4–8 cameras.

The design copies the conventions already proven in `/root/projects/OurWay` (module layout, gin/gorm store, hand-rolled OIDC in `sso/` with an in-repo test IdP, env config, multi-stage Dockerfiles, compose dev/prod pair, React 18 + Vite + Tailwind + vitest + Playwright console, MIT license, `scripts/verify.sh` + `scripts/coverage.sh` + `coverage.baseline.json`). Read `/root/projects/OurWay/server/main.go` (wiring order), `server/store/store.go:14-89` (store/migrate), `server/sso/oauth.go` (OIDC contract), `server/api/handlers.go:155-300` (route/middleware shape), `docker/Dockerfile.server`, `docker-compose.prod.yml`, `web/vite.config.ts`, `web/src/auth/context.tsx` before starting.

Environment facts (verified this session — they shape every verification step):
- Host has **no** `go`, **no** `ffmpeg`, **no** `pip`/`ensurepip` (system Python 3.14 cannot host the ML worker). Docker 29.9.2 with `runc` only, no `nvidia-container-toolkit`, **no `/dev/nvidia*` nodes** → GPU inference is config-only in v1; the CPU ONNX path is the verified default.
- Local images available offline: `golang:1.26`, `golang:1.26-alpine`, `postgres:16-alpine`, `redis:7-alpine`, `alpine:latest`, `nginx:1.27-alpine`. Node 22 + npm 9 exist on host for the console build.
- Network reachable: `proxy.golang.org`, `pypi.org`, `ghcr.io` (token endpoint 200), `github.com`. `hub.docker.com` pulls for `alexxit/go2rtc` work; the `ghcr.io/alexxit/go2rtc:master` tag is equivalent (its image ships FFmpeg).

## Locked decisions

| # | Decision | Value |
|---|---|---|
| 1 | Stack | Go control plane + Python detection worker + go2rtc sidecar (`ghcr.io/alexxit/go2rtc:master`) |
| 2 | Detection | YOLOv8n ONNX (COCO classes) + polygon zone rules `intrusion`/`loiter`; faces/LPR out of scope for v1 |
| 3 | Recording | Per-camera `record_mode`: `continuous` (always-on) / `events` (detect-triggered) / `off` |
| 4 | Storage | Drivers `s3` / `webdav` / `sftp` / `fs` (local dir incl. NFS mount); local-first, async upload, playback proxied by the server |
| 5 | Auth | OIDC authorization-code + PKCE + nonce (own implementation, like OurWay) + break-glass local admin; no SAML |
| 6 | IdP | No bundled IdP. Tests use an in-repo Go test IdP (`server/sso/testidp`, copy of the OurWay pattern) |
| 7 | Alerts | WebSocket in-app + per-camera outbound webhook with HMAC; no SMTP in v1 |
| 8 | Scale | 4–8 cameras; single server instance → **no Redis in v1** (WS hub is local, matching OurWay's Redis-off shape) |
| 9 | Inference host | CPU ONNX default (`DET_PROVIDERS=CPUExecutionProvider`); `docker-compose.gpu.yml` overlay shipped and documented, unverified on this box |
| 10 | Live view | go2rtc WebRTC-over-WebSocket (native `RTCPeerConnection`); HLS (`hls.js`) added only as the fallback if WebRTC proves unworkable |
| 11 | License / naming | MIT; Go module `nvr/server`; Python package `nvr_detector`; npm package `nvr-web`; images `ghcr.io/welcometotheweb/nvr/{server,web,detector}` |

## Approach

Build order below is a dependency order, not a calendar: Steps 2 (auth), 3 (cameras + go2rtc sync) and 7 (storage) touch no media pipeline and can run in parallel with Steps 4–6. Each step ships with its own tests (Step 11 lists them per package) and its own observable check; Step 10 is the acceptance gate for the whole system.

### Step 0 — Repo scaffold (no behavior)

Create the tree exactly:

```
nvrWay/
  LICENSE  README.md  DESIGN.md  PRODUCT.md  SECURITY.md  CONTRIBUTING.md
  .gitignore  .env.example  coverage.baseline.json
  docker-compose.prod.yml
  docker/{docker-compose.yml,Dockerfile.server,Dockerfile.web,Dockerfile.detector,go2rtc.yaml.template,nginx.conf}
  server/{main.go,go.mod,config/,models/,store/,auth/,sso/,api/,ws/,media/,go2rtc/,storage/,sweeper/}
  detector/{nvr_detector/,pyproject.toml,tests/}
  web/{package.json,vite.config.ts,src/,e2e/}
  testdata/{fixtures.json,segments/.gitkeep}
  scripts/{dev.sh,verify.sh,detect-smoke.sh,e2e-stack.sh,coverage.sh}
  docs/{architecture.md,configuration.md,api.md,installation.md}
  .github/workflows/build.yml
```

- `LICENSE` = MIT, "Copyright (c) 2026 nvrWay contributors" (copy of `OurWay/LICENSE` text).
- `README.md` H2 skeleton = `## Tech Stack`, `## Architecture`, `## Project Structure`, `## Quick Start`, `## Roadmap`, `## Key Design Decisions`, `## License`. `docs/configuration.md` = env-var table below, verbatim. Other docs: copy heading skeletons from OurWay's `SECURITY.md` / `CONTRIBUTING.md`.
- `.gitignore`: copy OurWay's, drop agent/installer-specific entries; add `data/`, `models/`, `web/test-results/`.
- `coverage.baseline.json`: `{"server": 0.0, "detector": 0.0, "web": 0.0}` and `scripts/coverage.sh --record` at the end of Step 9.
- `server/go.mod`: `module nvr/server`, `go 1.26`, requires (versions exactly as in `OurWay/server/go.mod`): `github.com/gin-gonic/gin v1.10.0`, `github.com/golang-jwt/jwt/v5 v5.2.1`, `github.com/google/uuid v1.6.0`, `golang.org/x/crypto v0.26.0`, `gorm.io/gorm v1.31.2`, `gorm.io/driver/postgres v1.6.3`, `gorm.io/driver/sqlite v1.6.0`, `nhooyr.io/websocket v1.8.10`, `gopkg.in/yaml.v3 v3.0.1`, plus `github.com/minio/minio-go/v7 v7.4.1`, `github.com/studio-b12/gowebdav v0.13.0`, `github.com/pkg/sftp v1.13.12`, `github.com/johannesboyne/gofakes3 v0.0.0-20240531170030-bc99e901ec87` (S3 emulator for tests; implementer runs `go get github.com/johannesboyne/gofakes3` to resolve the current pseudo-version and records it in `go.sum`).
  - **Recorded pin bump (P-02, do not "fix" back)** — `server/go.mod` as shipped carries `gin v1.12.0`, `golang.org/x/crypto v0.57.0`, `gofakes3 v1.2.0` instead of the three values above. The plan's rule is a bump recorded, not a constraint loosened, so here is the record: the table above is a snapshot of OurWay's `go.mod`, a different codebase on an older toolchain; nvrWay builds and tests on `go 1.26`, and the baseline commit `00e3c8e` cut the dependency set at the versions that resolve and pass there — gin 1.12.0 and x/crypto 0.57.0 are the current lines of their respective modules (x/crypto 0.26.0 predates the 2024–2025 SSH/zip fixes), and gofakes3 v1.2.0 is exactly what the parenthetical above instructed the implementer to resolve (the module's newest tag — `go list -m -versions` answers v1.0.0 / v1.1.0 / v1.2.0, no later release). Every `require` stays exactly pinned, no `replace`, no `latest`: `go mod verify` clean, `grep -c '=>' server/go.mod` = 0, suite green on golang:1.26 (S11-01).

### Step 1 — config, models, store, migrations

`server/config/config.go` mirrors `OurWay/server/config/config.go` (struct + `Load()` + `getEnv`/`getEnvInt`, `Version`/`GitCommit` stamped by ldflags). Env vars and defaults:

| Env | Default | Meaning |
|---|---|---|
| `SERVER_PORT` | `":8080"` | listen address |
| `DATABASE_URL` | `postgresql://nvr:nvr@localhost:5432/nvr?sslmode=disable` | gorm DSN; `sqlite://path` switches to sqlite (tests, `scripts/dev.sh`) |
| `JWT_SECRET` | empty → random per boot | HS256 signing key |
| `DATA_DIR` | `/var/lib/nvr` | root of media |
| `SEGMENTS_DIR`/`CLIPS_DIR`/`SNAPSHOTS_DIR` | `$DATA_DIR/{segments,clips,snapshots}` | derived if unset |
| `GO2RTC_URL` | `http://go2rtc:1984` | go2rtc API |
| `GO2RTC_USERNAME`/`GO2RTC_PASSWORD` | empty | basic auth for go2rtc API |
| `GO2RTC_RTSP_URL` | `rtsp://go2rtc:8554` | recorder ingest base |
| `GO2RTC_PUBLIC_URL` | `http://localhost:1984` | browser-facing base for live view |
| `GO2RTC_MJPEG_WIDTH`/`GO2RTC_MJPEG_HEIGHT`/`GO2RTC_MJPEG_FPS` | `640`/`360`/`5` | detection tap transcoding |
| `WORKER_TOKEN` | empty → internal API disabled | bearer for `/api/internal/*` |
| `BOOTSTRAP_ADMIN_USER`/`BOOTSTRAP_ADMIN_PASSWORD` | `admin` / empty | empty password = no local admin created |
| `OIDC_ISSUER`,`OIDC_CLIENT_ID`,`OIDC_CLIENT_SECRET` | empty | seeds provider row `oidc` on first boot (discovery at boot; failure logs, never fatal) |
| `OIDC_ROLE_CLAIM`, `OIDC_ROLE_MAPPING` | `groups`, `""` | claim name + `group=role,group=role` |
| `SEGMENT_SECONDS` | `5` | segment length |
| `PRE_ROLL_SECONDS`/`POST_ROLL_SECONDS` | `8`/`15` | event clip window |
| `EVENT_MAX_SECONDS` | `300` | forced event close |
| `LOCAL_RETENTION_DAYS` | `14` | global default retention |
| `REMOTE_RETENTION_DAYS` | `30` | 0 = keep remote forever |
| `EVENT_RETENTION_DAYS` | `90` | event rows + media age-out |
| `UPLOADER_TICK_SECONDS` | `30` | upload queue tick |
| `RATE_LIMIT_PER_MINUTE`/`RATE_LIMIT_BURST` | `120`/`0` | per-user REST budget (burst 0 = a tenth) |
| `WS_ORIGINS`, `WEB_URL` | `""`, `http://localhost:3000` | WS origin guard, console origin |
| `DET_PROVIDERS` | `CPUExecutionProvider` | forwarded in the detector config payload |
| `TZ` | `UTC` | — |

`server/models/`: `Camera`, `Event`, `Segment`, `StorageBackend`, `StorageJob`, `Webhook`, `WebhookDelivery`, `User`, `Role`, `UserRole`, `Session`, `SSOProvider`, `PendingIdentity`, `Audit`, `Setting`. Column types follow `OurWay/server/models/*.go` (string UUID PKs via `github.com/google/uuid`, `time.Time` UTC, JSON as `string` columns holding JSON text — reuse the `AlertS`/threshold JSON-text pattern; gorm `serializer:json` for typed fields, `datatypes` NOT introduced).

Load-bearing columns:
- `Camera`: `ID, Slug (uniqueIndex), Name, SourceURI, RecordMode, DetectionEnabled, DetectionFPS, DetectionWidth, DetectionHeight, Sensitivity float64, MinAreaRatio float64, ZonesJSON, LocalRetentionDays, RemoteRetentionDays, Active, CreatedAt, UpdatedAt`.
- `ZonesJSON` shape (the wire contract, exact): `[{"id":"z1","name":"driveway","polygon":[[0.12,0.35],[0.8,0.35],[0.8,0.95],[0.12,0.95]],"classes":["person","car"],"rule":"intrusion","dwell_seconds":30,"severity":3,"min_confidence":0.35,"min_area_ratio":0.004,"enabled":true}]`. Coordinates normalized `0..1` of the detection frame; polygons are closed implicitly, 3–32 vertices.
- `Event`: `ID, EventKey (uniqueIndex), CameraID (index), Action, StartedAt, EndedAt, Open bool, ClassesJSON, ZonesJSON, RulesJSON, ObjectsJSON, Severity int, SignalCount, SnapshotKey, ClipKey, LocalSnapshotPath, LocalClipPath, SnapshotUploadedAt, ClipUploadedAt, ClipSeconds, CreatedAt, UpdatedAt`.
- `Segment`: `ID, CameraID (index), Generation int, Path, StartTS, EndTS, Seconds, Bytes, Kind, UploadedAt, RemoteKey, LocalDeletedAt` with `index idx_seg_cam_gen_start:camera_id,generation,start_ts`.
- `StorageBackend`: `ID, Name (uniqueIndex), Kind, Endpoint, Region, Bucket, Prefix, Username, Password, AccessKey, SecretKey, PrivateKeyPath, InsecureTLS, RootPath, Enabled, IsDefault, CreatedAt, UpdatedAt` — `Password/AccessKey/SecretKey/PrivateKeyPath` are stored AES-256-GCM sealed (see Step 7).
- `StorageJob`: `ID (segment/event UUID), Kind, Target, BackendID, Action, State, Attempts, LastError, NextAttemptAt, RemoteKey, Bytes, CreatedAt, UpdatedAt`.

`server/store/`: `store.go` (struct with per-entity stores + `New(dsn)` + `NewWithDB(db)`, copy of OurWay's `store.go:14-145` with the Postgres→sqlite fallback behavior), `migrate.go` (`AutoMigrate` over every model above; called from `New`), `cameras.go`, `events.go`, `segments.go`, `storage.go` (backends + jobs), `settings.go`, `webhooks.go`, `users.go`, `roles.go`, `sessions.go`, `sso_providers.go`, `audit.go`. Mirror OurWay's per-store method style (`func (s *EventStore) UpsertByEventKey(ctx context.Context, in EventInput) (*models.Event, bool, error)`), file naming identical (`*_test.go` colocated).

`server/main.go` boot order = OurWay's `main.go:28-211`: `config.Load` → `store.New` → `jwtAuth` → `sso` handlers → `go2rtc.NewClient` → `media.NewRecorder` → `media.NewClipBuilder` → `storage.OpenDefault` + `storage.NewUploader` → `ws.NewHub` (local, origins from `WS_ORIGINS`/`WEB_URL`) → `api.NewHandlers` → `sweeper.New` → background `ctx` cancel on SIGINT/SIGTERM → `http.Server` with `Shutdown` → recorder/uploader/sweeper `Close`. Log a boot line naming the recording defaults and local-admin state (the OurWay habit: policy stated where an operator can read it).

### Step 2 — Auth: JWT sessions, break-glass admin, OIDC, WebSocket

Independent of media work; buildable right after Step 1.

`server/auth/jwt.go`: copy OurWay's `auth/jwt.go` API shape — `NewJWTAuth(secret string) *JWTAuth`, `Issue(userID string, roles []string, ttl time.Duration) (access string, err error)`, `Verify(token string) (*Claims, error)`, `IssueRefresh`, `VerifyRefresh`. HS256 only; the `alg` in the token header is never trusted. Access TTL 30 min, refresh 14 d, refresh rotation with reuse detection (`sessions.token_hash`, `rotated_from`).

Break-glass local admin: `server/store/bootstrap.go` seeds roles `admin`, `operator`, `viewer`, and — only when `BOOTSTRAP_ADMIN_PASSWORD` is non-empty and no user holds role `admin` — creates user `BOOTSTRAP_ADMIN_USER` (argon2id hash via `golang.org/x/crypto/argon2`) with role `admin`. Seeding is recorded in `settings` key `bootstrap_admin_seeded_at` so it never re-seeds after an operator deletes the account. Login goes through the same `POST /api/auth/login` handler OurWay uses, behind the same rate limiter.

`server/sso/` = port of `OurWay/server/sso/{discovery.go,jwks.go,oauth.go,contract.go,testidp/testidp.go}` with SAML removed (`saml.go`, `saml_test.go`, `capture_vendor_saml_test.go`, `testsaml/` are NOT copied) and provider kind fixed to `oidc`. Keep the exact named failure strings — they are the console's states: `DiscoveryOK="ok"`, `DiscoveryUnreachable="discovery_unreachable"`, `DiscoveryBadDocument="discovery_document"`, `DiscoveryIssuerMismatch="issuer_mismatch"`, `DiscoveryNoCodeFlow="no_code_flow"`, and `oauth.go`'s five `IdentityError` reasons (subject missing, email unverified, allow-list refusal, `id_token`/userinfo subject disagreement, provider disabled). Keep: PKCE S256 (`NewCodeVerifier`, `CodeChallenge`), `nonce` binding, `HandleCallback` ordering (verify signed `id_token` → require userinfo/`id_token` agreement → contract gate → resolve account), JIT provisioning at the `viewer` floor, `link_policy="pending"` default (an asserted email matching an existing local account parks a `pending_identities` row and refuses the login; `link` is explicit and audited), role mapping that owns mapped roles, `uniqueUsername` collision handling, JWKS refresh-on-unknown-`kid` exactly once.

Routes (public group): `GET /api/auth/status` (answers `{local_login: bool, oidc: [{provider, name, issuer}]}` so the login screen draws its own buttons), `POST /api/auth/login`, `POST /api/auth/refresh`, `POST /api/auth/logout`, `GET /api/auth/sso/:provider/authorize` (redirect to IdP; writes a short-lived `state` row carrying `nonce` + `code_verifier`), `GET /api/auth/sso/:provider/callback` (redirects to `${WEB_URL}/auth/callback#access_token=…&refresh_token=…`, OurWay's SPA handoff), `POST /api/auth/sso/exchange` (bearer → profile + roles), `GET /api/auth/profile`. Protected: `PUT /api/auth/password`, `GET/POST/PUT/DELETE /api/users`, `GET/POST /api/users/:id/roles` (admin). Public registration is off in v1: the register handler answers `403 {"error":"registration is admin-only"}`.

Middleware: copy `OurWay/server/api/middleware.go` behavior — `AuthMiddleware`, `RequireRole`, `RequireAnyRole`, audit on denial; add `InternalAuthMiddleware(token string)` for `/api/internal/*` (constant-time compare against `WORKER_TOKEN`; empty token → every internal route answers `403 {"error":"internal api disabled"}`).

`server/ws/hub.go`: local `ws.Hub` from OurWay (`nhooyr.io/websocket`, `?token=` query auth, origin check from `WS_ORIGINS`/`WEB_URL`, no Redis). Envelope `{"type": string, "payload": object, "ts": RFC3339}`; exact types: `event.new`, `event.update`, `event.close`, `camera.status` (`{camera_id, slug, online, producers}`), `recorder.status` (`{camera_id, mode, segment_seconds, last_segment_at}`), `storage.queue` (`{pending, uploading, failed, last_error}`), `presence`. Every mutation handler that changes a row an operator sees broadcasts exactly one of these.

### Step 3 — Cameras + go2rtc config sync + live-view plumbing

`server/go2rtc/client.go` (new; no equivalent exists in OurWay):
- `type Client struct{ URL, Username, Password string; HTTP *http.Client }`, `NewClient(cfg *config.Config) *Client`.
- `Streams(ctx) (map[string]StreamInfo, error)` → `GET /api/streams`; `StreamInfo{Producers, Consumers []string}` (go2rtc answers `{name:{producers:[…],consumers:[…]}}`).
- `PutStream(ctx, name, src string) error` → `PUT /api/streams?src=…&name=…`; `DeleteStream(ctx, name)` → `DELETE /api/streams?name=…`.
- `ApplyConfig(ctx, yml []byte) error` → `POST /api/config` with the whole YAML body (go2rtc rewrites its config file and reloads streams); confirm via `Streams` within 5 s.
- `Frame(ctx, src string, width int, cache string) ([]byte, error)` → `GET /api/frame.jpeg?src=…&width=…&cache=…`.
- `WebRTCOffer(ctx, src, offer string) (string, error)` → `POST /api/webrtc?src=…` (SDP text in, SDP answer out) for server-side probing only; browsers talk to go2rtc's `/api/ws` directly (Step 8), so v1 needs no server relay.
- Basic auth from `GO2RTC_USERNAME`/`GO2RTC_PASSWORD`; 3 retries with 500 ms backoff; typed errors `ErrGo2rtcUnreachable`, `ErrStreamRejected` quoting the go2rtc body.

`server/go2rtc/config.go` renders the full `go2rtc.yaml` from camera rows (pure function, golden-file tested):

```yaml
api: { listen: ":1984", origin: "*" }
rtsp: { listen: ":8554" }
webrtc: { listen: ":8555" }
streams:
  <slug>: ["<source_uri>"]
  <slug>_det: ["ffmpeg:<slug>#video=mjpeg#width=640#height=360#raw=-r 5"]
```

`<slug>_det` is emitted only when `DetectionEnabled`; width/height/fps come from the camera row, falling back to `GO2RTC_MJPEG_*`. Slug is validated `^[a-z0-9][a-z0-9-]{1,31}$`, unique, and is the go2rtc stream name — no mapping table. Camera CRUD calls `ApplyConfig` after every write (one write path, so config cannot drift from rows); `main.go` applies it at boot after a `Streams` probe.

**Amendment, 2026-11-02 (S3-05, S3-07, measured on go2rtc 1.9.15):** "go2rtc rewrites its config file and reloads streams" is half true. `POST /api/config` writes the file and validates it, and **does not instantiate streams** — streams are created at startup from that file. A stream added by a config push is in the file, and absent from `GET /api/streams`, until the sidecar boots from it; `PUT/POST /api/streams` write the file without starting a stream, no endpoint starts one at runtime, and the sidecar does not watch the file. A **config push is durable, not live**. Making a camera live is therefore two steps: `ApplyConfig`, then recreate the sidecar (`server/sidecar`: `docker compose -f <file> --no-deps --force-recreate go2rtc`, health-checked against `GET /api/streams`, configured by `GO2RTC_COMPOSE_*`, and a stated no-op when no compose project is configured). Boot applies, then recreates **only** when the live map is missing configured cameras. Step 9's server volume list (`nvr_data` only) gains two opt-in mounts for exactly this — the Docker socket and the compose file, read-only — the manager's decision of 2026-11-02, scoped to one command and one service. Verified end to end: after a config write the live map was empty; after the recreate, `porch`, `porch_det` and `garden` were live in 3.6 s.

Camera API (`server/api/cameras.go`): `GET /api/v1/cameras`, `GET /api/v1/cameras/:id`, `POST /api/v1/cameras` (operator/admin), `PUT /api/v1/cameras/:id`, `DELETE /api/v1/cameras/:id` (admin; rows + local media + remote purge jobs), `POST /api/v1/cameras/:id/probe` (answers `{online, producers}` from `Streams`), `GET /api/v1/cameras/:id/snapshot` (proxies `Frame` with `cache=5s&width=640`, `Cache-Control: private, max-age=5`). `PUT /api/v1/cameras/:id/zones` validates polygons (3–32 vertices, all coordinates in `0..1`) and answers `422 {"error":"zone polygon invalid"}`; every zone write is an audit row.

Role floor: `viewer` reads cameras/events/clips; `operator` adds camera CRUD + probe; `admin` adds users, storage backends, webhooks, OIDC provider CRUD (`GET/POST/PUT/DELETE /api/settings/oidc`).

### Step 4 — Recorder: continuous + event-gated segments

`server/media/recorder.go` (new; OurWay's `recordings/writer.go` is a frame-sequence writer for screenshots, not video — pattern of process ownership is copied, the code is not):
- `type Recorder struct{ cfg *config.Config; st *store.Store; hub *ws.Hub }`, `NewRecorder`, `Run(ctx)`, `Close()` (closes open ffmpeg processes on shutdown, so a restart resumes recording — OurWay's `main.go:40-53` rule).
- One supervisor goroutine per camera, re-created on camera row change (`hub`-free: a 10 s config poll on `cameras.updated_at`). `record_mode=continuous` → always recording; `events` → recording starts on the first event signal for that camera (`Recorder.OpenWindow(cameraID)`) and stops `POST_ROLL_SECONDS` after the window closes; `off` → never spawns.
- Exact ingest command (`exec.CommandContext`):
  `ffmpeg -hide_banner -loglevel error -rtsp_transport tcp -i <GO2RTC_RTSP_URL>/<slug> -c copy -f segment -segment_type mp4 -segment_time <SEGMENT_SECONDS> -reset_timestamps 1 -segment_start_number <n> -segment_list <SEGMENTS_DIR>/<slug>.csv -segment_list_type csv -segment_list_entry_prefix <slug>- <SEGMENTS_DIR>/<slug>-<n>.mp4`
  with `Generation` = a per-camera counter incremented at each spawn, persisted in `settings` key `recorder_gen_<camera_id>`; filenames `<slug>-<generation>-<n>.mp4`.
- Segment discovery: tail the CSV (the list ffmpeg writes as `filename,start_time,end_time`), parse appended lines, insert `segments` rows with `StartTS = procStart + start_time`, `EndTS = procStart + end_time`, `Seconds = end-start`, `Bytes` from `os.Stat`, `Kind = continuous`. Absolute times come from the supervisor's own start time — no camera clock, no filename clock parsing.
- Crash handling: ffmpeg exit → log + `camera.status` offline + restart after 10 s (max backoff 60 s); CSV offset resets on restart.
- `GET /api/v1/segments?camera_id=&from=&to=` (viewer) answers the manifest for the timeline scrubber.
- `recorder.status` broadcasts every completed segment tick (one per segment, not per frame).

### Step 5 — Detection worker (Python, `detector/`)

`detector/pyproject.toml`: `requires-python = ">=3.11,<3.13"` (the `onnxruntime` cp312 manylinux wheels exist; host Python 3.14 has no pip and is never used), pinned deps: `onnxruntime==1.31.1`, `opencv-python-headless==5.0.0.93` (cp37-abi3 manylinux wheels — imports fine on 3.12), `numpy>=2.1,<3`, `requests>=2.32`, `pydantic>=2.10`. Dev extras: `pytest>=8.3`.

Modules in `detector/nvr_detector/`:
- `mjpeg.py` — `class MjpegStream(url: str, timeout: float = 10.0)` iterator over JPEG bytes: `requests` streaming GET on `http://go2rtc:1984/api/stream.mjpeg?src=<slug>_det`, split on the multipart boundary (`--boundary` line parsed from the `Content-Type` header), decode each part with `cv2.imdecode(np.frombuffer(buf, np.uint8), cv2.IMREAD_COLOR)`. Reconnect on socket close with 5 s backoff. No `cv2.VideoCapture` HTTP backend dependency.
- `model.py` — `class YoloOnnx(path: str, providers: list[str], imgsz: int = 640)`: `InferenceSession(path, providers=providers)`; `preprocess(frame)` = letterbox to `imgsz×imgsz` (pad `114`), BGR→RGB, `/255.0`, CHW, batch dim, `float32`; `postprocess(out, frame_wh)` handles the YOLOv8 output tensor `[1, 84, 8400]` → transpose → `xywh` → corners → clip to frame → class = `argmax(rows[4:])` → filters: `conf >= min_confidence`, box area ratio `>= min_area_ratio` → NMS (numpy, IoU `0.45`, max 100 boxes). Returns `list[Detection{cls:int, conf:float, x1,y1,x2,y2 float (pixels)}]`. Class ids are COCO order; `CLASS_NAMES` is the 80-name COCO list verbatim, and the detection-eligible set is `person, bicycle, car, motorcycle, bus, truck, dog, cat, bear` (the rest are dropped: `IGNORED_CLASSES` constant).
- `tracker.py` — greedy IoU tracker: match detections to live tracks by IoU `>= 0.30` (best-first), unmatched detections spawn tracks, tracks expire after 6 frames, ids monotonic per camera.
- `zones.py` — `point_in_polygon(x, y, poly)` (ray cast, `float`), `centroid(det) = ((x1+x2)/2, y2)` normalized by frame size (bottom-center: a person's feet, not their torso), `evaluate(camera, tracks, frame_ts)` → per zone: `intrusion` fires when a matched class centroid is inside the polygon and `conf >= zone.min_confidence`; `loiter` fires when the same `track_id` stays inside continuously for `>= zone.dwell_seconds` (exit resets the timer).
- `events.py` — event state machine per camera+zone: `open` (first qualifying hit, emits a snapshot JPEG, `cv2.imencode('.jpg', frame, [int(IMWRITE_JPEG_QUALITY), 82])` of the frame containing the triggering box, scaled to width 640, capped 200 KB) → `extend` (hit within `10 s` of last hit, appends objects) → `close` (no hit for 10 s, or age `>= EVENT_MAX_SECONDS`). `event_key = f"{camera_id}:{zone_id}:{rule}:{open_unix}"`.
- `motion.py` — gate: downscale to width 160 grayscale, `cv2.absdiff` vs previous, threshold 17, `countNonZero / pixel_area < 0.010` → skip inference this frame (continuous-mode CPU saver). `events`-mode cameras never gate (a gate would erase the trigger).
- `client.py` — `ServerClient(base_url, token)`: `GET /api/internal/v1/detector/config` every 5 s; `POST /api/internal/v1/signals`; 3 retries + exponential backoff `min(5 * 2^n, 60) s`; signals buffer flushed every 2 s, max 40 per POST, dropped-oldest on persistent failure with a counted `signals_dropped` log line.
- `pipeline.py` — per camera thread: frame (respecting `DetectionFPS`) → motion gate → `model.run` → `tracker.update` → `zones.evaluate` → `events.transition` → signal buffer; snapshot sent only on `open` (base64 in the signal).
- `__main__.py` — env: `NVR_SERVER_URL` (default `http://server:8080`), `NVR_WORKER_TOKEN`, `DET_PROVIDERS` (`CPUExecutionProvider`, comma list), `DET_MODEL_PATH` (default `/models/yolov8n.onnx`), `DET_HTTP_TIMEOUT=10`, `DET_MAX_CAMERAS=12`. Exit 0 on SIGTERM after flushing signals.

Internal API (server side, `server/api/detector_api.go`):
- `GET /api/internal/v1/detector/config` → `{"cameras":[{"id","slug","name","det_stream":"<slug>_det","fps","width","height","sensitivity","min_area_ratio","zones":[…],"record_mode","providers":[…]}],"settings":{"event_max_seconds":300}}`; only `DetectionEnabled && Active` cameras; `providers` = `strings.Split(DET_PROVIDERS, ",")`.
- `POST /api/internal/v1/signals` body `{"signals":[…]}` where a signal is `{"camera_id","event_key","action":"open|extend|close","rule":"intrusion|loiter","zone_id","zone_name","severity":1|2|3,"classes":["person"],"ts":"RFC3339","objects":[{"class","conf","box":[x1,y1,x2,y2]}],"snapshot_base64":"…"|"","record_mode":"continuous|events"}`. Answers `200 {"accepted":n,"events":[{"event_key","id","action"}]}`; unknown `camera_id` → that signal is skipped with `"error":"unknown camera"`; a signal whose `camera_id` matches a camera with detection disabled is skipped with `"error":"detection disabled"`; `snapshot_base64` over 250 KB is stored as absent (the event survives).
- `POST /api/internal/v1/cameras/:id/window` is not needed: `record_mode=events` windows open from the first signal (the server calls `Recorder.OpenWindow` on signal ingest).

Model provenance (build-time, deterministic): `docker/Dockerfile.detector` stage 1 installs `ultralytics==8.4.195` + `onnx==1.17.0`, downloads `https://github.com/ultralytics/assets/releases/download/v8.3.0/yolov8n.pt` (verified 200, 6 549 796 bytes this session), exports `YOLO('yolov8n.pt').export(format='onnx', imgsz=640, opset=17, dynamic=False)`, and copies `/models/yolov8n.onnx` into the runtime stage. `testdata/fixtures.json` records `{name, url, bytes, sha256, license}` for `yolov8n.pt` (6 549 796 bytes), `yolov8n.onnx` (exported), and `bus.jpg` (see Verification); the implementer fills the SHA256s on first build and commits them. `yolov8n.onnx` is never committed to git.


### Step 6 — Events, clip assembly, snapshots, webhooks

`server/media/clips.go`: on `action=close` (and at event-row write time for the open case) the server schedules clip assembly `POST_ROLL_SECONDS` after `EndedAt` (a `time.AfterFunc` per open event, cancelled on close):
- Window = `[StartedAt - PRE_ROLL_SECONDS, EndedAt + POST_ROLL_SECONDS]`; segments selected from the `segments` table (overlapping, any `Kind`) ordered by `StartTS`.
- `ffmpeg -y -f concat -safe 0 -i <list> -c copy -movflags +faststart <CLIPS_DIR>/<event_id>.mp4` where `<list>` is `file '<abs path>'` per segment (temp list deleted after).
- No segments overlap the window → `events.clip_key=""`, `ClipSeconds=0`, log `"clip skipped: no segments for window"`, event stays visible with its snapshot. A camera in `record_mode=off` therefore yields snapshot-only events — intended, stated in `docs/architecture.md`.
- `ClipSeconds` = `min(window, sum(segment seconds))`; a clip whose measured duration is under 2 s is padded by re-running concat with the last segment repeated once (keeps the player honest about tiny events).
- Snapshot write: `open` signal decodes `snapshot_base64` to `<SNAPSHOTS_DIR>/<event_id>.jpg`.

`server/store/events.go` upsert semantics (`UpsertByEventKey`): `open` inserts (`StartedAt = EndedAt = ts`, `Open = true`); `extend` advances `EndedAt`, appends `objects` capped at 200 (`ObjectsJSON` trimmed oldest-first), unions `classes`/`zones`/`rules`, takes `max(severity)`; `close` sets `Open = false` and final `EndedAt`. `ts` is the **server** clock at receipt (`time.Now().UTC()`) — the worker's clock never reaches the DB, so host clock skew cannot create events in the future. Late-arriving `open` after close (`EndedAt < ts` and `!Open`) answers `200` with `"error":"event closed"` and changes nothing.

Event API: `GET /api/v1/events?camera_id=&from=&to=&class=&zone_id=&severity=&q=&limit=&offset=` (`limit` default 50 max 200; `q` matches class/zone names; `viewer`), `GET /api/v1/events/:id`, `DELETE /api/v1/events/:id` (admin, audit row + storage purge jobs), `GET /api/v1/events/:id/snapshot`, `GET /api/v1/events/:id/clip` (viewer).
- Clip delivery is server-proxied in v1: local file → `http.ServeFile` (Range/seek support for the timeline scrubber); no local file + `ClipUploadedAt != nil` → open the backend `Get`, copy with `Content-Type: video/mp4`, `Accept-Ranges: bytes` and `Cache-Control: private, max-age=3600`, streaming from byte 0 (seek-in-remote is a documented v1 gap in `docs/architecture.md`, not a hidden limit).
- Clip purge: `DELETE /api/v1/events/:id` enqueues delete jobs for both keys; the UI shows `{"error":"clip not available"}` (404) for events whose media is gone.

Webhooks (`server/webhooks/webhooks.go`): on `close`, for every enabled `webhook` row, POST the event payload `{"id","camera_id","camera_slug","started_at","ended_at","seconds","classes":[…],"zones":[…],"rules":[…],"severity","snapshot_url","clip_url"}` with `X-NVR-Signature: sha256=<hex(HMAC-SHA256(secret, body))>` and `User-Agent: nvr/1`; 3 attempts, 2 s / 10 s / 60 s; every attempt is a `webhook_deliveries` row (`status`, `attempt`, `error`). Route: `GET/POST/PUT/DELETE /api/settings/webhooks` (admin) + `GET /api/settings/webhooks/:id/deliveries`.

WS fan-out on the same events: `event.new` on open, `event.update` on extend (throttled to 1 per 5 s per event), `event.close` on close.

### Step 7 — Storage backends, upload queue, retention

`server/storage/storage.go`:

```go
type ObjectInfo struct{ Key string; Size int64; ModTime time.Time; ContentType string }
type Backend interface {
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Stat(ctx context.Context, key string) (ObjectInfo, error)
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, prefix string) ([]string, error)
	Ping(ctx context.Context) error
}
func Open(ctx context.Context, b *models.StorageBackend) (Backend, error) // kind dispatch
```

- `storage/s3.go` — `minio-go/v7`, endpoint/region/bucket/prefix from the row, `EnableSSL = !InsecureTLS`, `PathStyle` always true (MinIO, R2, B2, Wasabi, self-hosted all work path-style), credentials decrypted at `Open`.
- `storage/webdav.go` — `studio-b12/gowebdav`: `NewClient(endpoint, user, pass)`, `Connect()`, container = first path segment of `Prefix`; `Put` = `c.Writer().WriteCloser`, `Get` = `c.ReadCloser`, `List` = `c.Propfid`-based walk (gowebdav `Propfind(path, 0)`), `Stat` = `c.Stat`.
- `storage/sftp.go` — `golang.org/x/crypto/ssh` + `pkg/sftp`: password or private-key auth (`PrivateKeyPath`), `HostKeyFallback` off (host key verified on first use and pinned in `settings` key `sftp_hostkey_<backend_id>`; a changed pin fails `Ping` with `"error":"sftp host key changed"`), all paths under `RootPath`.
- `storage/fs.go` — `RootPath` directory (a local disk, an NFS/SMB mount, or a compose volume); keys become nested relative paths; writes go to `<key>.part` + `os.Rename` (no partial objects on a crash), `List` via `filepath.WalkDir`.
- Key layout (exact): segments `nvr/<camera_slug>/<YYYY>/<MM>/<DD>/<basename>.mp4`, event clips `nvr/events/<event_id>/clip.mp4`, snapshots `nvr/events/<event_id>/snapshot.jpg` (`<basename>` = the local file name, so a restore lands back in `SEGMENTS_DIR`).

Secret sealing: `storage/seal.go` — `Seal(plain string, key [32]byte) string` / `Unseal(sealed string, key [32]byte) (string, error)`, AES-256-GCM with a random nonce, base64 stored, key = `SHA-256(NVR_SECRET_KEY)` (`NVR_SECRET_KEY` = 64-hex env var, default derived from `JWT_SECRET` so a deployment has exactly one secret to back up). `NVR_SECRET_KEY` documented as required in production; the seal key never leaves the process.

API `server/api/storage.go` (admin): `GET/POST/PUT/DELETE /api/storage/backends`, `POST /api/storage/backends/:id/test` → `{"ok":true}` or `{"ok":false,"error":"discovery_unreachable"|"bad credentials"|"sftp host key changed"|"bucket missing"}` (the `Ping` failure mapped), `PUT …/default` sets `IsDefault`, `POST /api/storage/backends/:id/prefixes?prefix=` lists keys under a prefix (verify path in the UI), `GET /api/storage/queue?state=pending|uploading|failed|uploaded`. Every backend write is an audit row; secrets are never echoed back by any endpoint (`secret_set: true` is the response field).

`server/storage/uploader.go`: tick every `UPLOADER_TICK_SECONDS`; `ClaimNextJob` (`UPDATE … SET state='uploading', attempts=attempts+1, worker_id=? WHERE state='pending' AND next_attempt_at<=now() ORDER BY created_at LIMIT 1` — for sqlite the test-backed path is a `SELECT` then guarded `UPDATE` in a transaction; Postgres uses `FOR UPDATE SKIP LOCKED`); body from the local path → `Put` → mark row `uploaded` + `remote_key` + timestamp, enqueue `delete_local` for the source once its `local_retention_days` expiry passes (local delete is the sweeper's job, never the uploader's); on failure `state='pending'`, `attempts`, `NextAttemptAt = now + min(5min * 2^attempts, 6h)`, `LastError` = the wrapped error; `attempts >= 20` → `state='failed'` (visible on `GET /api/storage/queue?state=failed` and in `storage.queue` WS). `POST /api/storage/backends/:id/requeue?kind=segment|clip|snapshot` requeues `uploaded` rows to that backend (re-shelving across backends). `PUT /api/storage/backends/:id/enabled` with `false` stops new claims; in-flight jobs finish.

`server/sweeper/sweeper.go` — every 15 min: (1) delete `continuous` `segments` older than the camera's `local_retention_days` (local file + row, keeping rows whose `uploaded_at IS NULL` for up to `max(3 * local_retention_days, 7 days)` so an upload outage never destroys un-uploaded footage, then force-deleting with a counted log line); (2) delete `segments` referenced by events older than `EVENT_RETENTION_DAYS`; (3) delete event rows older than `EVENT_RETENTION_DAYS` (media purge first, then rows); (4) delete remote keys older than `REMOTE_RETENTION_DAYS` (`0` = keep forever) via `delete_remote` jobs, marking `remote_key=''` on rows; (5) prune `webhook_deliveries` older than `AuditRetention=30 days`; (6) prune `audit` older than 365 days; (7) prune `storage_jobs` in `uploaded` older than 30 days. Sweep result counts are logged in one line and broadcast as `storage.queue` — the operator sees disk discipline as a number, not a hope.

Retention defaults: continuous segments local 14 d; event clips local 14 d; snapshots local 30 d; remote 30 d (`0` = keep forever); event rows 90 d.


### Step 8 — Web console (React 18 + Vite + Tailwind, OurWay's shape)

Copy `web/{package.json,tsconfig.json,tsconfig.node.json,vite.config.ts,postcss.config.js,tailwind.config.ts,index.html}` from OurWay, keeping deps `react@18.3.1`, `react-dom`, `react-router-dom@6`, `axios@1.7.9`, `zustand@5`, `lucide-react`, `recharts`, `i18next`/`react-i18next` (en only in v1; the i18n wiring stays), `@vitejs/plugin-react`, `vitest`, `@testing-library/react`, `@playwright/test`; drop `react-window` until a list needs it. Package name `nvr-web`, version `0.1.0`. `vite.config.ts` keeps the `/api` + `/ws` proxy with `NVR_PROXY_TARGET` (default `http://localhost:9090`) — the e2e harness reads that variable, exactly as OurWay's does.

`src/api/client.ts` = OurWay's axios instance: `baseURL: '/api'`, request interceptor attaches `Authorization: Bearer <access_token>`, response interceptor on 401 → `POST /api/auth/refresh` once, then retry, else clear session and route to `/login`. Token storage mirrors `OurWay/web/src/auth/context.tsx` (`localStorage` keys `nvr_access_token`, `nvr_refresh_token`, `nvr_user`, `nvr_roles`).

Pages (`src/pages/`): `LoginPage` (renders `GET /api/auth/status` → local form and/or `Login with <issuer>` link to `/api/auth/sso/<provider>/authorize`), `AuthCallbackPage` (reads `#access_token`/`#refresh_token`, stores, routes), `LivePage` (camera grid + player), `EventsPage` (timeline, filters, detail drawer), `CamerasPage` (list + editor + zone editor), `SettingsPage` (storage backends with test-connection state, OIDC providers, webhooks, retention), `AuditPage`. Routes: `/login`, `/auth/callback`, `/live`, `/events`, `/events/:id`, `/cameras`, `/cameras/:id`, `/settings`, `/audit`.

Live player `src/live/go2rtc.ts` (no new deps — native `RTCPeerConnection`), protocol from go2rtc's WS API (`internal/api/ws/README.md`):
1. `ws = new WebSocket(`${GO2RTC_PUBLIC_URL.replace('http','ws')}/api/ws?src=${slug}`)`;
2. send `{"type":"webrtc/offer","value": pc.localDescription.sdp }` after `pc.addTransceiver('video',{direction:'recvonly'})` (and `'audio'` recvonly) + `setLocalDescription(createOffer())`;
3. on message `{"type":"webrtc/answer"}` → `setRemoteDescription(new RTCSessionDescription({type:'answer', sdp: msg.value}))`; handle `{"type":"webrtc/candidate"}` both directions; go2rtc accepts empty/trickle candidates;
4. `pc.ontrack` → `video.srcObject = e.streams[0]`.
The public base URL comes from `GET /api/v1/settings/live` → `{go2rtc_public_url}` (viewer), so the console never hard-codes the media host. Fallback (only if WebRTC fails in the target browser): HLS via `hls.js` against `${GO2RTC_PUBLIC_URL}/api/stream.m3u8?src=<slug>` behind a `mode` toggle in the player component — implement only as the contingency.

Zone editor `src/components/ZoneEditor.tsx`: `<canvas>` over `GET /api/v1/cameras/:id/snapshot`; click to append vertices, Enter closes the polygon, `Escape` cancels, drag moves a vertex, `Delete` removes the zone; normalized `0..1` coordinates are written by `PUT /api/v1/cameras/:id/zones` with the exact `ZonesJSON` shape of Step 1; overlay draws the polygon fill `--accent` 20 % + rule badge. The event detail drawer draws the same polygons plus recorded boxes (`ObjectsJSON`) on the snapshot at `ts = StartedAt`.

Event UI: `GET /api/v1/events` cards (thumbnail from `/api/v1/events/:id/snapshot`, class chips, severity ring, camera slug, duration); detail drawer plays `<video src="/api/v1/events/:id/clip">` with the timeline strip from `GET /api/v1/segments`; open events show `LIVE` and update from `event.update`. WS `src/stores/realtime.ts` (zustand) subscribes `/ws?token=`, handles the exact message types of Step 2, with 5 s reconnect backoff capped at 60 s and a `stale` banner when the socket is down.

Console behavior contract (OurWay's habit, kept): every mutation shows the server's state, not a spinner — test-connection answers a named state, upload queue counts render from `storage.queue`, an offline camera says `offline` from `camera.status`, and a snapshot failure says `no snapshot yet`.

Web tests: `src/api/client.test.ts` (401 → refresh → retry, using `vi.stubGlobal('fetch')`/axios mock adapter), `src/live/go2rtc.test.ts` (offer/answer message shapes with a fake `WebSocket` + fake `RTCPeerConnection`: asserts the offer message is `{type:'webrtc/offer'}` and that the answer is applied), `src/components/ZoneEditor.test.tsx` (vertex normalization `0..1` round-trip). No snapshot tests; behavior only.


### Step 9 — Containers, compose, scripts, CI

`docker/Dockerfile.server` (copy OurWay's structure): `golang:1.26-alpine` builder (`apk add git ca-certificates tzdata`, layer-cached `go mod download`, `CGO_ENABLED=0 go build -ldflags "-s -w -X nvr/server/config.Version=${VERSION} -X nvr/server/config.GitCommit=${GIT_COMMIT}"`), runtime `alpine:3.20` **plus `apk add --no-cache ffmpeg ca-certificates tzdata`** (the recorder and clip builder shell out to ffmpeg), non-root `nvr` user, `EXPOSE 8080`, `HEALTHCHECK wget -q --spider http://localhost:8080/health`, `CMD ["/app/server"]`.

`docker/Dockerfile.detector`: stage 1 `python:3.12-slim` — `pip install ultralytics==8.4.195 onnx==1.17.0`, fetch `yolov8n.pt` (pinned URL, Step 5), `python -c "from ultralytics import YOLO; YOLO('yolov8n.pt').export(format='onnx', imgsz=640, opset=17, dynamic=False)"`; stage 2 `python:3.12-slim` — `pip install onnxruntime==1.31.1 opencv-python-headless==5.0.0.93 numpy requests pydantic`, copy `/models/yolov8n.onnx`, non-root user, `CMD ["python","-m","nvr_detector"]`. Detector liveness is data, not a listener: the worker POSTs `/api/internal/v1/heartbeat {"uptime_seconds":N}` every 30 s, the server stores `settings.detector_heartbeat_at`, and `GET /api/healthz` answers `"detector":"fresh"` (< 90 s) or `"detector":"stale"`. The compose healthcheck for `detector` is the process check `sh -c "pgrep -f nvr_detector"`; `server`'s healthcheck stays `wget -q --spider /health`.

`docker/Dockerfile.web` = OurWay's (vite build → `nginx:1.27-alpine` + `docker/nginx.conf` proxying `/api` and `/ws` to `server:8080`, with `/api/v1/events/.*/clip` getting `proxy_buffering off; proxy_request_buffering off;`).

`docker/docker-compose.yml` (dev, build-based; mirrors `OurWay/docker/docker-compose.yml` conventions: `name: nvr`, `container_name: nvr-*`, `restart: unless-stopped`, json-file logging `max-size: 10m, max-file: 3`, bridge network `nvr_net`, named volumes, healthchecks with `depends_on: {condition: service_healthy}`):
- `postgres` (`postgres:16-alpine`, user/pass/db `nvr`, volume `postgres_data`, `pg_isready` healthcheck).
- `go2rtc` (`ghcr.io/alexxit/go2rtc:master` — ships FFmpeg; ports `1984:1984`, `8554:8554`, and for WebRTC `8555:8555` tcp+udp; volume `go2rtc_config:/config`; API basic auth from `GO2RTC_USERNAME`/`GO2RTC_PASSWORD` via `GO2RTC_API` env file).
- `server` (build `docker/Dockerfile.server`, env = the Step 1 table, ports `8080:8080`, volume `nvr_data:/var/lib/nvr`).
- `detector` (build `docker/Dockerfile.detector`, `NVR_SERVER_URL=http://server:8080`, `NVR_WORKER_TOKEN=${WORKER_TOKEN}`, volume `nvr_models:/models`).
- `web` (build `docker/Dockerfile.web`, port `3000:80`).
- Optional MinIO test service, commented in the dev compose like OurWay comments its agent service: `minio` (`minio/minio:latest`, `command: server /data --console-address ":9001"`, ports `9000:9000`, `9001:9001`) — used by `scripts/verify.sh` storage steps.
- GPU shape documented in `docs/installation.md` as a compose overlay (`docker-compose.gpu.yml`): `services.detector.deploy.resources.resolutions.devices: [{capabilities: [gpu]}]`, `NVIDIA_VISIBLE_DEVICES=all`, `DET_PROVIDERS=CUDAExecutionProvider,CPUExecutionProvider`; the overlay ships and is documented, verified on a host with the NVIDIA container toolkit — **not** on this dev box (no `/dev/nvidia*` nodes, no toolkit installed).

`docker-compose.prod.yml` = the same services with `image: ghcr.io/welcometotheweb/nvr/{server,web,detector}:latest` (tag-pinned `:latest`/`:edge` semantics and comments copied from `OurWay/docker-compose.prod.yml`), no build sections, MinIO absent, `GO2RTC_PUBLIC_URL` required.

`scripts/dev.sh` = `docker compose -f docker/docker-compose.yml up -d --build` + `--demo` flag that seeds a demo camera (Step 10) and prints the console URL. `scripts/verify.sh` = OurWay's colored `pass/fail/step` harness, steps in Step 10. `scripts/detect-smoke.sh` = build the detector image, run `docker compose run --rm detector python -m pytest tests/test_pipeline_smoke.py -m slow -q`. `scripts/e2e-stack.sh` (`restart|test|stop`) = throwaway sqlite DB + `go run` server on `:9090` with `NVR_E2E=1` + `vite preview`, exactly OurWay's harness (env `NVR_E2E_RUN_ID` for run-scoped seeds; rate dial raised to `RATE_LIMIT_PER_MINUTE=1000`). `scripts/coverage.sh` = OurWay's ratchet, extended to also measure the detector with `pytest --cov` (baseline recorded once at the end).

`.github/workflows/build.yml` (mirrors OurWay's split): `Server & Detector Tests` (ubuntu-latest; `setup-go@v5` with go 1.26; `apt-get install -y ffmpeg` for the media tests; `go test ./... -count=1` in `server/` + `scripts/coverage.sh`; `pip install -e detector[dev] && pytest detector/tests -q -m "not slow"`), `Web Tests` (`npm ci && npm run typecheck && npm run test:run`), `E2E` (`npm run test:e2e:install && scripts/e2e-stack.sh test`), `Build & Push` on tag (`docker/build-push-action@v6` for the three images, `ghcr.io`, tags `:latest` + `:${GIT_TAG}`, plus `:edge` on `main`).

Docs content that carries decisions (not filler): `docs/architecture.md` = the pipeline diagram (cameras → go2rtc → recorder segments → detector signals → events → clips → storage tier), the clock rule (server clock owns event time), the remote-playback gap (streaming, no seek), the single-instance rule (no Redis, one server process owns ffmpeg), the GPU story (CPU default, CUDA via overlay + host toolkit). `docs/configuration.md` = every env var from Step 1 with defaults. `docs/api.md` = the route table of Steps 2/3/6/7 + the internal worker contract of Step 5. `docs/installation.md` = quickstart, `.env` generation (`openssl rand -hex 32` for `JWT_SECRET`/`NVR_SECRET_KEY`/`WORKER_TOKEN`), camera setup, OIDC provider setup (redirect URI `${WEB_URL}/api/auth/sso/<provider>/callback` — state the exact string), MinIO/R2/B2 examples, GPU overlay, and the retention math table.


### Step 10 — Demo fixture, seeding, end-to-end scripts

The whole pipeline must be provable with no physical camera. `testdata/fixtures.json` pins the assets:

```json
{"assets":[
 {"name":"bus.jpg","url":"https://github.com/ultralytics/assets/releases/download/v0.0.0/bus.jpg","bytes":137419,"license":"COCO images, CC-BY-4.0 (attribution in README)"},
 {"name":"yolov8n.pt","url":"https://github.com/ultralytics/assets/releases/download/v8.3.0/yolov8n.pt","bytes":6549796,"license":"AGPL-3.0 (Ultralytics assets)"}
]}
```

`scripts/fetch-fixture.sh` downloads both into `testdata/`, verifies byte counts, and writes `sha256` into `testdata/fixtures.json` on first run; the files are not committed.

Demo camera (seeded by `scripts/dev.sh --demo` **through the public API** — the seeder is an API client, never a DB writer, so seeding exercises the same path an operator uses):
- `POST /api/v1/cameras` → `{"slug":"demo-doorbell","name":"Demo Doorbell","source_uri":"exec:ffmpeg -re -loop 1 -i /fixtures/bus.jpg -pix_fmt yuv420p -c:v libx264 -preset veryfast -f h264 -","record_mode":"continuous","detection_enabled":true,"detection_fps":5,"detection_width":640,"detection_height":360}`.
- `go2rtc/config.go` has exactly one demo-only rule, documented as such: for slug `demo-doorbell` the generated `<slug>_det` source is `exec:ffmpeg -re -loop 1 -i /fixtures/bus.jpg -f mjpeg -r 5 -` (the fixture is mounted into the `go2rtc` container as `/fixtures` in the dev compose) so detection never transcodes H.264 twice. The dev-only mount is `./testdata:/fixtures:ro`.
- `PUT /api/v1/cameras/:id/zones` → `[{"id":"porch","name":"porch","polygon":[[0.05,0.35],[0.95,0.35],[0.95,0.98],[0.05,0.98]],"classes":["person"],"rule":"loiter","dwell_seconds":4,"severity":3,"min_confidence":0.35,"min_area_ratio":0.004,"enabled":true}]`.
- `POST /api/storage/backends` → `{"kind":"fs","name":"local-tier","root_path":"/var/lib/nvr/remote","is_default":true}`.

`scripts/verify.sh` (colored `pass/fail/step`, exits non-zero on any failure — the acceptance gate for v1):
1. `GET :8080/health` → 200; `GET :8080/api/healthz` reports `"detector":"fresh"` (heartbeat `< 90 s`).
2. `POST /api/auth/login {"username":"admin","password":$BOOTSTRAP_ADMIN_PASSWORD}` → `access_token`.
3. seed demo camera; `POST /api/v1/cameras/:id/probe` → `{"online":true}` with producers containing `exec:` and `ffmpeg:`.
4. poll `GET /api/v1/events?camera_id=<id>` up to 90 s → **≥ 1 event with `classes` containing `"person"`, `rules` containing `"loiter"`, `zones` containing `porch`, `seconds >= 4`** (the AI-detection claim, observed end-to-end).
5. `GET /api/v1/events/:id/snapshot` → 200 `image/jpeg` > 8 KB. `GET /api/v1/events/:id/clip` → 200 `video/mp4`; `Range: bytes=0-99999` → 206 (seekable).
6. `GET /api/v1/segments?camera_id=<id>` → ≥ 3 rows, `end_ts - start_ts ≈ SEGMENT_SECONDS`; count strictly increases over a 15 s window (recording is live).
7. storage: `GET /api/storage/queue` → jobs reach `uploaded`; `find /var/lib/nvr/remote -name clip.mp4` non-empty; `PUT /api/storage/backends/:id/enabled {"enabled":false}` → new jobs stay `pending` (queue stops).
8. OIDC with the in-repo IdP: `cd server && go run ./sso/testidp -listen :9500 -client nvrdev -secret devsecret -issuer http://localhost:9500`; `POST /api/settings/oidc {"issuer":"http://localhost:9500","client_id":"nvrdev","client_secret":"devsecret"}` → `"discovery":"ok"`; the scripted `authorize → consent → callback` walk with `curl -b jar -L` ends at `/auth/callback#access_token=…`, and `POST /api/auth/sso/exchange` returns `roles: ["viewer"]` (the JIT floor, observed).
9. webhook: a listener on `:9501` receives the close payload; the script recomputes `HMAC-SHA256(secret, body)` and compares with `X-NVR-Signature`.

`scripts/detect-smoke.sh` success line: `smoke ok: person box conf>=0.5 on bus.jpg (yolov8n.onnx, CPUExecutionProvider)`.

### Step 11 — Test suite (the shape CI runs)

Go (`server/...`, sqlite-backed via `store.New(":memory:")` + `NewWithDB` mirroring OurWay's `store_test.go`):
- `go2rtc/client_test.go` + `go2rtc/config_test.go` — golden YAML from camera rows (two cameras, one detection-off, one demo slug: exact bytes asserted, so a stream-name or `raw=` change fails the test); `httptest` fake go2rtc asserting `PUT /api/streams?src=&name=` pairs, `POST /api/config` body, delete; retry-on-500 → 3 attempts; `ErrGo2rtcUnreachable` when the server is closed.
- `media/recorder_test.go` — real ffmpeg (`lavfi -f lavfi -i testsrc2=size=320x240:rate=10` as a fake RTSP source through the `source_uri` override hook) writes 3+ segments; asserts rows carry `start_ts`/`end_ts` matching the ffmpeg CSV, `bytes > 0`, generation counter increments across a forced process kill, restart resumes numbering.
- `media/clips_test.go` — event window over generated segments → clip file exists, ffprobe-measured duration within ±1.5 s of the requested window; empty-window case asserts `clip_key == ""` + the log line.
- `media/events_upsert_test.go` — `open`/`extend`/`close` state machine: one event across 3 signals (`event_key` dedupe), objects capped at 200 oldest-first, `severity` monotone max, `close` after the 10 s idle, a post-close `extend` answers `200` with `"error":"event closed"` and mutates nothing, `ts` is server-assigned (a signal with a future `ts` never produces `started_at > now()`).
- `storage/fs_test.go`, `storage/s3_test.go` (gofakes3 in-test server: put/get/stat/delete/list, prefix layout, `EnableSSL=false`, path-style), `storage/webdav_test.go` (`httptest` server implementing the PROPFIND/GET/PUT/DELETE subset the driver issues — protocol-level, not a mock of the driver), `storage/sftp_test.go` (`golang.org/x/crypto/ssh` in-test server + `sftp.NewServer` over a temp dir: auth, path confinement under `RootPath`, host-key pinning and the changed-key failure), `storage/seal_test.go` (round-trip, tamper detection, key derivation), `storage/uploader_test.go` (claim → upload → `uploaded`; failure → exponential `next_attempt_at`; `attempts>=20` → `failed`; disabled backend claims nothing), `sweeper/sweeper_test.go` (local delete eligibility with un-uploaded protection window; remote delete only for rows past `remote_retention_days` with `0 = keep forever`).
- `sso/{discovery,jwks,oauth,contract}_test.go` — port OurWay's suites as-is minus SAML: discovery states (`ok`, `unreachable`, bad document, issuer mismatch, no-code-flow), JWKS rotation with kid-triggered refresh-once, PKCE verifier/challenge, nonce binding, all five `IdentityError` strings, email-verified gate, allow-list, JIT `viewer` floor, `uniqueUsername` collision, `link_policy=pending` refusal → `GET /api/identities` row → `POST /api/identities/:id/link` → login works and the old row is audit-linked, mapped-role ownership (`admin` cannot map to `viewer` → `422`).
- `api/oidc_test.go` — full authorize→callback→exchange against `sso/testidp`, asserting the fragment handoff URL, refresh-token rotation on `POST /api/auth/refresh`, JIT-created user `roles == ["viewer"]`, and a `viewer` `POST /api/v1/cameras` → `403 {"error":"insufficient role"}`.
- `api/cameras_test.go` — slug validation/uniqueness (`409`), zone polygon validation (`422`, 3–32 vertices, `0..1` bounds), apply-config called on create/update/delete (counted via the fake go2rtc), `viewer` read/`operator` write/`viewer` write-denied matrix.
- `api/storage_test.go`, `api/events_test.go` — role gates, list filters (`camera_id`, `class`, `severity`, `from`/`to`, `q`, `limit` clamp at 200), clip proxy Range/206 against a temp mp4, upload-queue pagination, secret non-echo (`secret_set:true` and no secret field).
- `webhooks/webhooks_test.go` — signature recomputation, 3 attempts with backoff, delivery rows recorded, disabled webhook sends nothing.
- `ws/hub_test.go` — token auth (bad token closes), origin rejection, `event.new`/`event.update`/`event.close` fan-out to two clients.

Python (`detector/tests/`, `pytest -m "not slow"` in CI):
- `test_decode.py` — synthetic `[1,84,8400]` tensor with two hand-planted boxes → expected pixel corners, class ids, confidences; NMS suppression of an overlapping pair (IoU 0.62 > 0.45 keeps the higher conf); `min_area_ratio` drop; letterbox round-trip (a box at the padded edge maps back inside the frame).
- `test_model_smoke.py` (`slow`) — loads `testdata/yolov8n.onnx` + `bus.jpg` → ≥1 `person` with `conf >= 0.5`, box area ratio `>= 0.004` (the model/decode/preprocess contract, one assertion set).
- `test_zones.py` — point-in-polygon (concave polygon, on-edge, outside), centroid = bottom-center (a box `[100,50,140,150]` on a `640x360` frame → `(0.21875, 0.4167)`), `intrusion` fires at `conf >= min_confidence` and not below, `loiter` fires only after `dwell_seconds` of continuous presence, leaving resets the dwell timer.
- `test_events.py` — open→extend→close transitions with injected timestamps, `event_key` stability across extends, 10 s idle close, `EVENT_MAX_SECONDS` forced close, `open` carries exactly one snapshot.
- `test_tracker.py` — IoU matching across 5 synthetic frames keeps one stable `track_id`; an occlusion gap ≤ 6 frames resumes the same id; > 6 frames spawns a new id.
- `test_motion.py` — identical frames skip inference; a moved 16×16 block on a `testsrc2`-style frame passes the gate; gate never runs in `events` record mode (asserted via the pipeline seam).
- `test_signals.py` — serialization shape (exact JSON keys of Step 5), 2 s flush, 40-signal cap dropping oldest with a counted log, retry/backoff against a local `http.server` stub (500, 500, 200 → 3 attempts), buffer survives a dead server and flushes on recovery.

Web (`npm run test:run`, vitest + @testing-library): api client refresh-retry-then-logout on 401; `ZoneEditor` polygon normalization and `PUT` payload shape; `go2rtc.ts` offer/answer message sequence with fake `WebSocket`/`RTCPeerConnection`; `realtime.ts` store updates on each WS message type + `stale` flag on close; `EventsPage` renders filter chips from query params and an event card list from a stubbed `fetch`.

E2E (`scripts/e2e-stack.sh test`, Playwright chromium; specs in `web/e2e/`): `auth.spec.ts` (local admin login → console; `viewer` sees no Settings tab), `live.spec.ts` (player mounts, `<video>` `readyState >= 1`), `cameras.spec.ts` (create camera, draw a 4-vertex zone, reload, vertices persisted), `events.spec.ts` (seeded event appears in the timeline, detail drawer, clip `<video>` plays to `currentTime > 0`), `storage.spec.ts` (add fs backend, test-connection renders `ok`).

## Critical files

| Path | Origin |
| --- | --- |
| `server/main.go`, `config/config.go`, `store/store.go`, `store/migrate.go`, `auth/jwt.go`, `auth/argon2.go`, `sso/{discovery,jwks,oauth,contract,testidp}.go`, `api/middleware.go`, `ws/hub.go`, `docker-compose.yml`, `docker/{Dockerfile.server,Dockerfile.web}`, `docker/nginx.conf`, `web/{package.json,vite.config.ts,tailwind.config.ts,src/api/client.ts,src/auth/context.tsx}`, `scripts/{coverage.sh,e2e-stack.sh}`, `.github/workflows/build.yml`, `README.md` structure, `LICENSE`, `.gitignore`, `env.example` | adapted from `/root/projects/OurWay` (MIT) |
| `server/go2rtc/{client.go,config.go}`, `server/media/{recorder.go,clips.go}`, `server/media/segments.go`, `server/store/{cameras,segments,events,storage}.go`, `server/storage/{storage,s3,webdav,sftp,fs,seal,uploader}.go`, `server/sweeper/sweeper.go`, `server/webhooks/webhooks.go`, `server/api/{cameras,events,segments,storage,settings,live,healthz,detector_api}.go`, `server/models/*.go` | new |
| `detector/nvr_detector/{__main__.py,client.py,mjpeg.py,model.py,tracker.py,zones.py,events.py,motion.py,pipeline.py}` + `detector/{pyproject.toml,tests/*}`, `docker/Dockerfile.detector`, `docker-compose.gpu.yml` | new |
| `web/src/{pages,live,components,stores}/*`, `web/e2e/*` | new (OurWay conventions) |

## Verification

```bash
scripts/dev.sh --demo                       # compose up, seeded camera, console on :3000
scripts/verify.sh                            # Steps 1-9 of Step 10; exit 0
scripts/detect-smoke.sh                      # model+decode+bus.jpg smoke inside the detector image
scripts/fetch-fixture.sh                     # pinned asset bytes + recorded sha256
cd server && go test ./... -count=1           # includes ffmpeg-backed recorder/clip tests
cd server && go run ./sso/testidp -listen :9500 -client nvrdev -secret devsecret
cd detector && python3 -m pytest tests -q -m "not slow"
cd web && npm run typecheck && npm run test:run
scripts/e2e-stack.sh restart && scripts/e2e-stack.sh test
```

Acceptance = `scripts/verify.sh` prints 9 `pass` lines (person/loiter event observed end-to-end, snapshot + seekable clip, growing segment count, uploaded objects on the storage tier, OIDC login at the `viewer` floor, signed webhook), `detect-smoke.sh` prints its `smoke ok` line, and all four test suites exit 0.

## Assumptions & open risks

- Cameras in v1 = RTSP (and go2rtc's other source schemes) through go2rtc; ONVIF discovery is go2rtc's, surfaced later. No ONVIF event/preset handling.
- CPU inference is the verified baseline (4 vCPU dev box, no `/dev/nvidia*` nodes, no container toolkit); `yolov8n` at 5 FPS/640×360 is sized for ~4 cameras on CPU. CUDA is shipped as an overlay and remains unverified here — `docs/installation.md` states that.
- Detector needs Python 3.11–3.12 wheels; the host Python 3.14 has no pip/venv, so every Python task runs in the detector image (`scripts/*` invoke it via `docker compose run`), never on the host.
- WebRTC requires the browser to reach go2rtc (`GO2RTC_PUBLIC_URL` + published ports); LAN-only by default, `docs/installation.md` covers the reverse-proxy/Tailscale answer. v1 does not proxy media through the server.
- Single server instance (no Redis): `scripts/*` must not run more than one `server` container; the sweeper and ffmpeg supervisors assume sole ownership of the media tree.
- Remote playback streams the whole clip (no remote seek); recorded events on a remote tier are playable, scrub-on-remote is a v2 item.
- Model export runs at image build time from the pinned URLs of Step 5; a build with no network fails loudly, and the exported `yolov8n.onnx` hash is recorded in `testdata/fixtures.json` on the first successful build.
- `go 1.26` in `go.mod` and `onnxruntime==1.31.1` / `opencv-python-headless==5.0.0.93` / `ultralytics==8.4.195` pins were verified live this session (module headings, asset sizes, wheel filenames); if a pin breaks at build time, the fix is a pin bump recorded in the same commit, not a loosened constraint.

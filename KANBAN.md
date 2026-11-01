# nvrWay — Kanban

One card per deliverable (a module, a route group, a test suite), not per plan
step. Cards live in `kanban/cards/` and are the source of truth; this file is
generated from them. Convention and the move rules: [`docs/kanban.md`](docs/kanban.md).
Plan: [`NVRWAY_V1_PLAN.md`](NVRWAY_V1_PLAN.md) — cards cite the step they implement.

| In Progress | Review | Blocked | Backlog | Done |
| ---: | ---: | ---: | ---: | ---: |
| 0 | 0 | 1 | 86 | 24 |

Total cards: **111**. In Progress limit: **2**.


## In Progress

_empty_

## Review — built, not yet verified

_empty_

## Blocked


### Step 9 — Containers, compose, scripts, CI

| Card | Title | Depends |
| --- | --- | --- |
| [`S9-06`](kanban/cards/S9-06.md) | GPU overlay: docker-compose.gpu.yml + the CPU-baseline story in docs | S9-04 |

## Backlog


### Step 0 — Repo scaffold

| Card | Title | Depends |
| --- | --- | --- |
| [`S0-01`](kanban/cards/S0-01.md) | Repo tree skeleton (server packages + detector/web/docker/scripts/testdata/.github) | — |
| [`S0-02`](kanban/cards/S0-02.md) | Docs skeleton: DESIGN, PRODUCT, SECURITY, CONTRIBUTING + docs/ headings | — |
| [`S0-03`](kanban/cards/S0-03.md) | Compose + Dockerfile skeletons | S0-01 |
| [`S0-04`](kanban/cards/S0-04.md) | scripts/ + CI workflow skeletons | S0-01 |

### Step 2 — Auth: JWT, break-glass admin, OIDC, WebSocket

| Card | Title | Depends |
| --- | --- | --- |
| [`S2-03`](kanban/cards/S2-03.md) | store/bootstrap.go: seed roles + break-glass admin, recorded in settings | S1-11, S1-12, S2-02 |
| [`S2-04`](kanban/cards/S2-04.md) | sso/ port: discovery, jwks, oauth, contract (SAML removed, kind fixed to oidc) | S1-07, S1-12 |
| [`S2-05`](kanban/cards/S2-05.md) | sso/testidp: in-repo test IdP | S2-04 |
| [`S2-06`](kanban/cards/S2-06.md) | Auth routes: status, login, refresh, logout, profile, password (registration 403) | S2-01, S2-03 |
| [`S2-07`](kanban/cards/S2-07.md) | SSO routes: authorize, callback, exchange (+ state row carrying nonce + verifier) | S2-04, S2-05 |
| [`S2-08`](kanban/cards/S2-08.md) | api/middleware.go: auth, role gates, internal token, audit on denial | S2-01, S1-12 |
| [`S2-09`](kanban/cards/S2-09.md) | Users + roles admin routes | S2-08 |
| [`S2-10`](kanban/cards/S2-10.md) | ws/hub.go: local hub, token auth, origin guard, the seven message types | S2-01 |
| [`S2-11`](kanban/cards/S2-11.md) | Step 2 suites: sso discovery/jwks/oauth/contract, api/oidc, ws/hub, auth | S2-01, S2-04, S2-05, S2-06, S2-07, S2-10 |

### Step 3 — Cameras + go2rtc sync + live plumbing

| Card | Title | Depends |
| --- | --- | --- |
| [`S3-01`](kanban/cards/S3-01.md) | go2rtc/client.go: streams, put/delete, ApplyConfig, Frame, WebRTCOffer | S1-01 |
| [`S3-02`](kanban/cards/S3-02.md) | go2rtc/config.go: render go2rtc.yaml from camera rows (golden file) | S1-03, S3-01 |
| [`S3-03`](kanban/cards/S3-03.md) | Camera API: CRUD, probe, snapshot, slug rules | S1-10, S3-01, S3-02 |
| [`S3-04`](kanban/cards/S3-04.md) | Zone writes: PUT /api/v1/cameras/:id/zones + audit | S1-03, S3-03 |
| [`S3-05`](kanban/cards/S3-05.md) | Apply go2rtc config on every camera write + at boot | S3-01, S3-02, S3-03 |
| [`S3-06`](kanban/cards/S3-06.md) | Role floor matrix (viewer read / operator CRUD / admin) + camera suite | S2-08, S3-03, S3-04 |

### Step 4 — Recorder: continuous + event-gated segments

| Card | Title | Depends |
| --- | --- | --- |
| [`S4-01`](kanban/cards/S4-01.md) | Recorder supervisor: one goroutine per camera, record_mode dispatch | S1-10, S1-13, S2-10 |
| [`S4-02`](kanban/cards/S4-02.md) | ffmpeg ingest command + Generation counter persisted in settings | S4-01 |
| [`S4-03`](kanban/cards/S4-03.md) | Segment discovery: tail the ffmpeg CSV into segment rows on the server clock | S4-02, S1-10 |
| [`S4-04`](kanban/cards/S4-04.md) | Crash handling: ffmpeg exit -> log + camera.status offline + restart backoff | S4-01, S4-03, S2-10 |
| [`S4-05`](kanban/cards/S4-05.md) | GET /api/v1/segments — the timeline manifest | S1-10, S2-08, S4-03 |
| [`S4-06`](kanban/cards/S4-06.md) | recorder.status broadcast (one per segment tick) | S2-10, S4-03 |
| [`S4-07`](kanban/cards/S4-07.md) | Recorder suite: lavfi fake source writes real segments | S4-01, S4-02, S4-03, S4-04 |

### Step 5 — Detection worker (Python)

| Card | Title | Depends |
| --- | --- | --- |
| [`S5-01`](kanban/cards/S5-01.md) | detector/pyproject.toml: package nvr_detector + pinned deps | S0-01 |
| [`S5-02`](kanban/cards/S5-02.md) | mjpeg.py: MjpegStream over requests, boundary split, cv2 decode | S5-01 |
| [`S5-03`](kanban/cards/S5-03.md) | model.py: letterbox preprocess, YOLOv8 decode, NMS, class sets | S5-01 |
| [`S5-04`](kanban/cards/S5-04.md) | tracker.py: greedy IoU tracker (>=0.30, 6-frame expiry) | S5-03 |
| [`S5-05`](kanban/cards/S5-05.md) | zones.py: point-in-polygon, bottom-center centroid, intrusion + loiter | S5-04 |
| [`S5-06`](kanban/cards/S5-06.md) | events.py: open/extend/close state machine + snapshot on open | S5-05 |
| [`S5-07`](kanban/cards/S5-07.md) | motion.py: CPU gate (160px grayscale absdiff, 1%, continuous mode only) | S5-01 |
| [`S5-08`](kanban/cards/S5-08.md) | client.py: ServerClient config poll + signal POST with backoff | S5-01 |
| [`S5-09`](kanban/cards/S5-09.md) | pipeline.py + __main__.py: per-camera thread and env entrypoint | S5-02, S5-03, S5-04, S5-05, S5-06, S5-07, S5-08 |
| [`S5-10`](kanban/cards/S5-10.md) | Internal worker API: detector config payload + signals intake | S2-08, S1-10, S1-11, S6-03 |
| [`S5-11`](kanban/cards/S5-11.md) | Detector suite: decode, zones, events, tracker, motion, signals + slow model smoke | S5-02, S5-03, S5-04, S5-05, S5-06, S5-07, S5-08 |
| [`S5-12`](kanban/cards/S5-12.md) | Model provenance: yolov8n.pt -> onnx export at image build, hashes recorded | S9-02, S10-01 |

### Step 6 — Events, clips, snapshots, webhooks

| Card | Title | Depends |
| --- | --- | --- |
| [`S6-01`](kanban/cards/S6-01.md) | clips.go: windowed concat with padding + the empty-window rule | S1-10, S4-03, S2-10 |
| [`S6-02`](kanban/cards/S6-02.md) | Snapshot write: decode signal snapshot_base64 to <SNAPSHOTS_DIR>/<event_id>.jpg | S5-10, S6-03 |
| [`S6-04`](kanban/cards/S6-04.md) | Event API: list filters, detail, delete, snapshot, clip | S2-08, S6-01, S6-02 |
| [`S6-05`](kanban/cards/S6-05.md) | Clip delivery: local Range/206, remote streamed from byte 0 | S6-04, S7-01 |
| [`S6-06`](kanban/cards/S6-06.md) | webhooks/: signed delivery with 3 attempts + delivery rows | S1-11, S6-03 |
| [`S6-07`](kanban/cards/S6-07.md) | Webhook settings routes (admin) + delivery history | S2-08, S6-06 |
| [`S6-08`](kanban/cards/S6-08.md) | WS fan-out: event.new / event.update (throttled) / event.close | S2-10, S6-03 |

### Step 7 — Storage backends, upload queue, retention

| Card | Title | Depends |
| --- | --- | --- |
| [`S7-01`](kanban/cards/S7-01.md) | storage.go: Backend interface + Open dispatch (+ OpenDefault) | S1-04, S1-11, S7-02 |
| [`S7-02`](kanban/cards/S7-02.md) | seal.go: AES-256-GCM secret sealing keyed by SHA-256(NVR_SECRET_KEY) | S1-04 |
| [`S7-03`](kanban/cards/S7-03.md) | fs.go: local dir / NFS mount with .part + rename | S7-01 |
| [`S7-04`](kanban/cards/S7-04.md) | s3.go: minio-go driver, path-style always | S7-01, S7-02 |
| [`S7-05`](kanban/cards/S7-05.md) | webdav.go: gowebdav driver over the PROPFIND subset | S7-01, S7-02 |
| [`S7-06`](kanban/cards/S7-06.md) | sftp.go: password/key auth + host-key pinning | S7-01, S7-02 |
| [`S7-07`](kanban/cards/S7-07.md) | Key layout: nvr/<slug>/<YYYY>/<MM>/<DD>/<basename>.mp4, nvr/events/<id>/{clip,snapshot} | S7-01 |
| [`S7-08`](kanban/cards/S7-08.md) | uploader.go: claim, upload, exponential retry, failed at 20, requeue, enable flag | S7-01, S1-11 |
| [`S7-09`](kanban/cards/S7-09.md) | sweeper.go: the seven sweeps, with the un-uploaded protection window | S1-11, S7-08 |
| [`S7-10`](kanban/cards/S7-10.md) | Storage API: backends CRUD, test, default, prefixes, queue | S2-08, S7-01, S7-02 |

### Step 8 — Web console

| Card | Title | Depends |
| --- | --- | --- |
| [`S8-01`](kanban/cards/S8-01.md) | Console scaffold: package.json, vite, tailwind, tsconfig, index.html | S0-01 |
| [`S8-02`](kanban/cards/S8-02.md) | api/client.ts + auth context: bearer, 401 -> refresh once -> retry, session keys | S8-01, S2-06 |
| [`S8-03`](kanban/cards/S8-03.md) | LoginPage + AuthCallbackPage: status-driven buttons and the fragment handoff | S8-02, S2-07 |
| [`S8-04`](kanban/cards/S8-04.md) | GET /api/v1/settings/live (server): the console never hard-codes the media host | S2-08, S1-01 |
| [`S8-05`](kanban/cards/S8-05.md) | LivePage + go2rtc.ts: native RTCPeerConnection over go2rtc's WS API | S8-02, S8-04, S3-01 |
| [`S8-06`](kanban/cards/S8-06.md) | EventsPage + detail drawer: cards, filters, timeline strip, LIVE row | S8-02, S6-04, S8-10 |
| [`S8-07`](kanban/cards/S8-07.md) | CamerasPage + ZoneEditor: canvas polygon editor over the snapshot | S8-02, S3-03, S3-04 |
| [`S8-08`](kanban/cards/S8-08.md) | SettingsPage: backends + test-connection state, OIDC, webhooks, retention | S8-02, S7-10, S6-07, S2-07 |
| [`S8-09`](kanban/cards/S8-09.md) | AuditPage + routes/nav role gating | S8-02, S2-09 |
| [`S8-10`](kanban/cards/S8-10.md) | realtime.ts: zustand store over /ws?token= with backoff + stale banner | S8-02, S2-10 |
| [`S8-11`](kanban/cards/S8-11.md) | Console suite: vitest + @testing-library (behavior, no snapshots) | S8-02, S8-05, S8-06, S8-07, S8-10 |
| [`S8-12`](kanban/cards/S8-12.md) | Playwright specs: auth, live, cameras, events, storage | S8-11, S10-02, S9-08 |

### Step 9 — Containers, compose, scripts, CI

| Card | Title | Depends |
| --- | --- | --- |
| [`S9-01`](kanban/cards/S9-01.md) | Dockerfile.server: alpine builder + runtime with ffmpeg, non-root, healthcheck | S1-02, S1-13 |
| [`S9-02`](kanban/cards/S9-02.md) | Dockerfile.detector: export stage + runtime stage | S5-01, S5-12 |
| [`S9-03`](kanban/cards/S9-03.md) | Dockerfile.web + nginx.conf: vite build -> nginx proxying /api and /ws | S8-01 |
| [`S9-04`](kanban/cards/S9-04.md) | docker/docker-compose.yml (dev): six services, healthchecks, named volumes | S9-01, S9-02, S9-03, S3-01 |
| [`S9-05`](kanban/cards/S9-05.md) | docker-compose.prod.yml: image-based, no builds, GO2RTC_PUBLIC_URL required | S9-04 |
| [`S9-07`](kanban/cards/S9-07.md) | /health + /api/healthz with detector heartbeat (fresh/stale) | S1-13, S5-09 |
| [`S9-08`](kanban/cards/S9-08.md) | scripts: dev.sh, verify.sh, detect-smoke.sh, e2e-stack.sh, coverage.sh, fetch-fixture.sh | S9-04, S10-02, S10-03 |
| [`S9-09`](kanban/cards/S9-09.md) | .github/workflows/build.yml: four jobs | P-01, S1-14, S5-11, S8-11 |
| [`S9-10`](kanban/cards/S9-10.md) | Docs content: architecture, configuration, api, installation (decisions, not filler) | S0-02, S6-05, S7-09, S9-06 |

### Step 10 — Demo fixture, seeding, end-to-end

| Card | Title | Depends |
| --- | --- | --- |
| [`S10-01`](kanban/cards/S10-01.md) | testdata/fixtures.json + scripts/fetch-fixture.sh (pinned assets, recorded hashes) | S0-01 |
| [`S10-02`](kanban/cards/S10-02.md) | Demo camera seeded through the public API + the go2rtc demo-only source | S3-02, S3-03, S9-04, S10-01 |
| [`S10-03`](kanban/cards/S10-03.md) | verify.sh's nine gates pass (the v1 acceptance gate) | S2-06, S3-03, S4-05, S6-04, S7-08, S7-10, S6-06, S9-07, S10-02, S2-05 |
| [`S10-04`](kanban/cards/S10-04.md) | fs storage tier seeded as default (local-tier) and exercised by verify.sh gate 7 | S7-03, S7-08, S10-02 |

### Step 11 — Test suite CI runs

| Card | Title | Depends |
| --- | --- | --- |
| [`S11-01`](kanban/cards/S11-01.md) | Go suite green: go2rtc, media, storage, sweeper, sso, api, webhooks, ws | S2-11, S3-06, S4-07, S6-05, S7-10, S7-09, S6-06 |
| [`S11-02`](kanban/cards/S11-02.md) | Detector suite green: pytest -m 'not slow' + the slow smoke | S5-11 |
| [`S11-03`](kanban/cards/S11-03.md) | Console suite green: typecheck + vitest | S8-11 |
| [`S11-04`](kanban/cards/S11-04.md) | E2E suite green: scripts/e2e-stack.sh test (Playwright chromium) | S8-12, S10-02 |
| [`S11-05`](kanban/cards/S11-05.md) | Coverage ratchet recorded (coverage.sh --record) | S11-01, S11-02, S11-03 |

### Process — Repo mechanics

| Card | Title | Depends |
| --- | --- | --- |
| [`P-02`](kanban/cards/P-02.md) | Record the go.mod pin bump versus the plan (gin 1.12.0, x/crypto 0.57.0, gofakes3 v1.2.0) | S1-02 |

## Done


### Step 0 — Repo scaffold

| Card | Title | Depends |
| --- | --- | --- |
| [`S0-05`](kanban/cards/S0-05.md) | License, gitignore, env example, coverage baseline | — |
| [`S0-06`](kanban/cards/S0-06.md) | docs/configuration.md = the env table, verbatim | S1-01 |

### Step 1 — Config, models, store, migrations

| Card | Title | Depends |
| --- | --- | --- |
| [`S1-01`](kanban/cards/S1-01.md) | Config: env table, Load(), EnsureDirs, Version/GitCommit | — |
| [`S1-02`](kanban/cards/S1-02.md) | Pinned dependency set (go.mod + deps pin package) | — |
| [`S1-03`](kanban/cards/S1-03.md) | Models: Camera (+Zone/ZoneError), Segment, Event (+EventObject) | S1-01 |
| [`S1-04`](kanban/cards/S1-04.md) | Models: StorageBackend, StorageJob | S1-01 |
| [`S1-05`](kanban/cards/S1-05.md) | Models: Webhook, WebhookDelivery | S1-01 |
| [`S1-06`](kanban/cards/S1-06.md) | Models: User, Role, UserRole, Session, PendingIdentity | S1-01 |
| [`S1-07`](kanban/cards/S1-07.md) | Models: SSOProvider, Audit, Setting + JSON-text helpers | S1-01 |
| [`S1-08`](kanban/cards/S1-08.md) | Store core: New(dsn), NewWithDB(db), Postgres -> sqlite fallback | S1-01, S1-09 |
| [`S1-09`](kanban/cards/S1-09.md) | Migrations: baseline schema over all 15 models + versioned migration table | S1-03, S1-04, S1-05, S1-06, S1-07 |
| [`S1-10`](kanban/cards/S1-10.md) | Store: cameras, events, segments | S1-08, S1-09 |
| [`S1-11`](kanban/cards/S1-11.md) | Store: storage backends + jobs, settings, webhooks | S1-08, S1-09 |
| [`S1-12`](kanban/cards/S1-12.md) | Store: users, roles, sessions, sso_providers, audit | S1-08, S1-09 |
| [`S1-13`](kanban/cards/S1-13.md) | main.go boot order + router with /health | S1-01, S1-08 |
| [`S1-14`](kanban/cards/S1-14.md) | Step 1 suite green in golang:1.26 (go build + go test ./...) | S1-01, S1-02, S1-03, S1-04, S1-05, S1-06, S1-07, S1-08, S1-09, S1-10, S1-11, S1-12, S1-13 |

### Step 2 — Auth: JWT, break-glass admin, OIDC, WebSocket

| Card | Title | Depends |
| --- | --- | --- |
| [`S2-01`](kanban/cards/S2-01.md) | auth/jwt.go: HS256 access + refresh with rotation and reuse detection | S1-06, S1-12 |
| [`S2-02`](kanban/cards/S2-02.md) | auth/argon2.go: password hash + verify | — |

### Step 6 — Events, clips, snapshots, webhooks

| Card | Title | Depends |
| --- | --- | --- |
| [`S6-03`](kanban/cards/S6-03.md) | Event upsert semantics: open/extend/close on the server clock | S1-10 |

### Process — Repo mechanics

| Card | Title | Depends |
| --- | --- | --- |
| [`P-00`](kanban/cards/P-00.md) | git init + baseline commit + this board | — |
| [`P-01`](kanban/cards/P-01.md) | Create the GitHub remote and push main | P-00 |
| [`P-03`](kanban/cards/P-03.md) | Fix the red store test: empty-subject account lookup | — |
| [`P-04`](kanban/cards/P-04.md) | Keep the board honest (move cards, record evidence) | P-00 |
| [`P-05`](kanban/cards/P-05.md) | 'Track server/models: .gitignore''s models/ pattern ignored a Go package' | S0-05, S1-03, S1-04, S1-05, S1-06, S1-07 |

## Board health

- ✓ no violations: columns valid, WIP within limit, no Done card without evidence.

Regenerate with `python3 kanban/board.py` in the same commit as the card move.

# nvrWay

Self-hosted NVR: RTSP camera ingest, continuous **and** event-triggered
recording, YOLO detection with per-camera polygon zone rules, OIDC login with a
break-glass local admin, a web console, and pluggable remote storage. Target
scale 4–8 cameras on one server.

## Tech Stack

| Layer | Choice |
|---|---|
| Control plane | Go (`server/`, gin + gorm), module `nvr/server` |
| Detection | Python worker (`detector/`), YOLOv8n **ONNX** on `CPUExecutionProvider` (CUDA via compose overlay) |
| Media | go2rtc (`ghcr.io/alexxit/go2rtc:master`) for ingest/WebRTC, ffmpeg for segments + clips |
| Console | React 18 + Vite + Tailwind + vitest + Playwright (`web/`, package `nvr-web`) |
| Data | Postgres 16 (sqlite for tests/dev), **no Redis** — single server instance |
| Storage | `s3` / `webdav` / `sftp` / `fs` drivers, local-first with an async upload tier |
| Auth | Own OIDC authorization-code + PKCE + nonce, JWT sessions, break-glass local admin |

## Architecture

```
cameras ──RTSP──► go2rtc ──┬── RTSP ──► server/media recorder ──► segments (mp4)
                           │                                     │
                           └── mjpeg tap (<slug>_det) ──► detector│ (YOLO + zones)
                                                                       │
                          events ◄── internal API signals ─────────────┘
                            │
                            ├── clip assembly (pre-roll + post-roll concat)
                            ├── WebSocket fan-out + per-camera webhooks (HMAC)
                            └── storage tier (async upload, retention sweeps)
```

Three rules the shape follows:

- **The server clock owns event time.** A worker's timestamp never reaches the
  database, so host clock skew cannot create events in the future.
- **One server process owns the media tree.** Recording, clip assembly and the
  retention sweeper are single-writer by design — which is why there is no
  Redis and no second instance.
- **Playback is proxied by the server.** Local files are served with Range
  support; a clip that lives only on a remote tier streams from byte 0
  (seek-on-remote is a v2 item, stated in `docs/architecture.md`).

## Project Structure

```
server/       Go control plane: config, models, store, auth, sso, api, ws,
              media (recorder + clips), go2rtc, storage, sweeper
detector/     Python detection worker (nvr_detector) + tests
web/          React console (live, events, cameras, settings, audit)
docker/       Dockerfiles (server, web, detector) + go2rtc template (the three
              compose files — dev, prod, GPU overlay — live at the repo root)
testdata/     pinned fixtures (bus.jpg, yolov8n) — downloaded, not committed
scripts/      dev.sh, verify.sh, detect-smoke.sh, e2e-stack.sh, coverage.sh
docs/         architecture, configuration, api, installation, kanban (how the board works)
kanban/       the work board: KANBAN.md (generated) + cards/ (one per deliverable) + seed.py
```

## Quick Start

```bash
scripts/dev.sh --demo             # seeds .env from .env.dev.example, compose up, demo camera
scripts/verify.sh                 # the acceptance gate: 9 observed passes
scripts/detect-smoke.sh           # yolov8n.onnx + bus.jpg, CPU inference, in-image
scripts/fetch-fixture.sh          # pinned assets into testdata/, byte counts verified
scripts/e2e-stack.sh test         # Playwright against a throwaway sqlite stack
scripts/coverage.sh               # coverage ratchet against coverage.baseline.json
cd server && go test ./... -count=1
cd detector && python3 -m pytest tests -q -m "not slow"
cd web && npm run typecheck && npm run test:run
```

Console on `http://localhost:3000`, API on `:8080`, go2rtc on `:1984`.

`scripts/` is the harness, and every command above is a script in it. The six
scripts exist today as skeletons with their usage lines (card S0-04) — they
print what they will do and exit 2, so a skeleton can never be mistaken for a
green gate; `scripts/dev.sh`, `verify.sh`, `detect-smoke.sh`, `e2e-stack.sh`,
`coverage.sh` and `fetch-fixture.sh` get their content in Step 9 (S9-08).

Work is tracked one card per deliverable in [`KANBAN.md`](KANBAN.md) — cards in
[`kanban/cards/`](kanban/cards) cite the plan step they implement and carry the
command that proves them; [`docs/kanban.md`](docs/kanban.md) is the convention.

## Roadmap

| Version | Scope |
|---|---|
| v1 | RTSP ingest, continuous/event/off recording, YOLO zones (`intrusion`, `loiter`), OIDC + local admin, console (live, events, cameras, settings), storage tiers + retention, signed webhooks |
| v2 | Face/LPR classes, seek on remote storage, ONVIF events/presets surfaced, multi-instance media ownership, HLS path where WebRTC cannot reach |

## Key Design Decisions

- **CPU ONNX is the verified baseline**, not the fallback. `yolov8n` at
  5 FPS / 640×360 is sized for ~4 cameras on 4 vCPU; CUDA ships as the
  `docker-compose.gpu.yml` overlay, verified on a host with the NVIDIA container toolkit.
- **Slug is the stream name.** `^[a-z0-9][a-z0-9-]{1,31}$`, unique, and used
  verbatim by go2rtc — there is no mapping table to drift.
- **Camera writes always re-render go2rtc's config** from the rows, so config
  cannot diverge from the database.
- **Zones are normalized coordinates** (`0..1` of the detection frame), so a
  camera resolution change does not silently invalidate a rule.
- **Un-uploaded footage is protected** by the sweeper's window: a storage
  outage never destroys the only copy.
- **Secrets are sealed at rest** (AES-256-GCM) and never echoed by any endpoint
  — `secret_set: true` is what the console sees.

## License

MIT — see `LICENSE`. Detection assets (`yolov8n`, `bus.jpg`) are Ultralytics
assets under AGPL-3.0 / CC-BY-4.0 and are downloaded at build time, never
committed; attribution is in `testdata/fixtures.json`.

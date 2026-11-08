# Contributing to nvrWay

Thanks for contributing! This document describes the repository layout, the
verification gate every change must pass, the board a change is tracked on,
and the conventions the codebase follows.

<!-- skeleton: S0-02 (2026-11-02). Layout and the board are real today; the
     gate commands are the names Step 9 ships (S9-08), so they are written
     here verbatim and marked. Prose lands with S9-10. -->

## Repository layout

| Path | What it is |
|------|-----------|
| `server/` | Go control plane (module `nvr/server`): config, models, store, auth, sso, api, ws, media (recorder + clips), go2rtc client, storage drivers, sweeper, webhooks |
| `detector/` | Python detection worker (package `nvr_detector`): mjpeg tap, YOLOv8n ONNX decode, tracker, zones, events, motion gate, signal client |
| `web/` | React 18 + Vite + Tailwind console (package `nvr-web`), vitest unit suite + Playwright e2e |
| `docker/` | `Dockerfile.server`, `Dockerfile.web`, `Dockerfile.detector`, dev `docker-compose.yml`, `go2rtc.yaml.template`, `nginx.conf` |
| `docker-compose.prod.yml` | Production compose: published images, no build sections |
| `scripts/` | `dev.sh`, `verify.sh`, `detect-smoke.sh`, `e2e-stack.sh`, `coverage.sh`, `fetch-fixture.sh` |
| `testdata/` | pinned fixture manifest (`fixtures.json`); the assets themselves are downloaded, never committed |
| `docs/` | architecture, configuration, api, installation, kanban (how the board works) |
| `kanban/` | the work board: `KANBAN.md` (generated), `cards/` (source of truth), `board.py`, `seed.py`, `pi_board.py` |

One Go module (`server/go.mod`), one Python package, one npm package. There is
no `go.work` and no second Go module.

## The verification gate

A change is not done until the gate is green. The commands are the harness
names the README promises; the files are filled by S9-08.

```sh
# Server (no Go toolchain on the host — run it in the golang:1.26 image)
cd server && go test ./... -count=1

# Detector (host Python cannot host the ML worker — run it in the image)
cd detector && python3 -m pytest tests -q -m "not slow"

# Console
cd web && npm run typecheck && npm run test:run

# Coverage ratchet (never below coverage.baseline.json)
scripts/coverage.sh

# The acceptance gate for the whole system: 9 observed passes
scripts/dev.sh --demo && scripts/verify.sh
```

State, in prose: which of these CI runs on every push (`.github/workflows/build.yml`,
S9-09), which need Docker, and which need a camera-free fixture.

## The board (`docs/kanban.md`)

Every deliverable is a card in `kanban/cards/`, one per module / route group /
driver / suite. Pick the card before writing code, move it
`backlog → in-progress → review → done`, and put the card move, the
regenerated `KANBAN.md` and the code in **the same commit**. In Progress is
capped at 2. A card is Done only with its verification command's output
quoted in Notes. `python3 kanban/board.py --check` is the pre-commit habit.

## Conventions

The rules a reviewer enforces, each with the reason:

- **The server clock owns event time.** No worker timestamp reaches the
  database (`docs/architecture.md`, card S6-03). A signal carrying a future
  `ts` must never produce `started_at > now()`.
- **One server process owns the media tree.** No Redis, no second instance:
  recording, clip assembly and the sweeper are single-writer
  (`docs/architecture.md`).
- **Slug is the stream name.** `^[a-z0-9][a-z0-9-]{1,31}$`, unique, used
  verbatim by go2rtc — no mapping table that can drift (S3-02).
- **Every camera write re-renders go2rtc's config** from the rows, including
  at boot (S3-05).
- **Zones are normalized coordinates** (`0..1` of the detection frame), so a
  resolution change does not invalidate a rule (S5-05).
- **Secrets are sealed at rest and never echoed**; a backend row carries
  `secret_set: true` (S7-02, S7-10).
- **Store tests use sqlite** (`store.New(":memory:")`, `NewWithDB`); Postgres
  is the deployment target, not the test target.
- **Tests assert behavior, never snapshots** (console suite, S8-11).
- **Docs carry decisions, not filler** — a doc section that states no
  decision is a heading, and a heading is not documentation (S9-10).

## Installing locally

The quick start as the README states it: `scripts/dev.sh --demo` — on a
fresh clone it seeds `.env` from `.env.dev.example` (dev credentials,
gitignored from then on), builds the stack, and seeds the demo camera.
Console on `:3000`, API on `:8080`, go2rtc on `:1984`. A real deployment
never runs this: `docs/installation.md` walks the prod compose with a `.env`
copied from `.env.example` and generated secrets.

## Releasing

What a release is: a `v*` tag on `main`, which is the only thing that
publishes `ghcr.io/welcometotheweb/nvr/{server,web,detector}:latest`
(S9-09); `main` publishes `:edge`. State the pre-release checklist
(gate green, coverage recorded, cards moved to Done) and where release notes
live.

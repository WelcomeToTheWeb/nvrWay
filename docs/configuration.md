# Configuration

Every variable the server reads, its default, and what it means. Defaults are
what the server runs with when the variable is **absent**; `.env.example` is
this same table as an env file.

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
| `GO2RTC_COMPOSE_FILE` | empty | compose project file the go2rtc service lives in; **empty disables the live restart** |
| `GO2RTC_COMPOSE_DIR` | empty | project directory compose runs in (relative paths inside the project resolve as they do for the operator's own `docker compose up`) |
| `GO2RTC_COMPOSE_SERVICE` | `go2rtc` | the compose service name the server recreates |
| `GO2RTC_COMPOSE_BIN` | `docker` | compose binary (`docker` for the plugin, `docker-compose` for standalone) |
| `GO2RTC_RESTART_TIMEOUT` | `60s` | bound on the recreate itself |
| `GO2RTC_RESTART_SETTLE` | `2s` | grace before the first probe of the live stream map |
| `GO2RTC_RESTART_PROBE_BUDGET` | `30s` | bound on waiting for the streams to appear |

## The go2rtc restart (S3-07)

go2rtc instantiates its streams **at startup**. A camera write pushes the rendered
config (`POST /api/config`, which writes the file and validates it), and the
streams in that file are not live until the sidecar boots from it — measured on
go2rtc 1.9.15 on 2026-11-02 (card S3-05's Notes). So making a camera live means
recreating the sidecar, and the server does it:

```
<GO2RTC_COMPOSE_BIN> compose --project-directory <GO2RTC_COMPOSE_DIR> \
    -f <GO2RTC_COMPOSE_FILE> up -d --no-deps --force-recreate <GO2RTC_COMPOSE_SERVICE>
```

`--no-deps` is what keeps it safe to run from inside the app: the recreate touches
one service, the media sidecar, and not the manager, recorder, detector and media
tier it lives beside.

- **`GO2RTC_COMPOSE_FILE` empty is a supported installation**, not a broken one:
  dev runs go2rtc outside compose. The writes then report that the camera is saved
  and its stream goes live at the sidecar's next boot, and `GET
  /api/v1/cameras/:id/probe` answers "present and offline" for a camera the running
  sidecar has never booted.
- This is a deliberate privilege exception, recorded on 2026-11-02: the app
  container may recreate **its own go2rtc service**, which means it needs the
  Docker socket and the project file mounted. It is scoped to one command, one
  service, both named by configuration — the code contains no Docker client, and
  the suite asserts the argument list.
- A restart failure is its own answer: `502` with `config_applied: true`,
  `sidecar_restarted: false` and compose's own words. The camera row stays, the
  next write repairs it, and the ledger has the attempt.
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

## Not in the table above

| Env | Default | Meaning |
|---|---|---|
| `NVR_SECRET_KEY` | empty → derived from `JWT_SECRET` | 64 hex chars sealing storage-backend credentials (AES-256-GCM). **Required in production**: a deployment with `NVR_SECRET_KEY` set has one secret to back up, and sealing is stable across restarts. |

## Generating the secrets

```bash
openssl rand -hex 32   # JWT_SECRET
openssl rand -hex 32   # NVR_SECRET_KEY
openssl rand -hex 32   # WORKER_TOKEN
```

`JWT_SECRET` empty is a working configuration with a stated cost: every boot
mints a fresh signing key, so all sessions die with the process. Set it for
sticky sessions.

`WORKER_TOKEN` empty is not "no auth for the internal API": it is the internal
API switched off. `/api/internal/*` answers `403 {"error":"internal api
disabled"}`, which means a detection worker pointed at this server can do
nothing — the state is visible in the API, not silent.

`BOOTSTRAP_ADMIN_PASSWORD` empty means no local admin account is created: an
installation whose OIDC is fully configured logs in only through its IdP. An
installation with neither OIDC nor a bootstrap password has no way in, and the
boot line says so.

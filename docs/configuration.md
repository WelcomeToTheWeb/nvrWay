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

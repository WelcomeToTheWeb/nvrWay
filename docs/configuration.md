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
| `GO2RTC_USERNAME`/`GO2RTC_PASSWORD` | empty | basic auth for go2rtc API — **breaks browser live view when set**; see below |
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
| `WORKER_TOKEN` | empty → internal API disabled | bearer for `/api/internal/*` |
| `BOOTSTRAP_ADMIN_USER`/`BOOTSTRAP_ADMIN_PASSWORD` | `admin` / empty | empty password = no local admin created |
| `OIDC_ISSUER`,`OIDC_CLIENT_ID`,`OIDC_CLIENT_SECRET` | empty | seeds provider row `oidc` on first boot (discovery at boot; failure logs, never fatal) |
| `OIDC_ROLE_CLAIM`, `OIDC_ROLE_MAPPING` | `groups`, `""` | claim name + `group=role,group=role` |
| `SEGMENT_SECONDS` | `5` | segment length |
| `PRE_ROLL_SECONDS`/`POST_ROLL_SECONDS` | `8`/`15` | event clip window — served to the console by `GET /api/v1/settings/media`, so the drawer's strip spans the padded window instead of guessing (P-11) |
| `EVENT_MAX_SECONDS` | `300` | forced event close |
| `LOCAL_RETENTION_DAYS` | `14` | global default retention |
| `REMOTE_RETENTION_DAYS` | `30` | 0 = keep remote forever |
| `EVENT_RETENTION_DAYS` | `90` | event rows + media age-out |
| `UPLOADER_TICK_SECONDS` | `30` | upload queue tick |
| `RATE_LIMIT_PER_MINUTE`/`RATE_LIMIT_BURST` | `120`/`0` | per-user REST budget (burst 0 = a tenth) |
| `WS_ORIGINS`, `WEB_URL` | `""`, `http://localhost:3000` | WS origin guard, console origin |
| `PUBLIC_BASE_URL` | empty → `http://localhost:8080` | the origin a **receiver** dials; builds the webhook payload's `snapshot_url` and `clip_url` |
| `DET_PROVIDERS` | `CPUExecutionProvider` | forwarded in the detector config payload |
| `TZ` | `UTC` | — |

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

### Why `PUBLIC_BASE_URL` exists, and what an empty value costs

A webhook payload carries two URLs — the event's picture and its clip — and both
have to be dialable by the machine that received the notification. That is the one
address this server cannot know from the inside:

- the bind address is `:8080`, which names no host;
- the `Host` header of the worker's signal POST is the name the *worker* dials,
  which inside compose is `server:8080` and is useless to a Home Assistant or n8n
  instance outside that compose network;
- `GO2RTC_PUBLIC_URL` is the browser-facing base for *live view*, which is a
  different process on a different port.

## The sidecar API is a trusted-LAN service (P-14)

**The sidecar API is a trusted-LAN service (P-14).** Setting
`GO2RTC_USERNAME`/`GO2RTC_PASSWORD` makes the server render them into the
sidecar's `api:` block, and go2rtc then answers `401` to every browser
request — including the live view's `/api/ws` WebSocket, which a browser
**cannot** attach basic auth to (the WebSocket API offers no header, and the
console holds no sidecar credential to attach even if it could). With the
credentials set, live view fails as `unreachable` on every tile, and the
server logs a warning naming this at boot.

Why the answer is network placement and not a server-side WS proxy: the
sidecar is browser-facing *by design* — WebRTC media reaches its `8554`/`8555`
ports directly from the browser, so proxying only the control WebSocket does
not remove the LAN exposure it would be protecting against. The honest
choices are therefore: leave the credentials unset on a trusted network (the
template's default, and what the e2e stack runs), or set them and
reverse-proxy the sidecar so the proxy — not the browser — carries the auth.
`docker/.env.example` ships them commented out with this trade stated.

So the installation states it. Set it to the origin an outside machine uses —
`http://192.168.1.20:8080`, or `https://cam.example.com/nvr` behind a reverse proxy
that mounts the API under a path prefix (the prefix is preserved). Left empty the
payload carries `http://localhost:8080/...`, which works for a receiver on the same
host and reads to everyone else as a broken webhook; the server logs that fact once
at boot rather than letting it be deduced from a receiver's 404s.

The plan's configuration table names no such variable. The payload shape the same
plan specifies (`snapshot_url`, `clip_url`) requires one.

## Not in the table above

| Env | Default | Meaning |
|---|---|---|
| `NVR_SECRET_KEY` | empty → derived from `JWT_SECRET` | 64 hex chars sealing storage-backend credentials and webhook signing secrets (AES-256-GCM). **Required in production**: a deployment with `NVR_SECRET_KEY` set has one secret to back up, and sealing is stable across restarts. |

### What sealing does, and what losing the key costs

Every credential the server stores — a storage backend's password, access key,
secret key and private-key path, and a webhook's signing secret — is written to
the database sealed, never as plaintext. The format is
`base64(nonce‖AES-256-GCM(ciphertext)‖tag)` under a key that is
`unhexlify(NVR_SECRET_KEY)`, or `SHA-256(JWT_SECRET)` when it is unset
(`server/storage/seal.go`).

Three consequences an operator should know before they need them:

- **A database backup is readable and not a credential leak.** Restoring it
  onto a host with the same `NVR_SECRET_KEY` restores every backend. Restoring
  it onto one without it leaves every row present and every credential
  unopenable: the server logs the failure per backend and the Settings page
  shows `secret_set: true` next to a connection test that says so.
- **Losing `NVR_SECRET_KEY` means retyping the credentials**, not losing the
  footage. Set it on the first boot, and back it up with `JWT_SECRET`.
- **Deriving it from `JWT_SECRET` is the convenience default, and it has a
  date on it.** Rotating `JWT_SECRET` on a deployment that never set
  `NVR_SECRET_KEY` re-keys the seal and orphans every stored credential. That
  is why the boot warns.

No endpoint ever returns a sealed value. The response field that says a
credential is present is `secret_set: true`, and a write that omits the field
leaves the stored credential alone — the console cannot read it back, so it
must not have to send it back.

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

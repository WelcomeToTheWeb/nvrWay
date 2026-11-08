# nvrWay — Installation Guide

Self-hosted install of nvrWay v1: one server, one Postgres, one go2rtc, one
detection worker, one console. Target scale 4–8 cameras on one machine.

## Table of Contents

[Prerequisites](#prerequisites) · [1. Get the images](#1-get-the-images) ·
[2. Generate the secrets](#2-generate-the-secrets) · [3. Write the `.env`](#3-write-the-env) ·
[4. Start the stack](#4-start-the-stack) · [5. First login](#5-first-login-the-break-glass-admin) ·
[6. Camera setup](#6-camera-setup) · [7. OIDC provider setup](#7-oidc-provider-setup) ·
[8. Storage tier setup](#8-storage-tier-setup) · [9. Webhooks](#9-webhooks) ·
[10. Retention math](#10-retention-math) · [11. TLS and remote view](#11-tls-reverse-proxy-and-remote-view) ·
[12. GPU overlay](#12-gpu-overlay-shipped-verified-on-a-gpu-host) ·
[13. Verify](#13-verify-the-installation) · [14. Backup/upgrade/restore](#14-backup-upgrade-restore) ·
[15. Uninstall](#15-uninstall) · [Troubleshooting](#troubleshooting)

## Prerequisites

- Docker with the Compose v2 plugin (`docker compose version`).
- **~4 vCPU and 4 GB** for the CPU-ONNX baseline of 4–8 cameras — the
  detector's motion gate is what makes that number honest on quiet cameras
  (see `docs/architecture.md` § Detection for the sizing basis).
- A network the cameras can reach, and disk sized by
  [§ 10](#10-retention-math) before you add the fourth camera.
- **No Go toolchain and no usable host Python are required** — every build
  and test in this repo runs in an image.
- GPU (optional, for the overlay only): NVIDIA container toolkit on the host
  and `/dev/nvidia*` nodes in the container's reach. Without them the CPU
  path is the only supported one, and it is a supported one, not a consolation
  prize. See [§ 12](#12-gpu-overlay-shipped-verified-on-a-gpu-host).

## 1. Get the images

```bash
docker pull ghcr.io/welcometotheweb/nvr/server:latest
docker pull ghcr.io/welcometotheweb/nvr/web:latest
docker pull ghcr.io/welcometotheweb/nvr/detector:latest
```

`:latest` is a stable release; `:edge` follows `main`. Pin a tag for a fleet
that must not move on its own. Build-your-own is first-class:
`docker-compose.yml` builds all three from this checkout (that is what
`scripts/dev.sh` does), and the prod compose is the same services with
`image:` instead of `build:`.

## 2. Generate the secrets

Four values must be random and secret:

```bash
openssl rand -hex 32   # → JWT_SECRET
openssl rand -hex 32   # → NVR_SECRET_KEY
openssl rand -hex 32   # → WORKER_TOKEN
openssl rand -base64 24   # → POSTGRES_PASSWORD
```

What each one *not* being set costs:

- **`JWT_SECRET` empty** — every boot mints a fresh signing key: all sessions
  die with the process, so a restart logs everyone out. Fine for a first
  spin; set it before you rely on it.
- **`NVR_SECRET_KEY` empty** — the sealing key is derived from `JWT_SECRET`,
  so rotating `JWT_SECRET` later re-keys the seal and orphans every stored
  storage-backend credential and webhook secret (they stay in the database,
  unopenable; you retype them). Set it on the first boot and back it up with
  `JWT_SECRET`.
- **`WORKER_TOKEN` empty** — the internal API is **off**, not open:
  `/api/internal/*` answers `403 {"error":"internal api disabled"}`. No
  detection, no heartbeat, no events. The camera still records.

## 3. Write the `.env`

```bash
cp .env.example .env
```

Every variable and its default is the table in
[`docs/configuration.md`](configuration.md). The server container receives
this whole file (`env_file` in the compose), so anything you set here
arrives — the exceptions are `SERVER_PORT`, `DATA_DIR` and `DATABASE_URL`,
which the compose environment block owns and overrides, and
`POSTGRES_PASSWORD`, which the compose file itself consumes. The
deployment-only values — the ones whose dev defaults are wrong for a real
install:

| Variable | Why it is deployment-only |
|---|---|
| `POSTGRES_PASSWORD` | **required in prod compose** (compose refuses to resolve without it): it sets the database password *and* builds the server's container DSN from it — the `.env` `DATABASE_URL` line is for the non-container paths only |
| `DATA_DIR` | the media tree; make it a volume you back up |
| `WEB_URL` | the origin the console is served from — the WS origin guard and the OIDC redirect URI are both built from it |
| `GO2RTC_PUBLIC_URL` | **required in prod compose** (compose refuses to start without it): it is the address the *browser* dials for live view, and the server cannot guess it from inside the container |
| `PUBLIC_BASE_URL` | the address a webhook *receiver* dials; empty means the payload says `localhost` and every remote integration 404s |

## 4. Start the stack

```bash
# production images:
docker compose -f docker-compose.prod.yml up -d
# dev, from a checkout:
scripts/dev.sh            # compose up --build
scripts/dev.sh --demo     # + a seeded demo camera, no hardware
```

What answers where:

| Port | What | May face |
|---|---|---|
| `:3000` | console (nginx; proxies `/api` and `/ws` to the server) | the LAN; a WAN only behind TLS + auth (§ 11) |
| `:8080` | server API + `/ws` | the LAN; never a bare WAN socket — it is plain HTTP |
| `:1984` | go2rtc API (live view's `/api/ws`) | the LAN only — it is a **trusted-LAN service** (P-14): the browser cannot attach auth to a WebSocket, so this API is unauthenticated by design |
| `:8554` / `:8555` | go2rtc RTSP / WebRTC | the LAN; cameras and browsers dial these directly |

Nothing here needs to face a WAN. Remote view is a reverse proxy or a
Tailscale interface (§ 11), not a port forward.

## 5. First login: the break-glass admin

`BOOTSTRAP_ADMIN_USER` / `BOOTSTRAP_ADMIN_PASSWORD` seed one local admin on
first boot (S2-03). The password is **the value you set in `.env`** — it is
never generated for you, so there is nothing to go find in a log. The account
exists so OIDC misconfiguration cannot lock you out of your own NVR: its
password door is never closed by a link or a role edit, and deleting the last
admin is refused.

To stop it from being created: leave `BOOTSTRAP_ADMIN_PASSWORD` empty. An
installation with working OIDC and no bootstrap password logs in only through
its IdP — which is the right end state; do it once OIDC is verified, not
before. An installation with neither has no way in, and the boot line says so
rather than letting you discover it from a login form.

## 6. Camera setup

In the console: **Cameras → add**. The fields that matter:

- **slug** — lower-case, and then **it is the go2rtc stream name forever**.
  The console shows it and refuses to edit it, because renaming moves every
  key the camera ever wrote (segments, tier objects, the tap).
- **source_uri** — what go2rtc dials: `rtsp://user:pass@192.168.1.50:554/…`
  for a real camera. The demo camera's `exec:…` shape (an ffmpeg feeding a
  still image as a stream) is the seam the test suite and `--demo` use; it is
  a supported source kind, not a hack.
- **record_mode** — `continuous` (always recording), `events` (record only
  around detections), `off` (live view only).
- **detection fps/size** — the tap defaults (5 FPS, 640×360) are the sizing
  basis; raising them costs CPU per camera.

Then **probe** (the server asks the sidecar whether the stream is live), and
**draw zones** on a real snapshot — zones are normalized coordinates, so they
survive a tap resolution change.

Two facts the UI cannot show you: a camera write **re-renders go2rtc's
config** (so the config file is never hand-edited — your edit is gone on the
next write), and the render only takes effect when the sidecar reboots, which
the supervisor does for you (P-16) — a live tile may need a page reload
right after a save.

## 7. OIDC provider setup

`PUT /api/settings/oidc` with `issuer`, `client_id`, `client_secret`, and the
role claim + `group=role` mapping. Discovery runs at save time, and the row
carries what the IdP said (its endpoints, the signing algorithms it publishes,
whether it accepts `S256`) — the login is built from that, not from guesses.

**The redirect URI the operator must whitelist, verbatim:**

```
${WEB_URL}/api/auth/sso/<provider>/callback
```

On the default ports, with `WEB_URL=http://localhost:3000` and a provider named
`oidc`, that is exactly:

```
http://localhost:3000/api/auth/sso/oidc/callback
```

`GET /api/auth/sso/providers` answers this string per provider
(`redirect_uri`), and the authorize route refuses with it in the message when
the row carries a different one — the 422 says the string to paste, because
`invalid_redirect_uri` from the IdP is the failure this setup produces.

After that, in the IdP: the confidential client's secret, and one registered
redirect URI per provider row. Then, per provider row, the four contract knobs
and their effect on a real login:

| Knob | What it does to a login |
|---|---|
| **`viewer` JIT floor** | A new SSO user is provisioned as `viewer` and nothing else: they watch, they do not change. A new user sees an empty event list until you map a role. |
| **Role mapping** | `group=role` maps an IdP group to a role on *every* login, so an IdP that drops someone out of `nvr-operators` takes their `operator` role at the next sign-in. An empty mapping is not "everyone is admin": the floor stands. `operator` and `admin` are refused in the mapping response when no role row owns them (422), because mapping to a role nobody can un-grant is the same dead end as the break-glass admin with no password. |
| **`require_verified_email`** | Default on: the login requires the provider's signed `email_verified` claim. An IdP that does not assert it is switched off for this gate, or everyone is locked out. |
| **`allowed_domains`** | An allow-list on the asserted email's domain. Empty is no restriction; a list is a wall. |
| **`link_policy=pending`** | The default. An assertion whose email belongs to an existing account is **not** that account: the login is refused, the assertion is parked in `GET /api/identities`, and an admin approves it (its own account, `viewer` floor), links it (merged into the account it pointed at, audited, that account's old tokens stopped), or refuses it. `link_policy=link` is the operator's explicit choice to keep match-by-email, and it is audited too. |

There is no way to lock the break-glass admin out of the installation: its
password door is never closed by a link (`GET /api/users/:id` reports
`password_login: true` for it), and deleting the last admin is refused.

## 8. Storage tier setup

Settings → Storage → add a backend (`fs` / `s3` / `webdav` / `sftp`), then
**test** (the four answers: reachable, credentials, bucket/root present,
writable), then **set default**, then **enable upload**. A disabled backend
stops the queue without losing jobs — the rows wait.

Worked examples:

**MinIO** (self-hosted S3):

```
kind: s3   endpoint: minio:9000   bucket: nvr   prefix: ""
access_key / secret_key: the service account
region: us-east-1     force_path_style: true     enable_ssl: false
```

Path-style is the switch MinIO needs (its default vhost-style `bucket.host`
naming does not resolve inside a compose network). The dev compose ships a
commented `minio` service for exactly this test.

**Cloudflare R2** and **Backblaze B2** (S3-compatible):

```
kind: s3   endpoint: <acct>.s3.<region>.r2.cloudflarestorage.com   (R2)
           endpoint: s3.us-west-002.backblazeb2.com                (B2)
bucket: <bucket name>     region: <the region string the provider names>
access_key / secret_key: the app key
force_path_style: true    enable_ssl: true
```

**NFS as `fs`**: mount the export on the host, pass it into the server
container, and set the backend root to the mount point. The `fs` driver
checks its root on the miss path, so an unmounted volume answers
`bucket missing` (a 502) rather than pretending every clip was never recorded
(a 404) — the distinction is documented in `docs/api.md`.

Objects will appear under: `nvr/<slug>/<YYYY>/<MM>/<DD>/<basename>.mp4` for
segments, `nvr/events/<event_id>/clip` and `…/snapshot` for event media.
Prefixes are per-backend and per-purpose (S7-06); the console shows the
effective prefix per backend.

## 9. Webhooks

Settings → Webhooks: a URL, an optional signing secret, and the cameras it
fires for. The payload (full shape in `docs/api.md` § Webhooks) names the
event, the camera, the window, and absolute `snapshot_url` / `clip_url` —
which is why `PUBLIC_BASE_URL` must be the address the *receiver* dials.

Every delivery carries `X-NVR-Signature: sha256=<hmac>` over the raw body
with your secret; verify it before trusting the JSON (the api doc has the
five-line check). Deliveries retry with backoff, the attempt history is in
the console, and a slow receiver never blocks the event path — the send is
off the request's critical path by design.

## 10. Retention math

The table you size a disk with, at the shipped encode defaults (H.264, the
dev-stack `-b:v 2M`-class settings — your cameras' bitrates are the real
variable):

| Knob | Default | What it decides |
|---|---|---|
| `SEGMENT_SECONDS` | `5` | file size granularity; the CSV manifest and every window query round to this |
| `LOCAL_RETENTION_DAYS` | `14` | how long the local tree holds footage |
| `REMOTE_RETENTION_DAYS` | `30` | how long the tier holds it; **`0` = keep forever** |
| `EVENT_RETENTION_DAYS` | `90` | how long event rows (and their clip/snapshot) age out |

Arithmetic: `bytes/day ≈ bitrate × 86400`. At 2 Mbps that is ~21 GB per
camera per day; 6 cameras × 14 days ≈ **1.8 TB local**, plus clips and
snapshots (small next to the segments). Halve the bitrate, halve the disk.

And the rule that makes unattended retention safe: **a storage outage never
destroys the only copy — inside a stated window.** Un-uploaded footage is
protected for `max(3 × LOCAL_RETENTION_DAYS, 7 days)`; past that the sweep
force-deletes with a WARN, because a full disk stops recording everything.
The window is in `docs/architecture.md` § Retention; the queue depth is on
the Settings page.

## 11. TLS, reverse proxy and remote view

TLS terminates at your proxy; the server listens on plain HTTP and stays
that way. The proxy owes three things:

1. **WebSocket upgrade for `/ws`** — `proxy_set_header Upgrade
   $http_upgrade; proxy_set_header Connection "upgrade";` and long read
   timeouts (the hub is a long-lived connection; a 60 s proxy timeout looks
   exactly like a broken hub).
2. **`$http_host`, not `$host`** — the server's same-origin checks (WS guard,
   OIDC redirect building) compare origins including the port; `$host` drops
   it and every check starts failing in ways that read as auth bugs.
3. **The origin guard agrees with the proxy** — set `WEB_URL` to the public
   origin (`https://cam.example.com`) and `WS_ORIGINS` to the same, or the
   console's socket is refused while the REST calls work.

Remote view from outside the LAN, in order of preference: **Tailscale** (zero
exposed ports; set `GO2RTC_PUBLIC_URL` to the Tailscale address so the
browser dials the sidecar there), or a reverse proxy with authentication at
the proxy. What must change for live view to work remotely:
`GO2RTC_PUBLIC_URL` becomes the address the remote browser can reach on
`:1984` (proxied path or direct), and `:8554`/`:8555` (WebRTC media) must be
reachable too — if ICE cannot connect, the tile says `unreachable` and the
REST side keeps working, which is the diagnostic that tells you it is the
media path, not the console.

## 12. GPU overlay (shipped, verified on a GPU host)

The CUDA overlay is `docker-compose.gpu.yml`, and it is verified — on a host
with the NVIDIA container toolkit, which is a prerequisite, not a suggestion
(`scripts/gpu-probe.sh --cuda` is the check; it opens every device node and
runs a real CUDA compute, because NVML passing while CUDA fails is a real
failure mode this repo has lived through — `docs/gpu-on-lxc.md`).

```bash
# from the repo root, either stack:
docker compose -f docker-compose.yml -f docker-compose.gpu.yml up -d
docker compose -f docker-compose.prod.yml -f docker-compose.gpu.yml up -d
```

Both stacks **build** the GPU image (stage 3 of `docker/Dockerfile.detector`
— the CUDA variant is not published to ghcr: it is ~2 GB of CUDA wheels and
the CPU image is the baseline). The overlay changes only the detector: the
server, console, go2rtc and Postgres are identical, because GPU placement is
a worker concern and the worker is only an API client.

What to expect after `up -d`: the worker's startup line reads
`providers=CUDAExecutionProvider,CPUExecutionProvider` — that line prints
the **active** providers, so if CUDA failed to load you will see the
substitution warned and the CPU list instead. `nvidia-smi` shows the worker
holding a few hundred MiB. If CUDA is unavailable the worker **falls back to
CPU and keeps running** — the deployment degrades, it does not die.

The verified host (2026-11-07): RTX 4070 Laptop, driver 615.78.08,
nvidia-container-toolkit 1.20.1 — CUDA active on the real model, container
CPU from ~3.4 cores to under 5%, events flowing. What is **not** claimed:
performance at scale; the CPU baseline's 4–8-camera sizing stands, and the
overlay is for the site that outgrows it without buying another host.

## 13. Verify the installation

```bash
scripts/verify.sh          # the acceptance gate: 9 observed passes
scripts/detect-smoke.sh    # yolov8n.onnx + bus.jpg, CPU inference, in-image
```

The nine gates, in the operator's words:

1. server healthy **and** the detector's heartbeat fresh in `/api/healthz`;
2. login works (the break-glass admin);
3. a camera is online (the sidecar reports the stream);
4. a person/loiter event was observed end to end (detection → event row);
5. the event has a snapshot and a clip that plays;
6. segments are growing on disk and in the manifest;
7. uploads are landing on the configured tier;
8. OIDC login lands at the `viewer` floor;
9. a webhook fired and its signature verifies.

Each gate prints what it observed, not just pass/fail — a red gate names the
subsystem to open.

## 14. Backup, upgrade, restore

**Back up:** the Postgres dump (`pg_dump`), the `.env` (both secrets are in
it — treat the file as a credential), and the `DATA_DIR` volume if you want
the footage itself. The go2rtc volume is disposable: the server re-renders
it from the camera rows.

**Upgrade:** pull the new tag, `docker compose up -d`, and migrations run at
boot (versioned, forward-only). Sessions survive an upgrade only if
`JWT_SECRET` was set — that is the ephemeral-sessions cost from § 2, paid at
upgrade time.

**Restore on a fresh box:** restore the database, write the same
`NVR_SECRET_KEY` **before first boot**, mount the media volume, start the
stack. Same key → every backend credential opens. Different key → every row
is present, every credential is unopenable, and the Settings page says so per
backend (retype them; the footage is unaffected).

**Segments the old install never uploaded**: their job rows come back with
the database, and the uploader walks the queue on boot — an interrupted
upload story resumes, it does not restart.

## 15. Uninstall

```bash
docker compose down                    # containers
docker volume ls | grep nvr            # decide, per volume, keep or rm
```

The named volumes: `postgres_data` (the database), `nvr_data` (the media
tree), `go2rtc_config` (disposable), `nvr_models` (downloaded weights,
disposable). `docker compose down -v` removes them all in one line — that is
the delete-everything command, and it is the only one in this guide. What is
left under `DATA_DIR` on the host if you bind-mounted it: the same tree the
server wrote (segments, clips, snapshots) — `rm -rf` it once the database is
backed up or gone.

## Troubleshooting

The five failures an operator actually hits, each with the check to run
first:

- **Camera offline after a write.** `docker logs nvr-go2rtc` — if the
  supervisor just rebooted it (the "go2rtc.yaml changed on disk" line), wait
  one boot and re-probe. If the log shows the source failing to connect, the
  `source_uri` is wrong or the camera is unreachable from the sidecar's
  network. Probe again from the console; the probe's answer is the sidecar's
  words.
- **Live view connects, no video.** The WebSocket reached the sidecar (that
  is the "connecting" → no error part) but ICE failed — `GO2RTC_PUBLIC_URL`
  is an address this browser cannot dial. Open `http://<that URL>` in the
  same browser: if the sidecar's own page does not load, live view cannot
  either.
- **Detector `stale` in `/api/healthz`.** The heartbeat stopped: `docker logs
  nvr-detector` — the last line names what it was doing. `WORKER_TOKEN`
  mismatch between server and worker answers 403 and looks identical to a
  dead worker from the outside; the worker's startup line prints the server
  it is dialing.
- **Queue jobs stuck `pending`.** The backend is disabled (the queue stops by
  design) or the default backend is unset — Settings → Storage shows both.
  `failed` rows are a different animal: open the attempt history, fix the
  credential, requeue.
- **OIDC callback lands with no token.** The IdP refused the redirect URI:
  the registered URI must be exactly
  `${WEB_URL}/api/auth/sso/<provider>/callback` (§ 7). The server's 422 on
  the authorize route prints the exact string to paste; the IdP's error page
  usually says `invalid_redirect_uri` and means it.

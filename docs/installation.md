# nvrWay — Installation Guide

<!-- skeleton: S0-02 (2026-11-02). Headings, each naming what it must state.
     Content is card S9-10; the commands are the harness names of S9-08. -->

Self-hosted install of nvrWay v1: one server, one Postgres, one go2rtc, one
detection worker, one console. Target scale 4–8 cameras on one machine.

## Table of Contents

List the sections below once they have content.

## Prerequisites

Docker + the compose plugin, ~4 vCPU and 4 GB for the CPU-ONNX baseline, a
network the cameras can reach, and the host facts that matter: **no Go
toolchain and no usable host Python are required — every build and test runs
in an image**. State the GPU prerequisite as a prerequisite for the overlay
only (NVIDIA container toolkit + `/dev/nvidia*` nodes).

## 1. Get the images

`ghcr.io/welcometotheweb/nvr/{server,web,detector}` — `:latest` is a stable
release, `:edge` follows `main`. Pin a tag for a fleet that must not move on
its own. State the build-your-own path (`docker/docker-compose.yml` builds
from source).

## 2. Generate the secrets

The three values that must be random and secret, with the exact command:

```bash
openssl rand -hex 32   # → JWT_SECRET
openssl rand -hex 32   # → NVR_SECRET_KEY
openssl rand -hex 32   # → WORKER_TOKEN
```

State what each one not being set costs: ephemeral sessions (JWT_SECRET),
sealing key derived from JWT_SECRET so a rotation re-seals every backend
(NVR_SECRET_KEY), internal API disabled — no detection, no heartbeat
(WORKER_TOKEN).

## 3. Write the `.env`

`cp .env.example .env`, then the table in `docs/configuration.md`. Call out
the deployment-only values: `DATABASE_URL` (Postgres), `DATA_DIR`,
`WEB_URL`, and `GO2RTC_PUBLIC_URL` — **required in prod compose**, because it
is the address the browser dials, not the server.

## 4. Start the stack

```bash
docker compose -f docker-compose.prod.yml up -d
# dev, from a checkout:
scripts/dev.sh            # compose up --build
scripts/dev.sh --demo     # + a seeded demo camera, no hardware
```

Console `:3000`, API `:8080`, go2rtc `:1984` (+ `8554` RTSP, `8555`
WebRTC tcp/udp). State which of these may face a LAN and which must never
face a WAN.

## 5. First login: the break-glass admin

`BOOTSTRAP_ADMIN_USER` / `BOOTSTRAP_ADMIN_PASSWORD` seed one local admin on
first boot, recorded in settings (S2-03). State where the generated password
is found, why this account exists (so OIDC misconfiguration cannot lock you
out of your own NVR), and how to stop it from being created.

## 6. Camera setup

Add a camera from the console (`slug`, name, `source_uri`, `record_mode`,
detection fps/size), then probe it, then draw zones on its snapshot. State
that **slug is the stream name** go2rtc will use, that a camera write
re-renders go2rtc's config (so the config file is never hand-edited), and
the `exec:…` source shape the demo camera uses.

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

Add an `fs` / `s3` / `webdav` / `sftp` backend, test the connection, set the
default, enable upload. Worked examples: **MinIO** (path-style,
`EnableSSL=false`, the dev compose's commented `minio` service), **Cloudflare
R2** and **Backblaze B2** (path-style, region string, the prefix rules), an
NFS mount as an `fs` root. State the key layout objects will appear under, and
that a disabled backend stops the queue without losing jobs.

## 9. Webhooks

Per-camera webhook, the secret, the payload, the `X-NVR-Signature` HMAC, and
the delivery history the console shows (S6-07).

## 10. Retention math

The table an operator can size a disk with: `SEGMENT_SECONDS`,
`LOCAL_RETENTION_DAYS`, `REMOTE_RETENTION_DAYS` (`0` = keep remote forever),
`EVENT_RETENTION_DAYS`, and the bytes-per-camera-per-day arithmetic for the
target encode settings. State the un-uploaded protection window in plain
words: **a storage outage never destroys the only copy.**

## 11. TLS, reverse proxy and remote view

TLS terminates at the proxy; the server listens on plain HTTP. State the
proxy's WebSocket requirements (`/ws` upgrade, long read timeouts,
`$http_host`), the origin guard (`WS_ORIGINS`/`WEB_URL`), and the answer for
viewing from outside the LAN — reverse proxy or Tailscale — including what
`GO2RTC_PUBLIC_URL` must become for live view to work there.

## 12. GPU overlay (documented, not verified here)

`docker-compose.gpu.yml`: `services.detector.deploy.resources.resolutions.devices:
[{capabilities: [gpu]}]`, `NVIDIA_VISIBLE_DEVICES=all`,
`DET_PROVIDERS=CUDAExecutionProvider,CPUExecutionProvider`. State plainly
that this overlay ships and is documented, and that on the development box
(no toolkit, no `/dev/nvidia*` nodes) **the CPU path is the only verified
one** (S9-06).

## 13. Verify the installation

```bash
scripts/verify.sh          # the acceptance gate: 9 observed passes
scripts/detect-smoke.sh    # yolov8n.onnx + bus.jpg, CPU inference, in-image
```

List the nine gates in the operator's words: server healthy + detector fresh,
login works, camera online, a person/loiter event observed end to end,
snapshot + seekable clip, segments growing, uploads landing on the tier,
OIDC login at the `viewer` floor, webhook signature verified.

## 14. Backup, upgrade, restore

What to back up (Postgres, `.env` — `NVR_SECRET_KEY` included, and the
media tree), what an upgrade is (`:latest` pull + migrate at boot), what a
restore looks like on a fresh box, and what happens to segments the old
install never uploaded.

## 15. Uninstall

Stop the stack, keep-or-remove the named volumes (`postgres_data`,
`go2rtc_config`, `nvr_data`, `nvr_models`), and what is left on disk under
`DATA_DIR`.

## Troubleshooting

The five failures an operator actually hits: camera offline after a write,
live view connecting with no video, detector `stale` in `/api/healthz`, queue
jobs stuck `pending`, and an OIDC callback that lands with no token. Each
entry names the check to run first.

# nvrWay — API Reference

<!-- skeleton: S0-02 (2026-11-02). The route groups Step 2/3/6/7 + the internal
     worker contract of Step 5. Each section names the shape its route table
     must state. Filled by S9-10; the routes themselves arrive with the cards
     cited per section. Today the only route that exists is GET /health
     (server/router.go). -->

Base path `/api/v1` for the operator surface, `/api/auth` + `/api/settings`
for session and admin config, `/api/internal/v1` for the detection worker.
Every response shape below is the shape the console and `scripts/verify.sh`
depend on.

## Table of Contents

List the sections below once they have content.

## Authentication

Bearer `access_token` on every `/api/v1/*` route; `POST /api/auth/login`,
`POST /api/auth/refresh` (rotation + reuse detection, S2-01),
`POST /api/auth/logout`, `GET /api/auth/me`, `POST /api/auth/password`.
`POST /api/auth/register` is `403` in v1 — state that as a decision, not an
omission (S2-06). Public: `GET /api/auth/status` (what the login page renders
its buttons from).

## SSO (OIDC)

`GET /api/auth/sso/providers` (public), `GET /api/auth/sso/:provider/authorize`,
`GET /api/auth/sso/:provider/callback`, `POST /api/auth/sso/exchange` (the
fragment handoff and the JIT `viewer` floor, S2-07, S2-11). Admin: provider
list/create/update/delete, discovery, test. State the exact redirect URI
string the operator must whitelist in their IdP.

## Roles and the role floor

The matrix every route section below states per route: `viewer` read,
`operator` CRUD, `admin` everything; a `viewer` write answers
`403 {"error":"insufficient role"}` (S2-08, S3-06). Include the audit row a
denial writes.

## Users, roles and identities (admin)

`GET/POST /api/users`, role assignment, session revocation,
`GET /api/identities` + `POST /api/identities/:id/link` for the
`link_policy=pending` path (S2-09).

## Cameras

`GET/POST /api/v1/cameras`, `GET/PUT/DELETE /api/v1/cameras/:id`,
`POST /api/v1/cameras/:id/probe`, `GET /api/v1/cameras/:id/snapshot`,
`PUT /api/v1/cameras/:id/zones` (S3-03, S3-04, S3-06). State slug validation
and the `409` on collision, the zone polygon rules (3–32 vertices, `0..1`
bounds, `422`), and that every write re-renders go2rtc's config.

## Segments — the timeline manifest

`GET /api/v1/segments?camera_id=&from=&to=` (S4-05). State the row shape
(`start_ts`, `end_ts`, `bytes`, `generation`) and that the count grows while
recording.

## Events

`GET /api/v1/events` with the filters (`camera_id`, `class`, `severity`,
`from`, `to`, `q`, `limit` clamped at 200), `GET /api/v1/events/:id`,
`DELETE /api/v1/events/:id`, `GET /api/v1/events/:id/snapshot`
(`image/jpeg`), `GET /api/v1/events/:id/clip` (`video/mp4`, Range → 206)
(S6-04, S6-05). State the empty-window shape (`clip_key == ""`) and what a
delete removes on disk.

## Storage backends, queue and prefixes

`GET/POST /api/storage/backends`, `PUT/DELETE /api/storage/backends/:id`,
`POST /api/storage/backends/:id/test`, `PUT /api/storage/backends/:id/default`,
`PUT /api/storage/backends/:id/enabled`, `GET /api/storage/queue` (S7-10).
State the per-kind credential fields, that secrets are never echoed
(`secret_set: true`), and the queue's claim/retry/`failed` semantics.

## Webhooks

`GET/POST /api/settings/webhooks`, `PUT/DELETE /api/settings/webhooks/:id`,
`POST /api/settings/webhooks/:id/test`, `GET /api/settings/webhooks/:id/deliveries`
(S6-07). State the payload, the `X-NVR-Signature` HMAC, the 3 attempts with
backoff, and the delivery row shape.

## Settings

`GET /api/v1/settings/live` (the console never hard-codes the media host,
S8-04), `PUT /api/settings/oidc` (discovery result in the response),
retention and recording knobs (S8-08, S2-07).

## Audit

`GET /api/audit` (admin) with actor, action, target and the denial rows
(S2-09, S8-09).

## Health

`GET /health` — liveness only, no database touch (S1-13, mounted today).
`GET /api/healthz` — `"detector":"fresh"` (< 90 s) / `"stale"`, driven by the
worker heartbeat, not a listener (S9-07).

## Internal worker contract (`/api/internal/v1`)

Bearer `WORKER_TOKEN`; empty token = these routes do not exist.
`GET /api/internal/v1/cameras` (the config payload: sources, zones,
`DET_PROVIDERS`, fps/width/height), `POST /api/internal/v1/events` (the signal
batch — the exact JSON keys of Step 5), `POST /api/internal/v1/heartbeat`
(`{"uptime_seconds":N}` every 30 s) (S5-08, S5-10, S9-07). State the flush
rule (2 s window, 40-signal cap dropping oldest) and what a
`"event closed"` `200` means for the worker.

## WebSocket

`GET /ws?token=…` (S2-10, S8-10): the seven message types, the payload of
each (`event.new`, `event.update` throttled, `event.close`, `recorder.status`,
…), the origin guard, and what the console shows when the socket dies.

## Error format

The single `{"error": "…"}` shape, the status codes each route uses and what
each one means (`403 insufficient role`, `409 slug taken`, `422 zone
polygon`, `403 registration disabled`), and the rate-limit response
(`429`, `RATE_LIMIT_PER_MINUTE`).

## Rate limiting

Per-user budget, the burst default (`0` = one tenth), the e2e dial
(`RATE_LIMIT_PER_MINUTE=1000` in `scripts/e2e-stack.sh`) and why the harness
raises it.

## API testing with curl

The login → token → call sequence `scripts/verify.sh` performs, written as
copy-pasteable commands: health, login, camera create, probe, event poll,
snapshot, clip Range request, queue, OIDC walk, webhook signature check.

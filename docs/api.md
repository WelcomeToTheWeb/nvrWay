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

Bearer `Authorization: Bearer <access_token>` on every `/api/v1/*` route.
The **header is the only REST credential**: `?token=` is accepted on `/ws` and
nowhere else (S2-08). `POST /api/auth/register` is `403` in v1 — a decision, not
an omission (S2-06).

| Route | Answer |
|---|---|
| `GET /api/auth/status` | `{"local_login":bool,"oidc":[{"provider","name","issuer"}],"registration":false}`. Both buttons come from **rows** — an account that can authenticate with a password, and a provider row that is enabled — not from the environment (S2-06). |
| `POST /api/auth/login` | body `{"username","password"}`. `200` `{"access_token","refresh_token","token_type":"bearer","expires_in":1800,"user":{…}}`. Refusals: `401 {"error":"invalid credentials"}` (identical for an unknown account and a wrong password), `403 {"error":"this account signs in through its provider"}`. Behind the rate limiter. |
| `POST /api/auth/refresh` | body `{"refresh_token"}`. Rotation: the presented row closes, the replacement names it in `rotated_from`. `200` `{"access_token","refresh_token","token_type","expires_in"}`. A spent token is a replay: `401 {"error":"refresh token was already used","live_sessions_dropped":N}` and every live session of that account is closed. |
| `POST /api/auth/logout` | bearer token required. Body `{"refresh_token"}` closes that session; an empty body closes **every** live session of the account. `200 {"status":"logged out","live_sessions_dropped":N}`. The access token is denied by its id for as long as it would have lived. |
| `GET /api/auth/profile` | the account the token names: `{"id","username","name","email","provider","status","roles":[…],"created_at","updated_at","last_login"}`. No password hash. `401` when the account no longer exists, `403` when it is pending. |
| `PUT /api/auth/password` | body `{"current_password","new_password"}`. `401` on a wrong current password, `422` under 12 characters, `403` for a provider account (its password lives at the IdP). On success every session of the account is closed and every access token issued before the call stops verifying. |
| `POST /api/auth/register` | `403 {"error":"registration is admin-only"}`, and the attempt is a ledger row. |

The plan's `GET /api/auth/me` and `POST /api/auth/password` are this pair of
routes. `/api/auth/profile` and `PUT /api/auth/password` are the names the router
mounts, the console reads and this table keeps: a route with two names is two
routes an operator has to guess between.

Every refusal above is a ledger row: `auth.login` (ok), `auth.login_failed` (with
the reason the client does not get), `auth.refresh`, `auth.logout`,
`auth.password_change`, `auth.register`.

## SSO (OIDC)

Public: `GET /api/auth/sso/providers` — `{"providers":[{"provider","name",
"issuer","redirect_uri","link_policy","scopes","pkce_supported","discovery"}]}`.
`redirect_uri` is the exact string the operator whitelists:
`${WEB_URL}/api/auth/sso/<provider>/callback`.

`GET /api/auth/sso/:provider/authorize` records the state (nonce + PKCE verifier
+ redirect URI, five minutes, **single use** — the callback consumes it) and
redirects `302` to the IdP with `state`, `nonce` and the `S256` challenge. A row
whose registered redirect URI is not the one this router mounts answers `422`
with the string to whitelist.

`GET /api/auth/sso/:provider/callback` → on success `302` to
`${WEB_URL}/auth/callback#access_token=…&refresh_token=…` — the SPA handoff, in a
fragment so no server ever receives the tokens. On refusal the same page, with
the contract's reason: `#error=<reason>&provider=<name>&error_description=…`
(+ `pending_identity=<id>` when a queue row was created). The reasons are
`state_mismatch`, `state_expired`, `provider_disabled`, `code_exchange_failed`,
`domain_not_allowed`, `email_unverified`, `no_email_claim`, `identity_pending`,
`subject_missing`, `id_token_userinfo_disagreement`, `id_token_verification_failed`,
`nonce_mismatch` — each one a different fix on the IdP side, and each one a ledger
row (`sso.login_refused`) naming the subject and email the assertion carried.

`POST /api/auth/sso/exchange` (bearer) → the profile shape above, roles included.
It is the console's check that the token it caught from the fragment is its own.

The queue: `GET /api/identities?status=pending|all` (admin),
`POST /api/identities/:id/approve` (its own account, `viewer` floor),
`POST /api/identities/:id/link` (body `{"user_id"}`; merges the assertion into that
account, clears its password door, closes its live sessions and invalidates its
access tokens; the roles are untouched), `POST /api/identities/:id/refuse` (body
`{"reason"}` required). All three are audited (`identity.approve`,
`identity.link`, `identity.refuse`), all three refuse a row that is already
decided with `409` (S2-07, S2-09).

## Roles and the role floor

The matrix every route section below states per route: `viewer` read, `operator`
CRUD, `admin` everything; a `viewer` write answers
`403 {"error":"insufficient role"}` (S2-08, S3-06). Include the audit row a
denial writes.

The authority is the **role rows**, read live: the token's role names decide which
named gate a request runs, and the gate reads what that role is granted now
(`RequirePermission` reads `role.permissions`, stars expanded). A token whose role
an admin has taken away stops writing on its next request, and the role write closes
the account's sessions so it cannot mint a new one (S2-08, S2-09).

Denials are ledger rows named for the act they refused — `deniedActions` maps
`"METHOD /api/route/:id"` to the verb (`user.read`, `role.read`, `identity.read`,
`oidc.update`, …), with the fallback `route.denied` for a route that has not
declared one, so a new route group whose refusals are invisible is visible as one
row nobody expected (S2-08).

The authority for a permission check is the **role rows**, read live: the token's
role names decide which named gate the request runs, and the gate reads what that
role is granted now. A token whose role an admin has just taken away stops writing
on its next request (S2-08).

## Users, roles and identities (admin)

Admin only: a `viewer` or `operator` bearer token on any route here answers
`403 {"error":"insufficient role"}` and writes a ledger row under the verb of the
act it refused (S2-09).

| Route | Answer |
|---|---|
| `GET /api/users` | `{"users":[profile,…]}` with `?provider=`, `?role=`, `?status=`, `?q=`, `?limit=` (`limit=0` → `400`). Never a password hash. |
| `POST /api/users` | body `{"username","password","name","email","roles"}` → `201` profile. `422` under 12 characters (the number stated), `422` naming an unknown role, `409` on a taken username. No roles named ⇒ the `viewer` floor. The account is `provider: "local"`: it authenticates by password. |
| `GET /api/users/:id` | profile + `password_login` (bool: the break-glass door), `sessions` (live: `id`, `source_ip`, `user_agent`, `created_at`, `expires_at`), `live_sessions`. |
| `PUT /api/users/:id` | body with any of `username`, `name`, `email`, `status` (`active`/`pending`), `password`. Response `{"user":{…},"live_sessions_dropped":N,"sign_in_required":bool}`. `403` for a provider account (its password lives at the IdP), `409` parking the last admin, `409` on a taken username, `400` when nothing was named. A password reset or a park closes the account's sessions and invalidates its access tokens. |
| `DELETE /api/users/:id` | account, role grants and sessions go in one act (`409` on the last admin, with the reason). | 
| `GET /api/users/:id/roles` | `{"user_id","roles":[…]}` — the rows, not the token. |
| `POST /api/users/:id/roles` | body `{"roles":[…]}` → `{"user_id","roles","live_sessions_dropped":N,"sign_in_required":true}`. `422` naming an unknown role, `409` demoting the last admin. The change ends the account's sessions: the gate reads the role rows, and a live refresh token mints access tokens, so revoking on write is the difference between “removed” and “removed when they next ask”. |
| `GET /api/roles` | `{"roles":[{"id","name","description","permissions":[…],"builtin"}]}` — the permission authority the gates read. |
| `PUT /api/settings/oidc/:provider/role-mapping` | body `{"role_mapping":{"claim value":"role"}}`. `422` when the mapping names **`viewer`** — a mapping replaces roles on every login, and the floor is the only thing an identity arrives with, so a mapping that owns it locks out the people who cannot ask for it back (and the admin it locks out at the same time). `422` naming a role that does not exist. An empty mapping is allowed: no mapping is the floor standing. |

The identities routes are S2-07's (`GET /api/identities`,
`POST /api/identities/:id/approve|link|refuse`).

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

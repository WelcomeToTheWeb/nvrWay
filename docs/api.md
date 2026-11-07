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

`DELETE /api/v1/cameras/:id` (admin) destroys the camera's event and segment
rows, its local files, and queues the remote purge jobs; it answers
`{deleted, slug, segments, events, local_files, remote_jobs, config_applied}`.
`GET /api/v1/cameras/:id/delete-summary` (admin, S8-13) answers the same four
counts **before** the act — the delete's arithmetic without the destruction,
from the same row walks and the same slug sweep — so the console's
confirmation can name the size of an irreversible act instead of guessing
from list pages. `storage_tier: false` says there is no tier at all, which is
not the same sentence as `remote_jobs: 0`.

## Segments — the timeline manifest

`GET /api/v1/segments?camera_id=&from=&to=` (S4-05). State the row shape
(`start_ts`, `end_ts`, `bytes`, `generation`) and that the count grows while
recording.

## Events

`GET /api/v1/events` with the filters (`camera_id`, `class`, `zone_id`, `severity`,
`from`, `to`, `q`, `limit` clamped at 200, `offset`), answering
`{events, total, limit, offset, truncated}`; `GET /api/v1/events/:id`; and
`DELETE /api/v1/events/:id`, which is operator-only, cancels any clip assembly
still running for that event, and enqueues the purge of whatever media is on a
tier (S6-02, S6-03, S6-04).

`q` matches camera and zone *names*, because that is what an operator types; the
row stores zone ids, and the camera row holds the pair.

### What space `objects[].box` is in

A box is `[x1, y1, x2, y2]` in **pixels of the detection frame** — the
`<slug>_det` tap's picture, top-left origin, not normalised, not the model's
letterboxed square. The worker sends the corners untransformed and the server
stores them verbatim, so the pixels are only meaningful next to the frame's
size, and the event view names it: `det_width` × `det_height`.

A receiver is supposed to divide: a box's left edge is `x1 / det_width` of the
snapshot's width, its top is `y1 / det_height` of the height. The snapshot the
event carries is a scaled copy of the same tap, so the fractions land on it.

`det_width` or `det_height` of **0 means the frame was never named** — a row
written before the frame was recorded (P-10), or an installation with no tap
size anywhere. A consumer that finds 0 shows the count and places nothing: a
box drawn against a guessed frame looks like evidence and is not one.

The frame is resolved once, at signal intake, from the camera row's
`detection_width`/`detection_height` falling back to `GO2RTC_MJPEG_WIDTH`/
`GO2RTC_MJPEG_HEIGHT` — the same rule that renders the tap, so the stored
number is the size go2rtc was asked to transcode to. It is stored on the event
rather than re-derived on read, so it survives a later config change; the one
case it gets one frame wrong is a tap-size edit *during* a live event, and the
frame stored is the one the first signal named. The webhook payload carries no
boxes at all (classes, zones, rules, and two media URLs — see the payload
section), so there is no second place this space has to agree with.

Zone polygons are the opposite contract and always were: validated into `0..1`
on write, so they draw on any picture without a frame.

### The media routes

`GET /api/v1/events/:id/snapshot` → `image/jpeg`
`GET /api/v1/events/:id/clip` → `video/mp4`

Both are `viewer`-readable and both are server-proxied: the console never receives
a path, and a tier's credential never reaches the browser.

**Local file first, then the storage tier.** A file inside `CLIPS_DIR` /
`SNAPSHOTS_DIR` is served by `http.ServeContent`, which answers `Range: bytes=a-b`
with **206** and `Content-Range`, and answers `If-Modified-Since` with **304**. With
no readable local copy and the row's upload stamp set (`clip_uploaded_at`,
`snapshot_uploaded_at`), the object at the row's key is streamed from the
installation's default backend, **from byte 0, whole**:

```
Content-Type:   video/mp4 | image/jpeg
Accept-Ranges:  bytes
Cache-Control:  private, max-age=3600
Content-Length: <the object's size, from a Stat before the first byte>
Last-Modified:  <the object's mtime>
```

A remote object **cannot be seeked**: the storage interface reads an object from
byte 0 and has no ranged read, so a request carrying `Range` gets the whole object
with a **200** — the fallback HTTP sanctions, which browsers play forward. Seek on
a remote clip is a v2 item, and `docs/architecture.md` § Playback and the remote
seek gap states what v2 adds.

The answers, each named because the console draws each one differently:

| Answer | When |
| --- | --- |
| `200` | a local file, or the object on the tier |
| `206` | a `Range` request answered by a **local** file |
| `404 {"error":"event not found"}` | no such event |
| `404 {"error":"clip not available"}` / `"snapshot not available"` | the event stands without that media: it was never made (a camera in `record_mode=off`, a window with no segments), it aged out of both copies, or no backend is configured |
| `502 {"error":"…","tier":"<name>"}` | the tier is configured and not usable — `discovery_unreachable`, `bad credentials`, `bucket missing`, `sftp host key changed` |
| `500 {"error":"…","tier":""}` | this process cannot open the stored credential, or the configured kind has no driver in this build |

The 404 and the 502 are not interchangeable. An object that is gone from a tier
that is there is a fact about retention and the operator reads it as "this event
has no clip"; a tier that is not mounted is a fact about the host, and every clip
at once looks like it was never recorded. The `fs` driver checks its root on the
miss path so the two cannot be confused.

`clip_key == ""` on an event means the clip window held no segments. The event is
filed, the snapshot is there, and the clip route answers the named 404; the log
carries the window it looked at.

## Storage backends, queue and prefixes

Every route is admin-only. `GET /api/storage/backends`, `POST /api/storage/backends`,
`PUT /api/storage/backends/:id`, `DELETE /api/storage/backends/:id`,
`POST /api/storage/backends/:id/test`, `PUT /api/storage/backends/:id/default`,
`PUT /api/storage/backends/:id/enabled`, `POST /api/storage/backends/:id/prefixes?prefix=`,
`POST /api/storage/backends/:id/requeue?kind=`, `GET /api/storage/queue` (S7-10).

**Secrets have no path out.** The response carries `secret_set` and one flag per
credential column (`password_set`, `access_key_set`, `secret_key_set`,
`private_key_path_set`) and no secret field — not even `private_key_path`, because
which key file an installation uses is information. A write that **omits** a
secret keeps the stored one; a write that sends it **empty** clears it. The
stored columns hold sealed text (AES-256-GCM, `storage/seal.go`), so a database
backup is readable and a credential leak still needs the seal key.

**Per-kind fields.** `s3`: `endpoint`, `bucket`, optional `region`, `prefix`,
`access_key`, `secret_key`, `insecure_tls`. `webdav`: `endpoint` (a URL),
`username`, `password`, `prefix` whose first segment is the container. `sftp`:
`endpoint` (`host:22`), `username`, and either `password` or
`private_key_path` — a path on the **server host** — plus `root_path`, which is
what the writes stay inside. `fs`: `root_path` only (a local disk, an NFS mount,
a compose volume); it has no endpoint. The API validates that the fields a kind
needs are **present**; what an endpoint may look like is the driver's rule, and
the test route carries that answer back. `kind` cannot change on an existing
backend — the sealed columns mean something different per kind.

**`POST …/test` answers `{"ok":true}` or**
`{"ok":false,"error":"<name>","reason":"…"}`, where `<name>` is one of
`discovery_unreachable`, `bad credentials`, `sftp host key changed`,
`bucket missing` — the storage package's sentinel messages, verbatim, because
those four are four different fixes. A refusal with no name among them answers
`connection_failed` with the driver's sentence in `reason`; the case an
installation actually hits is `the stored credential cannot be opened with this
seal key`, which is not a tier state — the server and the credential may both be
fine, and only this process cannot open the row.

**`POST …/prefixes?prefix=`** is the Settings page's "verify this path": it asks
the tier what is really under the prefix, capped at 500 keys with `truncated`.
A prefix with nothing under it answers an empty list; a tier that is not mounted
answers the named failure, because an empty list is exactly what a wrong prefix
looks like.

**`PUT …/enabled` with `{"enabled":false}`** is a pause, not a cancellation: new
claims stop, the queue grows, and it resumes when the flag returns. Jobs already
claimed finish — the claim is the gate. The response carries `is_default`, so the
console can say "you just disabled the default destination" without a second
request. `PUT …/default` refuses a disabled backend for the same reason.

**`POST …/requeue?kind=segment|clip|snapshot`** re-shelves every `uploaded` row
of that kind onto this backend — the answer to "we moved the tier" that does not
require re-recording anything.

**`GET /api/storage/queue?state=&limit=&offset=`** answers the queue oldest
first, `limit` default 50 and clamped at 200, with `truncated` and `counts` (the
totals for the whole queue, not the page). `state` is one of `pending`,
`uploading`, `uploaded`, `failed`; anything else answers 400 rather than an empty
page that reads as a healthy queue. Each row carries `attempts`, `last_error`,
`next_attempt_at` and `backend_name`.

**The queue's semantics**, stated where the console reads them: a job is claimed
(`uploading`, `attempts + 1`), then either `uploaded` with its `remote_key`, or
back to `pending` with `next_attempt_at = now + min(5min × 2^attempts, 6h)` — so
10, 20, 40, 80, 160, 320 minutes and then 6 hours — and `failed` at 20 attempts.
A failure no retry can fix (the local file is gone, the backend row is gone)
stops at `failed` on the first attempt. `DELETE /api/storage/backends/:id` leaves
the queue rows that point at it and reports how many, because "the queue is not
moving" is a question whose answer is a number.

Every backend write is a ledger row: `storage.backend_read`,
`storage.backend_create`, `storage.backend_update`, `storage.backend_delete`,
`storage.backend_default`, `storage.backend_enable`, `storage.backend_test`,
`storage.backend_prefixes`, `storage.requeue`. The details name which secret
columns a write addressed, never what they held.

## Webhooks

Every route is admin-only (S6-07):

```
GET    /api/settings/webhooks
POST   /api/settings/webhooks
PUT    /api/settings/webhooks/:id
DELETE /api/settings/webhooks/:id
GET    /api/settings/webhooks/:id/deliveries?status=&limit=&offset=
```

The signing secret is write-only: the response carries `secret_set: true` and the
secret's bytes appear in no response — not in an error message either. An omitted
`secret` on a PUT keeps the stored one (the console can never send it back); an
explicitly empty `"secret": ""` clears it, and the ledger records the removal.

A write that is refused is a ledger row too, with the reason in its details.

The route refuses an endpoint that would save cleanly and then hear nothing: no name,
no URL, a URL that is not http/https or has no host, a URL carrying `user:pass`, an
empty `events` list, a trigger this build never sends, and a `camera_id` that names no
camera. Each is a 422 naming the field.

There is no "send a test notification" route in v1, deliberately: a test delivery
would land in the same history the operator is reading to answer "why did my hook
fail", and the payload shape for an event that never happened is a contract the plan
does not settle.

### The trigger, and who receives it

v1 sends one trigger: **`event.close`**. An event that is still moving is not a fact
anyone should be notified about yet, so `open` and `extend` send nothing.

An endpoint receives a close when three things are true: it is `enabled`, its
`camera_id` is empty (every camera) or equal to the event's camera, and its `events`
list contains `event.close`. An endpoint that fails any of those gets **no request at
all** — not a request it is told to ignore.

### The payload

The body is one JSON object, in this field order:

```json
{
  "id": "<event id>",
  "camera_id": "<camera id>",
  "camera_slug": "front-door",
  "started_at": "2026-11-04T09:15:03.400Z",
  "ended_at": "2026-11-04T09:15:45.400Z",
  "seconds": 42,
  "classes": ["person"],
  "zones": ["front step"],
  "rules": ["loiter"],
  "severity": 3,
  "snapshot_url": "http://192.168.1.20:8080/api/v1/events/<id>/snapshot",
  "clip_url": "http://192.168.1.20:8080/api/v1/events/<id>/clip"
}
```

- **Timestamps are UTC with milliseconds**, the same layout the API's own event view
  uses, so a receiver that reads a webhook and then asks the API sees one shape.
- **`zones` carries the names the operator typed**, not the row's zone ids: a
  receiver has never seen this installation's ids, and the camera row holds the pair.
  A zone deleted after the event keeps its id.
- **The two URLs are the server's own routes**, never a path on its disk and never a
  tier address. They are built from `PUBLIC_BASE_URL` (see `docs/configuration.md`);
  a base with a path prefix keeps it, so a reverse proxy at `/nvr` hands out URLs its
  receivers can fetch.
- **`snapshot_url` is empty when there is no picture.** The frame is written on the
  open, so by the close an empty field is a fact, and the receiver branches on the
  field instead of fetching a 404.
- **`clip_url` is empty only for a camera in `record_mode=off`.** For every other
  camera the clip is assembled *after* the close (at `ended_at + POST_ROLL_SECONDS`),
  so the URL is an address that becomes a video a few seconds later. A receiver that
  fetches it immediately and gets `{"error":"clip not available"}` should ask again.
- The signing secret is never in the body.

### The signature

```
X-NVR-Signature: sha256=<hex(HMAC-SHA256(secret, raw body))>
User-Agent: nvr/1
Content-Type: application/json
```

The HMAC is over the **bytes of the request body**, computed once per event and
reused unchanged on every attempt, so a receiver that verified the first attempt can
verify the third. Recompute it over the raw body before parsing: a decoded-then-
remarshalled copy can differ in field order or number formatting.

Three states where the header is **absent**, each meaning something different:

| State | Why no header |
| --- | --- |
| The endpoint's secret is empty | there is nothing to sign with, and a signature under an empty key would look like one that means something |
| The stored secret is not a sealed value | a row written before sealing existed, or inserted by hand: it is used as written, and the log says so |
| The stored secret cannot be opened | `NVR_SECRET_KEY` changed. Signing the sealed text would produce a signature that verifies nowhere, and a *bad* signature sends the operator to the endpoint instead of to the key |

### The attempts

Three, at **2 s, 10 s and 60 s after the close** — measured from the close, not from
the previous attempt, so a receiver that answers slowly does not push the whole
ladder back. Any 2xx stops it; a connection failure, a timeout (10 s per attempt) or
a non-2xx continues it. There is no permanent-failure fast path: a receiver's 4xx is
often a proxy answering while the real service restarts, and the whole ladder spans
72 seconds.

Delivery is asynchronous. The worker's signal request is answered as soon as the event
row is written; an endpoint that is down does not hold it open.

### The delivery history

Every attempt is one `webhook_deliveries` row, written when the attempt ends. The
history route returns them newest first, with the endpoint inline, and `status=failed`
is the filter the question asks for:

| Field | Meaning |
| --- | --- |
| `webhook_id` | the endpoint |
| `event` | the trigger, `event.close` |
| `payload` | the exact bytes that were sent, as a JSON object, so the console can answer "what did we send it" without a second request |
| `attempt` | 1, 2 or 3 |
| `status` | `delivered` or `failed` |
| `status_code` | the HTTP status, or 0 when there was no response |
| `error` | empty on success; otherwise the sentence — including the receiver's own words, capped at 200 bytes |
| `delivered_at` | set only on the attempt that worked |

`limit` defaults to 50 and is capped at 200; `offset` pages; `truncated` is a fact
rather than a guess (the route asks for one row more than it shows). The order carries
an `id` tiebreaker because two attempts of one delivery can land in the same
millisecond, and an offset page over a tie that the database resolves differently per
query skips a row on one page and repeats it on the next.

An attempt that never happened gets no row: a process that shuts down between attempts
records the sends it made and nothing else. The endpoint row also carries
`last_error` and `last_delivered_at`, which is how the Settings page answers "is this
webhook working" without reading the history. Deleting an endpoint deletes its history
with it and reports how many rows went along.

Rows are pruned after 30 days by the retention sweep (S7-09, sweep 5).

## Settings

`GET /api/v1/settings/live` (the console never hard-codes the media host,
S8-04), `PUT /api/settings/oidc` (discovery result in the response),
`GET /api/settings/retention` (admin, S8-08): the windows the sweeper will
honour, read from the config this process loaded — `local_days`, `remote_days`
(with `remote_forever: true` spelled out beside a zero), `event_days`,
`unuploaded_protection_days` (the sweeper's own `max(3 × local, 7)`), the three
housekeeping windows, and `author: "environment"`. **Read-only on purpose:**
the env is the policy's only author, and a console write path would fork the
policy into an env file and a settings row while the sweep kept reading the
env. Recording knobs live on the camera row (`PUT /api/v1/cameras/:id`).

## Audit

`GET /api/audit` (admin) answers **who did what, to what, and what the server
answered** — newest first. Query: `actor` (username), `action`, `target_id`,
`outcome` (one of `ok` / `refused` / `not_found` / `error` — anything else is
a 400 naming the four), `from` / `to` (RFC3339), `limit` (default 50, max
200), `offset`. Response `{audits, count, limit, offset, truncated}`; each row
is `{id, actor_user_id, actor_username, actor_roles, actor_provider, action,
target_type, target_id, outcome, details, source_ip, created_at}`.

The actor columns are text, not ids, and can be empty — a failed login has no
authenticated account, and the row still answers who knocked. **Refusals are
rows**: `outcome` is a stored column derived from the status code
(`AuditOutcomeFor`), so `?outcome=refused` finds the deletes a viewer attempted
and the server denied. The read itself is deliberately not audited: no read
verb exists in `models/audit.go`, and a ledger read that writes rows moves the
page count under the operator reading it.

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

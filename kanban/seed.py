#!/usr/bin/env python3
"""Seed the nvrWay Kanban cards from NVRWAY_V1_PLAN.md's deliverables.

One card per deliverable — a module, a route group, a test suite — not one card
per plan step, so a card is small enough to finish in one sitting and precise
enough to be checked by a command.

Run it once per card set. Existing cards are never overwritten: card files are
the source of truth and are hand-edited as work moves; KANBAN.md is derived from
them by kanban/board.py.

    python3 kanban/seed.py            # write missing cards
    python3 kanban/seed.py --check    # report cards missing from the seed set
"""

from __future__ import annotations

import pathlib
import sys

TODAY = "2026-11-02"
CARDS = pathlib.Path(__file__).resolve().parent / "cards"
CARDS.mkdir(parents=True, exist_ok=True)

SEED: list[dict] = []


def card(
    id: str,
    step: str,
    title: str,
    status: str,
    paths: list[str],
    depends: list[str],
    deliverable: str,
    criteria: list[str],
    verify: str = "",
    notes: str = "",
) -> None:
    SEED.append(
        {
            "id": id,
            "step": step,
            "title": title,
            "status": status,
            "paths": paths,
            "depends": depends,
            "deliverable": deliverable,
            "criteria": criteria,
            "verify": verify,
            "notes": notes,
        }
    )


# ----------------------------------------------------------------------------------
# Step 0 — repo scaffold (no behavior)
# ----------------------------------------------------------------------------------

card(
    "S0-01",
    "0",
    "Repo tree skeleton (server packages + detector/web/docker/scripts/testdata/.github)",
    "backlog",
    [
        "server/{auth,sso,api,ws,media,go2rtc,storage,sweeper,webhooks}/",
        "detector/nvr_detector/",
        "web/src/",
        "docker/",
        "scripts/",
        "testdata/",
        ".github/workflows/",
    ],
    [],
    "The plan's Step 0 tree exists exactly, so later steps only add files inside a shape that is already committed.",
    [
        "server subpackages present (auth, sso, api, ws, media, go2rtc, storage, sweeper, webhooks)",
        "detector/, web/, docker/, scripts/, testdata/, .github/workflows/ present",
        "empty dirs carry a .gitkeep so the tree survives a clone",
    ],
    "find server detector web docker scripts testdata .github -maxdepth 2 -type d | sort",
    "Missing today: every entry in `paths`. Present: server/{config,deps,models,store}, docs/, LICENSE, README.md, .gitignore, .env.example, coverage.baseline.json.",
)

card(
    "S0-02",
    "0",
    "Docs skeleton: DESIGN, PRODUCT, SECURITY, CONTRIBUTING + docs/ headings",
    "backlog",
    ["DESIGN.md", "PRODUCT.md", "SECURITY.md", "CONTRIBUTING.md", "docs/"],
    [],
    "Heading skeletons copied from OurWay's SECURITY.md / CONTRIBUTING.md, so the prose slots exist before Step 9 fills them.",
    [
        "DESIGN.md, PRODUCT.md, SECURITY.md, CONTRIBUTING.md created with headings",
        "docs/architecture.md, docs/api.md, docs/installation.md created with headings",
        "each heading names the decision it must answer (clock rule, remote seek gap, single-instance rule)",
    ],
    "ls DESIGN.md PRODUCT.md SECURITY.md CONTRIBUTING.md docs/",
    "docs/configuration.md already exists (see S0-06).",
)

card(
    "S0-03",
    "0",
    "Compose + Dockerfile skeletons",
    "backlog",
    [
        "docker-compose.prod.yml",
        "docker/docker-compose.yml",
        "docker/Dockerfile.server",
        "docker/Dockerfile.web",
        "docker/Dockerfile.detector",
        "docker/go2rtc.yaml.template",
        "docker/nginx.conf",
    ],
    ["S0-01"],
    "The container files exist as skeletons at Step 0; Step 9 gives them real content.",
    [
        "all seven files present",
        "dev compose names the six services (postgres, go2rtc, server, detector, web + commented minio)",
        "prod compose references ghcr.io/welcometotheweb/nvr/{server,web,detector} with no build sections",
    ],
    "ls docker-compose.prod.yml docker/",
    "Skeleton only — content is S9-01..S9-06.",
)

card(
    "S0-04",
    "0",
    "scripts/ + CI workflow skeletons",
    "backlog",
    [
        "scripts/dev.sh",
        "scripts/verify.sh",
        "scripts/detect-smoke.sh",
        "scripts/e2e-stack.sh",
        "scripts/coverage.sh",
        "scripts/fetch-fixture.sh",
        ".github/workflows/build.yml",
    ],
    ["S0-01"],
    "The harness entry points named in README exist and are executable, so nobody invents a command name later.",
    [
        "six scripts present, mode 755, with a usage line",
        ".github/workflows/build.yml present with the four job names",
        "README Quick Start block runs against these names verbatim",
    ],
    "ls -l scripts/ .github/workflows/",
    "Content is S9-08/S9-09; README already promises these commands.",
)

card(
    "S0-05",
    "0",
    "License, gitignore, env example, coverage baseline",
    "done",
    ["LICENSE", ".gitignore", ".env.example", "coverage.baseline.json", "README.md"],
    [],
    "The repo-level files the plan pins verbatim: MIT license, OurWay-derived .gitignore, env table in .env.example, coverage baseline at 0.0, README H2 skeleton.",
    [
        "LICENSE = MIT, 'Copyright (c) 2026 nvrWay contributors'",
        ".gitignore drops agent/installer entries; adds data/, models/, web/test-results/",
        ".env.example lists every Step 1 env var with its default",
        "coverage.baseline.json = {\"server\": 0.0, \"detector\": 0.0, \"web\": 0.0}",
        "README H2 skeleton matches the plan's seven headings",
    ],
    "head -4 LICENSE; cat coverage.baseline.json; grep -c '^## ' README.md",
    "Observed present on 2026-11-01 during board setup.",
)

card(
    "S0-06",
    "0",
    "docs/configuration.md = the env table, verbatim",
    "review",
    ["docs/configuration.md"],
    ["S1-01"],
    "Every env var with its default, matching server/config/config.go exactly — the operator's reference, not a summary.",
    [
        "every var in config.go appears in docs/configuration.md and in .env.example",
        "defaults match config.go's fallbacks (no drift between doc and code)",
        "NVR_SECRET_KEY documented as required in production, with the JWT_SECRET-derived fallback stated",
    ],
    "grep -o '\"[A-Z][A-Z0-9_]*\"' server/config/config.go | sort -u > /tmp/a; grep -o '^[A-Z][A-Z0-9_]*' docs/configuration.md | sort -u > /tmp/b; diff /tmp/a /tmp/b",
    "Present. config.go carries NVR_SECRET_KEY, which the plan's Step 1 table places in Step 7 — check it is documented as Step 7's seal key.",
)

# ----------------------------------------------------------------------------------
# Step 1 — config, models, store, migrations
# ----------------------------------------------------------------------------------

card(
    "S1-01",
    "1",
    "Config: env table, Load(), EnsureDirs, Version/GitCommit",
    "review",
    ["server/config/config.go", "server/config/config_test.go"],
    [],
    "struct + Load() + getEnv/getEnvInt, defaults exactly as the Step 1 env table, derived dirs, LocalAdminEnabled/OIDCConfigured helpers, Version/GitCommit stamped by ldflags.",
    [
        "all 38 env vars of the plan table are read with the plan's defaults",
        "sqlite:// switch and derived SEGMENTS/CLIPS/SNAPSHOTS dirs",
        "JWT_SECRET empty -> random per boot; WORKER_TOKEN empty -> internal API disabled",
        "config_test.go covers defaults, overrides, derived dirs, invalid ints",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./config -count=1",
    "Verified present 2026-11-01: all plan vars, plus NVR_SECRET_KEY. Test run pending (S1-14).",
)

card(
    "S1-02",
    "1",
    "Pinned dependency set (go.mod + deps pin package)",
    "review",
    ["server/go.mod", "server/go.sum", "server/deps/deps.go"],
    [],
    "module nvr/server, go 1.26, the plan's pinned requires kept in go.mod from the first commit by blank imports in deps/, so Step 7 changes code and not a dependency set.",
    [
        "every pinned require is present and resolvable",
        "gofakes3 pseudo/tag version recorded in go.sum",
        "pin drift versus the plan is recorded in the commit message, not loosened",
    ],
    "cd server && go mod verify && grep -c 'gin-gonic\\|golang.org/x/crypto' go.mod",
    "DRIFT vs the plan's Step 0 table: gin 1.10.0 -> 1.12.0, x/crypto 0.26.0 -> 0.57.0, gofakes3 pseudo-version -> v1.2.0. The plan allows a pin bump recorded in the same commit; P-02 files that record.",
)

card(
    "S1-03",
    "1",
    "Models: Camera (+Zone/ZoneError), Segment, Event (+EventObject)",
    "review",
    ["server/models/camera.go", "server/models/segment.go", "server/models/event.go"],
    ["S1-01"],
    "The media-tree entities with the plan's load-bearing columns, indexes and the exact ZonesJSON wire shape.",
    [
        "Camera columns and Slug uniqueIndex match the plan verbatim",
        "Segment carries Generation + index idx_seg_cam_gen_start(camera_id,generation,start_ts)",
        "Event carries EventKey uniqueIndex, Open, the JSON-text columns, snapshot/clip keys and upload stamps",
        "Zone validation: 3-32 vertices, coordinates 0..1, rule in {intrusion, loiter}",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./models -count=1",
    "Verified present 2026-11-01 with camera_test.go + models_test.go.",
)

card(
    "S1-04",
    "1",
    "Models: StorageBackend, StorageJob",
    "review",
    ["server/models/storage_backend.go", "server/models/storage_job.go"],
    ["S1-01"],
    "Backend rows (kind/endpoint/bucket/prefix/credentials/IsDefault) with the four secret fields stored sealed, and the job rows (Kind/Target/State/Attempts/NextAttemptAt/RemoteKey).",
    [
        "Password, AccessKey, SecretKey, PrivateKeyPath documented as AES-256-GCM sealed, never echoed",
        "Name uniqueIndex, Enabled/IsDefault flags",
        "StorageJob ID is the segment/event UUID (not a new UUID)",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./models -count=1",
    "Sealing itself is S7-02.",
)

card(
    "S1-05",
    "1",
    "Models: Webhook, WebhookDelivery",
    "review",
    ["server/models/webhook.go"],
    ["S1-01"],
    "Per-camera webhook rows (URL, secret, events, enabled) and one delivery row per attempt (status, attempt, error).",
    [
        "Webhook carries the HMAC secret as a sealed field and the camera scope",
        "WebhookDelivery records status, attempt, error, ts for every send",
        "secret is never serialized in responses (json tag omits it)",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./models -count=1",
    "Sender and routes are S6-06/S6-07.",
)

card(
    "S1-06",
    "1",
    "Models: User, Role, UserRole, Session, PendingIdentity",
    "review",
    [
        "server/models/user.go",
        "server/models/role.go",
        "server/models/user_role.go",
        "server/models/session.go",
        "server/models/pending_identity.go",
    ],
    ["S1-01"],
    "Account entities: argon2id hash column, builtin role names, role grants, refresh sessions with token_hash/rotated_from, and the pending identity parked by link_policy=pending.",
    [
        "Session carries token_hash + rotated_from for reuse detection",
        "Role carries the builtin admin/operator/viewer set",
        "PendingIdentity records provider, asserted email, subject, created ts",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./models -count=1",
    "Login/refresh behavior is S2-01..S2-03.",
)

card(
    "S1-07",
    "1",
    "Models: SSOProvider, Audit, Setting + JSON-text helpers",
    "review",
    [
        "server/models/sso_provider.go",
        "server/models/audit.go",
        "server/models/setting.go",
        "server/models/json.go",
    ],
    ["S1-01"],
    "OIDC provider row (issuer/client/jwks/link policy/role mapping), audit rows, settings key-value rows, and the JSON-as-string column pattern (no datatypes).",
    [
        "JSON columns are string columns holding JSON text (OurWay pattern), serializer:json only for typed fields",
        "AuditRetentionDays and the audit actor shape exist for the sweeper and middleware to use",
        "SSOProvider default link_policy is pending",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./models -count=1",
    "AuditRetentionDays is already consumed by main.go's boot log.",
)

card(
    "S1-08",
    "1",
    "Store core: New(dsn), NewWithDB(db), Postgres -> sqlite fallback",
    "review",
    ["server/store/store.go", "server/store/store_test.go"],
    ["S1-01", "S1-09"],
    "One struct holding every per-entity store, opened from a DSN, with the sqlite:// switch and the Postgres-unreachable fallback behavior OurWay ships.",
    [
        "store.New(\":memory:\") works for tests (sqlite path)",
        "Postgres DSN failure falls back to sqlite exactly as OurWay's store.go:14-145",
        "every entity store is reachable from the Store struct",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./store -count=1",
    "Verified present 2026-11-01.",
)

card(
    "S1-09",
    "1",
    "Migrations: baseline schema over all 15 models + versioned migration table",
    "review",
    ["server/store/migrate.go", "server/store/migrate_test.go"],
    ["S1-03", "S1-04", "S1-05", "S1-06", "S1-07"],
    "A fresh database lands in the Step 1 schema, and a new column arrives as a numbered migration rather than an edit to the baseline.",
    [
        "all 15 plan models are in the baseline",
        "migrate is idempotent and recorded (applied_migrations)",
        "migrate_test asserts a second run is a no-op",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./store -count=1 -run Migrat",
    "DEVIATION (deliberate, keep it): the plan says AutoMigrate; migrate.go gates AutoMigrate behind migration 001 and adds a versioned migration table, so schema evolution is reproducible. The plan's intent (a schema that cannot drift silently) is preserved.",
)

card(
    "S1-10",
    "1",
    "Store: cameras, events, segments",
    "review",
    ["server/store/cameras.go", "server/store/events.go", "server/store/segments.go"],
    ["S1-08", "S1-09"],
    "Per-entity method style copied from OurWay: create/update/delete, unique-slug lookup, event upsert by event_key, segment manifest queries by camera + window, generation counters.",
    [
        "slug uniqueness enforced (409 material for the API)",
        "segment manifest query answers camera_id + from/to in StartTS order",
        "event_key dedupe: open/extend/close mutate one row, never insert a second",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./store -count=1",
    "Verified present 2026-11-01; EventStore.UpsertByEventKey already exists, so S6-03 is a semantics audit, not a write.",
)

card(
    "S1-11",
    "1",
    "Store: storage backends + jobs, settings, webhooks",
    "review",
    ["server/store/storage.go", "server/store/settings.go", "server/store/webhooks.go"],
    ["S1-08", "S1-09"],
    "Backend CRUD with IsDefault, the storage job queue rows, settings get/set, webhook + delivery rows.",
    [
        "ClaimNextJob's inputs live here (state, next_attempt_at, attempts, created_at ordering)",
        "settings key/value get+set (recorder_gen_*, bootstrap_admin_seeded_at, detector_heartbeat_at, sftp_hostkey_*)",
        "delivery rows append per attempt",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./store -count=1",
    "The claim query's concurrency shape is S7-08.",
)

card(
    "S1-12",
    "1",
    "Store: users, roles, sessions, sso_providers, audit",
    "review",
    [
        "server/store/users.go",
        "server/store/roles.go",
        "server/store/sessions.go",
        "server/store/sso_providers.go",
        "server/store/audit.go",
    ],
    ["S1-08", "S1-09"],
    "Account store: user by username, role grants, session insert/rotate/lookup by token_hash, provider rows, audit append + query.",
    [
        "role grant/revoke is idempotent and queryable per user",
        "session rotation records rotated_from and keeps the old row for reuse detection",
        "audit append takes actor, action, target, before/after and answers the AuditPage query",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./store -count=1",
    "Verified present 2026-11-01 with per-entity tests + edge_test.go + lifecycle_test.go.",
)

card(
    "S1-13",
    "1",
    "main.go boot order + router with /health",
    "review",
    ["server/main.go", "server/router.go"],
    ["S1-01", "S1-08"],
    "Boot order as the dependency graph, the operator-readable boot log (recording defaults, retention, detection providers, how you get in), lifecycle ctx, graceful Shutdown.",
    [
        "boot log states recording + retention defaults and the auth path (local admin / OIDC / neither)",
        "ctx cancelled at SIGINT/SIGTERM, server Shutdown with timeout",
        "/health answers without touching the DB; readiness line does",
        "Step 2-7 workers insert into this order, not after it",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go build ./...",
    "Verified present 2026-11-01 (Step 1 scope: config -> store -> router).",
)

card(
    "S1-14",
    "1",
    "Step 1 suite green in golang:1.26 (go build + go test ./...)",
    "in-progress",
    ["server/"],
    ["S1-01", "S1-02", "S1-03", "S1-04", "S1-05", "S1-06", "S1-07", "S1-08", "S1-09", "S1-10", "S1-11", "S1-12", "S1-13"],
    "The whole Step 1 data layer compiles and passes its tests in the golang:1.26 container (the host has no Go).",
    [
        "go build ./... clean",
        "go test ./... -count=1 exits 0",
        "no test needs Postgres (sqlite-backed)",
        "run recorded in the commit message",
    ],
    "docker run --rm -v $PWD/server:/src -v nvr_gomodcache:/go/pkg/mod -w /src golang:1.26 sh -c 'go build ./... && go test ./... -count=1'",
    "Host has no Go: every Go check runs in the container with the nvr_gomodcache volume. OBSERVED 2026-11-01: build clean, config + models pass, store FAILS — TestAccountLookupShapes (store/auth_test.go:77) 'expected no account for an empty subject, got <nil>'. That assertion is the card: fix the empty-subject lookup (or the assertion) before anything stacks on it.",
)

# ----------------------------------------------------------------------------------
# Step 2 — auth: JWT sessions, break-glass admin, OIDC, WebSocket
# ----------------------------------------------------------------------------------

card(
    "S2-01",
    "2",
    "auth/jwt.go: HS256 access + refresh with rotation and reuse detection",
    "backlog",
    ["server/auth/jwt.go"],
    ["S1-06", "S1-12"],
    "NewJWTAuth(secret), Issue(userID, roles, ttl), Verify(token) -> *Claims, IssueRefresh, VerifyRefresh. Access TTL 30 min, refresh 14 d, rotation with rotated_from reuse detection.",
    [
        "the alg in the token header is never trusted (HS256 only)",
        "expired/foreign-secret/garbage tokens each fail with a distinct error",
        "a rotated-away refresh token is rejected when replayed (reuse detection)",
        "claims carry userID + roles and round-trip through Verify",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./auth -count=1",
    "Copy OurWay's auth/jwt.go API shape, not a new design.",
)

card(
    "S2-02",
    "2",
    "auth/argon2.go: password hash + verify",
    "backlog",
    ["server/auth/argon2.go"],
    [],
    "argon2id hash/verify via golang.org/x/crypto/argon2, used by the break-glass admin and PUT /api/auth/password.",
    [
        "hash encodes params + salt in the stored string",
        "verify is constant-time and false on mismatch",
        "params recorded in the card/commit (they are a decision, not a default)",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./auth -count=1",
    "",
)

card(
    "S2-03",
    "2",
    "store/bootstrap.go: seed roles + break-glass admin, recorded in settings",
    "backlog",
    ["server/store/bootstrap.go"],
    ["S1-11", "S1-12", "S2-02"],
    "Seed roles admin/operator/viewer; create BOOTSTRAP_ADMIN_USER with role admin only when BOOTSTRAP_ADMIN_PASSWORD is non-empty and no admin exists; record settings key bootstrap_admin_seeded_at so it never re-seeds.",
    [
        "deleting the admin account does not re-create it on next boot",
        "empty password creates no local admin",
        "the three builtin roles exist after a cold boot",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./store -count=1 -run Bootstr",
    "This is the path verify.sh step 2 depends on.",
)

card(
    "S2-04",
    "2",
    "sso/ port: discovery, jwks, oauth, contract (SAML removed, kind fixed to oidc)",
    "backlog",
    [
        "server/sso/discovery.go",
        "server/sso/jwks.go",
        "server/sso/oauth.go",
        "server/sso/contract.go",
    ],
    ["S1-07", "S1-12"],
    "OurWay's hand-rolled OIDC authorization-code + PKCE + nonce, with the exact named failure strings kept because the console renders them as states.",
    [
        "DiscoveryOK/Unreachable/BadDocument/IssuerMismatch/NoCodeFlow strings verbatim",
        "the five IdentityError reasons ported (subject missing, email unverified, allow-list, id_token/userinfo disagreement, provider disabled)",
        "PKCE S256 + nonce binding; HandleCallback ordering (signed id_token -> userinfo agreement -> contract gate -> resolve account)",
        "JIT provisioning at the viewer floor; uniqueUsername collision handling; JWKS refresh-on-unknown-kid exactly once",
        "no SAML file is copied",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./sso -count=1",
    "The failure strings are the console's states; do not rename them.",
)

card(
    "S2-05",
    "2",
    "sso/testidp: in-repo test IdP",
    "backlog",
    ["server/sso/testidp/testidp.go"],
    ["S2-04"],
    "Copy of the OurWay test IdP: flags -listen, -client, -secret, -issuer; serves discovery, jwks, authorize/consent, token, userinfo for verify.sh step 8.",
    [
        "go run ./sso/testidp -listen :9500 -client nvrdev -secret devsecret -issuer http://localhost:9500 boots",
        "the scripted authorize -> consent -> callback walk completes with curl -b jar -L",
    ],
    "cd server && go run ./sso/testidp -listen :9500 -client nvrdev -secret devsecret -issuer http://localhost:9500",
    "No bundled IdP in v1; this is the only IdP the tests use.",
)

card(
    "S2-06",
    "2",
    "Auth routes: status, login, refresh, logout, profile, password (registration 403)",
    "backlog",
    ["server/api/auth.go"],
    ["S2-01", "S2-03"],
    "GET /api/auth/status {local_login, oidc[]}; POST login/refresh/logout; GET /api/auth/profile; PUT /api/auth/password; POST register answers 403 registration is admin-only.",
    [
        "status answers both buttons the login screen draws, from rows not from env",
        "login sits behind the rate limiter",
        "refresh rotates and reuses-detected per S2-01",
        "registration returns 403 {\"error\":\"registration is admin-only\"}",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./api -count=1 -run Auth",
    "",
)

card(
    "S2-07",
    "2",
    "SSO routes: authorize, callback, exchange (+ state row carrying nonce + verifier)",
    "backlog",
    ["server/api/sso_routes.go"],
    ["S2-04", "S2-05"],
    "GET /api/auth/sso/:provider/authorize (writes a short-lived state row), GET .../callback (redirect to ${WEB_URL}/auth/callback#access_token=..&refresh_token=..), POST /api/auth/sso/exchange.",
    [
        "state row is short-lived and single-use",
        "the fragment handoff URL matches the plan's shape exactly (SPA handoff)",
        "exchange answers profile + roles from a bearer token",
        "identities routes: GET /api/identities, POST /api/identities/:id/link (audited)",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./api -count=1 -run SSO",
    "Redirect URI documented in docs/installation.md is ${WEB_URL}/api/auth/sso/<provider>/callback — state it verbatim.",
)

card(
    "S2-08",
    "2",
    "api/middleware.go: auth, role gates, internal token, audit on denial",
    "backlog",
    ["server/api/middleware.go"],
    ["S2-01", "S1-12"],
    "AuthMiddleware, RequireRole, RequireAnyRole, audit row on denial, rate limiter per user, plus InternalAuthMiddleware for /api/internal/* (constant-time compare against WORKER_TOKEN).",
    [
        "empty WORKER_TOKEN -> every internal route answers 403 {\"error\":\"internal api disabled\"}",
        "role denial is audited and answers 403 {\"error\":\"insufficient role\"}",
        "RATE_LIMIT_PER_MINUTE respected; burst 0 = a tenth",
        "the token is read from Authorization: Bearer, never from the query (WS is the documented exception)",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./api -count=1 -run Middleware",
    "Copy OurWay's middleware.go behavior.",
)

card(
    "S2-09",
    "2",
    "Users + roles admin routes",
    "backlog",
    ["server/api/users.go"],
    ["S2-08"],
    "GET/POST/PUT/DELETE /api/users and GET/POST /api/users/:id/roles, admin only, every write audited.",
    [
        "admin-only gate (viewer/operator get 403)",
        "deleting the last admin is refused (break-glass protection)",
        "role mapping ownership: admin cannot map to viewer -> 422",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./api -count=1 -run Users",
    "",
)

card(
    "S2-10",
    "2",
    "ws/hub.go: local hub, token auth, origin guard, the seven message types",
    "backlog",
    ["server/ws/hub.go"],
    ["S2-01"],
    "nhooyr.io/websocket hub, ?token= auth, origin check from WS_ORIGINS/WEB_URL, no Redis. Envelope {type, payload, ts}; types event.new, event.update, event.close, camera.status, recorder.status, storage.queue, presence.",
    [
        "bad token closes the socket; foreign origin is rejected",
        "envelope shape and type names are exact (the console switches on them)",
        "fan-out reaches every connected client",
        "one mutation handler broadcasts exactly one type",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./ws -count=1",
    "Single instance: no Redis, the hub is local (plan decision 8).",
)

card(
    "S2-11",
    "2",
    "Step 2 suites: sso discovery/jwks/oauth/contract, api/oidc, ws/hub, auth",
    "backlog",
    [
        "server/sso/discovery_test.go",
        "server/sso/jwks_test.go",
        "server/sso/oauth_test.go",
        "server/sso/contract_test.go",
        "server/api/oidc_test.go",
        "server/ws/hub_test.go",
        "server/auth/jwt_test.go",
    ],
    ["S2-01", "S2-04", "S2-05", "S2-06", "S2-07", "S2-10"],
    "OurWay's suites ported as-is minus SAML, plus the authorize -> callback -> exchange walk against testidp.",
    [
        "all four discovery states + JWKS kid refresh-once asserted",
        "all five IdentityError strings asserted",
        "JIT user roles == [viewer]; viewer POST /api/v1/cameras -> 403",
        "hub: token auth, origin rejection, fan-out to two clients",
        "refresh rotation asserted on POST /api/auth/refresh",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./... -count=1",
    "",
)

# ----------------------------------------------------------------------------------
# Step 3 — cameras + go2rtc config sync + live view plumbing
# ----------------------------------------------------------------------------------

card(
    "S3-01",
    "3",
    "go2rtc/client.go: streams, put/delete, ApplyConfig, Frame, WebRTCOffer",
    "backlog",
    ["server/go2rtc/client.go", "server/go2rtc/client_test.go"],
    ["S1-01"],
    "The API client for the go2rtc sidecar (new; OurWay has no equivalent): GET /api/streams, PUT/DELETE /api/streams, POST /api/config with confirm via Streams within 5 s, GET /api/frame.jpeg, POST /api/webrtc for server-side probing only.",
    [
        "basic auth from GO2RTC_USERNAME/GO2RTC_PASSWORD",
        "3 retries with 500 ms backoff",
        "typed errors ErrGo2rtcUnreachable and ErrStreamRejected quoting the go2rtc body",
        "httptest fake asserts the query pairs (?src=&name=), the config body, the delete",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./go2rtc -count=1",
    "Browsers talk to go2rtc's /api/ws directly (Step 8) — v1 needs no server relay.",
)

card(
    "S3-02",
    "3",
    "go2rtc/config.go: render go2rtc.yaml from camera rows (golden file)",
    "backlog",
    ["server/go2rtc/config.go", "server/go2rtc/config_test.go"],
    ["S1-03", "S3-01"],
    "Pure function rendering the full YAML: api/rtsp/webrtc listens, <slug>: [<source_uri>], and <slug>_det only when DetectionEnabled using the GO2RTC_MJPEG_* defaults or the camera row.",
    [
        "golden test asserts exact bytes for two cameras (one detection-off, one demo slug)",
        "slug validated ^[a-z0-9][a-z0-9-]{1,31}$ and unique",
        "ffmpeg mjpeg source string carries width/height/-r exactly",
        "the demo-only rule for slug demo-doorbell is documented in the file as demo-only",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./go2rtc -count=1 -run Config",
    "Slug IS the stream name — no mapping table (plan Key Design Decision).",
)

card(
    "S3-03",
    "3",
    "Camera API: CRUD, probe, snapshot, slug rules",
    "backlog",
    ["server/api/cameras.go", "server/api/cameras_test.go"],
    ["S1-10", "S3-01", "S3-02"],
    "GET/POST/PUT/DELETE /api/v1/cameras[/:id] (operator/admin; admin for delete), POST .../probe, GET .../snapshot proxying Frame with cache=5s&width=640.",
    [
        "duplicate/invalid slug -> 409, validation message names the pattern",
        "delete removes rows + local media + enqueues remote purge jobs",
        "probe answers {online, producers} from Streams",
        "snapshot sets Cache-Control: private, max-age=5",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./api -count=1 -run Cameras",
    "The seeder in scripts/dev.sh --demo uses this route, not the DB (Step 10).",
)

card(
    "S3-04",
    "3",
    "Zone writes: PUT /api/v1/cameras/:id/zones + audit",
    "backlog",
    ["server/api/zones.go"],
    ["S1-03", "S3-03"],
    "Polygon validation (3-32 vertices, coordinates 0..1) answering 422 {\"error\":\"zone polygon invalid\"}; every zone write is an audit row.",
    [
        "open polygon is closed implicitly; vertex count and bounds enforced",
        "rule accepted only in {intrusion, loiter}",
        "audit row records before/after",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./api -count=1 -run Zone",
    "Coordinates stay normalized 0..1 so a resolution change cannot invalidate a rule.",
)

card(
    "S3-05",
    "3",
    "Apply go2rtc config on every camera write + at boot",
    "backlog",
    ["server/api/cameras.go", "server/main.go", "server/router.go"],
    ["S3-01", "S3-02", "S3-03"],
    "One write path: every create/update/delete re-renders the YAML and calls ApplyConfig, so config cannot drift from rows; boot applies it after a Streams probe.",
    [
        "create/update/delete each call ApplyConfig exactly once (counted in the fake go2rtc)",
        "boot applies config after probing go2rtc, and a go2rtc outage at boot is logged, not fatal",
        "no other code path writes go2rtc's config",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./api -count=1 -run Apply",
    "",
)

card(
    "S3-06",
    "3",
    "Role floor matrix (viewer read / operator CRUD / admin) + camera suite",
    "backlog",
    ["server/api/cameras_test.go"],
    ["S2-08", "S3-03", "S3-04"],
    "viewer reads cameras/events/clips; operator adds camera CRUD + probe; admin adds users, storage backends, webhooks, OIDC provider CRUD.",
    [
        "viewer read / operator write / viewer write-denied matrix asserted",
        "GET/POST/PUT/DELETE /api/settings/oidc is admin-only",
        "denials are audited",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./api -count=1 -run Cameras",
    "",
)

# ----------------------------------------------------------------------------------
# Step 4 — recorder: continuous + event-gated segments
# ----------------------------------------------------------------------------------

card(
    "S4-01",
    "4",
    "Recorder supervisor: one goroutine per camera, record_mode dispatch",
    "backlog",
    ["server/media/recorder.go"],
    ["S1-10", "S1-13", "S2-10"],
    "NewRecorder/Run/Close. A supervisor per camera, re-created on camera row change via a 10 s poll on cameras.updated_at. continuous = always recording; events = starts on the first signal (OpenWindow), stops POST_ROLL_SECONDS after the window closes; off = never spawns.",
    [
        "Close() closes open ffmpeg processes so a restart resumes recording",
        "record_mode change on the row takes effect within one poll",
        "OpenWindow(cameraID) is callable from signal ingest (S6-03)",
        "every goroutine exits when the lifecycle ctx is cancelled",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./media -count=1",
    "OurWay's recordings/writer.go is a frame-sequence writer: copy the process-ownership pattern, not the code.",
)

card(
    "S4-02",
    "4",
    "ffmpeg ingest command + Generation counter persisted in settings",
    "backlog",
    ["server/media/recorder.go"],
    ["S4-01"],
    "The exact exec.CommandContext line from the plan (rtsp tcp ingest of GO2RTC_RTSP_URL/<slug>, -c copy, mp4 segments, csv segment list) with per-camera Generation persisted at settings key recorder_gen_<camera_id>.",
    [
        "filenames are <slug>-<generation>-<n>.mp4 and the list prefix matches",
        "Generation increments at each spawn and survives a restart (settings-backed)",
        "-segment_start_number continues the numbering after a restart",
        "segment length is SEGMENT_SECONDS from config",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./media -count=1 -run Recorder",
    "No ffmpeg on the host: the test uses lavfi inside the container image that carries ffmpeg.",
)

card(
    "S4-03",
    "4",
    "Segment discovery: tail the ffmpeg CSV into segment rows on the server clock",
    "backlog",
    ["server/media/segments.go", "server/media/recorder.go"],
    ["S4-02", "S1-10"],
    "Parse appended CSV lines (filename,start_time,end_time), insert Segment rows with StartTS = procStart + start_time, EndTS = procStart + end_time, Seconds, Bytes from os.Stat, Kind = continuous.",
    [
        "absolute times come from the supervisor's start time — no camera clock, no filename parsing",
        "the CSV offset is tracked and reset on restart (no re-ingest, no skips)",
        "rows carry bytes > 0 and match the file on disk",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./media -count=1 -run Segment",
    "This is the plan's clock rule made concrete: the server clock owns event time.",
)

card(
    "S4-04",
    "4",
    "Crash handling: ffmpeg exit -> log + camera.status offline + restart backoff",
    "backlog",
    ["server/media/recorder.go"],
    ["S4-01", "S4-03", "S2-10"],
    "ffmpeg exit logs, broadcasts camera.status offline, restarts after 10 s with 60 s max backoff; CSV offset resets; a recovered camera resumes numbering from persisted Generation.",
    [
        "killed process restarts and numbering continues (asserted)",
        "backoff caps at 60 s and resets on a healthy segment tick",
        "camera.status producers reflect the truth, not the intent",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./media -count=1 -run Crash",
    "",
)

card(
    "S4-05",
    "4",
    "GET /api/v1/segments — the timeline manifest",
    "backlog",
    ["server/api/segments.go"],
    ["S1-10", "S2-08", "S4-03"],
    "Manifest for the scrubber: ?camera_id=&from=&to=, viewer role, rows in StartTS order.",
    [
        "window filter excludes non-overlapping segments",
        "limit sane and documented; response is the manifest the scrubber draws",
        "empty window answers [] (not 404)",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./api -count=1 -run Segments",
    "",
)

card(
    "S4-06",
    "4",
    "recorder.status broadcast (one per segment tick)",
    "backlog",
    ["server/media/recorder.go"],
    ["S2-10", "S4-03"],
    "Broadcast {camera_id, mode, segment_seconds, last_segment_at} per completed segment — not per frame.",
    [
        "one broadcast per segment",
        "the LivePage status line reads from it (no polling)",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./media -count=1 -run Status",
    "",
)

card(
    "S4-07",
    "4",
    "Recorder suite: lavfi fake source writes real segments",
    "backlog",
    ["server/media/recorder_test.go"],
    ["S4-01", "S4-02", "S4-03", "S4-04"],
    "Real ffmpeg (lavfi testsrc2 through the source_uri override hook) produces 3+ segments; rows match the ffmpeg CSV; generation increments across a forced kill; restart resumes numbering.",
    [
        "start_ts/end_ts match the ffmpeg CSV",
        "bytes > 0 on every row",
        "forced process kill -> generation increments, numbering resumes",
        "test skips loudly (not silently) when ffmpeg is absent",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 sh -c 'command -v ffmpeg && go test ./media -count=1'",
    "CI installs ffmpeg (apt-get install -y ffmpeg) for these tests.",
)

# ----------------------------------------------------------------------------------
# Step 5 — detection worker (Python, detector/)
# ----------------------------------------------------------------------------------

card(
    "S5-01",
    "5",
    "detector/pyproject.toml: package nvr_detector + pinned deps",
    "backlog",
    ["detector/pyproject.toml"],
    ["S0-01"],
    "requires-python >=3.11,<3.13; onnxruntime==1.31.1, opencv-python-headless==5.0.0.93, numpy>=2.1,<3, requests>=2.32, pydantic>=2.10; dev extra pytest>=8.3; the slow marker registered.",
    [
        "pip install -e detector[dev] resolves on Python 3.12",
        "pytest markers: slow registered (the model smoke test is slow)",
        "package name is nvr_detector",
    ],
    "docker run --rm -v $PWD/detector:/src -w /src python:3.12-slim sh -c 'pip install -e .[dev] && python -c \"import nvr_detector\"'",
    "Host Python 3.14 has no pip/venv: every Python command runs in the detector image.",
)

card(
    "S5-02",
    "5",
    "mjpeg.py: MjpegStream over requests, boundary split, cv2 decode",
    "backlog",
    ["detector/nvr_detector/mjpeg.py", "detector/tests/test_decode.py"],
    ["S5-01"],
    "Streaming GET on http://go2rtc:1984/api/stream.mjpeg?src=<slug>_det, boundary parsed from Content-Type, each part cv2.imdecode'd; reconnect with 5 s backoff. No cv2.VideoCapture HTTP backend.",
    [
        "boundary parsed from the response header, not hard-coded",
        "socket close reconnects and the iterator resumes",
        "timeout honored (DET_HTTP_TIMEOUT)",
    ],
    "cd detector && python3 -m pytest tests -q -m 'not slow'",
    "",
)

card(
    "S5-03",
    "5",
    "model.py: letterbox preprocess, YOLOv8 decode, NMS, class sets",
    "backlog",
    ["detector/nvr_detector/model.py", "detector/tests/test_decode.py"],
    ["S5-01"],
    "InferenceSession(path, providers); preprocess = letterbox 640 (pad 114), BGR->RGB, /255, CHW, batch, float32; postprocess handles [1,84,8400] -> xywh -> corners -> clip -> argmax class -> conf/area filters -> numpy NMS (IoU 0.45, max 100).",
    [
        "CLASS_NAMES is the 80-name COCO list verbatim; IGNORED_CLASSES drops all but person, bicycle, car, motorcycle, bus, truck, dog, cat, bear",
        "min_confidence and min_area_ratio filters applied",
        "letterbox round-trip maps a padded-edge box back inside the frame",
        "providers list comes from DET_PROVIDERS, comma-split",
    ],
    "cd detector && python3 -m pytest tests/test_decode.py -q",
    "Detection-eligible set is a decision, not a filter knob.",
)

card(
    "S5-04",
    "5",
    "tracker.py: greedy IoU tracker (>=0.30, 6-frame expiry)",
    "backlog",
    ["detector/nvr_detector/tracker.py", "detector/tests/test_tracker.py"],
    ["S5-03"],
    "Best-first IoU matching; unmatched detections spawn tracks; tracks expire after 6 frames; ids monotonic per camera.",
    [
        "5 synthetic frames keep one stable track_id",
        "occlusion gap <= 6 frames resumes the same id; > 6 spawns a new one",
    ],
    "cd detector && python3 -m pytest tests/test_tracker.py -q",
    "The loiter rule is per track_id, so tracker stability is the loiter contract.",
)

card(
    "S5-05",
    "5",
    "zones.py: point-in-polygon, bottom-center centroid, intrusion + loiter",
    "backlog",
    ["detector/nvr_detector/zones.py", "detector/tests/test_zones.py"],
    ["S5-04"],
    "Ray-cast point_in_polygon; centroid = ((x1+x2)/2, y2) normalized by frame size (feet, not torso); intrusion fires on a matched class inside the polygon at conf >= zone.min_confidence; loiter fires when the same track_id stays inside for >= dwell_seconds (exit resets).",
    [
        "concave polygon, on-edge and outside cases asserted",
        "box [100,50,140,150] on 640x360 -> (0.21875, 0.4167)",
        "loiter fires only after dwell_seconds of continuous presence; leaving resets the timer",
    ],
    "cd detector && python3 -m pytest tests/test_zones.py -q",
    "Coordinates are normalized 0..1, matching the server's zone contract.",
)

card(
    "S5-06",
    "5",
    "events.py: open/extend/close state machine + snapshot on open",
    "backlog",
    ["detector/nvr_detector/events.py", "detector/tests/test_events.py"],
    ["S5-05"],
    "Per camera+zone: open (first qualifying hit, emits snapshot JPEG q82 width 640 capped 200 KB) -> extend (hit within 10 s) -> close (10 s idle, or age >= EVENT_MAX_SECONDS). event_key = camera:zone:rule:open_unix.",
    [
        "event_key stable across extends",
        "10 s idle close and EVENT_MAX_SECONDS forced close asserted",
        "open carries exactly one snapshot",
    ],
    "cd detector && python3 -m pytest tests/test_events.py -q",
    "ts in the signal is informational: the server clock is authoritative (S6-03).",
)

card(
    "S5-07",
    "5",
    "motion.py: CPU gate (160px grayscale absdiff, 1%, continuous mode only)",
    "backlog",
    ["detector/nvr_detector/motion.py", "detector/tests/test_motion.py"],
    ["S5-01"],
    "Downscale to width 160 grayscale, absdiff vs previous, threshold 17, countNonZero/area < 0.010 -> skip inference. Never gates events-mode cameras (a gate would erase the trigger).",
    [
        "identical frames skip inference",
        "a moved 16x16 block passes the gate",
        "gate is provably inert in events record mode (asserted at the pipeline seam)",
    ],
    "cd detector && python3 -m pytest tests/test_motion.py -q",
    "",
)

card(
    "S5-08",
    "5",
    "client.py: ServerClient config poll + signal POST with backoff",
    "backlog",
    ["detector/nvr_detector/client.py", "detector/tests/test_signals.py"],
    ["S5-01"],
    "GET /api/internal/v1/detector/config every 5 s; POST /api/internal/v1/signals; 3 retries + min(5*2^n, 60) s backoff; buffer flushed every 2 s, max 40 per POST, drop-oldest with a counted signals_dropped log.",
    [
        "serialization shape matches the plan's signal JSON keys exactly",
        "500, 500, 200 -> 3 attempts asserted against a local http.server stub",
        "buffer survives a dead server and flushes on recovery",
        "40-signal cap drops oldest with a counted log line",
    ],
    "cd detector && python3 -m pytest tests/test_signals.py -q",
    "",
)

card(
    "S5-09",
    "5",
    "pipeline.py + __main__.py: per-camera thread and env entrypoint",
    "backlog",
    [
        "detector/nvr_detector/pipeline.py",
        "detector/nvr_detector/__main__.py",
    ],
    ["S5-02", "S5-03", "S5-04", "S5-05", "S5-06", "S5-07", "S5-08"],
    "Frame (respecting DetectionFPS) -> motion gate -> model.run -> tracker.update -> zones.evaluate -> events.transition -> signal buffer. Env: NVR_SERVER_URL, NVR_WORKER_TOKEN, DET_PROVIDERS, DET_MODEL_PATH, DET_HTTP_TIMEOUT, DET_MAX_CAMERAS. Exit 0 on SIGTERM after flushing.",
    [
        "only DetectionEnabled && Active cameras are processed (from the config payload)",
        "SIGTERM exits 0 with signals flushed",
        "DET_MAX_CAMERAS respected",
        "python -m nvr_detector boots against a stub server and logs the camera set it picked up",
    ],
    "docker run --rm -v $PWD/detector:/src -w /src python:3.12-slim sh -c 'pip install -e . && timeout 20 python -m nvr_detector || true'",
    "",
)

card(
    "S5-10",
    "5",
    "Internal worker API: detector config payload + signals intake",
    "backlog",
    ["server/api/detector_api.go", "server/api/detector_api_test.go"],
    ["S2-08", "S1-10", "S1-11", "S6-03"],
    "GET /api/internal/v1/detector/config (only DetectionEnabled && Active cameras, providers split from DET_PROVIDERS, event_max_seconds) and POST /api/internal/v1/signals answering {accepted, events[]}.",
    [
        "unknown camera_id -> signal skipped with \"unknown camera\"",
        "camera with detection disabled -> skipped with \"detection disabled\"",
        "snapshot_base64 over 250 KB stored as absent, event survives",
        "record_mode=events signal opens the recorder window (Recorder.OpenWindow)",
        "routes sit behind InternalAuthMiddleware (S2-08)",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./api -count=1 -run Internal",
    "No /window route: windows open from signals (plan Step 5).",
)

card(
    "S5-11",
    "5",
    "Detector suite: decode, zones, events, tracker, motion, signals + slow model smoke",
    "backlog",
    [
        "detector/tests/test_decode.py",
        "detector/tests/test_zones.py",
        "detector/tests/test_events.py",
        "detector/tests/test_tracker.py",
        "detector/tests/test_motion.py",
        "detector/tests/test_signals.py",
        "detector/tests/test_model_smoke.py",
    ],
    ["S5-02", "S5-03", "S5-04", "S5-05", "S5-06", "S5-07", "S5-08"],
    "The shape CI runs: pytest -m 'not slow' green on host-free containers, plus the slow smoke asserting a person box conf >= 0.5 on bus.jpg with yolov8n.onnx.",
    [
        "hand-planted [1,84,8400] tensor decodes to expected corners/classes/confidences",
        "NMS suppresses an IoU 0.62 pair keeping the higher conf",
        "test_model_smoke.py (slow) passes inside the detector image",
        "no test touches the network",
    ],
    "docker run --rm -v $PWD:/src -w /src/detector python:3.12-slim sh -c 'pip install -e .[dev] && pytest tests -q -m \"not slow\"'",
    "The slow marker keeps the model/decode contract in CI without costing minutes per push.",
)

card(
    "S5-12",
    "5",
    "Model provenance: yolov8n.pt -> onnx export at image build, hashes recorded",
    "backlog",
    ["docker/Dockerfile.detector", "testdata/fixtures.json"],
    ["S9-02", "S10-01"],
    "Stage 1: ultralytics==8.4.195 + onnx==1.17.0, download the pinned yolov8n.pt URL (6 549 796 bytes), export onnx imgsz=640 opset=17 dynamic=False, copy into stage 2.",
    [
        "export runs at build time from the pinned URL; a network-less build fails loudly",
        "exported yolov8n.onnx sha256 recorded in testdata/fixtures.json on the first successful build",
        "yolov8n.onnx is never committed",
    ],
    "docker build -f docker/Dockerfile.detector -t nvr-detector:probe .",
    "Plan's Verification section: a pin break is a pin bump recorded in the same commit.",
)

# ----------------------------------------------------------------------------------
# Step 6 — events, clip assembly, snapshots, webhooks
# ----------------------------------------------------------------------------------

card(
    "S6-01",
    "6",
    "clips.go: windowed concat with padding + the empty-window rule",
    "backlog",
    ["server/media/clips.go", "server/media/clips_test.go"],
    ["S1-10", "S4-03", "S2-10"],
    "On close (and at write time for the open case) schedule assembly POST_ROLL_SECONDS after EndedAt: window [StartedAt - PRE_ROLL, EndedAt + POST_ROLL], segments from the table, ffmpeg concat -c copy -movflags +faststart.",
    [
        "no overlapping segments -> clip_key=\"\", ClipSeconds=0, log 'clip skipped: no segments for window', event stays visible with its snapshot",
        "ClipSeconds = min(window, sum(segment seconds))",
        "a measured clip under 2 s is padded by repeating the last segment once",
        "ffprobe-measured duration within +/-1.5 s of the requested window",
        "time.AfterFunc per open event, cancelled on close",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./media -count=1 -run Clip",
    "record_mode=off yields snapshot-only events — intended, and stated in docs/architecture.md.",
)

card(
    "S6-02",
    "6",
    "Snapshot write: decode signal snapshot_base64 to <SNAPSHOTS_DIR>/<event_id>.jpg",
    "backlog",
    ["server/media/clips.go", "server/api/events.go"],
    ["S5-10", "S6-03"],
    "open-signal snapshots land on disk and set LocalSnapshotPath; the 250 KB rule and the absent-snapshot case are honored.",
    [
        "snapshot written only on open",
        "missing snapshot leaves the event visible with snapshot_url empty",
        "the file is served by GET /api/v1/events/:id/snapshot",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./media -count=1 -run Snapshot",
    "",
)

card(
    "S6-03",
    "6",
    "Event upsert semantics: open/extend/close on the server clock",
    "review",
    ["server/store/events.go", "server/media/events_upsert_test.go"],
    ["S1-10"],
    "open inserts (StartedAt = EndedAt = ts, Open = true); extend advances EndedAt, appends objects capped at 200 oldest-first, unions classes/zones/rules, takes max severity; close sets Open=false + final EndedAt. ts is time.Now().UTC() at receipt.",
    [
        "one event across 3 signals (event_key dedupe)",
        "objects capped at 200, trimmed oldest-first",
        "severity monotone max",
        "post-close extend answers 200 with \"event closed\" and mutates nothing",
        "a signal with a future ts never produces started_at > now()",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./media -count=1 -run Upsert",
    "Audit of code that already exists: store/events.go has UpsertByEventKey (verified 2026-11-01). Check each of the five rules against it and file the test file.",
)

card(
    "S6-04",
    "6",
    "Event API: list filters, detail, delete, snapshot, clip",
    "backlog",
    ["server/api/events.go", "server/api/events_test.go"],
    ["S2-08", "S6-01", "S6-02"],
    "GET /api/v1/events with camera_id/from/to/class/zone_id/severity/q/limit/offset (limit default 50, max 200); GET .../:id; DELETE (admin, audit + purge jobs); GET .../snapshot and .../clip.",
    [
        "q matches class and zone names",
        "limit clamped at 200",
        "delete enqueues purge jobs for both media keys and writes an audit row",
        "gone media answers 404 {\"error\":\"clip not available\"}",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./api -count=1 -run Events",
    "",
)

card(
    "S6-05",
    "6",
    "Clip delivery: local Range/206, remote streamed from byte 0",
    "backlog",
    ["server/api/events.go", "server/api/events_test.go"],
    ["S6-04", "S7-01"],
    "Local file via http.ServeFile (Range support for the scrubber); no local file + ClipUploadedAt -> backend Get, Content-Type video/mp4, Accept-Ranges: bytes, Cache-Control private max-age=3600, streaming from byte 0.",
    [
        "Range: bytes=0-99999 -> 206 against a temp mp4",
        "remote clip streams without a local copy and without seek",
        "the seek gap is documented in docs/architecture.md, not hidden",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./api -count=1 -run Clip",
    "Seek-on-remote is a v2 item (README Roadmap).",
)

card(
    "S6-06",
    "6",
    "webhooks/: signed delivery with 3 attempts + delivery rows",
    "backlog",
    ["server/webhooks/webhooks.go", "server/webhooks/webhooks_test.go"],
    ["S1-11", "S6-03"],
    "On close, POST the plan's payload to every enabled webhook with X-NVR-Signature: sha256=HMAC-SHA256(secret, body) and User-Agent nvr/1; attempts at 2 s / 10 s / 60 s; every attempt is a webhook_deliveries row.",
    [
        "signature recomputes byte-for-byte (test recomputes it)",
        "disabled webhook sends nothing",
        "status, attempt and error recorded per send",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./webhooks -count=1",
    "verify.sh step 9 recomputes the same HMAC externally.",
)

card(
    "S6-07",
    "6",
    "Webhook settings routes (admin) + delivery history",
    "backlog",
    ["server/api/settings.go"],
    ["S2-08", "S6-06"],
    "GET/POST/PUT/DELETE /api/settings/webhooks and GET /api/settings/webhooks/:id/deliveries.",
    [
        "secret is never echoed (secret_set: true is the response field)",
        "every write is an audit row",
        "deliveries list answers the console's 'why did my hook fail' question",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./api -count=1 -run Webhooks",
    "",
)

card(
    "S6-08",
    "6",
    "WS fan-out: event.new / event.update (throttled) / event.close",
    "backlog",
    ["server/media/clips.go", "server/ws/hub.go"],
    ["S2-10", "S6-03"],
    "event.new on open, event.update on extend throttled to 1 per 5 s per event, event.close on close.",
    [
        "throttle asserted (extend storm -> at most one update per 5 s)",
        "two clients both receive the sequence",
        "an open event shows LIVE in the console from these messages alone",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./ws -count=1 -run Event",
    "",
)

# ----------------------------------------------------------------------------------
# Step 7 — storage backends, upload queue, retention
# ----------------------------------------------------------------------------------

card(
    "S7-01",
    "7",
    "storage.go: Backend interface + Open dispatch (+ OpenDefault)",
    "backlog",
    ["server/storage/storage.go"],
    ["S1-04", "S1-11", "S7-02"],
    "Put/Get/Stat/Delete/List/Ping over a Backend interface, kind dispatch (s3, webdav, sftp, fs), OpenDefault from the IsDefault row.",
    [
        "Open returns the driver named by the row's Kind",
        "Ping failure maps to the named errors the API reports (discovery_unreachable, bad credentials, sftp host key changed, bucket missing)",
        "no driver is linked by a binary that does not use it",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./storage -count=1",
    "deps/ already pins the drivers (S1-02).",
)

card(
    "S7-02",
    "7",
    "seal.go: AES-256-GCM secret sealing keyed by SHA-256(NVR_SECRET_KEY)",
    "backlog",
    ["server/storage/seal.go", "server/storage/seal_test.go"],
    ["S1-04"],
    "Seal(plain, key) / Unseal(sealed, key) with random nonce, base64 stored; key derived from NVR_SECRET_KEY (64-hex), falling back to a derivation from JWT_SECRET so a deployment backs up one secret.",
    [
        "round-trip + tamper detection asserted",
        "key derivation asserted (same JWT_SECRET -> same key)",
        "NVR_SECRET_KEY documented as required in production",
        "no sealed value is ever returned by an endpoint",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./storage -count=1 -run Seal",
    "The plan's Step 7 work, already declared in config.go — see S0-06.",
)

card(
    "S7-03",
    "7",
    "fs.go: local dir / NFS mount with .part + rename",
    "backlog",
    ["server/storage/fs.go", "server/storage/fs_test.go"],
    ["S7-01"],
    "RootPath backend: keys become nested relative paths; writes go to <key>.part then os.Rename (no partial objects on a crash); List via filepath.WalkDir.",
    [
        "put/get/stat/delete/list asserted against a temp dir",
        "a crash mid-write leaves no <key> file",
        "path confinement: a key cannot escape RootPath",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./storage -count=1 -run FS",
    "This is the backend verify.sh step 7 seeds (local-tier).",
)

card(
    "S7-04",
    "7",
    "s3.go: minio-go driver, path-style always",
    "backlog",
    ["server/storage/s3.go", "server/storage/s3_test.go"],
    ["S7-01", "S7-02"],
    "endpoint/region/bucket/prefix from the row, EnableSSL = !InsecureTLS, PathStyle true (MinIO, R2, B2, Wasabi, self-hosted), credentials decrypted at Open.",
    [
        "gofakes3 in-test server: put/get/stat/delete/list, prefix layout, EnableSSL=false, path-style asserted",
        "bad credentials -> the named error, not a panic",
        "no secret in any log line",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./storage -count=1 -run S3",
    "gofakes3 is pinned in deps/ for exactly this test.",
)

card(
    "S7-05",
    "7",
    "webdav.go: gowebdav driver over the PROPFIND subset",
    "backlog",
    ["server/storage/webdav.go", "server/storage/webdav_test.go"],
    ["S7-01", "S7-02"],
    "NewClient(endpoint, user, pass) + Connect(); container = first path segment of Prefix; Put via WriteCloser, Get via ReadCloser, List via Propfind(path, 0), Stat via c.Stat.",
    [
        "httptest server implementing the PROPFIND/GET/PUT/DELETE subset the driver issues (protocol-level, not a driver mock)",
        "container derived from Prefix asserted",
        "auth failure maps to 'bad credentials'",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./storage -count=1 -run WebDav",
    "",
)

card(
    "S7-06",
    "7",
    "sftp.go: password/key auth + host-key pinning",
    "backlog",
    ["server/storage/sftp.go", "server/storage/sftp_test.go"],
    ["S7-01", "S7-02"],
    "x/crypto/ssh + pkg/sftp; password or PrivateKeyPath auth; HostKeyFallback off, host key pinned in settings key sftp_hostkey_<backend_id>; a changed pin fails Ping with 'sftp host key changed'; all paths under RootPath.",
    [
        "in-test ssh server (sftp.NewServer over a temp dir): auth, path confinement, pinning, changed-key failure asserted",
        "no unsigned host key is ever accepted",
        "RootPath escape attempts fail",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./storage -count=1 -run SFTP",
    "",
)

card(
    "S7-07",
    "7",
    "Key layout: nvr/<slug>/<YYYY>/<MM>/<DD>/<basename>.mp4, nvr/events/<id>/{clip,snapshot}",
    "backlog",
    ["server/storage/keys.go"],
    ["S7-01"],
    "The exact key layout, one function, so a restore lands the file back in SEGMENTS_DIR.",
    [
        "segment basename is the local file name",
        "clip -> nvr/events/<event_id>/clip.mp4, snapshot -> .../snapshot.jpg",
        "layout asserted by a test on all four drivers",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./storage -count=1 -run Key",
    "",
)

card(
    "S7-08",
    "7",
    "uploader.go: claim, upload, exponential retry, failed at 20, requeue, enable flag",
    "backlog",
    ["server/storage/uploader.go", "server/storage/uploader_test.go"],
    ["S7-01", "S1-11"],
    "Tick every UPLOADER_TICK_SECONDS; ClaimNextJob (FOR UPDATE SKIP LOCKED on Postgres, SELECT + guarded UPDATE in a tx on sqlite); Put then mark uploaded + remote_key; on failure state pending + NextAttemptAt = now + min(5min*2^attempts, 6h); attempts >= 20 -> failed.",
    [
        "claim -> upload -> uploaded asserted",
        "failure produces exponential next_attempt_at",
        "attempts >= 20 lands in failed and is visible on ?state=failed and in storage.queue",
        "a disabled backend claims nothing; in-flight jobs finish",
        "the uploader never deletes local files (the sweeper's job)",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./storage -count=1 -run Uploader",
    "POST /api/storage/backends/:id/requeue?kind= is part of this card.",
)

card(
    "S7-09",
    "7",
    "sweeper.go: the seven sweeps, with the un-uploaded protection window",
    "backlog",
    ["server/sweeper/sweeper.go", "server/sweeper/sweeper_test.go"],
    ["S1-11", "S7-08"],
    "Every 15 min: local segment delete past local_retention_days (keeping uploaded_at IS NULL rows for max(3*local_retention_days, 7 days)), event-referenced segments, event rows past EVENT_RETENTION_DAYS (media first), remote keys past REMOTE_RETENTION_DAYS (0 = forever), webhook_deliveries past 30 d, audit past 365 d, uploaded jobs past 30 d.",
    [
        "un-uploaded footage is never destroyed inside the protection window; force-delete is counted in the log",
        "remote delete only past REMOTE_RETENTION_DAYS, 0 = keep forever asserted",
        "one summary log line with the counts, broadcast as storage.queue",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./sweeper -count=1",
    "Single-writer rule: the sweeper assumes sole ownership of the media tree (no second instance).",
)

card(
    "S7-10",
    "7",
    "Storage API: backends CRUD, test, default, prefixes, queue",
    "backlog",
    ["server/api/storage.go", "server/api/storage_test.go"],
    ["S2-08", "S7-01", "S7-02"],
    "Admin CRUD on /api/storage/backends, POST .../test -> {ok} or {ok:false,error:<named>}, PUT .../default, POST .../prefixes?prefix=, GET /api/storage/queue?state=, PUT .../enabled.",
    [
        "Ping failure mapped to the four named errors, verbatim",
        "secrets never echoed: secret_set: true and no secret field",
        "every backend write is an audit row",
        "queue pagination",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./api -count=1 -run Storage",
    "This is the surface SettingsPage and verify.sh step 7 drive.",
)

# ----------------------------------------------------------------------------------
# Step 8 — web console (React 18 + Vite + Tailwind, OurWay's shape)
# ----------------------------------------------------------------------------------

card(
    "S8-01",
    "8",
    "Console scaffold: package.json, vite, tailwind, tsconfig, index.html",
    "backlog",
    [
        "web/package.json",
        "web/vite.config.ts",
        "web/tsconfig.json",
        "web/tailwind.config.ts",
        "web/postcss.config.js",
        "web/index.html",
    ],
    ["S0-01"],
    "Copied from OurWay: react 18.3.1, react-router-dom 6, axios, zustand 5, lucide-react, recharts, i18next (en only), vitest + @testing-library, @playwright/test. Package nvr-web 0.1.0. /api + /ws proxy with NVR_PROXY_TARGET (default http://localhost:9090).",
    [
        "npm ci && npm run typecheck clean",
        "the proxy honors NVR_PROXY_TARGET (the e2e harness reads it)",
        "no react-window until a list needs it",
    ],
    "cd web && npm ci && npm run typecheck",
    "Node 22 + npm 9 exist on the host, so the console builds natively.",
)

card(
    "S8-02",
    "8",
    "api/client.ts + auth context: bearer, 401 -> refresh once -> retry, session keys",
    "backlog",
    ["web/src/api/client.ts", "web/src/auth/context.tsx"],
    ["S8-01", "S2-06"],
    "Axios instance baseURL /api; request interceptor attaches the bearer; response interceptor on 401 posts /api/auth/refresh once then retries, else clears session and routes to /login. localStorage keys nvr_access_token, nvr_refresh_token, nvr_user, nvr_roles.",
    [
        "401 -> refresh -> retry asserted (vi.stubGlobal('fetch') / axios mock adapter)",
        "refresh failure logs out, not loops",
        "session keys named exactly as the plan's",
    ],
    "cd web && npm run test:run",
    "Copy OurWay's context.tsx shape; rename keys to nvr_*.",
)

card(
    "S8-03",
    "8",
    "LoginPage + AuthCallbackPage: status-driven buttons and the fragment handoff",
    "backlog",
    ["web/src/pages/LoginPage.tsx", "web/src/pages/AuthCallbackPage.tsx"],
    ["S8-02", "S2-07"],
    "Login renders GET /api/auth/status (local form and/or 'Login with <issuer>'); callback reads #access_token/#refresh_token, stores, routes.",
    [
        "a deployment with no local admin shows no local form",
        "the callback parses the fragment and routes to /live",
        "a failed exchange shows a named state, not a spinner",
    ],
    "cd web && npm run test:run",
    "This is the observable half of verify.sh step 8.",
)

card(
    "S8-04",
    "8",
    "GET /api/v1/settings/live (server): the console never hard-codes the media host",
    "backlog",
    ["server/api/settings.go"],
    ["S2-08", "S1-01"],
    "viewer-Readable endpoint answering {go2rtc_public_url} from config, feeding the WebRTC WS base.",
    [
        "answers GO2RTC_PUBLIC_URL verbatim",
        "viewer role allowed; unauthenticated 401",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./api -count=1 -run Live",
    "Server-side card, placed in Step 8 because the player is what needs it.",
)

card(
    "S8-05",
    "8",
    "LivePage + go2rtc.ts: native RTCPeerConnection over go2rtc's WS API",
    "backlog",
    ["web/src/pages/LivePage.tsx", "web/src/live/go2rtc.ts", "web/src/live/go2rtc.test.ts"],
    ["S8-02", "S8-04", "S3-01"],
    "new WebSocket(${GO2RTC_PUBLIC_URL ws}/api/ws?src=${slug}); addTransceiver video+audio recvonly; send {type:'webrtc/offer', value: sdp} after setLocalDescription; apply webrtc/answer; exchange candidates (empty/trickle accepted); ontrack -> video.srcObject.",
    [
        "offer/answer message shapes asserted with a fake WebSocket + fake RTCPeerConnection",
        "no new dependency (native RTCPeerConnection)",
        "a camera with no producers shows offline, not a black box",
        "HLS fallback exists only as a documented mode toggle if WebRTC fails in the target browser",
    ],
    "cd web && npm run test:run",
    "Protocol from go2rtc's internal/api/ws README; server relays nothing in v1.",
)

card(
    "S8-06",
    "8",
    "EventsPage + detail drawer: cards, filters, timeline strip, LIVE row",
    "backlog",
    ["web/src/pages/EventsPage.tsx", "web/src/pages/EventDetail.tsx"],
    ["S8-02", "S6-04", "S8-10"],
    "Event cards (snapshot thumbnail, class chips, severity ring, camera slug, duration), filter chips from query params, drawer with <video src=/api/v1/events/:id/clip> + the segment strip from GET /api/v1/segments + the recorded boxes drawn on the snapshot at StartedAt.",
    [
        "filters round-trip through the URL",
        "open events show LIVE and update from event.update",
        "a missing clip renders 'clip not available', not a broken player",
    ],
    "cd web && npm run test:run",
    "",
)

card(
    "S8-07",
    "8",
    "CamerasPage + ZoneEditor: canvas polygon editor over the snapshot",
    "backlog",
    [
        "web/src/pages/CamerasPage.tsx",
        "web/src/components/ZoneEditor.tsx",
        "web/src/components/ZoneEditor.test.tsx",
    ],
    ["S8-02", "S3-03", "S3-04"],
    "<canvas> over GET /api/v1/cameras/:id/snapshot: click appends a vertex, Enter closes, Escape cancels, drag moves, Delete removes the zone; overlay fill --accent 20% + rule badge; PUT writes the exact ZonesJSON.",
    [
        "normalized 0..1 round-trip asserted",
        "PUT payload shape asserted",
        "a 2-vertex attempt is refused with the server's 422 message",
    ],
    "cd web && npm run test:run",
    "",
)

card(
    "S8-08",
    "8",
    "SettingsPage: backends + test-connection state, OIDC, webhooks, retention",
    "backlog",
    ["web/src/pages/SettingsPage.tsx"],
    ["S8-02", "S7-10", "S6-07", "S2-07"],
    "Storage backends with the named test-connection state, OIDC provider CRUD, webhook CRUD with delivery history, retention settings.",
    [
        "test-connection renders ok / discovery_unreachable / bad credentials / sftp host key changed / bucket missing",
        "secrets show as set/unset, never as values",
        "upload queue counts render from storage.queue",
        "admin-only tab hidden for viewer (verify.sh step: viewer sees no Settings)",
    ],
    "cd web && npm run test:run",
    "Console contract: every mutation shows the server's state, not a spinner.",
)

card(
    "S8-09",
    "8",
    "AuditPage + routes/nav role gating",
    "backlog",
    ["web/src/pages/AuditPage.tsx", "web/src/App.tsx"],
    ["S8-02", "S2-09"],
    "Routes /login, /auth/callback, /live, /events, /events/:id, /cameras, /cameras/:id, /settings, /audit with role-aware nav.",
    [
        "viewer sees no Settings tab",
        "deep links survive reload",
        "audit list answers who did what",
    ],
    "cd web && npm run test:run",
    "",
)

card(
    "S8-10",
    "8",
    "realtime.ts: zustand store over /ws?token= with backoff + stale banner",
    "backlog",
    ["web/src/stores/realtime.ts"],
    ["S8-02", "S2-10"],
    "Subscribe /ws?token=, handle the seven message types exactly, 5 s reconnect backoff capped at 60 s, stale banner when the socket is down.",
    [
        "each message type updates the store (asserted per type)",
        "stale flag on close and cleared on reopen",
        "origin guard understood: a rejected origin says so, silently reconnecting is the failure mode to avoid",
    ],
    "cd web && npm run test:run",
    "",
)

card(
    "S8-11",
    "8",
    "Console suite: vitest + @testing-library (behavior, no snapshots)",
    "backlog",
    ["web/src/**/*.test.ts(x)"],
    ["S8-02", "S8-05", "S8-06", "S8-07", "S8-10"],
    "client.test.ts (401 refresh-retry-then-logout), go2rtc.test.ts (offer/answer shapes), ZoneEditor.test.tsx (normalization + payload), realtime store per type + stale, EventsPage filter chips + card list from a stubbed fetch.",
    [
        "npm run typecheck && npm run test:run exit 0",
        "no snapshot tests (behavior only, per the plan)",
    ],
    "cd web && npm run typecheck && npm run test:run",
    "",
)

card(
    "S8-12",
    "8",
    "Playwright specs: auth, live, cameras, events, storage",
    "backlog",
    ["web/e2e/auth.spec.ts", "web/e2e/live.spec.ts", "web/e2e/cameras.spec.ts", "web/e2e/events.spec.ts", "web/e2e/storage.spec.ts"],
    ["S8-11", "S10-02", "S9-08"],
    "auth: local admin login -> console, viewer sees no Settings; live: player mounts, video readyState >= 1; cameras: create + draw a 4-vertex zone, reload, vertices persisted; events: seeded event in timeline, drawer, clip plays to currentTime > 0; storage: add fs backend, test-connection renders ok.",
    [
        "scripts/e2e-stack.sh test exits 0",
        "specs run against the throwaway sqlite stack, never a dev database",
    ],
    "scripts/e2e-stack.sh restart && scripts/e2e-stack.sh test",
    "Needs S10's stack: file the specs when the stack runs, not before.",
)

# ----------------------------------------------------------------------------------
# Step 9 — containers, compose, scripts, CI
# ----------------------------------------------------------------------------------

card(
    "S9-01",
    "9",
    "Dockerfile.server: alpine builder + runtime with ffmpeg, non-root, healthcheck",
    "backlog",
    ["docker/Dockerfile.server"],
    ["S1-02", "S1-13"],
    "golang:1.26-alpine builder (git, ca-certificates, tzdata; layer-cached go mod download; CGO_ENABLED=0 build with -ldflags stamping config.Version/config.GitCommit), alpine:3.20 runtime with ffmpeg, non-root nvr user, EXPOSE 8080, wget --spider /health.",
    [
        "image builds; version line in the boot log names the stamped version and commit",
        "ffmpeg present at runtime (recorder and clip builder shell out to it)",
        "runs non-root and the healthcheck passes",
    ],
    "docker build -f docker/Dockerfile.server -t nvr-server:probe .",
    "Copy OurWay's Dockerfile.server structure; the ffmpeg line is the nvrWay delta.",
)

card(
    "S9-02",
    "9",
    "Dockerfile.detector: export stage + runtime stage",
    "backlog",
    ["docker/Dockerfile.detector"],
    ["S5-01", "S5-12"],
    "Stage 1 python:3.12-slim installs ultralytics==8.4.195 + onnx==1.17.0, downloads the pinned yolov8n.pt, exports onnx (imgsz 640, opset 17, dynamic=False); stage 2 installs onnxruntime==1.31.1 + opencv-python-headless==5.0.0.93 + numpy + requests + pydantic, copies /models/yolov8n.onnx, non-root, CMD python -m nvr_detector.",
    [
        "build succeeds offline for stage 2 (stage 1 needs network and says so loudly)",
        "python -m pytest tests/test_pipeline_smoke.py -m slow -q passes inside the image",
        "the exported model hash is recorded in testdata/fixtures.json",
    ],
    "scripts/detect-smoke.sh",
    "Detector liveness is data, not a listener: heartbeat in S9-07, compose healthcheck is pgrep -f nvr_detector.",
)

card(
    "S9-03",
    "9",
    "Dockerfile.web + nginx.conf: vite build -> nginx proxying /api and /ws",
    "backlog",
    ["docker/Dockerfile.web", "docker/nginx.conf"],
    ["S8-01"],
    "OurWay's pattern: build stage then nginx:1.27-alpine serving dist, proxying /api and /ws to server:8080, with proxy_buffering off + proxy_request_buffering off on the clip route.",
    [
        "clip route streams (no buffering) and WS upgrade headers set",
        "SPA fallback to index.html for the router's deep links",
    ],
    "docker build -f docker/Dockerfile.web -t nvr-web:probe .",
    "",
)

card(
    "S9-04",
    "9",
    "docker/docker-compose.yml (dev): six services, healthchecks, named volumes",
    "backlog",
    ["docker/docker-compose.yml", "docker/go2rtc.yaml.template"],
    ["S9-01", "S9-02", "S9-03", "S3-01"],
    "name nvr, container_name nvr-*, restart unless-stopped, json-file logging 10m x3, bridge network nvr_net, depends_on service_healthy. postgres:16-alpine (pg_isready), go2rtc (ghcr.io/alexxit/go2rtc:master, ports 1984/8554/8555 tcp+udp, volume go2rtc_config), server (env = Step 1 table, 8080, nvr_data), detector (NVR_SERVER_URL, WORKER_TOKEN, nvr_models), web (3000:80), commented minio service.",
    [
        "docker compose up -d --build reaches healthy",
        "go2rtc API basic auth wired from GO2RTC_USERNAME/GO2RTC_PASSWORD",
        "testdata mounted read-only at /fixtures for the demo camera",
        "MinIO present but commented (verify.sh storage steps use it)",
    ],
    "docker compose -f docker/docker-compose.yml up -d --build && docker compose ps",
    "One server instance only: the sweeper and ffmpeg supervisors assume sole ownership.",
)

card(
    "S9-05",
    "9",
    "docker-compose.prod.yml: image-based, no builds, GO2RTC_PUBLIC_URL required",
    "backlog",
    ["docker-compose.prod.yml"],
    ["S9-04"],
    "Same services with image: ghcr.io/welcometotheweb/nvr/{server,web,detector}:latest, no build sections, no MinIO, GO2RTC_PUBLIC_URL required, OurWay's tag semantics and comments.",
    [
        "config validates without a build context",
        "missing GO2RTC_PUBLIC_URL fails with a message naming the variable",
    ],
    "docker compose -f docker-compose.prod.yml config -q",
    "",
)

card(
    "S9-06",
    "9",
    "GPU overlay: docker-compose.gpu.yml + the CPU-baseline story in docs",
    "blocked",
    ["docker-compose.gpu.yml", "docs/installation.md"],
    ["S9-04"],
    "Overlay adding detector deploy.resources.resolutions.devices gpu, NVIDIA_VISIBLE_DEVICES=all, DET_PROVIDERS=CUDAExecutionProvider,CPUExecutionProvider, documented as shipped-and-documented, verified only on a host with the NVIDIA container toolkit.",
    [
        "overlay composes cleanly with the dev file (docker compose config)",
        "docs/installation.md states plainly that CPU is the verified baseline and CUDA is unverified on the dev box",
        "verification on a GPU host is recorded with the host's toolkit version — NOT done here",
    ],
    "docker compose -f docker/docker-compose.yml -f docker-compose.gpu.yml config -q",
    "BLOCKED on this host: Docker 29.9.2 with runc only, no nvidia-container-toolkit, no /dev/nvidia* nodes. The overlay ships unverified; only the GPU-host check is blocked.",
)

card(
    "S9-07",
    "9",
    "/health + /api/healthz with detector heartbeat (fresh/stale)",
    "backlog",
    ["server/api/health.go", "server/api/detector_api.go"],
    ["S1-13", "S5-09"],
    "Detector POSTs /api/internal/v1/heartbeat {uptime_seconds} every 30 s; server stores settings.detector_heartbeat_at; GET /api/healthz answers detector fresh (< 90 s) or stale. /health stays DB-free.",
    [
        "fresh/stale boundary asserted at 90 s",
        "a detector that never reported answers stale (not ok, not an error)",
        "/health never touches the DB (a healthcheck must not restart a recording server)",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./api -count=1 -run Health",
    "verify.sh steps 1 depends on both endpoints.",
)

card(
    "S9-08",
    "9",
    "scripts: dev.sh, verify.sh, detect-smoke.sh, e2e-stack.sh, coverage.sh, fetch-fixture.sh",
    "backlog",
    ["scripts/"],
    ["S9-04", "S10-02", "S10-03"],
    "dev.sh (compose up --build, --demo seeds via the public API, prints the console URL); verify.sh (OurWay's colored pass/fail/step harness, the 9 gates); detect-smoke.sh (detector image smoke); e2e-stack.sh (restart|test|stop: throwaway sqlite + go run on :9090 with NVR_E2E=1 + vite preview, NVR_E2E_RUN_ID, RATE_LIMIT_PER_MINUTE=1000); coverage.sh (OurWay's ratchet + pytest --cov).",
    [
        "all six run from a fresh clone with no manual setup",
        "each prints a usage line and exits non-zero on failure",
        "e2e-stack.sh never touches a dev database",
    ],
    "scripts/dev.sh --demo",
    "The harness is the product's acceptance surface: these names are already promised in README.",
)

card(
    "S9-09",
    "9",
    ".github/workflows/build.yml: four jobs",
    "backlog",
    [".github/workflows/build.yml"],
    ["P-01", "S1-14", "S5-11", "S8-11"],
    "Server & Detector Tests (setup-go@v5 go 1.26, apt-get ffmpeg, go test ./... -count=1 + coverage.sh, pip install -e detector[dev] + pytest -m 'not slow'); Web Tests (npm ci, typecheck, test:run); E2E (test:e2e:install + scripts/e2e-stack.sh test); Build & Push on tag (docker/build-push-action@v6, three images, :latest + :${GIT_TAG}, :edge on main).",
    [
        "green on the pinned toolchain, not on a floating one",
        "the four job names match the plan",
        "Build & Push fires on tag only",
    ],
    "gh workflow list && gh run list --limit 3",
    "Needs the remote repo (P-01) and gh's workflow scope, which the token has.",
)

card(
    "S9-10",
    "9",
    "Docs content: architecture, configuration, api, installation (decisions, not filler)",
    "backlog",
    ["docs/architecture.md", "docs/api.md", "docs/installation.md", "docs/configuration.md"],
    ["S0-02", "S6-05", "S7-09", "S9-06"],
    "architecture.md: pipeline diagram, the server-clock rule, the remote-playback gap (stream, no seek), single-instance rule (no Redis), the GPU story. api.md: the route table of Steps 2/3/6/7 + the internal worker contract. installation.md: quickstart, .env generation with openssl rand -hex 32, camera setup, OIDC redirect URI verbatim, MinIO/R2/B2 examples, GPU overlay, retention math table.",
    [
        "every env var in config.go documented with its default",
        "the redirect URI stated verbatim: ${WEB_URL}/api/auth/sso/<provider>/callback",
        "the three v1 gaps (remote seek, ONVIF events, GPU verification) are stated in docs, not discovered by users",
    ],
    "grep -c 'redirect' docs/installation.md",
    "The plan calls these decisions-bearing: they are the record of why v1 stops where it stops.",
)

# ----------------------------------------------------------------------------------
# Step 10 — demo fixture, seeding, end-to-end
# ----------------------------------------------------------------------------------

card(
    "S10-01",
    "10",
    "testdata/fixtures.json + scripts/fetch-fixture.sh (pinned assets, recorded hashes)",
    "backlog",
    ["testdata/fixtures.json", "scripts/fetch-fixture.sh"],
    ["S0-01"],
    "Pin bus.jpg (137 419 bytes, CC-BY-4.0) and yolov8n.pt (6 549 796 bytes, AGPL-3.0) with URL, bytes, sha256, license; the fetcher downloads, verifies byte counts and writes sha256s on first run. Files are not committed.",
    [
        "byte counts verified against the pins, fetch fails loudly on a mismatch",
        "fixtures.json carries license + attribution",
        "the three asset files stay out of git (gitignore already lists them)",
    ],
    "scripts/fetch-fixture.sh && cat testdata/fixtures.json",
    "The only network-dependent step in the acceptance path; it runs once per machine.",
)

card(
    "S10-02",
    "10",
    "Demo camera seeded through the public API + the go2rtc demo-only source",
    "backlog",
    ["scripts/dev.sh", "server/go2rtc/config.go"],
    ["S3-02", "S3-03", "S9-04", "S10-01"],
    "POST /api/v1/cameras with the plan's demo-doorbell exec: source, then PUT zones (porch loiter dwell 4 severity 3), then POST /api/storage/backends fs local-tier is_default. go2rtc emits demo-doorbell_det from the mounted /fixtures/bus.jpg so detection never transcodes H.264 twice.",
    [
        "the seeder is an API client, never a DB writer",
        "the demo-only rule is documented as demo-only in config.go and in docs",
        "probe reports online with exec: and ffmpeg: producers",
        "dev-only mount ./testdata:/fixtures:ro present in the dev compose",
    ],
    "scripts/dev.sh --demo",
    "This is the no-physical-camera proof: the whole pipeline runs on a fixture.",
)

card(
    "S10-03",
    "10",
    "verify.sh's nine gates pass (the v1 acceptance gate)",
    "backlog",
    ["scripts/verify.sh"],
    ["S2-06", "S3-03", "S4-05", "S6-04", "S7-08", "S7-10", "S6-06", "S9-07", "S10-02", "S2-05"],
    "1 health + healthz detector fresh; 2 admin login -> access_token; 3 demo camera online with exec:/ffmpeg: producers; 4 >=1 event with person + loiter + porch + seconds >= 4 within 90 s; 5 snapshot 200 image/jpeg > 8 KB and clip 200 video/mp4 with Range -> 206; 6 >=3 segment rows, span ~= SEGMENT_SECONDS, count strictly increasing over 15 s; 7 queue reaches uploaded + clip.mp4 on the tier + disabled backend stops new jobs; 8 in-repo IdP discovery ok + scripted authorize/consent/callback lands on the fragment + exchange roles == [viewer]; 9 webhook listener recomputes X-NVR-Signature and matches.",
    [
        "nine pass lines, exit 0, on a fresh clone",
        "gate 4 is observed end-to-end (the AI-detection claim, not a unit test)",
        "every gate is observed via HTTP, never via the DB",
    ],
    "scripts/verify.sh",
    "Acceptance for v1 = these nine passes + detect-smoke.sh's 'smoke ok' line + all four test suites exit 0.",
)

card(
    "S10-04",
    "10",
    "fs storage tier seeded as default (local-tier) and exercised by verify.sh gate 7",
    "backlog",
    ["scripts/dev.sh", "scripts/verify.sh"],
    ["S7-03", "S7-08", "S10-02"],
    "POST /api/storage/backends {kind: fs, name: local-tier, root_path: /var/lib/nvr/remote, is_default: true} during demo seeding, so the upload tier and the disabled-backend behavior are exercised in the demo stack.",
    [
        "gate 7's find /var/lib/nvr/remote -name clip.mp4 is non-empty",
        "PUT .../enabled false leaves new jobs pending",
    ],
    "scripts/verify.sh  # gate 7",
    "",
)

# ----------------------------------------------------------------------------------
# Step 11 — the suite CI runs
# ----------------------------------------------------------------------------------

card(
    "S11-01",
    "11",
    "Go suite green: go2rtc, media, storage, sweeper, sso, api, webhooks, ws",
    "backlog",
    ["server/"],
    ["S2-11", "S3-06", "S4-07", "S6-05", "S7-10", "S7-09", "S6-06"],
    "Every Step's Go suite, sqlite-backed via store.New(\":memory:\") + NewWithDB, run together as CI runs it.",
    [
        "go test ./... -count=1 exits 0 in golang:1.26 with ffmpeg installed",
        "no test requires Postgres, go2rtc, or a camera",
        "the golden YAML test asserts exact bytes",
    ],
    "docker run --rm -v $PWD/server:/src -w /src golang:1.26 go test ./... -count=1",
    "S1-14 is the current red test in this suite.",
)

card(
    "S11-02",
    "11",
    "Detector suite green: pytest -m 'not slow' + the slow smoke",
    "backlog",
    ["detector/tests/"],
    ["S5-11"],
    "The Python suite CI runs, plus the slow model smoke inside the detector image.",
    [
        "pytest tests -q -m 'not slow' exits 0",
        "scripts/detect-smoke.sh prints 'smoke ok: person box conf>=0.5 on bus.jpg (yolov8n.onnx, CPUExecutionProvider)'",
    ],
    "scripts/detect-smoke.sh",
    "Host Python 3.14 has no pip: run inside the image.",
)

card(
    "S11-03",
    "11",
    "Console suite green: typecheck + vitest",
    "backlog",
    ["web/"],
    ["S8-11"],
    "npm run typecheck && npm run test:run exit 0, behavior tests only.",
    ["typecheck clean", "test:run exits 0 with no snapshot tests"],
    "cd web && npm run typecheck && npm run test:run",
    "",
)

card(
    "S11-04",
    "11",
    "E2E suite green: scripts/e2e-stack.sh test (Playwright chromium)",
    "backlog",
    ["web/e2e/"],
    ["S8-12", "S10-02"],
    "The five specs against the throwaway stack.",
    ["restart + test exit 0", "reports land in web/playwright-report (gitignored)"],
    "scripts/e2e-stack.sh restart && scripts/e2e-stack.sh test",
    "",
)

card(
    "S11-05",
    "11",
    "Coverage ratchet recorded (coverage.sh --record)",
    "backlog",
    ["coverage.baseline.json", "scripts/coverage.sh"],
    ["S11-01", "S11-02", "S11-03"],
    "Baseline recorded once at the end of Step 9 (plan) and ratcheted up from there, never down.",
    [
        "coverage.baseline.json holds real numbers, not 0.0",
        "CI fails when coverage drops below the baseline",
        "the detector is measured with pytest --cov",
    ],
    "scripts/coverage.sh --record",
    "The floor exists to stop a green suite from quietly shrinking.",
)

# ----------------------------------------------------------------------------------
# Process — repo mechanics the board has to track
# ----------------------------------------------------------------------------------

card(
    "P-00",
    "process",
    "git init + baseline commit + this board",
    "done",
    [".git/", "KANBAN.md", "kanban/", "docs/kanban.md"],
    [],
    "nvrWay had no version control (no .git, so no commits, no CI, no remote, and nothing to diff a card's work against). Initialize the repo, commit the Step 1 data layer as the baseline, and seed the per-deliverable board derived from the plan's Steps 0-11.",
    [
        "repository initialized on branch main",
        "every existing source file in the first commit, with .env excluded",
        "one card per plan deliverable, each citing its plan step and a verification command",
        "KANBAN.md generated from the cards, not hand-maintained",
        "move rules written down in docs/kanban.md",
    ],
    "git log --oneline && python3 kanban/board.py --check",
    "Observed 2026-11-01: git init created the repo (was: not a git repository); board seeded 110 cards from NVRWAY_V1_PLAN.md Steps 0-11; gh is authenticated as WelcomeToTheWeb, so the remote is P-01.",
)

card(
    "P-01",
    "process",
    "Create the GitHub remote and push main",
    "done",
    ["origin"],
    ["P-00"],
    "nvrWay is not on a forge: gh is authenticated as WelcomeToTheWeb (repo + workflow scopes), so gh repo create welcometotheweb/nvrWay --private --source . --push, then main is the branch CI and the board can point at. 'CI can run against it' is S9-09's gate, not this card's.",
    [
        "remote named origin, main pushed",
        "visibility decision recorded (private: MIT + ghcr image namespace welcometotheweb/nvr)",
    ],
    "git remote -v && gh repo view --json name,visibility",
    "Observed 2026-11-01: gh repo create -> https://github.com/WelcomeToTheWeb/nvrWay (private), HEAD -> main, main tracks origin/main. gh 2.46.0 has no --main-branch flag; local HEAD was already main. Private matches the ghcr.io/welcometotheweb/nvr/* image namespace (plan decision 11), and the token's workflow scope unblocks S9-09.",
)

card(
    "P-02",
    "process",
    "Record the go.mod pin bump versus the plan (gin 1.12.0, x/crypto 0.57.0, gofakes3 v1.2.0)",
    "backlog",
    ["server/go.mod", "server/go.sum"],
    ["S1-02"],
    "The plan pins gin v1.10.0, x/crypto v0.26.0 and a 2024 gofakes3 pseudo-version; go.mod carries 1.12.0 / 0.57.0 / v1.2.0. The plan's rule is a pin bump recorded in the same commit, not a loosened constraint — so the bump needs writing down with the reason.",
    [
        "commit message (or a note in docs/configuration.md / commit body) names the three pins and why they moved",
        "no require is left unpinned",
        "the OurWay comparison table in the plan is annotated, so the next reader does not 'fix' it back",
    ],
    "cd server && go mod verify",
    "gofakes3 is expected to move (the plan says run go get and record the version); gin and x/crypto are unexplained drift as of the board setup on 2026-11-01.",
)

card(
    "P-03",
    "process",
    "Fix the red store test: empty-subject account lookup",
    "backlog",
    ["server/store/auth_test.go", "server/store/users.go"],
    [],
    "Step 1's suite is not green: TestAccountLookupShapes asserts no account for an empty subject and gets one (auth_test.go:77). Fix the lookup (empty subject must never resolve an account) or the assertion, with the reason stated.",
    [
        "go test ./store -count=1 exits 0",
        "empty subject cannot resolve a user (the SSO contract gate depends on this)",
        "the fix is in the lookup, not a loosened assertion, unless the assertion is what is wrong — say which",
    ],
    "docker run --rm -v $PWD/server:/src -v nvr_gomodcache:/go/pkg/mod -w /src golang:1.26 go test ./store -count=1",
    "Observed 2026-11-01 during board setup; S1-14 stays red until this closes. Highest-priority card on the board: Steps 2, 3 and 6 all read through this lookup.",
)

card(
    "P-04",
    "process",
    "Keep the board honest (move cards, record evidence)",
    "in-progress",
    ["kanban/cards/", "KANBAN.md"],
    ["P-00"],
    "The board only earns its keep if cards move with the work: move the card before starting, to Review when the code is in, to Done when the card's verification command exits 0.",
    [
        "at most 2 cards In Progress at any time",
        "no card reaches Done without its verification command's output quoted in the commit message",
        "KANBAN.md regenerated with python3 kanban/board.py in the same commit as the card move",
    ],
    "python3 kanban/board.py && git diff --stat KANBAN.md",
    "Convention lives in docs/kanban.md.",
)

card(
    "P-05",
    "process",
    "Track server/models: .gitignore's models/ pattern ignored a Go package",
    "backlog",
    [".gitignore", "server/models/"],
    ["S0-05", "S1-03", "S1-04", "S1-05", "S1-06", "S1-07"],
    "The plan's Step 0 line says .gitignore adds `models/`, meaning the detector's ONNX export directory. Unanchored, the pattern also matched `server/models/`, so the entire Step 1 models package (18 files) was never committed: the pushed tree cannot build, because every `nvr/server/models` import resolves to nothing. Anchor the artifact pattern to the repo root (`/models/`, `*.onnx`) and commit the package.",
    [
        "server/models/*.go tracked (18 files: 14 models + json helpers + 3 test files)",
        "the detector model directory stays ignored (`/models/`, `*.onnx`)",
        "a fresh clone builds: go build ./... and go test ./... exit 0 on the tracked tree",
    ],
    "git ls-files server/models | wc -l && git clone --quiet $PWD /tmp/nvrclone && cd /tmp/nvrclone/server && go build ./...",
    "Found while staging Step 2 (2026-11-01): `git add server/models` answered \"paths are ignored by one of your .gitignore files\". S1-03..S1-07 are Done with green suite output quoted — the code existed and passed; it was simply never in git. P-04 (board honesty) covers exactly this failure mode.",
)

card(
    "P-06",
    "process",
    "Mirror the cards into the pi-kanban board's task store (kanban/pi_board.py)",
    "in-progress",
    ["kanban/pi_board.py", "kanban/test_pi_board.py", "kanban/pre-commit.sh", "kanban/seed.py", "docs/kanban.md"],
    ["P-00", "P-04"],
    "The pi-kanban board (the dev tool, not this repo's board) shows its own task list, which was empty while this board carried 111 cards: pi-kanban reads task JSON from ~/.pi/agent/kanban/tasks/<list>/<id>.json and nothing wrote it. kanban/pi_board.py mirrors these cards into that store so the tool's board shows this project's real state — cards stay the source of truth, the mirror is derived and disposable, and --check proves the mirror matches the cards.",
    [
        "one task file per card, carrying the fields pi-kanban parses (id, subject, description, activeForm, status, blocks, blockedBy)",
        "task ids are numeric and stable (step + card number), so the board's numeric sort lists cards the way KANBAN.md does",
        "the five columns collapse onto the board's three states without losing the Blocked reason — it is quoted in the task description",
        "--check reports drift (wrong status, missing task, ghost task) and exits non-zero",
        "re-running the mirror rewrites nothing when nothing changed and deletes tasks whose card is gone",
        "the suite runs on stdlib alone (host Python 3.14 has no pip)",
        "pre-commit regenerates the mirror, so the tool's board cannot lag the cards",
        "the mirror writes only into the board's task store — never into kanban/cards/",
    ],
    "python3 kanban/test_pi_board.py && python3 kanban/pi_board.py && python3 kanban/pi_board.py --check && pi-kanban task list --all",
    "Found 2026-11-02: the board said 'no active tasks' with 111 cards in kanban/cards/. pi-kanban's task store (server.js: TASKS_DIR = <agent dir>/kanban/tasks, one JSON per task; lib/task-store.js normalizeTask defines the fields) is a separate board from KANBAN.md and nothing in this repo wrote it. Its statuses are only pending | in_progress | completed, and its task list sorts by parseInt(id, 10) — so card ids like S2-04 need a numeric id.",
)

card(
    "P-07",
    "process",
    "Ignore the Hindsight artifacts the memory tool drops in the repo root",
    "in-progress",
    [".gitignore"],
    ["P-00", "P-05"],
    "Two files the coding-agent memory tooling writes into the repo root — nvr_cfg2.json (the bank's config dump) and spec.json (the Hindsight HTTP API OpenAPI spec) — sit untracked and unignored, so every git status shows them and a `git add -A` would commit a tool's config into the product. Ignore them with patterns anchored to the repo root, and prove no source path is caught by them.",
    [
        "`/nvr_cfg2.json` and `/spec.json` are ignored (git check-ignore names the pattern)",
        "the patterns are root-anchored with a leading slash, per P-05: an unanchored `spec.json` would also ignore a real spec file in any package",
        "nothing under server/, kanban/, docs/, web/, scripts/ became ignored: git status --ignored -s shows only the two artifacts and the known artifact directories",
        "git status --porcelain is empty on a clean tree",
    ],
    "git check-ignore -v nvr_cfg2.json spec.json && git status --porcelain",
    "Observed 2026-11-02 while committing S2-04: `git status --short` listed `?? nvr_cfg2.json` and `?? spec.json`. They are this machine's memory tooling's own files (the bank config, and the API spec it fetched), not project files — the repo has no OpenAPI document at its root, and its deployment config is .env / docker/, not a bank config.",
)

# ----------------------------------------------------------------------------------
# Emit
# ----------------------------------------------------------------------------------

def render(c: dict) -> str:
    fm = ["---", f"id: {c['id']}", f"step: {c['step']}", f"title: {c['title']}", f"status: {c['status']}"]
    fm.append("paths:")
    fm += [f"  - {p}" for p in c["paths"]]
    fm.append("depends:")
    fm += [f"  - {d}" for d in c["depends"]]
    fm.append(f"updated: {TODAY}")
    fm.append("---")
    body = [
        "",
        f"# {c['id']} — {c['title']}",
        "",
        "## Deliverable",
        "",
        c["deliverable"],
        "",
        "## Exit criteria",
        "",
        *[f"- [{'x' if c['status'] == 'done' else ' '}] {k}" for k in c["criteria"]],
    ]
    if c["verify"]:
        body += ["", "## Verification", "", "`" + c["verify"] + "`"]
    if c["notes"]:
        body += ["", "## Notes", "", c["notes"]]
    return "\n".join(fm + body) + "\n"


def main() -> int:
    check = "--check" in sys.argv
    ids = [c["id"] for c in SEED]
    if len(ids) != len(set(ids)):
        dupes = sorted({i for i in ids if ids.count(i) > 1})
        print(f"duplicate card ids: {dupes}")
        return 1
    missing, skipped = [], []
    for c in SEED:
        path = CARDS / f"{c['id']}.md"
        if path.exists():
            skipped.append(c["id"])
            continue
        missing.append(c["id"])
        if not check:
            path.write_text(render(c))
    print(f"{len(SEED)} cards in the seed set")
    if check:
        print(f"missing: {len(missing)} {missing if missing else ''}")
    else:
        print(f"written: {len(missing)} {missing if missing else ''}")
        print(f"kept:    {len(skipped)}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

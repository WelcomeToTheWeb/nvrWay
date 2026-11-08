# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

One self-hosted installation, one browser console. There is no native app and
no mobile target in v1 — the console is a desktop-width browser tab on the LAN.

## Users

**Primary: the technical self-hosting owner-operator — homelab first.** For
v1 that operator is the project's own author, who installed the stack
themselves, knows what RTSP, OIDC and Docker are, and reviews their own
doors; they are the design's proxy user. The stated trajectory is that
**other self-hosters adopt it too**, so design decisions must not lean on
knowledge unique to the author's box: a stranger's homelab is the assumed
deployment, and anything only its author would understand is a defect.
The console is designed for people who tolerate density, read audit logs,
and set their own zone rules.

Secondary: household or workshop members in the `viewer` role — they glance at
live view and review events after the fact, and the server role-gates them out
of Settings and Audit. The admin who wires cameras, storage and OIDC is the
same primary operator.

Frequency: live view is a glance, events are reviewed in bursts after an
alert, cameras/settings are touched at setup and rarely after.

## Product Purpose

Answer, without a camera vendor's cloud, **what happened at a door**:
continuously recorded, with events found by on-device AI over the operator's
own polygon zone rules, and footage that lives on storage they own. Success
is one browser tab on their own hardware answering "is anything happening
right now, and what did it look like" — with no cloud round-trip anywhere in
the answer.

## Positioning

Open source (MIT), self-hosted: no cloud dependency, no per-camera licensing,
no vendor app. The mechanism a neighbouring NVR cannot copy: the operator's
own OIDC identity, the operator's own storage tier (S3, WebDAV, SFTP, NFS),
open ONNX detection, and a documented acceptance gate (`scripts/verify.sh`)
that proves the AI-detection claim end to end rather than asserting it.

## Operating Context

The deployment facts that shape the design, each with its consequence:

- Single server instance, no Redis — one console tab per operator; state is
  local and the WebSocket hub is the only fan-out.
- 4–8 cameras at 5 FPS / 640×360 — the layout must read well at 1, 4 and 8
  feeds, not a wall of 32.
- CPU ONNX is the verified baseline; the GPU overlay is opt-in. Detection
  latency is a property of the box, not of a service tier.
- LAN-only live view by default; WebRTC reaches go2rtc directly on its own
  ports; TLS terminates at a reverse proxy — the console never assumes it
  owns the edge.
- Console routes: `/live`, `/events`, `/cameras`, `/settings`, `/audit`;
  role-gated nav (a viewer sees no Settings, no Audit).
- On the primary dev host the stack runs **natively** (`.dev/stack.sh`:
  go2rtc binary + supervisor, Go server on sqlite, uv-managed py3.12
  detector, vite dev on :3000 proxying to :8080) because that container has
  no Docker daemon; `scripts/dev.sh` remains the compose path elsewhere.

## Capabilities and Constraints

**v1 in:** RTSP ingest through go2rtc; per-camera `continuous` / `events` /
`off` recording; YOLOv8n ONNX with `intrusion` + `loiter` polygon zones
(normalized 0..1 coordinates); OIDC auth-code + PKCE + nonce with a
break-glass local admin; WebRTC live view; event timeline with snapshot and
clip; storage tiers (`s3`/`webdav`/`sftp`/`fs`) with async upload and
retention sweeps; HMAC-signed webhooks; WebSocket event fan-out.

**v1 out, stated as out:** face/LPR classes, seek on remote storage, ONVIF
events/presets, multi-instance media ownership, HLS as a primary playback
path, SMTP alerting, a bundled IdP, GPU as the verified baseline.

One locale (English) in v1; every string goes through `t(...)` so a later
locale is additive. No mobile target.

## Brand Commitments

MIT. No telemetry. Pinned assets with recorded hashes and attribution
(`testdata/fixtures.json`). Every feature ships with the test that proves it.
Docs state honestly what is unverified on which host. Naming is fixed: Go
module `nvr/server`, Python package `nvr_detector`, npm package `nvr-web`,
images `ghcr.io/welcometotheweb/nvr/{server,web,detector}`.

## Evidence on Hand

Verified on the homelab host (2026-11-08, native stack, sqlite):

- End-to-end pipeline observed: camera online with an `exec:` source, 137+
  segments recorded, `loiter` events opened/extended/closed on the server
  clock, a clip assembled, and the fs storage tier received snapshots and
  clips via the async uploader. Console, login, and role-gated API all live.
- **GPU overlay verified on this host**: `onnxruntime-gpu` 1.31.1 with
  `CUDAExecutionProvider` active on an RTX 4070 (8 GB); the worker logs the
  session's active providers, not the requested ones. This supersedes the
  plan's "unverified on this box" for this host — with the honest caveat
  that it was verified through the native path; the `docker-compose.gpu.yml`
  build itself has not run here (no Docker daemon in this container).

Server-side, evidenced earlier in CI/dev: config/models/store green on
`golang:1.26`; JWT + argon2 + break-glass seeding + the WS hub; the OIDC
discovery/JWKS/PKCE contract against the in-repo test IdP (`server/sso/testidp`).

**Absences future work must not fabricate:** no real RTSP camera has ever
been wired — every media/detection observation is the pinned `bus.jpg`
fixture loop. No customers, testimonials, benchmarks, or pricing exist.

## Product Principles

1. The server clock owns event time — a worker's timestamp never reaches the
   database, and the console shows only server-assigned times.
2. Local-first storage never destroys the only copy — the sweeper's window
   protects un-uploaded footage through any storage outage.
3. A camera write always re-renders go2rtc's config — the database is the
   only source of stream truth.
4. Secrets are sealed at rest (AES-256-GCM) and never echoed by any endpoint.
5. A claim ships with the command that proves it.

## Accessibility & Inclusion

The stated v1 bar: keyboard reach for every console action, contrast that
holds in a dim room (the live row's real usage scene), sensible focus order
in the zone editor, and a motion-reduction rule for the live row. **No
accessibility audit has been run on the console yet** — the bar is intent,
not evidence, and future work must verify rather than cite it.

# Product

<!-- impeccable:product-schema 1 -->

<!-- skeleton: S0-02 (2026-11-02). Headings with the claims each must make; the
     prose lands with S9-10. The plan's locked decisions
     (NVRWAY_V1_PLAN.md "Locked decisions") are the upstream of every slot. -->

## Platform

web — one self-hosted installation, one browser console. State the platform
and what is *not* a platform (no native app, no mobile target in v1).

## Users

Primary: **the person who owns the boxes** — a homeowner, a shop, a workshop,
a small site with 4–8 cameras, running one server in a closet or a homelab
rack. Secondary: the operator who reviews events after the fact and the
admin who sets up cameras, storage and OIDC. Say what each of them is doing
in the console and how often.

## Product Purpose

What nvrWay is for, and the success claim: an operator can answer, without a
camera vendor's cloud, what happened at a door — continuously recorded, with
events found by on-device AI over their own zone rules, and footage that
lives on storage they own.

## Positioning

Open source (MIT), self-hosted, **no cloud dependency, no per-camera
licensing, no vendor app**. The mechanism a neighbouring NVR cannot copy: the
operator's own OIDC identity, the operator's own storage tier (S3, WebDAV,
SFTP, NFS), open ONNX detection, and a documented acceptance gate
(`scripts/verify.sh`) that proves the AI-detection claim end to end rather
than asserting it.

## Operating Context

The deployment facts that shape the design: single server instance, no
Redis, CPU ONNX as the verified baseline, 4–8 cameras at 5 FPS / 640×360,
LAN-only live view by default, WebRTC reaching go2rtc directly, TLS
terminated at a reverse proxy. Every one of these is a product statement,
not an implementation detail — state each with the consequence it has for
the user.

## Capabilities and Constraints

v1 in, v2 out. **In:** RTSP ingest through go2rtc, `continuous` / `events` /
`off` recording, YOLOv8n ONNX with `intrusion` + `loiter` polygon zones,
OIDC + break-glass admin, live view, event timeline with snapshot + clip,
storage tiers with async upload and retention sweeps, signed webhooks.
**Out (stated as out, not left silent):** face/LPR classes, seek on remote
storage, ONVIF events/presets, multi-instance media ownership, HLS as a
primary playback path, SMTP alerting, a bundled IdP, GPU as the verified
baseline.

## Brand Commitments

What the project promises and how it can be checked: MIT, no telemetry,
pinned assets with recorded hashes and attribution
(`testdata/fixtures.json`), every feature shipped with the test that proves
it, honest docs about what is unverified on which host.

## Evidence on Hand

The claims the repo can already back with an observed command, and the date:
config/models/store green in `golang:1.26`; JWT + argon2 + break-glass
seeding + the WS hub; the OIDC discovery/JWKS/PKCE contract and the in-repo
test IdP. Say what is *not* yet evidenced — nothing media, detection,
console or storage has run here.

## Product Principles

The rules the product is judged by when a feature is cut: the server clock
owns event time; local-first storage never destroys the only copy; a camera
write always re-renders go2rtc's config; secrets are sealed at rest and
never echoed; CPU inference is the verified default; a claim ships with the
command that proves it.

## Accessibility & Inclusion

The accessibility bar for v1 (keyboard reach, contrast, focus order in the
zone editor, the motion-reduction rule for the live row) and the honest
statement of what has not been checked.

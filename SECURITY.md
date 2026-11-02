# Security Policy

<!-- skeleton: S0-02 (2026-11-02). Headings copied from OurWay's SECURITY.md,
     retuned for what nvrWay actually exposes; the prose lands with S9-10. -->

## Supported versions

State the policy in one line and one table (latest `v*` tag on `main` only,
older unsupported), as OurWay does. nvrWay v1 is pre-release: say so, and say
what "supported" means for a self-hosted install with no hosted service.

## Reporting a vulnerability

Private security advisory on the GitHub remote, not a public issue. State the
receipt / triage / fix windows we intend to keep (OurWay's: 5 business days /
14 days) and what to include: impact, reproduction, affected version.

## Scope

In scope: the Go control plane (`server/`), the Python detection worker
(`detector/`), the web console (`web/`), the Dockerfiles, compose files and
`scripts/` as shipped from this repository.

Out of scope (third-party, report to those projects): PostgreSQL, go2rtc,
FFmpeg, nginx, Node/Vite/React, ONNX Runtime, OpenCV, Ultralytics.

## Key areas to keep in mind

Each bullet is a promise the code must keep; each is checkable.

- **Secrets at rest.** Storage-backend credentials are sealed with
  AES-256-GCM keyed by `SHA-256(NVR_SECRET_KEY)` (`server/storage/seal.go`,
  card S7-02). No endpoint echoes a secret — the console sees
  `secret_set: true` and nothing else. The question this section answers:
  what does an operator do when `NVR_SECRET_KEY` is unset and derived from
  `JWT_SECRET`, and what does rotating each key cost?
- **`JWT_SECRET` has no default.** An unset value yields an ephemeral per-boot
  key: sessions die on restart. Production must set one
  (`openssl rand -hex 32`).
- **Break-glass admin.** `BOOTSTRAP_ADMIN_USER`/`BOOTSTRAP_ADMIN_PASSWORD`
  seeds one local admin (S2-03); an empty password creates no account. Say
  how to tell the account exists, why it exists (OIDC lockout), and how to
  disable it.
- **Worker token.** `WORKER_TOKEN` is the bearer for `/api/internal/*`; empty
  disables the internal API entirely. The internal API accepts signals that
  create events — treat it as a credential, not a config value.
- **WebSocket credentials.** nvrWay dials `/ws?token=…` (S8-10), so the
  access token appears in a URL, which access logs can record. State the
  mitigation and the accepted risk, and the origin guard
  (`WS_ORIGINS`/`WEB_URL`) that limits who may connect at all.
- **go2rtc's API is a second service with its own basic auth**
  (`GO2RTC_USERNAME`/`GO2RTC_PASSWORD`), and its RTSP/WebRTC ports
  (8554/8555) are published. The question: which ports may face a LAN, and
  which must never face a WAN.
- **Webhooks are signed, not encrypted.** `X-NVR-Signature` is
  HMAC-SHA256 over the delivery payload (S6-06). Say what a recipient must
  verify, and that the payload itself is plaintext.
- **Command execution is a designed surface.** The recorder and clip builder
  shell out to FFmpeg with a `source_uri` an operator can set
  (`exec:…` included). The section must state who may set a `source_uri`
  (the role floor: operator write, S3-06) and why that is a
  privilege-escalation surface, given the server runs as the non-root `nvr`
  user in a container.
- **TLS is terminated at the reverse proxy**; the server listens on plain
  HTTP. Point at `docs/installation.md` for the proxy and Tailscale answers.

## Contact

How to reach maintainers through the GitHub security-advisory system, and
what is out of band (a hosted service does not exist; there is nothing to
disable server-side for you).

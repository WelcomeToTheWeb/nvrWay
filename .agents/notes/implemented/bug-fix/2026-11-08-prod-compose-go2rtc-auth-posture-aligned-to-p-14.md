# Agent Note: prod-compose-go2rtc-auth-posture-aligned-to-p-14

Status: implemented

## Problem

The prod compose forced `GO2RTC_USERNAME`/`GO2RTC_PASSWORD` (`${VAR:?}` gates on server, detector, and inline `-config` pairs on the sidecar), but the project's own P-14 analysis (`docs/configuration.md`, `SECURITY.md`, the P-14 card, the dev stack, and installation.md § 4's "unauthenticated by design") says the sidecar is a **trusted-LAN service**: a browser cannot attach basic auth to a WebSocket, so with the pair set, live view fails as `unreachable` on every tile. The shipped prod default therefore broke the NVR's core feature out of the box, and installation.md never named the two gated vars at all — the documented first boot died at compose interpolation. Three documents stated three different postures.

## Decision

Align the outlier (the compose file) to the documented posture: **trusted-LAN is the prod default**. `GO2RTC_USERNAME`/`GO2RTC_PASSWORD` are now optional pass-throughs (`${VAR:-}`) on the detector and the sidecar's environment; the server receives them via `env_file` only. The go2rtc `command:` became an `sh -c` that applies the inline `-config api.username/password` pairs **only when both are non-empty** — so the first-boot auth window the earlier note closed stays closed for operators who opt in, and an empty pair boots the open API exactly as the renderer would write it. installation.md § 2/§ 3/§ 4 now state the default and the opt-in, and § 11 gained the proxy recipe (nginx WS-upgrade location + injected `Authorization` header + `GO2RTC_PUBLIC_URL` path prefix, which `web/src/live/go2rtc.ts` preserves).

## Alternatives considered

Keeping the gates and documenting the proxy as mandatory: rejected — it makes the documented default deployment broken (live view dead until the operator finds a recipe the guide did not have), and contradicts P-14's own conclusion that API credentials do not remove the LAN exposure WebRTC's 8554/8555 ports already create. Conditional compose interpolation of command arguments: compose cannot omit list items conditionally — the `sh -c` guard is the honest form, and the dev compose already establishes the pattern.

## Consequences

Bought: the documented happy path boots with working live view; the four real gates (`POSTGRES_PASSWORD`, `JWT_SECRET`, `WORKER_TOKEN`, `GO2RTC_PUBLIC_URL`) still refuse a half-configured stack by name; operators who want an authenticated sidecar keep the first-boot-window closure and now have a written recipe. Cost: the prod default exposes the sidecar API on the LAN — stated in § 4's table and `.env.example`, and it is the posture the whole design already assumed. Verified with docker-compose v5.7.0: `config -q` exits 0 with the pair unset and with the pair set; removing `JWT_SECRET` still fails naming it; the resolved sidecar command renders the guard with container-shell `$`.

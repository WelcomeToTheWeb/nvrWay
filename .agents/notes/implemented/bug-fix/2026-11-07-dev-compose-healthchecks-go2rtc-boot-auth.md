# Agent Note: dev-compose-healthchecks-go2rtc-boot-auth

Status: implemented

## Problem

The dev compose skeleton could not reach the card's "up -d --build reaches healthy" state: the detector healthcheck ran `pgrep -f nvr_detector` but python:3.12-slim ships no procps (always-unhealthy); the web healthcheck probed `http://localhost:8080/` but busybox wget prefers `::1` while nginx's `listen 8080` is IPv4-only (connection refused, always-unhealthy); the fixtures mount `./testdata:/fixtures:ro` resolved relative to docker/ and pointed at a nonexistent path; and go2rtc had no healthcheck at all, so the server gated on service_started. Separately, the go2rtc API-auth criterion exposed a dependency behavior worth pinning: go2rtc initializes its HTTP basic-auth middleware only at startup.

## Decision

All five services reach (healthy) from `docker compose up -d --build` (secrets in the gitignored docker/.env). The detector healthcheck greps `/proc/1/cmdline` for nvr_detector — PID 1 IS the worker (exec-form CMD), no new package. The web healthcheck (compose and Dockerfile.web HEALTHCHECK) names `127.0.0.1`, not localhost. go2rtc gained a healthcheck probing `http://localhost:1984/api/streams` — valid without credentials because go2rtc always passes localhost requests when `api.local_auth` is off — and the server now gates on `service_healthy`. The fixtures mount is `../testdata:/fixtures:ro`. The sidecar carries no GO2RTC_* env: its config is the server's push (renderListeners writes `api: { username, password }` from GO2RTC_USERNAME/GO2RTC_PASSWORD), which go2rtc persists to /config/go2rtc.yaml; auth therefore enforces from the sidecar's next start, and the first boot after `up` is open — accepted for the loopback-published dev stack, and the reason verify.sh (S10-03) must restart go2rtc before gating on auth. A host curl through the published port is not a valid auth probe (loopback source bypasses).
## Alternatives considered

apt-get install procps in the detector runtime image for pgrep: rejected — adds a package to a pinned image to re-check what /proc/1/cmdline says for free. Seeding /config/go2rtc.yaml (bind-mount or entrypoint -config chain) to close the first-boot auth window in dev: rejected for dev — the dev API is loopback-published and the push-then-restart flow is the honest dev state; the prod file (S9-05) is where a pre-seeded config belongs. Keeping GO2RTC_USERNAME/PASSWORD env on the go2rtc container: rejected — go2rtc has no such config keys; credentials the renderer did not write would be erased by the first config push, and env that only looks wired is the failure mode renderListeners' comment already names.
## Consequences

Bought: the dev stack is a truthful gate — every healthcheck can actually fail, and the go2rtc credential pair is proven load-bearing in both directions (drained pair: the server boot refuses to push, logging "answered 401 — go2rtc refused the credentials ... config not applied"). Cost: the go2rtc auth first-boot window is a documented dev property; S10-03's gate must `docker compose restart go2rtc` after up before asserting 401, and S9-05's prod file must decide the pre-seeded-config question explicitly. The web image's own HEALTHCHECK changed, so any consumer of nvr/web:latest without a compose override now gets the working probe.


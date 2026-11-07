# Agent Note: prod-compose-required-secrets-first-boot-auth

Status: implemented

## Problem

The deployment file must run on a host with no source checkout and must refuse to boot a half-configured installation: an empty JWT_SECRET is random per boot (every restart logs everyone out), an empty WORKER_TOKEN silently disables the internal API the detector lives on, and a guessed GO2RTC_PUBLIC_URL is a live view that works on the server and nowhere else. S9-04 also left a known gap: go2rtc initializes its API basic-auth middleware at startup, so credentials arriving only with the server's first push leave the sidecar open until its next restart — acceptable for the loopback dev stack, not for a deployment.

## Decision

docker-compose.prod.yml (repo root, per card path and docs/installation.md) runs the same five services as the dev file with `image: ghcr.io/welcometotheweb/nvr/{server,web,detector}:latest`, zero build sections, no MinIO, and OurWay tag semantics documented (`:latest` floats with CI; pin by `@sha256:` digest for reproducibility; the compose file, not a rebuild, is the unit of upgrade). Secrets are `${VAR:?message}` compile-time gates with no dev defaults: POSTGRES_PASSWORD, JWT_SECRET, WORKER_TOKEN, GO2RTC_USERNAME/PASSWORD, GO2RTC_PUBLIC_URL. A `:?` message containing ": " must be double-quoted or the compose YAML scanner dies with "mapping values are not allowed in this context". The go2rtc service overrides `command:` with `go2rtc` first (the image entrypoint is bare tini) plus inline `-config api.username=... -config api.password=...` pairs, so the sidecar starts authenticated before the server's first config push — closing the first-boot window the dev stack accepts. No nvr_models volume: the exported ONNX ships in the detector image and the image digest is its provenance. Same project name `nvr` as dev: dev and prod are two ways to run one installation and share its volumes (a dev-era postgres volume rejects the prod password with SASL failure — the honest consequence, not a bug).
## Alternatives considered

Bind-mounting a seeded /config/go2rtc.yaml to close the auth window: rejected — the server's POST /api/config push persists to exactly that path, so a :ro bind breaks the push's save and a writable bind races two writers; inline `-c` key=value pairs layer over the file cleanly and are re-applied on every start. A separate compose project name (nvr-prod) to isolate volumes: rejected — dev and prod are two ways to run one installation; sharing postgres_data/nvr_data is the point, and the SASL mismatch when switching passwords is the honest signal that the .env changed under a live volume. Keeping the nvr_models volume in prod: rejected — a named volume outlives the image it was seeded from, silently pinning an older model than the running image.
## Consequences

Bought: `config -q` is a real gate (missing variables fail naming themselves, verified for GO2RTC_PUBLIC_URL and WORKER_TOKEN); the prod stack is authenticated from the very first boot on an empty volume (mutation: inline pairs removed + fresh volume → first boot answers 200 credential-less; restored → 401). Cost: credentials interpolated into `command:` are visible in `docker inspect` exactly as env is — documented in the file; operators switching dev→prod .env against a live postgres volume get a SASL failure and must rotate the password inside the database or recreate the volume.


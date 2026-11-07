# Agent Note: server-image-cgo-off-postgres-only

Status: implemented

## Problem

The server image builds with CGO_ENABLED=0 (static binary, no libc surface), which compiles go-sqlite3 to its runtime stub — a sqlite DATABASE_URL inside the container fails at boot with "Binary was compiled with 'CGO_ENABLED=0'". The store supports sqlite (S1-08) and the Dockerfile sets DATA_DIR, so a reader can reasonably expect the container to run sqlite standalone. It cannot, and that boundary needs a recorded decision because the obvious "fix" (CGO/musl build) was considered and rejected.

## Decision

The nvrWay server image is Postgres-only by build, not by accident: CGO_ENABLED=0 stays, the compose stack (S9-04) always ships postgres:16-alpine, and the image's default DATABASE_URL is the documented postgres DSN. sqlite remains the host-dev and test path only. The runtime image is alpine:3.20 with ffmpeg (the recorder and clip builder shell out to it), runs as uid nvr (adduser -S), EXPOSE 8080, HEALTHCHECK wget --spider /health. Version stamping goes through -ldflags -X nvr/server/config.{Version,GitCommit} build args; a typo'd symbol is ignored silently by the linker, so the boot log's version line is the only guard — the card's verification asserts it. .dockerignore excludes node_modules, .git, runtime state, and model weights (the detector image exports ONNX at build time).
## Alternatives considered

CGO_ENABLED=1 with musl-dev/gcc so sqlite works in-container: rejected — it trades the static-binary property for a capability the deployment never needs (compose always provides postgres), and adds a libc build surface to every image rebuild. Baking DATABASE_URL=sqlite://… into the image ENV: rejected — it would silently mask a misconfigured postgres in production (typo'd compose env falling back to a throwaway in-container DB) and diverges from the docs/configuration.md env table.
## Consequences

Bought: a static, minimal, non-root image whose healthcheck proves the whole boot chain (migrations, roles seed, HTTP). Cost: anyone running the image bare must supply a reachable Postgres — the boot failure names it clearly; the sqlite-stub error only appears if someone points DATABASE_URL at sqlite inside the container, which the card and this note now document. The ldflags-silent-typo hazard means release tooling must pass VERSION/GIT_COMMIT and someone must read the boot log (or /health) once per release.


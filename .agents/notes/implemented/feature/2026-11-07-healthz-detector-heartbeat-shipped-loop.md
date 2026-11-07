# Agent Note: healthz-detector-heartbeat-shipped-loop

Status: implemented

## Problem

The detector has no listener, so "is detection alive?" cannot be a port probe — it must be data the server answers with. verify.sh step 1 gates on both endpoints, and the healthcheck must never be the thing that restarts a recording server: a /health that queries the database turns a slow database into a container restart for a process that is happily recording. And a worker that died an hour ago must read as "stale" (actionable), not as an error that would restart the healthy server.

## Decision

server/api/health.go owns the unauthenticated pair, mounted inside Handlers.Register so the fixture router and the production router mount the same routes (P-12's lesson; router_test.go pins /health and a fresh-install healthz on the real newRouter). Handlers.Health's signature takes no store — "never touches the DB" is structural, and the test closes the underlying *sql.DB before asking /health. DetectorHeartbeat (POST /api/internal/v1/heartbeat, behind the worker-token gate) validates uptime_seconds (non-negative number or 400) and stores only the receipt stamp: settings.detector_heartbeat_at = server clock RFC3339Nano; the uptime value is deliberately not stored. Healthz (GET /api/healthz) answers 200 always: detector fresh when age < DetectorFreshWindow (90 s = three 30 s beats; the boundary is `< 90`, asserted at 89.9 s fresh and 90.0 s stale via SetClock), stale — with overall status "degraded" — when never reported (last_seen null), when aged out, or when the row does not parse (plus a log line). The beat is scheduled in pipeline.Worker.run — the loop the shipped binary runs — first pass immediate, then every HEARTBEAT_SECONDS (30 s); ServerClient.run keeps no beat (its docstring now names the difference). A failed beat is a log line, never buffered or retried: a beat describes now.
## Alternatives considered

Buffering/retrying a failed heartbeat: rejected — the next beat is the retry; an old beat is worthless for a now-question. Storing uptime_seconds: rejected — validated for contract shape only; the stored fact is when the worker was last heard, stamped on the server's clock so a drifted worker cannot look fresh forever. healthz answering 5xx when the detector is stale: rejected — the server itself is fine; a Docker restart would replace one dead thing with two. Wiring the beat into ServerClient.run (the client's convenience loop): rejected by evidence — the first implementation did exactly that, and the live stack showed healthz stale with a healthy four-minute-old detector; only pipeline.Worker.run ships. A separate /health handler in router.go: superseded — moved into the api package so the suite and the binary share one mount.
## Consequences

Bought: the fresh/stale boundary is a tested constant (DetectorFreshWindow), the never-reported and damaged-row cases are defined answers, and the production mount is pinned. Live cycle proved the whole path: fresh at 14 s from real beats, stale at 116 s after docker stop (200/degraded), fresh 2.7 s after start. Cost: the 90 s window means a dead detector reads fresh for up to three beats — deliberate (one lost POST must not flap the wall); verify.sh (S10-03) must account for the window when gating on detector freshness. The pre-existing boot-test flake (kill arrives before the SIGTERM handler registers; assert -15 == 0) is recorded as P-13, not fixed here.


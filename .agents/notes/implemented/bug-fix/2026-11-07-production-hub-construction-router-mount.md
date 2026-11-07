# Agent Note: production-hub-construction-router-mount

Status: implemented

## Problem

The shipped server had no live feed: `newRouter` passed nil as the hub to `api.NewHandlers`, nothing ever set `handlers.Hub`, and every broadcast site guards with `if h.Hub == nil` — so the gap was silent. The recorder took the same nil (`media.NewRecorder(cfg, st, nil)`), killing `recorder.status` ticks too. S6-08's card wired the broadcast callbacks, not the hub construction; the router comment deferred the hub to "S6-04", which never owned it. No test caught it because api tests inject fixture hubs and console tests mock the socket — nothing booted the production binary and asked for `/ws`. S9-03's WS-upgrade probe through nginx (404) is what surfaced it.

## Decision

`newRouter` constructs the one hub per process before `handlers.Register(router)` — the mount point mounts `/ws` only when a hub exists at mount time, so construction cannot happen later in main.go: `handlers.Hub = ws.NewHub(handlers.JWT, []string{origins})` where origins is `cfg.WSOrigins` falling back to `cfg.WebURL` (the console's own origin is always dialable; a console hosted elsewhere must say so via WS_ORIGINS). `main.go` passes `handlers.Hub` to `media.NewRecorder`, so segment ticks and event rows travel the same socket. `server/router_test.go` — the first test in package main — pins both facts: the process builds one hub, and `GET /ws` without a token answers 401 from the hub's token gate (a 404 is the P-12 failure). Negative guarantee: the hub is still nil-safe in the handler set (fixture-driven api tests keep working); this adds construction, not a requirement.
## Alternatives considered

Construct the hub in main.go after newRouter returns: impossible — Register already ran, and gin cannot mount a route after the fact without restructuring the mount point. Pass the hub as a NewHandlers argument from main: rejected — newRouter owns Register, so the hub must exist inside it anyway; main would then need to duplicate the origins fallback. Make handlers panic when a broadcast hits a nil hub: rejected — the nil guards are load-bearing for fixture tests and the internal-only boot paths; the honest guard is a package-main test that boots the real router.
## Consequences

Bought: the live feed works in the shipped image (101 Switching Protocols through nginx, S9-03 evidence), and the class of "route exists only in fixtures" gaps now has a pinning pattern — package main tests boot the production router. Cost: package main now has a test file, so `go test ./...` compiles and runs it on every CI job; any future change to newRouter's boot order must keep `/ws` answering 401 (not 404) without a token. The S6-08 card's "wiring in main.go" claim was about callbacks — future cards that claim "wired" must name the file and the constructed object.


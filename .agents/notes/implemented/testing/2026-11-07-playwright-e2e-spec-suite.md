# Agent Note: playwright-e2e-spec-suite

Status: implemented

## Problem

Every suite up to S8-12 ran against fakes: mocked sockets, fixture hubs, vitest stubs. The P-12 failure class (components built, unit-tested, never wired into the shipped binary) had already bitten once, and the e2e stack (S9-08) had a runner but no specs. The card demanded browser-level acceptance for auth, live, cameras, events, storage against the throwaway stack — the first time any of these paths would run end to end in a real browser against the real server, sidecar, and recorder.

## Decision

web/e2e/ holds five specs (auth, live, cameras, events, storage), playwright.config.ts, global-setup.ts, helpers.ts. The config pins workers:1 + fullyParallel:false (events.spec mutates the seeded camera's record_mode/detection_enabled and live.spec reads it), reporter [['list'],['html']] so web/playwright-report (gitignored) carries failed runs, and globalSetup fetches ${API_URL}/health and refuses to start with one line — "e2e: the stack is not answering at … — run scripts/e2e-stack.sh restart first" — instead of letting five specs each burn their timeout. Fixtures are API-side on purpose: helpers.ts logs in via POST /api/auth/login, creates the viewer via POST /api/users (registration is 403 by design), flips camera fields via partial PUT (cameraInput is all-pointers), and posts worker signals (open→extend→close with the worker token) as the event seed — the explicit close builds the clip at close, so no post-roll wait. Only auth.spec drives the login form; the other specs inject the four localStorage session keys (nvr_access_token/nvr_refresh_token/nvr_user/nvr_roles — readRoles reads nvr_roles, not nvr_user). The e2e sidecar runs with GO2RTC_USERNAME/PASSWORD empty: the rendered config protects /api/ws with basic auth and a browser WebSocket cannot present one (P-14), so a credentialed sidecar makes live.spec ungreenable for a product reason, not a test reason; the reason is a comment in e2e-stack.sh. The recorder needs ffmpeg on the host (installed, 8.0.1) — the media unit tests always ran inside the golang container where ffmpeg exists.
## Alternatives considered

Seeding through the UI: rejected — a spec that seeds through the path it claims to verify proves nothing, and a viewer account cannot exist through the UI at all. Parallel workers: rejected — events.spec's camera mutation races live.spec's read. Letting GO2RTC_PUBLIC_URL carry userinfo credentials so the browser WS authenticates: rejected — settings/live hands that URL to every authenticated user, so a viewer would hold sidecar credentials that can rewrite the go2rtc config. Proxying the live WS through the server: the correct long-term fix (P-14 option 1) but feature-sized; a test card files the finding, it does not grow a feature.
## Consequences

The suite is the only place the shipped binary, sidecar, recorder, and browser meet; it caught P-14 (live view unreachable behind go2rtc API auth — dev stack included) on its first browser run, which no unit suite could have. Costs: the host now needs ffmpeg for e2e-stack.sh; specs are serial (~50s green, dominated by events.spec's segment wait); the no-auth sidecar means the suite never covers the credentialed sidecar path — P-14 must add that coverage when it decides the product answer. A cleanup loop that "passes" by finding nothing let a stale camera poison live.spec once; cleanups in this suite assert their target exists before deleting.


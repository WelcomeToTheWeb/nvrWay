# Agent Note: demo-seeder-detector-go2rtc-auth-gate

Status: implemented

## Problem

S10-02 must prove the whole pipeline with no physical camera: one command seeds a demo camera through the public API and the fixture produces a real event. Two live-stack truths broke the plan as written: go2rtc instantiates streams only at boot (the pushed config sat in /config/go2rtc.yaml while /api/streams was {}), and once the server pushes api.username the detector's mjpeg tap gets 401-as-text/plain forever; separately, the motion gate correctly starves a looped still image in continuous mode (422 frames, 1 inferred, 0 events).

## Decision

scripts/dev.sh --demo is the seeder and it is an API client only: login (BOOTSTRAP_ADMIN_USER/PASSWORD from docker/.env) → POST /api/v1/cameras (demo-doorbell, exec:ffmpeg fixture source, record_mode events) → PUT zones (porch loiter dwell 4 severity 3, bottom-strip polygon) → POST /api/storage/backends (fs local-tier, /var/lib/nvr/storage, is_default) → `compose restart -t 5 go2rtc` → probe retry loop (18×5 s; the exec source is lazy and the recorder's RTSP consumer, at up to 60 s backoff, is what starts it). Idempotency is by slug; an existing camera whose record_mode is not events is PUT-reconciled (the PUT merges, naming one field changes one field). The dev server has no docker socket so its Compose restarter is Noop — dev.sh performs the sidecar restart itself, as the dev-time operator. The detector authenticates to go2rtc: GO2RTC_USERNAME/GO2RTC_PASSWORD env → __main__ builds the tuple only when both are non-empty → Worker(go2rtc_auth=) → CameraPipeline field → MjpegStream(auth=) → requests.get(auth=); the opener seam keeps its (url, timeout) signature because the default opener is built as a closure around the credentials. The boot log carries auth=on/off so a 401-starved tap is diagnosable from one line. Both compose files pass the credentials to the detector; prod gates them with :?. The demo camera is record_mode events, not the plan's continuous: the gate is a continuous-mode CPU saver and an events camera's frames are the trigger.
## Alternatives considered

Seeder writing the database directly — rejected: it hides broken public routes, the exact P-12 failure class. Mounting docker.sock into the dev server container so the Compose restarter works — rejected: a socket in every dev server container for one restart; the script restarting the sidecar is what an operator with no restarter does anyway. Keeping record_mode continuous and disabling the gate by config — rejected: the gate is correct for continuous cameras; the fixture is not a continuous scene, and events mode is the honest demo (it also exercises the pre/post-roll recorder path). Asserting the literal "ffmpeg:" substring in probe producers — rejected: go2rtc reports one exec: producer whose binary is ffmpeg; no ffmpeg:-scheme producer exists for this source shape, and the assertion would fail forever on working code.
## Consequences

The demo path now covers recorder→go2rtc→detector→zone-rule→event end to end and is the acceptance surface for S10-03's gates. dev.sh must keep the go2rtc restart after any future seeding that adds streams, until the server restarter gains a socket. The auth tuple is both-or-neither by design: half a credential is a 401 with a friendlier header, so __main__ drops it and auth=off shows in the log. The runtime detector image still lacks onnx (5 suite failures + boot tests need it via /tmp/pylibs) — S11-02 territory, unchanged here.


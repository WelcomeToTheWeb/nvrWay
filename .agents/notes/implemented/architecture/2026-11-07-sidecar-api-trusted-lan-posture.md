# Agent Note: sidecar-api-trusted-lan-posture

Status: implemented

## Problem

S3-02 renders api.username/password into the sidecar config whenever GO2RTC_USERNAME/GO2RTC_PASSWORD are set, and go2rtc then answers 401 WWW-Authenticate: Basic on its whole API including /api/ws. S8-05's console dials /api/ws with a native WebSocket, which offers no way to attach an Authorization header and holds no credential to attach. So on any installation that sets the credentials — which docker/.env did — live view cannot work, and the tile's "unreachable" reads like a network fault. No test had ever joined the two decisions: the unit tests mock the socket, and the e2e stack runs the sidecar with no API auth.

## Decision

The shipped posture: the sidecar runs without API auth by default, and GO2RTC_USERNAME/GO2RTC_PASSWORD are documented as what an operator sets when they knowingly trade browser live view for config-write protection, with the reverse-proxy escape hatch named. server/main.go logs a WARNING at boot when either credential is set, naming the cause (a browser cannot present basic auth on a WebSocket) and pointing at docs/configuration.md, because the console's tile reads "unreachable", which looks like a network fault, not a policy one. docs/configuration.md gained a "The sidecar API is a trusted-LAN service (P-14)" section; SECURITY.md's go2rtc bullet answers its own open question the same way; docker/.env.example's credential lines are commented out with the trade stated inline; scripts/e2e-stack.sh carries a "WHY NO API AUTH HERE" comment above the sidecar run so the green suite is never mistaken for coverage of the credentialed path.

## Consequences

The default no-credential stack has a working live view (verified end to end in the browser after P-15's seed and P-16's supervisor landed — the three findings were one user-visible symptom). Setting the credentials still protects config writes and still boots, and now says out loud what it costs (boot warning observed firing 2026-11-07). The honest boundary: the sidecar's three ports (API 1984, RTSP 8554, WebRTC 8555) are all browser-facing by design, so the deployment must live on a trusted network and never face the WAN; the docs say so in the places an operator reads before setting the env.


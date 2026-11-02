#!/usr/bin/env bash
#
# nvrWay — acceptance gate (skeleton, card S0-04)
#
# Usage: scripts/verify.sh [SERVER_URL]      default: http://localhost:8080
#
# The acceptance gate for v1: nine observed passes, coloured pass/fail/step
# (OurWay's harness shape), non-zero exit on any failure. Card S10-03 is the
# card that makes it pass; card S9-08 writes it. The nine gates, in order:
#
#   1. GET /health 200 and /api/healthz reporting "detector":"fresh"
#   2. POST /api/auth/login for the break-glass admin -> access_token
#   3. seed the demo camera; POST /api/v1/cameras/:id/probe -> online
#   4. poll GET /api/v1/events -> a person/loiter event with >= 4 s dwell
#   5. event snapshot (image/jpeg > 8 KB) and clip (video/mp4, Range -> 206)
#   6. GET /api/v1/segments -> >= 3 rows, count strictly growing
#   7. storage: queue jobs reach uploaded; disabling a backend stops the queue
#   8. OIDC against the in-repo IdP: discovery ok, authorize -> callback ->
#      exchange, roles == ["viewer"]
#   9. webhook: a listener receives the close payload and the recomputed
#      HMAC-SHA256 matches X-NVR-Signature
set -euo pipefail

usage() {
    echo "Usage: scripts/verify.sh [SERVER_URL]   # default http://localhost:8080"
    echo "Runs the nine v1 acceptance gates and exits non-zero on any failure."
}

case "${1:-}" in
    -h | --help)
        usage
        exit 0
        ;;
esac

echo "verify.sh: skeleton (S0-04). The nine gates are card S10-03; the harness is S9-08."
echo "nothing is asserted here — a gate that prints pass without observing it is worse than no gate."
exit 2

#!/usr/bin/env bash
#
# nvrWay — the throwaway stack Playwright runs against (skeleton, card S0-04)
#
# Usage: scripts/e2e-stack.sh <restart|test|stop>
#
# What it will do (card S9-08 fills this in; the shape is OurWay's harness):
#   restart : throwaway sqlite DB + `go run` the server on :9090 with NVR_E2E=1
#             and RATE_LIMIT_PER_MINUTE=1000, vite preview of the built console,
#             run-scoped seeds under NVR_E2E_RUN_ID
#   test    : `npm run test:e2e:install && npm run test:e2e` (chromium)
#   stop    : tear the stack down
#
# It is a *separate* stack from docker/docker-compose.yml: one server process
# owns the media tree, so the e2e stack and the compose stack must not run the
# same media tree at the same time.
set -euo pipefail

usage() {
    cat <<'TXT'
Usage: scripts/e2e-stack.sh <restart|test|stop>

  restart  start the throwaway e2e stack (sqlite, server on :9090, vite preview)
  test     run the Playwright specs against it (web/e2e/)
  stop     tear it down
TXT
}

case "${1:-}" in
    -h | --help)
        usage
        exit 0
        ;;
esac

echo "e2e-stack.sh: skeleton (S0-04). The harness is card S9-08; the specs are S8-12."
exit 2

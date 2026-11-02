#!/usr/bin/env bash
#
# nvrWay — development stack (skeleton, card S0-04)
#
# Usage: scripts/dev.sh [--demo] [--down] [--logs [SERVICE]]
#
# What it will do (card S9-08 fills this in):
#   docker compose -f docker/docker-compose.yml up -d --build
#   --demo : seed the demo camera through the public API (S10-02) — the seeder
#            is an API client, never a DB writer — and print the console URL.
#
# Console :3000, API :8080, go2rtc :1984.
set -euo pipefail

usage() {
    cat <<'TXT'
Usage: scripts/dev.sh [--demo] [--down] [--logs [SERVICE]]

  --demo   start the stack and seed the demo camera through the public API
  --down   docker compose -f docker/docker-compose.yml down
  --logs   tail the stack's logs (optionally one service)

Starts docker/docker-compose.yml: postgres, go2rtc, server, detector, web.
TXT
}

case "${1:-}" in
    -h | --help)
        usage
        exit 0
        ;;
esac

echo "dev.sh: skeleton (S0-04). The dev-stack harness is card S9-08."
echo "the compose file it will drive exists: docker/docker-compose.yml"
usage
exit 2

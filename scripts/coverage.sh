#!/usr/bin/env bash
#
# nvrWay — coverage ratchet (skeleton, card S0-04)
#
# Usage: scripts/coverage.sh [--record]
#
# What it will do (card S9-08 fills this in; the ratchet is OurWay's, extended
# to the detector):
#   server   : go test ./... -cover, in the golang:1.26 image (no Go toolchain
#              on the host)
#   detector : pytest --cov (inside the detector image)
#   web      : vitest coverage
# Each measured number must be >= coverage.baseline.json, or this exits
# non-zero. --record writes the new baselines, and is run only at the end of
# Step 9 (S11-05 records the ratchet).
#
# Baseline file: coverage.baseline.json = {"server":0.0,"detector":0.0,"web":0.0}
set -euo pipefail

usage() {
    echo "Usage: scripts/coverage.sh [--record]"
    echo "Measures server (go), detector (pytest --cov) and web (vitest) coverage"
    echo "against coverage.baseline.json; --record writes the new floor."
}

case "${1:-}" in
    -h | --help)
        usage
        exit 0
        ;;
esac

echo "coverage.sh: skeleton (S0-04). The ratchet is card S9-08; the recording is S11-05."
exit 2

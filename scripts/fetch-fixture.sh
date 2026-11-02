#!/usr/bin/env bash
#
# nvrWay — pinned detection fixtures (skeleton, card S0-04)
#
# Usage: scripts/fetch-fixture.sh [bus.jpg|yolov8n.pt|all]
#
# What it will do (card S9-08 fills this in; the manifest is card S10-01):
#   read testdata/fixtures.json, download each pinned asset into testdata/,
#   check the byte count against the manifest, and record the sha256 into
#   testdata/fixtures.json on the first successful run.
#
# The assets are never committed — .gitignore ignores testdata/bus.jpg,
# testdata/yolov8n.pt and testdata/yolov8n.onnx; the manifest carries the
# attribution (Ultralytics assets: AGPL-3.0 / CC-BY-4.0, see README's License
# section).
set -euo pipefail

usage() {
    echo "Usage: scripts/fetch-fixture.sh [bus.jpg|yolov8n.pt|all]   # default all"
    echo "Downloads the pinned assets into testdata/, verifies byte counts,"
    echo "and records sha256 sums in testdata/fixtures.json on first run."
}

case "${1:-}" in
    -h | --help)
        usage
        exit 0
        ;;
esac

echo "fetch-fixture.sh: skeleton (S0-04). The harness is card S9-08; the manifest is S10-01."
exit 2

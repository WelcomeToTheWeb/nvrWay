#!/usr/bin/env bash
#
# nvrWay — model + decode smoke inside the detector image (skeleton, card S0-04)
#
# Usage: scripts/detect-smoke.sh
#
# What it will do (card S9-08 fills this in):
#   build the detector image (docker/Dockerfile.detector), then
#   docker compose run --rm detector python -m pytest tests/test_model_smoke.py -m slow -q
#
# Success line, verbatim from the plan:
#   smoke ok: person box conf>=0.5 on bus.jpg (yolov8n.onnx, CPUExecutionProvider)
#
# The host's Python cannot host the ML worker, so this always runs in the
# image — never on the host.
set -euo pipefail

usage() {
    echo "Usage: scripts/detect-smoke.sh"
    echo "Builds the detector image and runs the slow model smoke (yolov8n.onnx + bus.jpg, CPU)."
}

case "${1:-}" in
    -h | --help)
        usage
        exit 0
        ;;
esac

echo "detect-smoke.sh: skeleton (S0-04). The harness is card S9-08; the smoke test is S5-11."
exit 2

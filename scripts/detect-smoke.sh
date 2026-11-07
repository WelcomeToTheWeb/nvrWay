#!/usr/bin/env bash
#
# nvrWay — model + decode smoke inside the detector image (card S9-08)
#
# Usage: scripts/detect-smoke.sh
#
# Builds the detector image (docker/Dockerfile.detector) and runs the slow
# model smoke (tests/test_model_smoke.py, marked `slow`) inside it: the real
# exported yolov8n.onnx against the real bus.jpg, on CPUExecutionProvider.
# This is the one test that proves the decode path matches what YOLOv8
# actually emits — a planted tensor only proves self-consistency.
#
# The host's Python cannot host the ML worker, so this always runs in the
# image — never on the host. The fixture photo is gitignored; the script
# fetches it through scripts/fetch-fixture.sh (pinned bytes + sha256) and
# mounts testdata/ read-only at /testdata, which is where the test looks.
#
# A SKIP IS A FAILURE here. The test skips when the model or the photo is
# missing, and pytest exits 0 on a skip — so the script greps the output for
# the test's own "smoke ok" line and fails without it.
set -uo pipefail

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

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

# compose needs an env file to interpolate even for `run`; a fresh clone has
# only the example, so fall back to a throwaway one with the keys the file
# references. The smoke asserts nothing about these values.
env_file="$repo_root/docker/.env"
if [ ! -f "$env_file" ]; then
    env_file=/tmp/nvr-detect-smoke.env
    {
        echo "BOOTSTRAP_ADMIN_USER=admin"
        echo "BOOTSTRAP_ADMIN_PASSWORD=detect-smoke"
        echo "JWT_SECRET=detect-smoke"
        echo "NVR_SECRET_KEY=$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
        echo "WORKER_TOKEN=detect-smoke"
        echo "DET_PROVIDERS=CPUExecutionProvider"
        echo "WEB_URL=http://localhost:3000"
        echo "VERSION=detect-smoke"
        echo "GIT_COMMIT=detect-smoke"
    } > "$env_file"
    echo "detect-smoke: docker/.env is missing — using a throwaway $env_file"
fi

compose() { docker compose -f "$repo_root/docker/docker-compose.yml" --env-file "$env_file" "$@"; }

if [ ! -f "$repo_root/testdata/bus.jpg" ]; then
    echo "detect-smoke: testdata/bus.jpg missing — fetching the pinned fixture"
    "$repo_root/scripts/fetch-fixture.sh" bus.jpg || {
        echo "detect-smoke: FAILED — fetch-fixture.sh could not produce bus.jpg" >&2
        exit 1
    }
fi

echo "detect-smoke: building the detector image (first build exports the ONNX model and is slow)"
compose build detector || {
    echo "detect-smoke: FAILED — the detector image did not build" >&2
    exit 1
}

echo "detect-smoke: running the slow model smoke in the image (yolov8n.onnx, CPUExecutionProvider)"
out=$(compose run --rm -v "$repo_root/testdata:/testdata:ro" detector \
    python -m pytest tests/test_model_smoke.py -m slow -q -s 2>&1)
rc=$?
printf '%s\n' "$out"

if [ "$rc" -ne 0 ]; then
    echo "detect-smoke: FAILED — pytest exited $rc" >&2
    exit 1
fi
if ! printf '%s\n' "$out" | grep -q "smoke ok"; then
    # pytest exits 0 on a skip; the smoke line is the proof it actually ran.
    echo "detect-smoke: FAILED — pytest exited 0 without a 'smoke ok' line (the test skipped?)" >&2
    exit 1
fi

printf '%s\n' "$out" | grep "smoke ok"
echo "detect-smoke: smoke ok (yolov8n.onnx, CPUExecutionProvider)"

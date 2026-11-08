#!/usr/bin/env bash
#
# nvrWay — coverage ratchet (card S9-08)
#
# Usage: scripts/coverage.sh [--record]
#
# Measures all three suites and refuses any number that fell below
# coverage.baseline.json — the ratchet only ever turns one way:
#
#   server   : go test ./... -coverprofile, in golang:1.26 (the host has no Go
#              toolchain on PATH by default, and the Debian image carries the
#              gcc the CGO sqlite driver needs — the production build is
#              CGO_ENABLED=0, the test suite is not)
#   detector : pytest --cov inside the detector image (the host's Python
#              cannot host the ML worker). pytest-cov is not baked into the
#              runtime image — production does not measure itself — so the
#              run installs it into the throwaway container. `onnx` is baked
#              in since S11-02: the suite cannot run in the image without it.
#   web      : vitest run --coverage (json-summary; the text table is for
#              humans, the JSON is for the ratchet)
#
# --record writes the measured numbers as the new floor. It is run at the end
# of a step (S11-05 records the Step 9/10/11 ratchet), never casually: the
# floor is the promise that a later refactor cannot quietly delete coverage.
set -uo pipefail

usage() {
    echo "Usage: scripts/coverage.sh [--record]"
    echo "Measures server (go), detector (pytest --cov) and web (vitest) coverage"
    echo "against coverage.baseline.json; --record writes the new floor."
}

record=0
case "${1:-}" in
    -h | --help)
        usage
        exit 0
        ;;
    "" | --record)
        [ "${1:-}" = "--record" ] && record=1
        ;;
    *)
        usage >&2
        exit 2
        ;;
esac

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
baseline="$repo_root/coverage.baseline.json"
out_dir=$(mktemp -d /tmp/nvr-coverage.XXXXXX)
trap 'rm -rf "$out_dir"' EXIT

[ -f "$baseline" ] || {
    echo '{"server": 0.0, "detector": 0.0, "web": 0.0}' > "$baseline"
    echo "coverage: created $baseline at zero"
}

fail=0

# ---- server -----------------------------------------------------------------
echo "coverage: server — go test ./... -coverprofile (golang:1.26)"
export PATH="$PATH:/usr/local/go/bin"
if command -v go >/dev/null 2>&1; then
    # Host toolchain present (this box): run here, same numbers, no image pull.
    (cd "$repo_root/server" && go test ./... -coverprofile="$out_dir/cover.out" > "$out_dir/server.log" 2>&1)
    server_rc=$?
    server_total=$(cd "$repo_root/server" && go tool cover -func="$out_dir/cover.out" 2>/dev/null | awk '/^total:/ {gsub("%", "", $3); print $3}')
else
    docker run --rm \
        -v "$repo_root/server":/src \
        -v nvr-gomod:/go/pkg/mod \
        -v "$out_dir":/out \
        -w /src golang:1.26 \
        sh -c 'go test ./... -coverprofile=/out/cover.out > /out/server.log 2>&1;
               rc=$?; go tool cover -func=/out/cover.out | awk "/^total:/ {gsub(\"%\", \"\", \$3); print \$3}" > /out/server.total; exit $rc' \
        > "$out_dir/server.total" 2>&1
    server_rc=$?
    server_total=$(cut -d= -f2 "$out_dir/server.total" 2>/dev/null | tail -1)
fi
if [ "$server_rc" -ne 0 ]; then
    echo "coverage: server — FAILED (go test exited $server_rc; tail of the log:)" >&2
    tail -20 "$out_dir/server.log" >&2
    fail=1
    server_total=""
fi
echo "coverage: server total = ${server_total:-n/a}%"

# ---- detector ----------------------------------------------------------------
env_file="$repo_root/.env"
if [ ! -f "$env_file" ]; then
    env_file=/tmp/nvr-coverage.env
    {
        echo "BOOTSTRAP_ADMIN_USER=admin"
        echo "BOOTSTRAP_ADMIN_PASSWORD=coverage"
        echo "JWT_SECRET=coverage"
        echo "NVR_SECRET_KEY=$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
        echo "WORKER_TOKEN=coverage"
        echo "DET_PROVIDERS=CPUExecutionProvider"
        echo "WEB_URL=http://localhost:3000"
        echo "VERSION=coverage"
        echo "GIT_COMMIT=coverage"
    } > "$env_file"
    echo "coverage: .env is missing — using a throwaway $env_file"
fi

echo "coverage: detector — pytest --cov in the detector image (not slow)"
docker compose -f "$repo_root/docker-compose.yml" --env-file "$env_file" \
    run --rm detector sh -c \
    'pip install --quiet --disable-pip-version-check --target=/tmp/pcov pytest-cov >/dev/null &&
     PYTHONPATH=/tmp/pcov python -m pytest -m "not slow" -q --cov=nvr_detector --cov-report=term 2>&1 |
     tee /dev/stderr | grep -E "^TOTAL" | awk "{gsub(\"%\", \"\", \$4); print \$4}"' \
    > "$out_dir/detector.total" 2> "$out_dir/detector.log"
detector_rc=$?
detector_total=$(tail -1 "$out_dir/detector.total" 2>/dev/null)
if [ "$detector_rc" -ne 0 ] || [ -z "$detector_total" ]; then
    echo "coverage: detector — FAILED (exit $detector_rc; tail of the log:)" >&2
    tail -20 "$out_dir/detector.log" >&2
    fail=1
    detector_total=""
fi
echo "coverage: detector total = ${detector_total:-n/a}%"

# ---- web ----------------------------------------------------------------------
echo "coverage: web — vitest run --coverage"
if [ ! -d "$repo_root/web/node_modules" ]; then
    (cd "$repo_root/web" && npm ci --no-audit --no-fund > "$out_dir/npm.log" 2>&1) || {
        echo "coverage: web — npm ci failed:" >&2
        tail -10 "$out_dir/npm.log" >&2
        fail=1
    }
fi
(cd "$repo_root/web" && npm run --silent test:run -- --coverage --coverage.reporter=json-summary \
    > "$out_dir/web.log" 2>&1)
web_rc=$?
web_total=$(python3 -c "
import json
try:
    d = json.load(open('$repo_root/web/coverage/coverage-summary.json'))
    print(d['total']['lines']['pct'])
except Exception:
    pass
" 2>/dev/null)
if [ "$web_rc" -ne 0 ] || [ -z "$web_total" ]; then
    echo "coverage: web — FAILED (vitest exited $web_rc; tail of the log:)" >&2
    tail -20 "$out_dir/web.log" >&2
    fail=1
    web_total=""
fi
echo "coverage: web total = ${web_total:-n/a}%"

# ---- the ratchet ---------------------------------------------------------------
if [ "$fail" -ne 0 ]; then
    echo "coverage: FAILED — a suite did not run; the ratchet does not record or compare a partial picture" >&2
    exit 1
fi

if [ "$record" -eq 1 ]; then
    python3 - "$baseline" "$server_total" "$detector_total" "$web_total" <<'PY'
import json, sys
path, server, detector, web = sys.argv[1:5]
json.dump({"server": float(server), "detector": float(detector), "web": float(web)},
          open(path, "w"), indent=2)
print(f"coverage: recorded baseline server={server} detector={detector} web={web}")
PY
    exit 0
fi

python3 - "$baseline" "$server_total" "$detector_total" "$web_total" <<'PY'
import json, sys
path, server, detector, web = sys.argv[1:5]
base = json.load(open(path))
measured = {"server": float(server), "detector": float(detector), "web": float(web)}
bad = {k: (measured[k], base.get(k, 0.0)) for k in measured if measured[k] < base.get(k, 0.0)}
for k, (got, floor) in sorted(bad.items()):
    print(f"coverage: {k} fell below the baseline: {got}% < {floor}%", file=sys.stderr)
if bad:
    print("coverage: FAILED — the ratchet only turns one way; raise the floor with --record "
          "only when the drop is deliberate and explained", file=sys.stderr)
    sys.exit(1)
print("coverage: ratchet ok — every suite is at or above its baseline")
PY
exit $?

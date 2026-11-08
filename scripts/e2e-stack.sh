#!/usr/bin/env bash
#
# nvrWay — the throwaway stack Playwright runs against (card S9-08)
#
# Usage: scripts/e2e-stack.sh <restart|test|stop>
#
#   restart  start the throwaway e2e stack: a sqlite database under /tmp, the
#            Go server built and run on :9090, a private go2rtc container on
#            :1985 (API) / :18555 (WebRTC), one seeded camera, and `vite
#            preview` of the built console on 127.0.0.1:3113 proxying /api and
#            /ws to :9090 (NVR_PROXY_TARGET in web/vite.config.ts).
#   test     run the Playwright specs against it (web/e2e/ — the specs are
#            card S8-12; without them this exits non-zero, honestly)
#   stop     tear it down: kill both processes, remove the container and its
#            config volume, delete the run directory
#
# It is a *separate* stack from docker-compose.yml and never touches a
# dev database: DATABASE_URL is `sqlite://` under /tmp, the media directories
# are under /tmp, the go2rtc is its own container on its own ports, and the
# secrets are generated per run and written to the state file. One e2e stack
# at a time (fixed container name): the single-writer rule that the compose
# Notes state for the dev stack applies here too.
#
# NVR_E2E=1 is exported to the server as the plan names it; no server code
# reads it yet — the throwaway-ness is structural (paths and ports), not a
# mode switch. NVR_E2E_RUN_ID scopes the seeded camera's slug so two runs
# cannot collide, and the specs read it from the environment.
#
# Host facts this script respects (S8-01/S8-02 evidence): `vite preview` binds
# [::1] only by default, so it starts with --host 127.0.0.1 and everything
# reaches it over 127.0.0.1, never "localhost".
set -uo pipefail

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

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
state_dir=/tmp/nvr-e2e
state_file="$state_dir/state.env"
container=nvr-e2e-go2rtc
volume=nvr-e2e-go2rtc-config
api=http://127.0.0.1:9090
console=http://127.0.0.1:3113

log() { echo "e2e-stack: $*"; }
die() { echo "e2e-stack: $*" >&2; exit 1; }

stop_quietly() {
    if [ -f "$state_file" ]; then
        # shellcheck disable=SC1090
        . "$state_file"
        for pid in "${server_pid:-}" "${preview_pid:-}"; do
            [ -n "$pid" ] && kill "$pid" 2>/dev/null
        done
    fi
    docker rm -f "$container" >/dev/null 2>&1
    # The config volume carries the previous run's API credentials — go2rtc
    # persists the config the server pushed, and a fresh server would be
    # locked out of its own sidecar (401 from the host, `{}` from localhost).
    # Wiping it is right for a throwaway stack: the server re-renders the
    # whole config from its camera rows at boot.
    docker volume rm "$volume" >/dev/null 2>&1
    pkill -f "nvr-e2e-server" 2>/dev/null
    # The preview pid is the subshell; npm and the vite node process are its
    # children and survive its death, so the pattern teardown catches them.
    # It cannot hit the dev console: that runs `vite`, never `vite preview`.
    pkill -f "vite preview --host 127.0.0.1 --port 3113" 2>/dev/null
}

case "$1" in
# ---------------------------------------------------------------------------
stop)
    stop_quietly
    docker volume rm "$volume" >/dev/null 2>&1
    # The run directory is throwaway by name; deleting it is the promise.
    [ -n "${run_dir:-}" ] && rm -rf "$run_dir"
    rm -f "$state_file"
    log "stopped; the run directory and the go2rtc volume are gone"
    ;;
# ---------------------------------------------------------------------------
restart)
    stop_quietly
    rm -rf "$state_dir"
    mkdir -p "$state_dir"

    # The run id survives restart→test→stop so the seeded slug and the specs
    # agree on which run they belong to.
    runid=$(head -c 4 /dev/urandom | od -An -tx1 | tr -d ' \n')
    run_dir="$state_dir/$runid"
    mkdir -p "$run_dir"/{data,segments,snapshots,clips}

    admin_user=admin
    admin_pass=$(head -c 12 /dev/urandom | od -An -tx1 | tr -d ' \n')
    worker_token=$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')

    # --- go2rtc: its own container, its own config volume -------------------
    # The server pushes the rendered config over the API and go2rtc persists
    # it into /config; the restart below is what instantiates the streams
    # (streams are created at boot, S3-07 — the e2e stack has no compose
    # project for the server to restart, so the script plays that part).
    #
    # The sidecar runs with NO api auth here, deliberately: the rendered
    # config protects /api/ws with basic auth, and a browser WebSocket cannot
    # present one — live view is unreachable behind those credentials (P-14).
    # The sidecar binds 127.0.0.1 only and the run is throwaway, so the
    # trusted-LAN assumption the template defaults make holds outright.
    # WHY NO API AUTH HERE (P-14): GO2RTC_USERNAME/PASSWORD are deliberately
    # empty below, and that means this suite never exercises the credentialed
    # sidecar — with them set, go2rtc answers 401 to the browser's live-view
    # WebSocket (a browser cannot present basic auth on a WS) and live view
    # cannot work at all; the decision and the trade are documented in
    # docs/configuration.md. A green live spec here is coverage of the
    # no-auth path only, and must not be read as coverage of the other.
    log "starting go2rtc ($container) on 127.0.0.1:1985/18555"
    # Same seed + supervisor as docker-compose.yml (P-15, P-16): the
    # template puts api.origin:"*" on disk before go2rtc starts, and the
    # supervisor exits when the server re-renders the file so the container
    # boots the new config (go2rtc never reloads one in place). The explicit
    # `docker restart` below stays: it is the deterministic instant of the
    # streams' first boot, not a wait-on-poll.
    sidecar_cmd='[ -s /config/go2rtc.yaml ] || cp /go2rtc.seed.yaml /config/go2rtc.yaml
go2rtc -config /config/go2rtc.yaml &
pid=$!
trap '"'"'kill $pid 2>/dev/null; exit 0'"'"' INT TERM
last=$(md5sum /config/go2rtc.yaml 2>/dev/null)
while kill -0 $pid 2>/dev/null; do
  sleep 2
  now=$(md5sum /config/go2rtc.yaml 2>/dev/null)
  if [ "$now" != "$last" ]; then
    echo "go2rtc.yaml changed on disk; exiting so the fresh config is booted"
    kill $pid 2>/dev/null
    exit 0
  fi
done
wait $pid'
    docker run -d --restart unless-stopped --name "$container" \
        -v "$volume:/config" \
        -v "$repo_root/docker/go2rtc.yaml.template:/go2rtc.seed.yaml:ro" \
        -p 127.0.0.1:1985:1984 \
        -p 127.0.0.1:18554:8554 \
        -p 127.0.0.1:18555:8555 \
        -p 127.0.0.1:18555:8555/udp \
        ghcr.io/alexxit/go2rtc:master \
        sh -c "$sidecar_cmd" \
        >/dev/null || die "the go2rtc container did not start"
    for _ in $(seq 1 20); do
        curl -fsS "http://127.0.0.1:1985/api/streams" >/dev/null 2>&1 && break
        sleep 1
    done
    curl -fsS "http://127.0.0.1:1985/api/streams" >/dev/null 2>&1 || die "go2rtc never answered on :1985"

    # --- the server: built, not `go run`, so the pid is the server ----------
    export PATH="$PATH:/usr/local/go/bin"
    command -v go >/dev/null 2>&1 || die "no Go toolchain (looked on PATH and /usr/local/go/bin)"
    log "building the server"
    (cd "$repo_root/server" && go build -o "$run_dir/nvr-e2e-server" .) || die "the server did not build"

    log "starting the server on :9090 (sqlite under $run_dir)"
    env \
        DATABASE_URL="sqlite://$run_dir/nvr.db" \
        SERVER_PORT=":9090" \
        DATA_DIR="$run_dir/data" \
        SEGMENTS_DIR="$run_dir/segments" \
        SNAPSHOTS_DIR="$run_dir/snapshots" \
        CLIPS_DIR="$run_dir/clips" \
        WEB_URL="$console" \
        WS_ORIGINS="$console" \
        GO2RTC_URL="http://127.0.0.1:1985" \
        GO2RTC_USERNAME="" \
        GO2RTC_PASSWORD="" \
        GO2RTC_RTSP_URL="rtsp://127.0.0.1:18554" \
        GO2RTC_PUBLIC_URL="http://127.0.0.1:1985" \
        BOOTSTRAP_ADMIN_USER="$admin_user" \
        BOOTSTRAP_ADMIN_PASSWORD="$admin_pass" \
        WORKER_TOKEN="$worker_token" \
        RATE_LIMIT_PER_MINUTE=1000 \
        NVR_E2E=1 \
        NVR_E2E_RUN_ID="$runid" \
        TZ=UTC \
        "$run_dir/nvr-e2e-server" > "$run_dir/server.log" 2>&1 &
    server_pid=$!
    for _ in $(seq 1 30); do
        curl -fsS "$api/health" >/dev/null 2>&1 && break
        sleep 1
    done
    curl -fsS "$api/health" >/dev/null 2>&1 || {
        tail -20 "$run_dir/server.log" >&2
        die "the server did not come up on :9090 (log above)"
    }

    # --- the run-scoped seed -------------------------------------------------
    token=$(curl -fsS -X POST "$api/api/auth/login" -H 'Content-Type: application/json' \
        -d "{\"username\":\"$admin_user\",\"password\":\"$admin_pass\"}" \
        | python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])') || die "seed login failed"
    slug="e2e-$runid"
    # testsrc, not the demo fixture: go2rtc's own ffmpeg renders it, so the
    # e2e stack needs no mounted files, and a moving picture keeps every
    # liveness path honest. record_mode off: the host has no ffmpeg, and the
    # specs are about the console, not the recorder.
    camera_json=$(curl -fsS -X POST "$api/api/v1/cameras" -H "Authorization: Bearer $token" -d "{
        \"slug\": \"$slug\",
        \"name\": \"E2E $runid\",
        \"source_uri\": \"exec:ffmpeg -re -f lavfi -i testsrc=size=640x360:rate=25 -pix_fmt yuv420p -c:v libx264 -preset veryfast -g 50 -f h264 -\",
        \"record_mode\": \"off\",
        \"detection_enabled\": false
    }") || die "the seed camera was not created"
    camera_id=$(printf '%s' "$camera_json" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')

    docker restart "$container" >/dev/null || die "could not restart $container"
    online=""
    for _ in $(seq 1 30); do
        state=$(curl -fsS -X POST "$api/api/v1/cameras/$camera_id/probe" -H "Authorization: Bearer $token" -d '{}' 2>/dev/null \
            | python3 -c 'import json,sys; print(json.load(sys.stdin).get("online", False))' 2>/dev/null || true)
        [ "$state" = "True" ] && { online=1; break; }
        sleep 2
    done
    [ -n "$online" ] || die "the seed camera never came online (check $run_dir/server.log)"

    # --- the console: built bundle behind vite preview ------------------------
    if [ ! -d "$repo_root/web/node_modules" ]; then
        log "npm ci (first run)"
        (cd "$repo_root/web" && npm ci --no-audit --no-fund > "$run_dir/npm.log" 2>&1) || die "npm ci failed (see $run_dir/npm.log)"
    fi
    log "building the console"
    (cd "$repo_root/web" && npm run build > "$run_dir/build.log" 2>&1) || die "the console build failed (see $run_dir/build.log)"
    log "vite preview on $console (proxying /api and /ws to :9090)"
    # The redirect belongs on the subshell, not the inner command: the
    # subshell itself would otherwise keep the script's stdout open, and a
    # caller piping this script's output would wait forever for EOF.
    (cd "$repo_root/web" && NVR_PROXY_TARGET="$api" npx vite preview --host 127.0.0.1 --port 3113) \
        > "$run_dir/preview.log" 2>&1 &
    preview_pid=$!
    for _ in $(seq 1 20); do
        curl -fsS "$console/" >/dev/null 2>&1 && break
        sleep 1
    done
    curl -fsS "$console/" >/dev/null 2>&1 || die "vite preview did not answer on $console (see $run_dir/preview.log)"

    cat > "$state_file" <<EOF
runid=$runid
run_dir=$run_dir
server_pid=$server_pid
preview_pid=$preview_pid
camera_id=$camera_id
camera_slug=$slug
admin_user=$admin_user
admin_pass=$admin_pass
worker_token=$worker_token
EOF
    log "stack up: console $console · api $api · camera $slug ($camera_id)"
    log "credentials and ids are in $state_file; teardown: scripts/e2e-stack.sh stop"
    ;;
# ---------------------------------------------------------------------------
test)
    [ -f "$state_file" ] || die "no e2e stack — run scripts/e2e-stack.sh restart first"
    # shellcheck disable=SC1090
    . "$state_file"
    [ -d "$repo_root/web/e2e" ] || die "web/e2e/ does not exist yet — the Playwright specs are card S8-12"
    # events.spec.ts embeds the pinned bus.jpg at import time (the snapshot
    # fixture), and testdata assets are never committed (S10-01) — a fresh
    # runner has none until the fetch script puts it there.
    if [ ! -f "$repo_root/testdata/bus.jpg" ]; then
        log "testdata/bus.jpg missing — fetching the pinned fixture"
        "$repo_root/scripts/fetch-fixture.sh" bus.jpg || die "fetch-fixture.sh could not produce bus.jpg"
    fi
    cd "$repo_root/web"
    export NVR_E2E_RUN_ID="$runid"
    export E2E_CONSOLE_URL="$console"
    export E2E_API_URL="$api"
    export BOOTSTRAP_ADMIN_USER="$admin_user"
    export BOOTSTRAP_ADMIN_PASSWORD="$admin_pass"
    export E2E_WORKER_TOKEN="$worker_token"
    export E2E_CAMERA_ID="$camera_id"
    export E2E_CAMERA_SLUG="$camera_slug"
    log "installing the chromium browser (cached after the first run)"
    npm run --silent test:e2e:install || die "playwright install failed"
    log "running the Playwright suite"
    npm run test:e2e
    rc=$?
    [ "$rc" -eq 0 ] && log "e2e ok" || echo "e2e-stack: the suite failed (exit $rc)" >&2
    exit "$rc"
    ;;
# ---------------------------------------------------------------------------
*)
    usage >&2
    exit 2
    ;;
esac

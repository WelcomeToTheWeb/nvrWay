#!/usr/bin/env bash
#
# nvrWay — development stack (cards S9-08 + S10-02)
#
# Usage: scripts/dev.sh [--demo] [--down] [--logs [SERVICE]]
#
#   docker compose -f docker/docker-compose.yml up -d --build
#   --demo : seed the demo camera through the public API and print the console
#            URL. The seeder is an API client, never a DB writer — seeding
#            exercises the same path an operator uses (login, POST camera,
#            PUT zones, POST backend, probe), so a broken public path cannot
#            hide behind a working seeder.
#   --down : docker compose down (volumes stay; `docker volume rm` to wipe)
#   --logs : tail the stack's logs (optionally one service)
#
# Console :3000, API :8080, go2rtc :1984.
#
# The demo camera is the no-physical-camera proof: `exec:ffmpeg -re -loop 1 -i
# /fixtures/bus.jpg ...` records a looped fixture, and go2rtc/config.go's one
# demo-only rule gives detection a second, mjpeg-from-the-fixture source so
# the pipeline never transcodes H.264 twice. /fixtures is the dev-only mount
# ./testdata:/fixtures:ro — a production install does not have it, and neither
# does this script's business outside --demo.
set -euo pipefail

usage() {
    cat <<'TXT'
Usage: scripts/dev.sh [--demo] [--down] [--logs [SERVICE]]

  --demo   start the stack and seed the demo camera through the public API
  --down   docker compose -f docker/docker-compose.yml down
  --logs   follow the stack's logs (optionally: --logs server)
TXT
}

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
compose_file="$repo_root/docker/docker-compose.yml"
env_file="$repo_root/docker/.env"
api="http://127.0.0.1:8080"
console="http://localhost:3000"

compose() { docker compose -f "$compose_file" --env-file "$env_file" "$@"; }

if [ ! -f "$env_file" ]; then
    # A fresh clone has no docker/.env (it is gitignored). The example carries
    # dev credentials on purpose, so copying it is the no-manual-setup path;
    # production never runs this script (docs/installation.md).
    if [ -f "$repo_root/docker/.env.example" ]; then
        cp "$repo_root/docker/.env.example" "$env_file"
        echo "dev.sh: created $env_file from docker/.env.example — dev credentials, edit it for anything real"
    else
        echo "dev.sh: $env_file is missing and docker/.env.example is not there to seed it" >&2
        exit 1
    fi
fi

env_get() { grep -E "^$1=" "$env_file" | tail -1 | cut -d= -f2- || true; }

wait_health() {
    local i
    for i in $(seq 1 60); do
        if curl -fsS -o /dev/null "$api/health"; then return 0; fi
        sleep 2
    done
    echo "dev.sh: the server never answered $api/health within 120s — see: scripts/dev.sh --logs server" >&2
    return 1
}

# --- the demo seeder (S10-02) ------------------------------------------------
# Every call below is a public-API call with a real session token. Nothing
# here opens a database.

seed_demo() {
    local token camera_id backend_json probe_json attempt
    local admin_user admin_pass
    admin_user="$(env_get BOOTSTRAP_ADMIN_USER)"; admin_user="${admin_user:-admin}"
    admin_pass="$(env_get BOOTSTRAP_ADMIN_PASSWORD)"
    [ -n "$admin_pass" ] || { echo "dev.sh: BOOTSTRAP_ADMIN_PASSWORD is empty in docker/.env — cannot log in to seed" >&2; return 1; }

    echo "demo: logging in as $admin_user"
    token=$(curl -fsS -X POST "$api/api/auth/login" \
        -H 'Content-Type: application/json' \
        -d "{\"username\":\"$admin_user\",\"password\":\"$admin_pass\"}" \
        | python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])')
    api_auth() { curl -fsS "$@" -H "Authorization: Bearer $token" -H 'Content-Type: application/json'; }

    # 1. The camera (plan's demo-doorbell payload), idempotent by slug.
    camera_id=$(api_auth -s "$api/api/v1/cameras" | python3 -c '
import json, sys
for cam in json.load(sys.stdin).get("cameras", []):
    if cam["slug"] == "demo-doorbell":
        print(cam["id"]); break
') || true
    if [ -n "$camera_id" ]; then
        # A camera seeded by an older payload must be moved forward, and the
        # two fields that changed are both load-bearing:
        #   record_mode=events — the motion gate starves a static loop in
        #     continuous mode (422 frames, 1 inferred, 0 events);
        #   -g 50 in the source — the recorder copies the stream, so segments
        #     can only cut at keyframes, and the default 250-frame GOP made
        #     every segment 10 s against SEGMENT_SECONDS=5.
        demo_source='exec:ffmpeg -re -loop 1 -i /fixtures/bus.jpg -pix_fmt yuv420p -c:v libx264 -preset veryfast -g 50 -f h264 -'
        need=$(api_auth -s "$api/api/v1/cameras/$camera_id" | python3 -c '
import json, sys
d = json.load(sys.stdin)
print("yes" if d.get("record_mode") != "events" or d.get("source_uri") != sys.argv[1] else "no")
' "$demo_source")
        if [ "$need" = yes ]; then
            echo "demo: existing camera predates the current demo payload — moving it to events + a 2 s GOP source"
            api_auth -X PUT "$api/api/v1/cameras/$camera_id" \
                -d "{\"record_mode\": \"events\", \"source_uri\": \"$demo_source\"}" -o /dev/null
        fi
    fi
    if [ -z "$camera_id" ]; then
        echo "demo: POST /api/v1/cameras (demo-doorbell)"
        # record_mode is `events`, not the plan's `continuous`, and the
        # reason is the motion gate: a continuous camera skips inference
        # when the thumbnail has not moved 1%, and a looped still image
        # never moves — the gate would correctly starve the demo forever.
        # An events camera's frames *are* the trigger, so its gate is off,
        # and the fixture's person gets inferred every frame.
        camera_id=$(api_auth -X POST "$api/api/v1/cameras" -d '{
            "slug": "demo-doorbell",
            "name": "Demo Doorbell",
            "source_uri": "exec:ffmpeg -re -loop 1 -i /fixtures/bus.jpg -pix_fmt yuv420p -c:v libx264 -preset veryfast -g 50 -f h264 -",
            "record_mode": "events",
            "detection_enabled": true,
            "detection_fps": 5,
            "detection_width": 640,
            "detection_height": 360
        }' | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
    else
        echo "demo: camera demo-doorbell already exists ($camera_id) — not re-creating"
    fi

    # 2. Zones: the porch loiter rule (dwell 4 s, severity 3) across the
    #    bottom of the frame — where a visitor stands on the bus.jpg stoop.
    echo "demo: PUT zones (porch loiter dwell=4 severity=3)"
    api_auth -X PUT "$api/api/v1/cameras/$camera_id/zones" -d '[{
        "id": "porch",
        "name": "Porch",
        "polygon": [[0.1, 0.55], [0.9, 0.55], [0.9, 1.0], [0.1, 1.0]],
        "classes": ["person"],
        "rule": "loiter",
        "dwell_seconds": 4,
        "severity": 3,
        "min_confidence": 0,
        "min_area_ratio": 0,
        "enabled": true
    }]' -o /dev/null

    # 3. The fs tier as default, idempotent: only seed when there is none.
    if [ "$(api_auth -s "$api/api/storage/backends" | python3 -c 'import json,sys; print(len(json.load(sys.stdin).get("backends", [])))')" = "0" ]; then
        echo "demo: POST /api/storage/backends (fs local-tier, default)"
        backend_json=$(api_auth -X POST "$api/api/storage/backends" -d '{
            "name": "local-tier",
            "kind": "fs",
            "root_path": "/var/lib/nvr/storage",
            "enabled": true,
            "is_default": true
        }')
        echo "$backend_json" | python3 -c 'import json,sys; b=json.load(sys.stdin); print("demo:   backend {} ({}) default={}".format(b["name"], b["kind"], b["is_default"]))'
    else
        echo "demo: storage backends already exist — not re-seeding"
    fi

    # 4. Restart the sidecar so the pushed config becomes running streams.
    #    go2rtc instantiates streams at boot, not on POST /api/config — the
    #    push lands in /config/go2rtc.yaml and waits for the next start
    #    (S9-04's finding). In production the server can recreate the
    #    sidecar itself (GO2RTC_COMPOSE_FILE + a mounted docker socket);
    #    the dev server has no socket, so the script — which is the
    #    operator here — does it.
    echo "demo: restarting go2rtc so the new streams instantiate"
    compose restart -t 5 go2rtc >/dev/null
    for attempt in 1 2 3 4 5 6 7 8 9 10; do
        [ "$(docker inspect -f '{{.State.Health.Status}}' "$(compose ps -q go2rtc)" 2>/dev/null)" = healthy ] && break
        sleep 2
    done

    # 5. Probe: the card's criterion — online, with the exec:ffmpeg producer
    #    running. An exec: source is lazy inside go2rtc: it instantiates when
    #    a consumer connects, and the first consumer here is the recorder's
    #    ffmpeg — whose restart backoff has grown to 60 s if the stream was
    #    missing for a while, so the window is 90 s, not 30.
    echo "demo: POST probe (expecting online with the exec:ffmpeg producer)"
    for attempt in $(seq 1 18); do
        probe_json=$(api_auth -X POST "$api/api/v1/cameras/$camera_id/probe" -d '{}' || true)
        if printf '%s' "$probe_json" | python3 -c '
import json, sys
p = json.load(sys.stdin)
prods = " ".join(p.get("producers") or [])
ok = p.get("online") is True and "exec:" in prods and "ffmpeg" in prods
sys.exit(0 if ok else 1)
'; then
            echo "demo: probe online — $(printf '%s' "$probe_json" | python3 -c 'import json,sys; print(", ".join(json.load(sys.stdin).get("producers") or []))')"
            break
        fi
        if [ "$attempt" = 18 ]; then
            echo "dev.sh: the demo camera never came online: $probe_json" >&2
            echo "dev.sh: the exec source reads /fixtures/bus.jpg inside the go2rtc container — check: docker compose -f docker/docker-compose.yml exec go2rtc ls /fixtures" >&2
            return 1
        fi
        sleep 5
    done

    echo "demo: detection follows within ~35 s (config poll) — watch: scripts/dev.sh --logs detector"
}

case "${1:-}" in
    -h | --help)
        usage
        exit 0
        ;;
    --down)
        compose down
        ;;
    --logs)
        shift
        compose logs -f "$@"
        ;;
    --demo)
        compose up -d --build
        wait_health
        seed_demo
        echo
        echo "console: $console   api: $api   go2rtc: http://localhost:1984"
        echo "demo camera: demo-doorbell (fixture loop, porch loiter rule, local-tier default)"
        ;;
    "")
        compose up -d --build
        wait_health
        echo
        echo "console: $console   api: $api   go2rtc: http://localhost:1984"
        echo "no camera yet — scripts/dev.sh --demo seeds the fixture demo camera"
        ;;
    *)
        usage >&2
        exit 2
        ;;
esac

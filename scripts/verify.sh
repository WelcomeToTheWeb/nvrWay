#!/usr/bin/env bash
#
# nvrWay — the v1 acceptance gate (card S10-03)
#
# Usage: scripts/verify.sh
#
# Nine gates, each observed through HTTP against the running stack — never
# through the database. The database can lie about what an operator sees;
# these gates are what an operator sees.
#
#   1  health + healthz with a fresh detector heartbeat
#   2  admin login -> access_token
#   3  demo camera online with the exec:ffmpeg producer
#   4  >=1 event with person + loiter + porch + seconds >= 4 within 90 s
#      (the AI-detection claim, end to end, not a unit test)
#   5  snapshot 200 image/jpeg > 8 KB; clip 200 video/mp4; Range -> 206
#   6  >=3 segment rows, span ~= SEGMENT_SECONDS, count strictly rising
#   7  queue reaches uploaded + clip.mp4 on the tier + a disabled backend
#      stops claiming
#   8  in-repo IdP: discovery ok, scripted authorize -> consent -> callback
#      lands on the fragment, exchange roles == ["viewer"]
#   9  webhook listener recomputes X-NVR-Signature and it matches
#
# Gates 5/7/9 hang off the event CLOSE, which the detector forces at
# EVENT_MAX_SECONDS (300 s default), so a cold run takes ~6 minutes. That is
# the honest cost of "observed end-to-end": the clip is real ffmpeg over real
# segments of a real fixture, and the webhook signs a real close.
#
# Prerequisite: `scripts/dev.sh --demo` (the script says so, and seeds the
# camera itself if the stack is up but the camera is not).
set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
compose_file="$repo_root/docker/docker-compose.yml"
env_file="$repo_root/docker/.env"
api="http://127.0.0.1:8080"
segment_seconds="${SEGMENT_SECONDS:-5}"

red=$'\e[31m'; green=$'\e[32m'; yellow=$'\e[33m'; dim=$'\e[2m'; off=$'\e[0m'
fails=0
pass() { printf '%spass%s %s\n' "$green" "$off" "$1"; }
fail() { printf '%sfail%s %s\n' "$red" "$off" "$1"; fails=$((fails + 1)); }
step() { printf '%sstep%s %s\n' "$yellow" "$off" "$1"; }

env_get() { grep -E "^$1=" "$env_file" | tail -1 | cut -d= -f2- || true; }
jget() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)" 2>/dev/null; }

[ -f "$env_file" ] || { echo "verify.sh: docker/.env is missing — copy docker/.env.example" >&2; exit 1; }
admin_pass="$(env_get BOOTSTRAP_ADMIN_PASSWORD)"

# -- the stack must be up -----------------------------------------------------
if ! curl -fsS -o /dev/null "$api/health"; then
    printf '%sfail%s 0 the stack is not answering %s/health — run scripts/dev.sh --demo first\n' "$red" "$off" "$api"
    exit 1
fi
if [ ! -f "$repo_root/testdata/bus.jpg" ]; then
    step "0 testdata/bus.jpg is missing — fetching pinned fixtures"
    "$repo_root/scripts/fetch-fixture.sh" >/dev/null || { echo "verify.sh: fetch-fixture.sh failed" >&2; exit 1; }
fi

token=""
api_auth() { curl -fsS "$@" -H "Authorization: Bearer $token" -H 'Content-Type: application/json'; }

# The container-side address of the host: gates 8 and 9 run a server (the IdP)
# and a listener that the *server container* must reach, so they must be
# published on the bridge gateway, not on 127.0.0.1.
gateway=$(docker network inspect nvr_nvr_net -f '{{range .IPAM.Config}}{{.Gateway}}{{end}}' 2>/dev/null)
[ -n "$gateway" ] || { echo "verify.sh: cannot find the nvr_nvr_net gateway (is the dev stack up?)" >&2; exit 1; }

cleanup() {
    [ -n "${idp_pid:-}" ] && kill "$idp_pid" 2>/dev/null
    [ -n "${hook_pid:-}" ] && kill "$hook_pid" 2>/dev/null
    return 0
}
trap cleanup EXIT

# -- gate 1: health + a fresh detector heartbeat ------------------------------
step "1 health + healthz"
if curl -fsS "$api/health" | grep -q '"status":"ok"'; then
    hz_status=$(curl -fsS "$api/api/healthz" | jget 'd["detector"]["status"]')
    if [ "$hz_status" = fresh ]; then
        pass "1 /health ok; healthz detector fresh"
    else
        fail "1 healthz detector is '$hz_status', not fresh — the heartbeat is older than the 90 s window (or the detector never beat)"
    fi
else
    fail "1 /health did not answer ok"
fi

# -- gate 2: admin login -------------------------------------------------------
step "2 login as the bootstrap admin"
token=$(curl -fsS -X POST "$api/api/auth/login" -H 'Content-Type: application/json' \
    -d "{\"username\":\"$(env_get BOOTSTRAP_ADMIN_USER | grep . || echo admin)\",\"password\":\"$admin_pass\"}" \
    | jget 'd["access_token"]')
if [ -n "$token" ]; then
    pass "2 access_token received (${#token} chars)"
else
    fail "2 login returned no access_token — is BOOTSTRAP_ADMIN_PASSWORD in docker/.env the one the server booted with?"
    exit 1
fi

# -- gate 3: the demo camera is online ----------------------------------------
step "3 demo camera probe"
camera_id=$(api_auth -s "$api/api/v1/cameras" | jget 'next((c["id"] for c in d["cameras"] if c["slug"]=="demo-doorbell"), "")')
if [ -z "$camera_id" ]; then
    step "3 demo-doorbell is not seeded — running dev.sh --demo"
    "$repo_root/scripts/dev.sh" --demo >/dev/null 2>&1 || true
    camera_id=$(api_auth -s "$api/api/v1/cameras" | jget 'next((c["id"] for c in d["cameras"] if c["slug"]=="demo-doorbell"), "")')
fi
probe_json=""
for _ in $(seq 1 18); do
    probe_json=$(api_auth -X POST "$api/api/v1/cameras/$camera_id/probe" -d '{}' || true)
    printf '%s' "$probe_json" | python3 -c '
import json, sys
p = json.load(sys.stdin)
prods = " ".join(p.get("producers") or [])
sys.exit(0 if p.get("online") is True and "exec:" in prods and "ffmpeg" in prods else 1)
' && break
    sleep 5
done
if printf '%s' "$probe_json" | python3 -c '
import json, sys
p = json.load(sys.stdin)
prods = " ".join(p.get("producers") or [])
sys.exit(0 if p.get("online") is True and "exec:" in prods and "ffmpeg" in prods else 1)
'; then
    pass "3 probe online with the exec:ffmpeg producer"
else
    fail "3 probe never reported online with the exec:ffmpeg producer: $probe_json"
fi

# -- gate 9 setup: the listener must be up before the event closes -------------
step "9a webhook listener on :9501 + endpoint registration (before the close)"
hook_file=$(mktemp)
python3 - "$hook_file" <<'PY' &
import http.server, sys, threading
out = sys.argv[1]
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        with open(out, "ab") as f:
            f.write(b"\x00SEP\x00" + self.headers.get("X-NVR-Signature", "").encode() + b"\x00SEP\x00" + body)
        self.send_response(200); self.end_headers()
    def log_message(self, *_a): pass
srv = http.server.ThreadingHTTPServer(("0.0.0.0", 9501), H)
threading.Thread(target=srv.serve_forever, daemon=True).start()
import time
while True: time.sleep(1)
PY
hook_pid=$!
sleep 1
# The signing secret is STABLE across runs (kept in a file), not per-run.
# Why: an event that was already open when this script started closes on its
# own schedule, and that close can race the endpoint recreation below — the
# deliverer reads the webhook rows at close time, so a delivery can go out
# under the previous run's row. With a per-run secret that delivery verifies
# under no secret this script knows and gate 9 fails on a harness race; with
# a stable one, whichever row signs, the HMAC recomputes. The gate still
# proves the signature: a wrong-body or wrong-secret delivery would not match.
hook_secret_file=/tmp/nvr-verify-hook-secret
if [ ! -s "$hook_secret_file" ]; then
    printf 'verify-secret-%s' "$(date +%s)" > "$hook_secret_file"
fi
hook_secret=$(cat "$hook_secret_file")
# Idempotent: a previous run's endpoint would deliver with the old secret, and
# a signature recomputed against a new one would never match.
for wid in $(api_auth -s "$api/api/settings/webhooks" | jget '",".join(w["id"] for w in d.get("webhooks", []) if w.get("name")=="verify-listener")'); do
    api_auth -X DELETE "$api/api/settings/webhooks/$wid" -o /dev/null
done
hook_json=$(api_auth -X POST "$api/api/settings/webhooks" -d "{
    \"name\": \"verify-listener\",
    \"url\": \"http://$gateway:9501/hook\",
    \"secret\": \"$hook_secret\",
    \"events\": [\"event.close\"]
}" || true)
if printf '%s' "$hook_json" | python3 -c 'import json,sys; d=json.load(sys.stdin); sys.exit(0 if d.get("webhook",{}).get("id") or d.get("id") else 1)'; then
    pass "9a webhook endpoint registered -> http://$gateway:9501/hook"
else
    fail "9a webhook registration failed: $hook_json"
fi

# -- gate 4: the AI-detection claim, end to end --------------------------------
step "4 polling events for person + loiter + porch (up to 90 s)"
event_json=""
for _ in $(seq 1 18); do
    event_json=$(api_auth -s "$api/api/v1/events?camera_id=$camera_id&limit=20" || true)
    if printf '%s' "$event_json" | python3 -c '
import json, sys
for e in json.load(sys.stdin).get("events", []):
    if ("person" in (e.get("classes") or []) and "loiter" in (e.get("rules") or [])
            and "porch" in (e.get("zone_ids") or []) and e.get("seconds", 0) >= 4):
        sys.exit(0)
sys.exit(1)
'; then break; fi
    sleep 5
done
event_id=$(printf '%s' "$event_json" | python3 -c '
import json, sys
for e in json.load(sys.stdin).get("events", []):
    if ("person" in (e.get("classes") or []) and "loiter" in (e.get("rules") or [])
            and "porch" in (e.get("zone_ids") or []) and e.get("seconds", 0) >= 4):
        print(e["id"]); break
')
if [ -n "$event_id" ]; then
    pass "4 event $event_id: person + loiter + porch, seconds >= 4 (observed over HTTP)"
else
    fail "4 no person/loiter/porch event with seconds >= 4 within 90 s — the detector or the zone rule is not producing"
fi

# -- gate 5a: the snapshot (written on event open) ------------------------------
step "5a snapshot"
snap_ct=$(curl -s -o /tmp/nvr-verify-snap.jpg -w '%{content_type}' -H "Authorization: Bearer $token" "$api/api/v1/events/$event_id/snapshot")
snap_size=$(stat -c %s /tmp/nvr-verify-snap.jpg 2>/dev/null || echo 0)
if [ "$snap_ct" = "image/jpeg" ] && [ "$snap_size" -gt 8192 ]; then
    pass "5a snapshot 200 image/jpeg ${snap_size} B (> 8 KB)"
else
    fail "5a snapshot: content-type '$snap_ct', ${snap_size} B (want image/jpeg > 8192 B)"
fi

# -- gate 6: recording is live --------------------------------------------------
step "6 segments: >=3 rows, span ~= ${segment_seconds}s, strictly rising over 15 s"
# `from` is not decoration: rows come back ascending and the camera has long
# outgrown the limit, so an unbounded query returns the oldest 200 and the
# "newest" row is 2026's oldest, not the recorder's latest.
seg_from=$(python3 -c 'import datetime; print((datetime.datetime.now(datetime.timezone.utc)-datetime.timedelta(seconds=90)).strftime("%Y-%m-%dT%H:%M:%SZ"))')
seg_to=$(python3 -c 'import datetime; print((datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(seconds=10)).strftime("%Y-%m-%dT%H:%M:%SZ"))')
seg_check=$(api_auth -s "$api/api/v1/segments?camera_id=$camera_id&from=$seg_from&to=$seg_to&limit=200" | python3 -c "
import json, sys, datetime
rows = json.load(sys.stdin).get('segments', [])
def ts(s): return datetime.datetime.fromisoformat(s.replace('Z', '+00:00'))
spans = [(ts(r['end_ts']) - ts(r['start_ts'])).total_seconds() for r in rows]
# 2 of the last 3 within tolerance: when an event closes, the window's last
# segment is cut short by the ffmpeg kill, and that boundary row must not
# fail the gate for the recorder working correctly.
ok = len(rows) >= 3 and sum(1 for s in spans[-3:] if abs(s - $segment_seconds) <= 1.5) >= 2
print(f'{rows[-1][\"start_ts\"] if rows else \"\"}|{ok}')
")
seg_last1="${seg_check%%|*}"
if [ "${seg_check##*|}" = "True" ]; then
    sleep 15
    seg_last2=$(api_auth -s "$api/api/v1/segments?camera_id=$camera_id&from=$seg_from&to=$seg_to&limit=200" | jget 'd["segments"][-1]["start_ts"] if d["segments"] else ""')
    if [ -n "$seg_last2" ] && [ "$seg_last2" \> "$seg_last1" ]; then
        pass "6 segments span ~= ${segment_seconds}s; newest row $seg_last1 -> $seg_last2 over 15 s (recording is live)"
    else
        fail "6 newest segment did not advance over 15 s ($seg_last1 -> $seg_last2) — the recorder window is not open"
    fi
else
    fail "6 segments: $seg_check (want >=3 rows with span ~= ${segment_seconds}s)"
fi

# -- gate 8: OIDC against the in-repo IdP (runs while the event runs to close) --
step "8 OIDC: testidp on :9500, discovery, scripted walk, exchange at the viewer floor"
export PATH="$PATH:/usr/local/go/bin"
(cd "$repo_root/server" && go build -o /tmp/nvr-verify-testidp ./sso/testidp) || { fail "8 testidp would not build"; }
/tmp/nvr-verify-testidp -listen :9500 -issuer "http://$gateway:9500" >/tmp/nvr-verify-idp.log 2>&1 &
idp_pid=$!
for _ in $(seq 1 30); do curl -fsS -o /dev/null "http://127.0.0.1:9500/.well-known/openid-configuration" && break; sleep 1; done
oidc_id=$(api_auth -s "$api/api/settings/oidc" | jget 'next((p["id"] for p in d.get("providers", [])), "")')
oidc_name=$(api_auth -s "$api/api/settings/oidc" | jget 'next((p["name"] for p in d.get("providers", [])), "")')
if [ -z "$oidc_id" ]; then
    oidc_resp=$(api_auth -X POST "$api/api/settings/oidc" -d "{\"name\":\"verify-idp\",\"issuer\":\"http://$gateway:9500\",\"client_id\":\"nvrdev\",\"client_secret\":\"devsecret\"}")
    oidc_id=$(printf '%s' "$oidc_resp" | jget 'd.get("id", "")')
    oidc_name=$(printf '%s' "$oidc_resp" | jget 'd.get("name", "")')
else
    # The route resolves the provider by name, not by row id.
    oidc_resp=$(api_auth -X PUT "$api/api/settings/oidc/$oidc_name" -d "{\"name\":\"$oidc_name\",\"issuer\":\"http://$gateway:9500\",\"client_id\":\"nvrdev\",\"client_secret\":\"devsecret\"}")
fi
discovery=$(printf '%s' "$oidc_resp" | jget 'd.get("discovery_state", d)')
if [ "$discovery" = ok ]; then
    jar=$(mktemp)
    # The walk addresses the provider by name: `providerFor` resolves
    # GetByName, not the row id.
    loc1=$(curl -s -o /dev/null -D - -c "$jar" -b "$jar" "$api/api/auth/sso/$oidc_name/authorize" | tr -d '\r' | grep -i '^location:' | cut -d' ' -f2-)
    state=$(python3 -c "import sys,urllib.parse as u; q=u.parse_qs(u.urlparse('$loc1').query); print(q['state'][0])")
    curl -s -o /dev/null -c "$jar" -b "$jar" "$loc1"
    loc2=$(curl -s -o /dev/null -D - -c "$jar" -b "$jar" "http://$gateway:9500/consent?approve=$state" | tr -d '\r' | grep -i '^location:' | cut -d' ' -f2-)
    case "$loc2" in /*) loc2="$api$loc2";; esac
    loc3=$(curl -s -o /dev/null -D - -c "$jar" -b "$jar" "$loc2" | tr -d '\r' | grep -i '^location:' | cut -d' ' -f2-)
    frag_tok="${loc3##*#}"; frag_tok="${frag_tok#access_token=}"; frag_tok="${frag_tok%%&*}"
    roles=$(curl -s -X POST -H "Authorization: Bearer $frag_tok" "$api/api/auth/sso/exchange" | jget '",".join(d.get("roles", []))')
    if [[ "$loc3" == *"/auth/callback#access_token="* && "$roles" = viewer ]]; then
        pass "8 discovery ok; walk landed on $loc3"
        pass "8 exchange roles == [$roles] (the JIT floor, observed)"
    else
        fail "8 walk ended at '$loc3' with roles '$roles' (want /auth/callback#access_token=… and roles viewer)"
    fi
else
    fail "8 OIDC provider save reported discovery_state '$discovery', not ok: $oidc_resp"
fi
kill "$idp_pid" 2>/dev/null; idp_pid=""

# -- gate 5b: the clip (built at the event's close, ~300 s after open) ----------
step "5b clip — waiting for the event to close (EVENT_MAX_SECONDS forces it)"
clip_code=""
for _ in $(seq 1 84); do
    clip_code=$(curl -s -o /tmp/nvr-verify-clip.mp4 -w '%{http_code}' -H "Authorization: Bearer $token" "$api/api/v1/events/$event_id/clip")
    [ "$clip_code" = 200 ] && break
    sleep 5
done
clip_ct=$(curl -s -o /dev/null -D - -H "Authorization: Bearer $token" "$api/api/v1/events/$event_id/clip" | tr -d '\r' | grep -i '^content-type:' | cut -d' ' -f2-)
range_code=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $token" -H 'Range: bytes=0-99999' "$api/api/v1/events/$event_id/clip")
if [ "$clip_code" = 200 ] && [ "$clip_ct" = "video/mp4" ] && [ "$range_code" = 206 ]; then
    pass "5b clip 200 video/mp4 ($(stat -c %s /tmp/nvr-verify-clip.mp4) B); Range -> 206 (seekable)"
else
    fail "5b clip: code $clip_code, content-type '$clip_ct', Range -> $range_code (want 200/video-mp4/206)"
fi

# -- gate 9: the signature ------------------------------------------------------
step "9b webhook delivery + signature"
for _ in $(seq 1 12); do [ -s "$hook_file" ] && break; sleep 5; done
if python3 - "$hook_file" "$hook_secret" <<'PY'
import hashlib, hmac, sys
raw = open(sys.argv[1], "rb").read()
parts = raw.split(b"\x00SEP\x00")
ok = False
for i in range(1, len(parts) - 1, 2):
    header, body = parts[i].decode(), parts[i + 1]
    want = "sha256=" + hmac.new(sys.argv[2].encode(), body, hashlib.sha256).hexdigest()
    if hmac.compare_digest(header, want):
        ok = True
sys.exit(0 if ok else 1)
PY
then
    pass "9 listener received the close; X-NVR-Signature recomputes and matches"
else
    fail "9 no matching X-NVR-Signature delivery arrived at :9501 (listener file $(wc -c < "$hook_file") B)"
fi

# -- gate 7: the upload queue ---------------------------------------------------
step "7 queue -> uploaded; clip.mp4 on the tier; a disabled backend stops claiming"
backend_id=$(api_auth -s "$api/api/storage/backends" | jget 'next((b["id"] for b in d["backends"] if b.get("is_default")), "")')
uploaded=false
for _ in $(seq 1 24); do
    n=$(api_auth -s "$api/api/storage/queue?state=uploaded" | jget 'd.get("count", 0)')
    [ "${n:-0}" -gt 0 ] && { uploaded=true; break; }
    sleep 5
done
if ! $uploaded; then
    fail "7 no clip job reached uploaded within 120 s — the uploader is not draining the queue"
else
    # The clip's upload job lands within a tick of the build; poll the tier's
    # listing until the object shows up rather than racing it.
    listed=""
    for _ in $(seq 1 24); do
        listed=$(api_auth -X POST "$api/api/storage/backends/$backend_id/prefixes?prefix=nvr/events/$event_id/" -d '{}' | jget '",".join(d.get("keys", []))')
        [[ "$listed" == *clip* ]] && break
        sleep 5
    done
    if [[ "$listed" != *clip* ]]; then
        fail "7 the tier lists '$listed' under nvr/events/$event_id/ — no clip object"
    else
        # Disable the tier and re-queue: a disabled backend claims nothing, so
        # the requeued jobs must sit pending.
        api_auth -X PUT "$api/api/storage/backends/$backend_id/enabled" -d '{"enabled": false}' -o /dev/null
        api_auth -X POST "$api/api/storage/backends/$backend_id/requeue?kind=clip" -o /dev/null
        sleep 70
        busy=$(api_auth -s "$api/api/storage/queue?state=uploading" | jget 'd.get("count", 0)')
        pending=$(api_auth -s "$api/api/storage/queue?state=pending" | jget 'd.get("count", 0)')
        api_auth -X PUT "$api/api/storage/backends/$backend_id/enabled" -d '{"enabled": true}' -o /dev/null
        if [ "${busy:-0}" -eq 0 ] && [ "${pending:-0}" -gt 0 ]; then
            pass "7 queue reached uploaded; $listed on the tier; disabled backend: 0 uploading, $pending pending"
        else
            fail "7 disabled backend still moved jobs: uploading=${busy:-?} pending=${pending:-?}"
        fi
    fi
fi

# -- summary ---------------------------------------------------------------------
echo
if [ "$fails" -eq 0 ]; then
    printf '%sverify: 9 gates passed%s\n' "$green" "$off"
    exit 0
fi
printf '%sverify: %d gate(s) failed%s\n' "$red" "$fails" "$off"
exit 1

#!/usr/bin/env bash
#
# nvrWay — pinned detection fixtures (card S10-01)
#
# Usage: scripts/fetch-fixture.sh [bus.jpg|yolov8n.pt|all]
#
# Reads testdata/fixtures.json, downloads each pinned asset into testdata/,
# and verifies it against the manifest: byte count first (cheap, catches a
# truncated transfer or an upstream that quietly swapped the file), then the
# sha256. A mismatch is a hard failure with both numbers printed — the point
# of a pin is that a changed upstream does not silently change what the tests
# assert about. If a manifest entry ever ships without a sha256, the first
# verified download records it (that is how the pins were born, S5-12).
#
# The assets are never committed — .gitignore ignores testdata/bus.jpg,
# testdata/yolov8n.pt and testdata/yolov8n.onnx; the manifest carries the
# attribution (Ultralytics assets: AGPL-3.0 / CC-BY-4.0, see README's License
# section). yolov8n.onnx is in the manifest's "exported" list, not "assets":
# it is built by the image, and this script must not try to download it.
set -euo pipefail

usage() {
    echo "Usage: scripts/fetch-fixture.sh [bus.jpg|yolov8n.pt|all]   # default all"
    echo "Downloads the pinned assets into testdata/ and verifies byte counts"
    echo "and sha256 sums against testdata/fixtures.json."
}

case "${1:-}" in
    -h | --help)
        usage
        exit 0
        ;;
esac

target="${1:-all}"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
manifest="$repo_root/testdata/fixtures.json"
dest="$repo_root/testdata"

command -v curl >/dev/null || { echo "fetch-fixture.sh: curl is required" >&2; exit 1; }
command -v python3 >/dev/null || { echo "fetch-fixture.sh: python3 is required (manifest parsing)" >&2; exit 1; }
[ -f "$manifest" ] || { echo "fetch-fixture.sh: no manifest at $manifest" >&2; exit 1; }

# Emit one TAB-separated "name url bytes sha256" line per asset to fetch.
# An empty sha256 field means "record it on this run".
rows="$(python3 - "$manifest" "$target" <<'PY'
import json, sys
manifest, target = sys.argv[1], sys.argv[2]
data = json.load(open(manifest))
names = [a["name"] for a in data["assets"]]
if target != "all" and target not in names:
    sys.exit(f"unknown fixture {target!r}; the manifest pins: {', '.join(names)}")
for a in data["assets"]:
    if target in ("all", a["name"]):
        print("\t".join([a["name"], a["url"], str(a["bytes"]), a.get("sha256") or ""]))
PY
)"

cd "$dest"
while IFS=$'\t' read -r name url want_bytes want_sha; do
    [ -n "$name" ] || continue
    if [ -f "$name" ]; then
        got_bytes=$(wc -c < "$name" | tr -d ' ')
        got_sha=$(sha256sum "$name" | cut -d' ' -f1)
        if [ "$got_bytes" = "$want_bytes" ] && { [ -z "$want_sha" ] || [ "$got_sha" = "$want_sha" ]; }; then
            echo "$name: already present, verified ($got_bytes bytes)"
            continue
        fi
        echo "$name: present but does NOT match the pin (bytes $got_bytes/$want_bytes, sha ${got_sha:0:12}/${want_sha:0:12}) — refetching" >&2
        rm -f "$name"
    fi
    echo "$name: fetching $url"
    curl -fsSL --retry 3 -o "$name.part" "$url"
    got_bytes=$(wc -c < "$name.part" | tr -d ' ')
    if [ "$got_bytes" != "$want_bytes" ]; then
        rm -f "$name.part"
        echo "$name: FAIL byte count $got_bytes, manifest pins $want_bytes — upstream changed or transfer truncated" >&2
        exit 1
    fi
    got_sha=$(sha256sum "$name.part" | cut -d' ' -f1)
    if [ -z "$want_sha" ]; then
        echo "$name: recording sha256 $got_sha into the manifest (first verified download)"
        python3 - "$manifest" "$name" "$got_sha" <<'PY'
import json, sys
manifest, name, sha = sys.argv[1], sys.argv[2], sys.argv[3]
data = json.load(open(manifest))
for a in data["assets"]:
    if a["name"] == name:
        a["sha256"] = sha
json.dump(data, open(manifest, "w"), indent=2)
open(manifest, "a").write("\n")
PY
    elif [ "$got_sha" != "$want_sha" ]; then
        rm -f "$name.part"
        echo "$name: FAIL sha256 $got_sha, manifest pins $want_sha — the bytes differ from what the tests were written against" >&2
        exit 1
    fi
    mv "$name.part" "$name"
    echo "$name: verified ($got_bytes bytes, sha256 ${got_sha:0:12}…)"
done <<< "$rows"

echo "fixtures ready in $dest"

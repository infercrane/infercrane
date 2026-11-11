#!/usr/bin/env sh
# Runs a pack's hidden evaluator on CPU with Triton's interpreter. Reads one
# evaluation request on stdin and prints the result. Correctness only: the
# sanitizer and timing stages need a GPU and are reported as not run.
set -eu

here=$(CDPATH= cd -- "$(dirname "$0")" && pwd)
pack_dir=${INFERCRANE_KERNEL_PACK_DIR:-$here/packs}
image=${INFERCRANE_KERNEL_CPU_SCREEN_IMAGE:-}
if [ -z "$image" ]; then
  image=$(docker build -q "$here/cpu-screen")
fi

request=$(mktemp)
trap 'rm -f "$request"' EXIT
cat >"$request"

family=$(python3 -c 'import json, re, sys; f = json.load(open(sys.argv[1]))["family"]; sys.exit(1) if not re.fullmatch(r"[a-z0-9][a-z0-9-]{0,63}", f) else print(f)' "$request")
pack="$pack_dir/$family"
test -f "$pack/evaluate.py"

exec docker run --rm -i \
  --network none \
  --read-only \
  --tmpfs /tmp:rw,nosuid,size=512m \
  --cap-drop ALL \
  --security-opt no-new-privileges \
  --pids-limit 256 \
  --env TMPDIR=/tmp \
  --env HOME=/tmp \
  --env TRITON_CACHE_DIR=/tmp/triton \
  --volume "$pack:/pack:ro" \
  "$image" \
  python3 /pack/evaluate.py <"$request"

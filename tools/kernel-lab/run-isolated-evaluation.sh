#!/usr/bin/env sh
# Reference launcher for infercrane-kernel-adapter. Generated kernels are
# untrusted, so each attempt runs in a fresh container with no network, no
# credentials, a read-only root, and only the reviewed pack mounted.
set -eu

: "${INFERCRANE_KERNEL_EVAL_IMAGE:?set a digest-pinned evaluator image}"
: "${INFERCRANE_KERNEL_PACK_DIR:?set the reviewed pack directory}"

case "$INFERCRANE_KERNEL_EVAL_IMAGE" in
  *@sha256:*) ;;
  *) echo "evaluator image must be pinned by digest" >&2; exit 1 ;;
esac

request=$(mktemp)
trap 'rm -f "$request"' EXIT
cat >"$request"

family=$(jq -er '.family | select(test("^[a-z0-9][a-z0-9-]{0,63}$"))' "$request")
pack="$INFERCRANE_KERNEL_PACK_DIR/$family"
test -f "$pack/evaluate.py"

exec docker run --rm -i \
  --gpus all \
  --network none \
  --read-only \
  --tmpfs /tmp:rw,nosuid,size=1g \
  --cap-drop ALL \
  --security-opt no-new-privileges \
  --pids-limit 512 \
  --env TMPDIR=/tmp \
  --env HOME=/tmp \
  --env TRITON_CACHE_DIR=/tmp/triton \
  --volume "$pack:/pack:ro" \
  "$INFERCRANE_KERNEL_EVAL_IMAGE" \
  python3 /pack/evaluate.py <"$request"

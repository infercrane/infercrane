#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT HUP INT TERM
mkdir -p "$fixture/bin" "$fixture/home"

cat >"$fixture/bin/infercrane" <<'EOF'
#!/bin/sh
printf 'infercrane:%s\n' "$*" >>"$ENTRYPOINT_TEST_LOG"
EOF
cat >"$fixture/bin/runpod" <<'EOF'
#!/bin/sh
printf 'runpod:%s\n' "$*" >>"$ENTRYPOINT_TEST_LOG"
EOF
cat >"$fixture/bin/sky" <<'EOF'
#!/bin/sh
printf 'sky:%s\n' "$*" >>"$ENTRYPOINT_TEST_LOG"
exit 91
EOF
chmod 755 "$fixture/bin/infercrane" "$fixture/bin/runpod" "$fixture/bin/sky"

export PATH="$fixture/bin:$PATH"
export HOME="$fixture/home"
export ENTRYPOINT_TEST_LOG="$fixture/log"

: >"$ENTRYPOINT_TEST_LOG"
env -u RUNPOD_API_KEY -u RUNPOD_API_KEY_FILE -u INFERCRANE_SKYPILOT_API \
  sh "$root/scripts/entrypoint.sh" infercrane serve
grep -qx 'infercrane:serve' "$ENTRYPOINT_TEST_LOG"
if grep -Eq '^runpod:|^sky:' "$ENTRYPOINT_TEST_LOG"; then
  echo 'provider-neutral entrypoint unexpectedly started provider tooling' >&2
  exit 1
fi

key_file="$fixture/runpod-key"
printf '%s\n' 'test-only-key' >"$key_file"
: >"$ENTRYPOINT_TEST_LOG"
RUNPOD_API_KEY_FILE="$key_file" INFERCRANE_SKYPILOT_API=disabled \
  sh "$root/scripts/entrypoint.sh" infercrane version
grep -qx 'runpod:config test-only-key' "$ENTRYPOINT_TEST_LOG"
grep -qx 'infercrane:version' "$ENTRYPOINT_TEST_LOG"

if env -u RUNPOD_API_KEY -u RUNPOD_API_KEY_FILE INFERCRANE_SKYPILOT_API=enabled \
  sh "$root/scripts/entrypoint.sh" infercrane serve >"$fixture/out" 2>"$fixture/error"; then
  echo 'enabled SkyPilot mode accepted a missing RunPod credential' >&2
  exit 1
fi
grep -q 'requires a RunPod API key' "$fixture/error"

if INFERCRANE_SKYPILOT_API=invalid sh "$root/scripts/entrypoint.sh" infercrane serve \
  >"$fixture/out" 2>"$fixture/error"; then
  echo 'entrypoint accepted an invalid SkyPilot mode' >&2
  exit 1
fi
grep -q 'must be auto, enabled, or disabled' "$fixture/error"

echo 'provider-neutral entrypoint behavior passed'

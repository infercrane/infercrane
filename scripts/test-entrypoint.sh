#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
fixture=$(mktemp -d)
gcloud_runtime=$(mktemp -d /tmp/infercrane-gcloud-entrypoint.XXXXXX)
persistent_gcloud=$(mktemp -d "$root/.infercrane-gcloud-entrypoint.XXXXXX")
trap 'rm -rf "$fixture" "$gcloud_runtime" "$persistent_gcloud"' EXIT HUP INT TERM
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
env -u RUNPOD_API_KEY -u RUNPOD_API_KEY_FILE -u VAST_API_KEY -u VAST_API_KEY_FILE -u INFERCRANE_SKYPILOT_API \
  sh "$root/scripts/entrypoint.sh" infercrane serve
grep -qx 'infercrane:serve' "$ENTRYPOINT_TEST_LOG"
if grep -Eq '^runpod:|^sky:' "$ENTRYPOINT_TEST_LOG"; then
  echo 'provider-neutral entrypoint unexpectedly started provider tooling' >&2
  exit 1
fi

key_file="$fixture/runpod-key"
printf '%s\n' 'test-only-key' >"$key_file"
printf '[default]\napi_key = "%s"\n' 'test-only-key' >"$fixture/expected-runpod-config"
: >"$ENTRYPOINT_TEST_LOG"
RUNPOD_API_KEY_FILE="$key_file" INFERCRANE_SKYPILOT_API=disabled \
  sh "$root/scripts/entrypoint.sh" infercrane version
grep -qx 'infercrane:version' "$ENTRYPOINT_TEST_LOG"
cmp "$fixture/expected-runpod-config" "$HOME/.runpod/config.toml"
test "$(stat -c %a "$HOME/.runpod/config.toml" 2>/dev/null || stat -f %Lp "$HOME/.runpod/config.toml")" = 600
if grep -q '^runpod:' "$ENTRYPOINT_TEST_LOG"; then
  echo 'entrypoint ran the RunPod CLI; runpod 2.x has no config command' >&2
  exit 1
fi

# The key is written as a TOML basic string: quotes and backslashes are escaped.
printf '[default]\napi_key = "%s"\n' 'quote\"back\\slash' >"$fixture/expected-runpod-config"
RUNPOD_API_KEY='quote"back\slash' INFERCRANE_SKYPILOT_API=disabled \
  sh "$root/scripts/entrypoint.sh" infercrane version
cmp "$fixture/expected-runpod-config" "$HOME/.runpod/config.toml"
rm -rf "$HOME/.runpod"
[ ! -e "$HOME/.config/vastai/vast_api_key" ] || {
  echo 'entrypoint wrote a Vast key without one being configured' >&2
  exit 1
}

vast_file="$fixture/vast-key"
printf '%s\n' 'test-only-vast-key' >"$vast_file"
VAST_API_KEY_FILE="$vast_file" INFERCRANE_SKYPILOT_API=disabled \
  sh "$root/scripts/entrypoint.sh" sh -c '
    test "$(cat "$HOME/.config/vastai/vast_api_key")" = test-only-vast-key
    test "$(stat -c %a "$HOME/.config/vastai/vast_api_key" 2>/dev/null || stat -f %Lp "$HOME/.config/vastai/vast_api_key")" = 600
    test "$VAST_API_KEY" = test-only-vast-key
  '
rm -rf "$HOME/.config/vastai"

: >"$ENTRYPOINT_TEST_LOG"
INFERCRANE_BREZEL_SANDBOX_TOKEN='brezel-test-token' INFERCRANE_SKYPILOT_API=disabled \
  sh "$root/scripts/entrypoint.sh" sh -c '
    test -r "$INFERCRANE_BREZEL_SANDBOX_TOKEN_FILE"
    test "$(stat -c %a "$INFERCRANE_BREZEL_SANDBOX_TOKEN_FILE" 2>/dev/null || stat -f %Lp "$INFERCRANE_BREZEL_SANDBOX_TOKEN_FILE")" = 600
    test "$(cat "$INFERCRANE_BREZEL_SANDBOX_TOKEN_FILE")" = brezel-test-token
    test -z "${INFERCRANE_BREZEL_SANDBOX_TOKEN:-}"
  '

env -u INFERCRANE_PASSPORT_SIGNING_KEY_FILE INFERCRANE_PASSPORT_SIGNING_KEY='passport-test-key' \
  INFERCRANE_SKYPILOT_API=disabled \
  sh "$root/scripts/entrypoint.sh" sh -c '
    test -r "$INFERCRANE_PASSPORT_SIGNING_KEY_FILE"
    test "$(stat -c %a "$INFERCRANE_PASSPORT_SIGNING_KEY_FILE" 2>/dev/null || stat -f %Lp "$INFERCRANE_PASSPORT_SIGNING_KEY_FILE")" = 600
    test "$(cat "$INFERCRANE_PASSPORT_SIGNING_KEY_FILE")" = passport-test-key
    test -z "${INFERCRANE_PASSPORT_SIGNING_KEY:-}"
    rm -f "$INFERCRANE_PASSPORT_SIGNING_KEY_FILE"
  '
if INFERCRANE_PASSPORT_SIGNING_KEY='passport-test-key' INFERCRANE_PASSPORT_SIGNING_KEY_FILE="$fixture/passport-key" \
  INFERCRANE_SKYPILOT_API=disabled sh "$root/scripts/entrypoint.sh" infercrane version \
  >"$fixture/out" 2>"$fixture/error"; then
  echo 'entrypoint accepted both a passport signing key and a key file' >&2
  exit 1
fi
grep -q 'not both' "$fixture/error"

cat >"$fixture/bin/sky" <<'SKY'
#!/bin/sh
printf 'sky:%s\n' "$*" >>"$ENTRYPOINT_TEST_LOG"
echo 'HTTPError: 401 for url: https://console.vast.ai/api/v0/ssh/?api_key=leaked-test-secret&x=1'
exit 91
SKY
chmod 755 "$fixture/bin/sky"
if INFERCRANE_SKYPILOT_API=enabled INFERCRANE_SKYPILOT_PROVIDERS_JSON='[{"cloud":"vast"}]' \
  sh "$root/scripts/entrypoint.sh" infercrane serve >"$fixture/out" 2>&1; then
  echo 'entrypoint ignored a SkyPilot API server that exited' >&2
  exit 1
fi
if grep -q 'leaked-test-secret' "$fixture/out"; then
  echo 'SkyPilot output leaked a provider API key' >&2
  exit 1
fi
grep -q 'api_key=\[REDACTED\]&x=1' "$fixture/out"

if env -u INFERCRANE_SKYPILOT_PROVIDERS_JSON INFERCRANE_SKYPILOT_API=enabled \
  sh "$root/scripts/entrypoint.sh" infercrane serve >"$fixture/out" 2>"$fixture/error"; then
  echo 'enabled SkyPilot mode accepted a missing provider manifest' >&2
  exit 1
fi
grep -q 'requires INFERCRANE_SKYPILOT_PROVIDERS_JSON' "$fixture/error"

if INFERCRANE_SKYPILOT_API=invalid sh "$root/scripts/entrypoint.sh" infercrane serve \
  >"$fixture/out" 2>"$fixture/error"; then
  echo 'entrypoint accepted an invalid SkyPilot mode' >&2
  exit 1
fi
grep -q 'must be auto, enabled, or disabled' "$fixture/error"

mkdir -p "$fixture/gcloud-source/configurations"
printf '%s\n' '[core]' 'project = qualification' >"$fixture/gcloud-source/configurations/config_default"
: >"$ENTRYPOINT_TEST_LOG"
INFERCRANE_GCLOUD_CONFIG_SOURCE="$fixture/gcloud-source" CLOUDSDK_CONFIG="$gcloud_runtime" \
  sh "$root/scripts/entrypoint.sh" infercrane version
cmp "$fixture/gcloud-source/configurations/config_default" "$gcloud_runtime/configurations/config_default"
printf '%s\n' ephemeral >"$gcloud_runtime/credentials.db"
[ ! -e "$fixture/gcloud-source/credentials.db" ] || {
  echo 'entrypoint persisted an ephemeral gcloud credential database into the bootstrap profile' >&2
  exit 1
}
if INFERCRANE_GCLOUD_CONFIG_SOURCE="$fixture/gcloud-source" CLOUDSDK_CONFIG="$persistent_gcloud" \
  sh "$root/scripts/entrypoint.sh" infercrane version >"$fixture/out" 2>"$fixture/error"; then
  echo 'entrypoint accepted a persistent writable gcloud runtime path' >&2
  exit 1
fi
grep -q 'must use container-local /tmp storage' "$fixture/error"

echo 'provider-neutral entrypoint behavior passed'

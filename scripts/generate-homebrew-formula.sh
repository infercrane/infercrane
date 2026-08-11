#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
dist=${1:-"$root/dist"}
tag=${2:-v2.0.0-rc.1}
output=${3:-"$dist/homebrew/infercrane.rb"}
version=${tag#v}
base_url=${INFERCRANE_HOMEBREW_BASE_URL:-"https://github.com/infercrane/infercrane/releases/download/$tag"}
checksums="$dist/checksums.txt"

printf '%s\n' "$tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$' || {
  echo "invalid release tag: $tag" >&2
  exit 2
}
test -f "$checksums" || { echo "missing $checksums" >&2; exit 1; }

checksum() {
  name=$1
  value=$(awk -v wanted="$name" '$2 == wanted {print $1}' "$checksums")
  test "${#value}" = 64 || { echo "missing checksum for $name" >&2; exit 1; }
  printf '%s' "$value"
}

darwin_arm64=$(checksum "infercrane_${version}_darwin_arm64.tar.gz")
darwin_amd64=$(checksum "infercrane_${version}_darwin_amd64.tar.gz")
linux_arm64=$(checksum "infercrane_${version}_linux_arm64.tar.gz")
linux_amd64=$(checksum "infercrane_${version}_linux_amd64.tar.gz")

mkdir -p "$(dirname "$output")"
sed \
  -e "s|RELEASE_VERSION|$version|g" \
  -e "s|RELEASE_BASE_URL|$base_url|g" \
  -e "s|RELEASE_DARWIN_ARM64_SHA256|$darwin_arm64|g" \
  -e "s|RELEASE_DARWIN_AMD64_SHA256|$darwin_amd64|g" \
  -e "s|RELEASE_LINUX_ARM64_SHA256|$linux_arm64|g" \
  -e "s|RELEASE_LINUX_AMD64_SHA256|$linux_amd64|g" \
  "$root/packaging/homebrew/infercrane.rb" >"$output"

if grep -q 'RELEASE_' "$output"; then
  echo "unresolved Homebrew formula token" >&2
  exit 1
fi
ruby -c "$output" >/dev/null
echo "$output"

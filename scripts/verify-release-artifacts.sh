#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
dist=${1:-dist}
tag=${2:-$(jq -er '.candidate_tag' "$root/.release/version.json")}
version=${tag#v}

printf '%s\n' "$tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$' || {
  echo "invalid release tag: $tag" >&2
  exit 2
}
test -f "$dist/checksums.txt"

expected="darwin_amd64 darwin_arm64 linux_amd64 linux_arm64"
for target in $expected; do
  archive="$dist/infercrane_${version}_${target}.tar.gz"
  sbom="$archive.sbom.json"
  test -f "$archive" || { echo "missing archive: $archive" >&2; exit 1; }
  test -f "$sbom" || { echo "missing archive SBOM: $sbom" >&2; exit 1; }
  listing=$(tar -tzf "$archive" | sed '/\/$/d' | sort)
  for required_file in LICENSE NOTICE README.md THIRD_PARTY_NOTICES.md infercrane \
    THIRD_PARTY_LICENSES/manifest.csv THIRD_PARTY_LICENSES/gopkg.in/yaml.v3/NOTICE; do
    printf '%s\n' "$listing" | grep -Fqx "$required_file" || {
      echo "missing $required_file from $archive" >&2
      exit 1
    }
  done
  unexpected=$(printf '%s\n' "$listing" | grep -Ev '^(LICENSE|NOTICE|README\.md|THIRD_PARTY_NOTICES\.md|infercrane|THIRD_PARTY_LICENSES/.+)$' || true)
  test -z "$unexpected" || {
    echo "unexpected archive contents: $archive" >&2
    printf '%s\n' "$unexpected" >&2
    exit 1
  }
  bundled_license_count=$(printf '%s\n' "$listing" | grep -Ec '^THIRD_PARTY_LICENSES/.*/(LICENSE[^/]*|COPYING[^/]*)$')
  test "$bundled_license_count" -ge 35 || {
    echo "expected at least 35 dependency license files in $archive, found $bundled_license_count" >&2
    exit 1
  }
  jq -e '.spdxVersion | startswith("SPDX-")' "$sbom" >/dev/null
done

archive_count=$(find "$dist" -maxdepth 1 -type f -name "infercrane_${version}_*.tar.gz" | wc -l | tr -d ' ')
sbom_count=$(find "$dist" -maxdepth 1 -type f -name "infercrane_${version}_*.tar.gz.sbom.json" | wc -l | tr -d ' ')
test "$archive_count" = 4 || { echo "expected four archives, found $archive_count" >&2; exit 1; }
test "$sbom_count" = 4 || { echo "expected four archive SBOMs, found $sbom_count" >&2; exit 1; }

if command -v sha256sum >/dev/null 2>&1; then
  (cd "$dist" && sha256sum -c checksums.txt)
else
  (cd "$dist" && shasum -a 256 -c checksums.txt)
fi

host_os=$(uname -s | tr '[:upper:]' '[:lower:]')
case $(uname -m) in arm64|aarch64) host_arch=arm64 ;; x86_64|amd64) host_arch=amd64 ;; *) host_arch=unknown ;; esac
native="$dist/infercrane_${version}_${host_os}_${host_arch}.tar.gz"
if test -f "$native"; then
  install_root=$(mktemp -d)
  trap 'rm -rf "$install_root"' EXIT HUP INT TERM
  tar -xzf "$native" -C "$install_root"
  "$install_root/infercrane" version | grep -F "$version" >/dev/null
  "$install_root/infercrane" completion bash >/dev/null
  "$install_root/infercrane" --help >/dev/null
fi

echo "Release artifacts verified for $tag: four archives, Apache-2.0 and dependency notices, checksums, SPDX SBOMs, and native CLI smoke."

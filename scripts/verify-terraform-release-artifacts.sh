#!/bin/sh
set -eu

dist=${1:?usage: verify-terraform-release-artifacts.sh DIST TAG}
tag=${2:?usage: verify-terraform-release-artifacts.sh DIST TAG}
printf '%s\n' "$tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[1-9][0-9]*)?$' || {
  echo "invalid Terraform release tag: $tag" >&2
  exit 2
}
version=${tag#v}
checksums="terraform-provider-infercrane_${version}_SHA256SUMS"
test -f "$dist/$checksums"

expected=0
for platform in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64; do
  goos=${platform%/*}
  goarch=${platform#*/}
  archive="terraform-provider-infercrane_${version}_${goos}_${goarch}.zip"
  binary="terraform-provider-infercrane_v${version}"
  test -f "$dist/$archive"
  test "$(unzip -Z1 "$dist/$archive")" = "$binary"
  expected=$((expected + 1))
done
test "$(find "$dist" -maxdepth 1 -type f -name "terraform-provider-infercrane_${version}_*.zip" | wc -l | tr -d ' ')" = "$expected"

(
  cd "$dist"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum -c "$checksums"
  else
    shasum -a 256 -c "$checksums"
  fi
)
echo "Terraform provider artifacts verified for $tag: four platform ZIPs and checksums."

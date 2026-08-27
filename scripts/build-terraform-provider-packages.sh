#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
output=${1:?usage: build-terraform-provider-packages.sh DIST TAG}
tag=${2:?usage: build-terraform-provider-packages.sh DIST TAG}
candidate_tag=$(jq -er '.candidate_tag' "$root/.release/version.json")
stable_tag=$(jq -er '.stable_tag' "$root/.release/version.json")
case "$tag" in "$candidate_tag"|"$stable_tag") ;; *) echo "unsupported Terraform release tag: $tag" >&2; exit 2;; esac
version=${tag#v}

temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT HUP INT TERM
mkdir -p "$output"
output=$(CDPATH= cd -- "$output" && pwd)

for platform in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64; do
  goos=${platform%/*}
  goarch=${platform#*/}
  binary="terraform-provider-infercrane_v${version}"
  [ "$goos" != windows ] || binary="$binary.exe"
  build_dir="$temporary/${goos}_${goarch}"
  mkdir -p "$build_dir"
  (
    cd "$root/integrations/terraform"
    CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch go build \
      -trimpath -ldflags "-s -w -X main.version=$version" \
      -o "$build_dir/$binary" .
  )
  archive="terraform-provider-infercrane_${version}_${goos}_${goarch}.zip"
  (cd "$build_dir" && zip -q "$output/$archive" "$binary")
done

checksums="terraform-provider-infercrane_${version}_SHA256SUMS"
(
  cd "$output"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum terraform-provider-infercrane_${version}_*.zip >"$checksums"
  else
    shasum -a 256 terraform-provider-infercrane_${version}_*.zip >"$checksums"
  fi
)

"$root/scripts/verify-terraform-release-artifacts.sh" "$output" "$tag"
echo "Terraform provider release packages built for $tag"

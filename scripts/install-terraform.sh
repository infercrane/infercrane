#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
version=1.15.8
tools_dir="$root/.infercrane/tools"
terraform_bin="$tools_dir/terraform"

terraform_version() {
  "$1" version -json 2>/dev/null | jq -er '.terraform_version' 2>/dev/null || true
}

if [ -x "$terraform_bin" ] && [ "$(terraform_version "$terraform_bin")" = "$version" ]; then
  printf '%s\n' "$terraform_bin"
  exit 0
fi

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "unsupported Terraform architecture: $arch" >&2; exit 1 ;;
esac

case "$os/$arch" in
  darwin/amd64) checksum=e2e812e783771159bf758fd4e55d6dc9bb08f63e2af2c63d212721807a02c5dc ;;
  darwin/arm64) checksum=f210110c5698b94d803a7a63cdb0251b5455c150841478808e2bbb343f95ed68 ;;
  linux/amd64) checksum=d25ce7b6902013ad905db3d2eab0be4cd905887fe88b81a6171b8d5503c31f3d ;;
  linux/arm64) checksum=8891e9dcedc9e3b8950bc6af9d4d8af1f4cfade3062f53b9dc403a89f6ce8c9c ;;
  *) echo "unsupported Terraform platform: $os/$arch" >&2; exit 1 ;;
esac

command -v curl >/dev/null 2>&1 || { echo "curl is required to install Terraform" >&2; exit 1; }
command -v jq >/dev/null 2>&1 || { echo "jq is required to verify Terraform" >&2; exit 1; }
command -v unzip >/dev/null 2>&1 || { echo "unzip is required to install Terraform" >&2; exit 1; }

mkdir -p "$tools_dir"
temporary=$(mktemp -d "$tools_dir/.terraform-$version.XXXXXX")
cleanup() { rm -rf "$temporary"; }
trap cleanup EXIT HUP INT TERM

archive="terraform_${version}_${os}_${arch}.zip"
url="https://releases.hashicorp.com/terraform/$version/$archive"
echo "Installing checksum-pinned Terraform $version for $os/$arch..." >&2
curl --proto '=https' --tlsv1.2 --fail --silent --show-error --location \
  --output "$temporary/$archive" "$url"

if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$temporary/$archive" | awk '{print $1}')
else
  actual=$(shasum -a 256 "$temporary/$archive" | awk '{print $1}')
fi
[ "$actual" = "$checksum" ] || {
  echo "Terraform archive checksum mismatch for $archive" >&2
  exit 1
}

unzip -q "$temporary/$archive" terraform -d "$temporary"
chmod 0755 "$temporary/terraform"
[ "$(terraform_version "$temporary/terraform")" = "$version" ] || {
  echo "downloaded Terraform binary did not report version $version" >&2
  exit 1
}
mv "$temporary/terraform" "$terraform_bin"
printf '%s\n' "$terraform_bin"

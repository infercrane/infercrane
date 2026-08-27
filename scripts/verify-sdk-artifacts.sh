#!/bin/sh
set -eu

dist=${1:?usage: verify-sdk-artifacts.sh DIST VERSION [prepare|verify]}
version=${2:?usage: verify-sdk-artifacts.sh DIST VERSION [prepare|verify]}
mode=${3:-verify}

case "$mode" in
  prepare|verify) ;;
  *) echo "mode must be prepare or verify" >&2; exit 2 ;;
esac

printf '%s\n' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$' || {
  echo "invalid SDK version: $version" >&2
  exit 2
}

wheel="infercrane-${version}-py3-none-any.whl"
npm_package="infercrane-sdk-${version}.tgz"

test "$(find "$dist" -maxdepth 1 -type f -name 'infercrane-*.whl' | wc -l | tr -d ' ')" = 1
test "$(find "$dist" -maxdepth 1 -type f -name 'infercrane-sdk-*.tgz' | wc -l | tr -d ' ')" = 1
test -f "$dist/$wheel"
test -f "$dist/$npm_package"

python3 - "$dist/$wheel" "$version" <<'PY'
import sys
import zipfile

wheel, version = sys.argv[1:]
with zipfile.ZipFile(wheel) as archive:
    names = archive.namelist()
    metadata = archive.read(next(name for name in names if name.endswith(".dist-info/METADATA"))).decode()
    assert "Name: infercrane\n" in metadata
    assert f"Version: {version}\n" in metadata
    assert any(name.endswith(".dist-info/licenses/LICENSE") for name in names)
    assert any(name.endswith(".dist-info/licenses/NOTICE") for name in names)
PY

tar -xOf "$dist/$npm_package" package/package.json | \
  jq -e --arg version "$version" '.name == "@infercrane/sdk" and .version == $version' >/dev/null
tar -tzf "$dist/$npm_package" | grep -Eq '^package/LICENSE$'
tar -tzf "$dist/$npm_package" | grep -Eq '^package/NOTICE$'

checksum() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$@"
  else
    shasum -a 256 "$@"
  fi
}

verify_checksums() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum -c sdk-checksums.txt
  else
    shasum -a 256 -c sdk-checksums.txt
  fi
}

if [ "$mode" = prepare ]; then
  (
    cd "$dist"
    checksum "$wheel" "$npm_package" > sdk-checksums.txt
  )
fi

test -f "$dist/sdk-checksums.txt"
(cd "$dist" && verify_checksums)

echo "SDK artifacts verified for $version: exact wheel and npm package, Apache-2.0 notices, and SHA-256 checksums."

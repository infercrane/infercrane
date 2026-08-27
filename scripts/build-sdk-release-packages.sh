#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
output=${1:?usage: build-sdk-release-packages.sh DIST TAG}
tag=${2:?usage: build-sdk-release-packages.sh DIST TAG}
stable_version=$(jq -er '.version' "$root/.release/version.json")
candidate_tag=$(jq -er '.candidate_tag' "$root/.release/version.json")
stable_tag=$(jq -er '.stable_tag' "$root/.release/version.json")

case "$tag" in
  "$stable_tag")
    npm_version=$stable_version
    python_version=$stable_version
    ;;
  "$candidate_tag")
    npm_version=${tag#v}
    rc_number=${npm_version##*.}
    python_version=${stable_version}rc${rc_number}
    ;;
  *)
    echo "release SDK tag must be $candidate_tag or $stable_tag: $tag" >&2
    exit 2
    ;;
esac

temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT HUP INT TERM
mkdir -p "$output"
output=$(CDPATH= cd -- "$output" && pwd)

git -C "$root" archive HEAD sdk/python | tar -xf - -C "$temporary"
python_root="$temporary/sdk/python"
if [ "$python_version" != "$stable_version" ]; then
  test "$(grep -Fc "version = \"$stable_version\"" "$python_root/pyproject.toml")" = 1
  sed "s/version = \"$stable_version\"/version = \"$python_version\"/" \
    "$python_root/pyproject.toml" >"$python_root/pyproject.toml.next"
  mv "$python_root/pyproject.toml.next" "$python_root/pyproject.toml"
  test "$(grep -Fc "infercrane-python/$stable_version" "$python_root/src/infercrane/client.py")" = 2
  sed "s#infercrane-python/$stable_version#infercrane-python/$python_version#g" \
    "$python_root/src/infercrane/client.py" >"$python_root/src/infercrane/client.py.next"
  mv "$python_root/src/infercrane/client.py.next" "$python_root/src/infercrane/client.py"
  test "$(grep -Fc 'Development Status :: 5 - Production/Stable' "$python_root/pyproject.toml")" = 1
  sed 's/Development Status :: 5 - Production\/Stable/Development Status :: 4 - Beta/' \
    "$python_root/pyproject.toml" >"$python_root/pyproject.toml.next"
  mv "$python_root/pyproject.toml.next" "$python_root/pyproject.toml"
fi
python3 -m pip wheel --disable-pip-version-check --no-deps --wheel-dir "$output" "$python_root"

if [ "$tag" = "$candidate_tag" ]; then
  "$root/scripts/build-npm-bootstrap-package.sh" "$output" >/dev/null
  rm -f "$output/infercrane-sdk-${npm_version}.tgz.sha256"
else
  git -C "$root" archive HEAD sdk/typescript | tar -xf - -C "$temporary"
  npm_root="$temporary/sdk/typescript"
  npm --prefix "$npm_root" ci --no-audit --no-fund
  npm --prefix "$npm_root" test
  (cd "$npm_root" && npm pack --pack-destination "$output" --json) >"$temporary/npm-pack.json"
fi

"$root/scripts/verify-sdk-artifacts.sh" "$output" "$npm_version" prepare "$python_version"
jq -n --arg tag "$tag" --arg npm_version "$npm_version" --arg python_version "$python_version" \
  '{schema_version:1,tag:$tag,npm_version:$npm_version,python_version:$python_version}' \
  >"$output/sdk-release.json"

echo "SDK release packages built: tag=$tag python=$python_version npm=$npm_version"

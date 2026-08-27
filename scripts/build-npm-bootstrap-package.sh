#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
output=${1:-"$root/dist/npm-bootstrap"}
stable_version=$(jq -er '.version' "$root/.release/version.json")
candidate_tag=$(jq -er '.candidate_tag' "$root/.release/version.json")
candidate_version=${candidate_tag#v}

case "$candidate_version" in
  "$stable_version"-rc.[1-9]|"$stable_version"-rc.[1-9][0-9]*) ;;
  *) echo "candidate npm version must be an RC of $stable_version: $candidate_version" >&2; exit 1 ;;
esac

temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT HUP INT TERM
mkdir -p "$output"
output=$(CDPATH= cd -- "$output" && pwd)

git -C "$root" archive HEAD sdk/typescript | tar -xf - -C "$temporary"
package_root="$temporary/sdk/typescript"

npm --prefix "$package_root" version "$candidate_version" \
  --no-git-tag-version --ignore-scripts >/dev/null
node - "$package_root/src/client.ts" "$stable_version" "$candidate_version" <<'NODE'
const fs = require('node:fs');
const [file, stableVersion, candidateVersion] = process.argv.slice(2);
const before = fs.readFileSync(file, 'utf8');
const stableAgent = `infercrane-typescript/${stableVersion}`;
const occurrences = before.split(stableAgent).length - 1;
if (occurrences !== 2) throw new Error(`expected two stable User-Agent values, found ${occurrences}`);
fs.writeFileSync(file, before.replaceAll(stableAgent, `infercrane-typescript/${candidateVersion}`));
NODE

npm --prefix "$package_root" ci --no-audit --no-fund
npm --prefix "$package_root" test
(cd "$package_root" && npm pack --pack-destination "$output" --json) >"$temporary/npm-pack.json"

archive_name="infercrane-sdk-${candidate_version}.tgz"
archive="$output/$archive_name"
test -f "$archive"
tar -xOf "$archive" package/package.json | jq -e --arg version "$candidate_version" \
  '.name == "@infercrane/sdk" and .version == $version and .license == "Apache-2.0" and
   .repository.url == "https://github.com/infercrane/infercrane.git"' >/dev/null
tar -tzf "$archive" | grep -Eq '^package/LICENSE$'
tar -tzf "$archive" | grep -Eq '^package/NOTICE$'
packaged_agents=$(tar -xOf "$archive" package/dist/client.js | \
  grep -Eo 'infercrane-typescript/[0-9A-Za-z.-]+' | sort -u)
if [ "$packaged_agents" != "infercrane-typescript/$candidate_version" ]; then
  echo "bootstrap package has inconsistent User-Agent versions: $packaged_agents" >&2
  exit 1
fi

smoke_root="$temporary/smoke"
mkdir "$smoke_root"
(cd "$smoke_root" && npm init -y >/dev/null && npm install --ignore-scripts "$archive" >/dev/null)
(cd "$smoke_root" && node --input-type=module -e \
  'import { InferCrane } from "@infercrane/sdk"; if (typeof InferCrane !== "function") process.exit(1)')

(
  cd "$output"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$archive_name" >"$archive_name.sha256"
  else
    shasum -a 256 "$archive_name" >"$archive_name.sha256"
  fi
)

echo "$archive"

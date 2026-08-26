#!/usr/bin/env sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
cd "$root"

for file in LICENSE sdk/python/LICENSE sdk/typescript/LICENSE; do
  grep -Fqx 'MIT License' "$file" || {
    echo "$file must contain the MIT license" >&2
    exit 1
  }
done

grep -Fq 'license = "MIT"' sdk/python/pyproject.toml
node -e 'const p=require("./sdk/typescript/package.json"); if (p.license !== "MIT") process.exit(1)'
grep -Fq 'license "MIT"' packaging/homebrew/infercrane.rb
grep -Fq 'licensed under MIT' docs/editions.mdx
grep -Fq '| Public | MIT |' docs/editions.mdx
grep -Fq 'Hosted tenancy, supplier credentials, billing, payments, and warm-pool operations | Private service | Proprietary' docs/editions.mdx
grep -Fq 'Marketing site and hosted account experience | Company-operated | Proprietary' docs/editions.mdx
grep -Fq 'under the [MIT License](LICENSE)' README.md
grep -Fq 'SPDX SBOM' THIRD_PARTY_NOTICES.md
grep -Fq 'THIRD_PARTY_NOTICES.md' .goreleaser.yaml
grep -Fq 'THIRD_PARTY_NOTICES.md' scripts/verify-release-artifacts.sh

echo 'MIT core and SDK license boundaries verified.'

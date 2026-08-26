#!/usr/bin/env sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
cd "$root"

grep -Fq 'Apache License' LICENSE
grep -Fq 'Version 2.0, January 2004' LICENSE
test -f NOTICE
if command -v sha256sum >/dev/null 2>&1; then
  license_sha256=$(sha256sum LICENSE | awk '{print $1}')
else
  license_sha256=$(shasum -a 256 LICENSE | awk '{print $1}')
fi
test "$license_sha256" = cfc7749b96f63bd31c3c42b5c471bf756814053e847c10f3eb003417bc523d30 || {
  echo 'LICENSE does not match the canonical Apache-2.0 text' >&2
  exit 1
}
cmp -s LICENSE sdk/python/LICENSE
cmp -s LICENSE sdk/typescript/LICENSE
cmp -s NOTICE sdk/python/NOTICE
cmp -s NOTICE sdk/typescript/NOTICE

grep -Fq 'license = "Apache-2.0"' sdk/python/pyproject.toml
grep -Fq 'license-files = ["LICENSE", "NOTICE"]' sdk/python/pyproject.toml
node -e 'const p=require("./sdk/typescript/package.json"); if (p.license !== "Apache-2.0") process.exit(1)'
node -e 'const p=require("./sdk/typescript/package.json"); if (!p.files.includes("NOTICE")) process.exit(1)'
grep -Fq 'license "Apache-2.0"' packaging/homebrew/infercrane.rb
grep -Fq 'licensed under Apache-2.0' docs/editions.mdx
grep -Fq '| Public | Apache-2.0 |' docs/editions.mdx
grep -Fq 'Hosted tenancy, supplier credentials, billing, payments, and warm-pool operations | Private service | Proprietary' docs/editions.mdx
grep -Fq 'Marketing site and hosted account experience | Company-operated | Proprietary' docs/editions.mdx
grep -Fq 'under the [Apache License 2.0](LICENSE)' README.md
grep -Fq 'SPDX SBOM' THIRD_PARTY_NOTICES.md
grep -Fq 'THIRD_PARTY_LICENSES/' THIRD_PARTY_NOTICES.md

test -x scripts/generate-go-license-bundle.sh
grep -Fq 'github.com/google/go-licenses/v2@v2.0.1' scripts/generate-go-license-bundle.sh
grep -Fq './scripts/generate-go-license-bundle.sh .release/generated-licenses/go' .goreleaser.yaml
grep -Fq 'dst: THIRD_PARTY_LICENSES' .goreleaser.yaml
grep -Fq 'THIRD_PARTY_LICENSES/gopkg.in/yaml.v3/NOTICE' scripts/verify-release-artifacts.sh

grep -Fq 'org.opencontainers.image.licenses="Apache-2.0"' Dockerfile
grep -Fq '/usr/share/licenses/infercrane/' Dockerfile
grep -Fq '/usr/share/licenses/infercrane/' images/vllm-runpod/Dockerfile
test -f packaging/container/THIRD_PARTY_COMPONENTS.md
test -f packaging/container/licenses/aws-cli-LICENSE.txt
test -f images/vllm-runpod/THIRD_PARTY_COMPONENTS.md

go run github.com/google/go-licenses/v2@v2.0.1 check \
  --ignore github.com/infercrane/infercrane \
  --allowed_licenses=Apache-2.0,MIT,BSD-3-Clause \
  ./cmd/infercrane

echo 'Apache-2.0 core, SDK, archive, image, and dependency license boundaries verified.'

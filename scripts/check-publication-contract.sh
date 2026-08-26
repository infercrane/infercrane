#!/usr/bin/env sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
release_workflow="$root/.github/workflows/release.yml"
publish_workflow="$root/.github/workflows/publish-stable.yml"

test -f "$root/.release/publication-runbook.md"
test -f "$root/.release/e2e-verification-prompt.md"
test -f "$root/.gitleaks.toml"

# Stable publication must remain an explicit, protected action over an already
# built draft. Tag creation alone is not authority to publish irreversible SDK
# versions or update the Homebrew tap.
grep -Fq 'workflow_dispatch:' "$publish_workflow"
grep -Fq 'environment: stable-publication' "$publish_workflow"
grep -Fq '.stable_tag' "$publish_workflow"
grep -Fq '.two_factor_requirement_enabled' "$publish_workflow"
grep -Fq '.visibility)" = public' "$publish_workflow"
grep -Fq '.isDraft == true' "$publish_workflow"
grep -Eq 'pypa/gh-action-pypi-publish@[0-9a-f]{40}' "$publish_workflow"
grep -Fq 'npm publish publication-assets/npm/' "$publish_workflow"
grep -Fq 'HOMEBREW_TAP_TOKEN' "$publish_workflow"
grep -Fq 'gh release edit "$RELEASE_TAG" --draft=false --latest' "$publish_workflow"

# The tag workflow may build and attach artifacts, but it must leave the GitHub
# release as a draft and provide verifiable provenance for archives and images.
grep -Fq 'draft: true' "$root/.goreleaser.yaml"
grep -Fq './scripts/generate-homebrew-formula.sh dist "$GITHUB_REF_NAME"' "$release_workflow"
test "$(grep -Ec 'uses: actions/attest@[0-9a-f]{40}' "$release_workflow")" -eq 2
grep -Fq 'subject-name: ghcr.io/infercrane/infercrane' "$release_workflow"
if grep -REq 'uses: [^ ]+@(v[0-9]+|release/v[0-9]+)([[:space:]]|$)' "$root/.github/workflows"; then
  echo "GitHub Actions must be pinned to full commit SHAs" >&2
  exit 1
fi

jq -e '.publishConfig.access == "public" and .publishConfig.registry == "https://registry.npmjs.org/"' \
  "$root/sdk/typescript/package.json" >/dev/null
grep -Fq 'requires = ["hatchling==1.27.0"]' "$root/sdk/python/pyproject.toml"
grep -Fq 'Development Status :: 5 - Production/Stable' "$root/sdk/python/pyproject.toml"

grep -Fq 'fetch-depth: 0' "$root/.github/workflows/security.yml"
grep -Fq 'gitleaks" git --redact --verbose .' "$root/.github/workflows/security.yml"

echo "Stable publication remains manual, protected, draft-first, attested, and OIDC-scoped."

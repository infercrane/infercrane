#!/usr/bin/env sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
release_workflow="$root/.github/workflows/release.yml"
publish_workflow="$root/.github/workflows/publish-stable.yml"

test -f "$root/.release/publication-runbook.md"
test -f "$root/.release/e2e-verification-prompt.md"
test -f "$root/.gitleaks.toml"
test -x "$root/scripts/build-npm-bootstrap-package.sh"
test -x "$root/scripts/build-sdk-release-packages.sh"
test -x "$root/scripts/build-terraform-provider-packages.sh"
test -x "$root/scripts/verify-terraform-release-artifacts.sh"
grep -Fq 'npm-bootstrap-artifact:' "$root/Makefile"
if grep -Eq 'npm (publish|stage publish)' "$root/scripts/build-npm-bootstrap-package.sh"; then
  echo "npm bootstrap builder must not publish" >&2
  exit 1
fi

# Candidate and stable publication must remain explicit, protected actions over
# already built drafts. Tag creation alone is not publication authority.
grep -Fq 'workflow_dispatch:' "$publish_workflow"
grep -Fq 'environment: stable-publication' "$publish_workflow"
grep -Fq '.stable_tag' "$publish_workflow"
grep -Fq '.candidate_tag' "$publish_workflow"
grep -Fq '.two_factor_requirement_enabled' "$publish_workflow"
grep -Fq 'ORG_SECURITY_READ_TOKEN' "$publish_workflow"
grep -Fq '.visibility)" = public' "$publish_workflow"
grep -Fq 'jq -r .isDraft)" = true' "$publish_workflow"
grep -Eq 'pypa/gh-action-pypi-publish@[0-9a-f]{40}' "$publish_workflow"
grep -Fq 'npm publish "$package" --access public' "$publish_workflow"
grep -Fq 'node-version: "24.20.0"' "$publish_workflow"
grep -Fq 'npm install --global npm@12.0.2' "$publish_workflow"
grep -Fq 'HOMEBREW_TAP_TOKEN' "$publish_workflow"
grep -Fq 'gh auth setup-git' "$publish_workflow"
grep -Fq 'diff -qr --exclude=README.md expected-package/package registry-package/package' "$publish_workflow"
grep -Fq 'gh release edit "$RELEASE_TAG" --draft=false --prerelease --latest=false' "$publish_workflow"
grep -Fq 'gh release edit "$RELEASE_TAG" --draft=false --latest' "$publish_workflow"
grep -Fq './scripts/verify-sdk-artifacts.sh publication-assets "$npm_version" verify "$python_version"' "$publish_workflow"
grep -Fq './scripts/verify-terraform-release-artifacts.sh publication-assets "$RELEASE_TAG"' "$publish_workflow"
grep -Fq 'gh attestation verify "$artifact"' "$publish_workflow"
grep -Fq -- '--signer-workflow "$GITHUB_REPOSITORY/.github/workflows/release.yml"' "$publish_workflow"
grep -Fq -- '--source-digest "$(git rev-parse HEAD)"' "$publish_workflow"
grep -Fq -- '--source-ref "refs/tags/$RELEASE_TAG"' "$publish_workflow"
grep -Fq -- '--deny-self-hosted-runners' "$publish_workflow"

# The tag workflow may build and attach artifacts, but it must leave the GitHub
# release as a draft and provide verifiable provenance for archives and images.
grep -Fq 'draft: true' "$root/.goreleaser.yaml"
grep -Fq './scripts/generate-homebrew-formula.sh dist "$GITHUB_REF_NAME"' "$release_workflow"
if grep -A2 -F 'sdk-packages:' "$release_workflow" | grep -Fq 'if:'; then
  echo "release candidates must build SDK artifacts for qualification" >&2
  exit 1
fi
test "$(grep -Ec 'uses: actions/attest@[0-9a-f]{40}' "$release_workflow")" -eq 3
grep -Fq 'subject-name: ghcr.io/infercrane/infercrane' "$release_workflow"
grep -Fq './scripts/build-sdk-release-packages.sh "$RUNNER_TEMP/sdk-dist" "$GITHUB_REF_NAME"' "$release_workflow"
grep -Fq './scripts/build-terraform-provider-packages.sh "$RUNNER_TEMP/sdk-dist" "$GITHUB_REF_NAME"' "$release_workflow"
test -x "$root/scripts/verify-sdk-artifacts.sh"
grep -Fq 'sdk-checksums.txt' "$root/scripts/verify-sdk-artifacts.sh"
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

echo "Candidate and stable publication remain manual, protected, draft-first, attested, and OIDC-scoped."

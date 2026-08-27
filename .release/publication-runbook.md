# Stable publication runbook

This runbook publishes the first stable InferCrane release. Publication is intentionally separate
from tag creation: the tag workflow builds a draft GitHub release and exact GHCR image first;
maintainers verify those immutable outputs before the manual stable-publication workflow makes any
registry or GitHub release public.

## One-time launch prerequisites

Complete every item before creating the stable tag:

1. Resolve GitHub Actions billing and prove `quality`, `security`, and `docs` are green on `main`.
2. Make `infercrane/infercrane` public. Confirm the license, history, issues, discussions, and every
   tracked file are intended for public disclosure before changing visibility.
3. Require two-factor authentication for the `infercrane` GitHub organization. Add a second owner
   and recovery procedure so the release process has no single-account recovery dependency. Create
   a short-lived fine-grained token from an organization owner for the publication preflight, with
   no repository write access or organization write permissions, and store it as the repository
   secret `ORG_SECURITY_READ_TOKEN`. GitHub's repository-scoped `GITHUB_TOKEN` cannot see the
   owner-only `two_factor_requirement_enabled` field. Delete this preflight token after publication.
4. Protect `main` and release tags. Require the quality, security, docs, DCO, and release-safety
   checks; prevent force pushes and deletion; require CODEOWNER review for security/release paths.
   Enable the organization policy that requires actions to be pinned to full commit SHAs after all
   workflows have been migrated.
5. Create a protected GitHub environment named `stable-publication`. Require maintainer approval,
   disallow self-review where the plan supports it, and restrict deployment to stable tags.
6. Create the public `infercrane/homebrew-tap` repository with a protected `main` branch and a
   `Formula/` directory. Store a fine-grained, tap-only write credential as the
   `HOMEBREW_TAP_TOKEN` secret on the `stable-publication` environment.
7. Configure a PyPI pending trusted publisher for project `infercrane`, organization `infercrane`,
   repository `infercrane`, workflow `publish-stable.yml`, and environment
   `stable-publication`. A pending publisher does not reserve the name, so do this immediately before
   the release and verify the name is still available.
8. Ensure the npm `infercrane` organization owns the `@infercrane` scope. npm trusted publishers are
   configured on an existing package; if npm does not offer a pre-publication configuration for the
   scope, reserve `@infercrane/sdk` before the stable release with a legitimate RC package published
   interactively under 2FA. Then configure its GitHub trusted publisher for
   `publish-stable.yml`/`stable-publication`, allow `npm publish`, require 2FA, and disallow legacy
   token publishing. Do not publish the stable version manually. The protected workflow pins npm
   `11.6.4`; npm trusted publishing requires npm `11.5.1` or newer.
9. Confirm the GHCR package inherits public repository visibility and anonymous users can pull the
   exact RC tag. Do not rely on authenticated maintainer pulls as visibility evidence.
10. Increase free disk space to at least the qualification harness minimum, then run the complete
    clean-clone RC qualification. Archive the report by commit and tag.

Terraform Registry publication is not part of v1.0.0. The provider must first move to a separate,
public, lowercase `infercrane/terraform-provider-infercrane` repository with Registry-formatted
documentation, its own semantic tags, platform ZIPs, manifest, checksums, and GPG-signed checksum
file. Keep the current integration marked unpublished until that independent release path passes.

## Build and qualify the release candidate

1. Start from a clean clone with the candidate tag and stable tag absent.
2. Run `make qualify-rc`. Real-cloud gates require their explicit paid-resource approval and exact
   tuple evidence; never reinterpret fixture results as GPU/provider qualification.
3. Create and push only the configured candidate tag. Let `.github/workflows/release.yml` build the
   draft prerelease, archives, checksums, SPDX SBOMs, Homebrew formula, unpublishable SDK test
   artifacts with a separate checksum manifest and attestations, exact-tag GHCR image,
   vulnerability results, and GitHub attestations.
4. Download the draft assets into a clean machine. Run `scripts/verify-release-artifacts.sh` with the
   candidate tag, confirm its Apache-2.0 `LICENSE`, InferCrane `NOTICE`, generated linked-dependency
   license bundle, and upstream attribution notices, install the native archive, run the documented
   quickstart against the isolated fixture stack, test the Python wheel and npm tarball, and install
   the generated Homebrew formula.
5. Verify the image by immutable digest and verify its GitHub attestation. Run the container as the
   documented non-root user and execute the production Compose configuration checks.
6. Record every untested real provider, GPU, Kubernetes, upgrade, and paid path. A green local gate
   cannot close a real-infrastructure evidence row.

## Promote the exact candidate commit

1. Run `make release-tag-check` and `make release-tag-stable` in the qualified candidate checkout.
   The stable-tag guard requires the stable tag to point to the exact candidate commit.
2. Push only `v1.0.0`. Do not rebuild from a different commit and do not move either tag.
3. Wait for the stable tag workflow. Confirm all jobs pass and the GitHub release remains a draft.
4. Download and independently verify the stable archives, licenses/notices, checksums, SBOMs,
   formula, wheel, npm tarball, SDK checksum manifest, image digest, vulnerability scan, and
   attestations. Confirm all embedded/package versions are exactly `1.0.0` and all release assets
   originate from the stable tag commit.

## Publish

1. In GitHub Actions, run `publish stable release` with input `v1.0.0`.
2. Approve the protected `stable-publication` environment only after reviewing the preflight output.
   The workflow refuses a private repository, disabled organization 2FA, an unprotected environment,
   an RC/mismatched tag, a non-draft release, incomplete artifacts, checksum failures, or artifact
   attestations that do not originate from the exact release workflow, tag ref, and commit.
3. The workflow publishes the exact wheel to PyPI with OIDC, the exact npm tarball with OIDC, updates
   the Homebrew tap, and publishes the already-verified GitHub draft last. If any registry step
   fails, the GitHub release stays draft. Do not replace an artifact at the same version; diagnose and
   release a new version when registry content has already become public.
4. Make the exact GHCR package tag public if repository inheritance did not do so. Verify an
   anonymous pull by digest.

## Post-publication smoke and documentation flip

From a clean machine with no repository checkout:

```bash
brew install infercrane/tap/infercrane
infercrane version
python -m pip install 'infercrane==1.0.0'
npm install '@infercrane/sdk@1.0.0'
docker pull ghcr.io/infercrane/infercrane:v1.0.0
gh attestation verify --repo infercrane/infercrane \
  oci://ghcr.io/infercrane/infercrane:v1.0.0
```

Then update the README and quickstart install sections in one release-follow-up change: replace the
authenticated-clone preview path with Homebrew/archive/PyPI/npm/GHCR commands, remove only the
publication-pending caveats that are now true, and keep the Terraform Registry and hosted-console
private-preview caveats. Run the documented commands and docs checks before merging.

Finally, verify package/release pages, checksum downloads, attestations, anonymous GHCR pulls,
Homebrew installation on Apple Silicon and Intel macOS, PyPI installation on every supported Python
minor, npm installation on supported Node versions, the security-reporting path, and release notes.
Announce only after those checks pass.

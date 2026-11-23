---
title: InferCrane v2.0.1
description: First stable release of InferCrane, with the CLI, SDKs, Terraform provider, and control-plane image.
sidebarTitle: v2.0.1
---

# InferCrane v2.0.1

`v2.0.1` is the first stable release of InferCrane. It is the supported line for security fixes.

The `v1.0.0-rc.*` tags were public betas. The product changed enough during those betas, in the
CLI, the SDKs, and the control API, that the first stable release takes a new major version
instead of `1.0.0`. No `1.0.0` package was ever published.

The `v2.0.0` tag exists but was never published: its build named the CLI archives after the
candidate tag on the same commit, and release tags cannot be moved or deleted. `v2.0.1` is the same
release, built from a commit that carries only the stable tag.

## Install

```bash
brew install infercrane/tap/infercrane
python -m pip install 'infercrane==2.0.1'
npm install '@infercrane/sdk@2.0.1'
```

If you installed a `v1.0.0-rc.*` beta with Homebrew, run `brew upgrade infercrane`.

The [GitHub release](https://github.com/infercrane/infercrane/releases/tag/v2.0.1) provides native
CLI archives, checksums, SPDX SBOMs, attestations, a Homebrew formula, SDK packages, and Terraform
provider ZIPs for macOS and Linux on amd64 and arm64.

## Upgrading from a beta

- Use SDKs and the CLI from the same release as the control plane.
- The npm `rc` tag stays on the last beta. `npm install @infercrane/sdk` now resolves to `2.0.1`.
- Python installs no longer need `--pre`.
- The GitHub Action defaults to `v2.0.1`.

## What is in it

Everything in [`v1.0.0-rc.4`](/release-notes-v1.0.0-rc.4), plus:

- **Failover accounting.** A request that fails over now counts against the deployment revision it
  left, so failover rates and error budgets point at the revision that failed.
- **Hosted model APIs stay up when one product lapses.** A product whose evidence has lapsed is
  withheld on its own; other hosted routes keep serving after a restart.
- **Whole agent gateway streams.** The agent gateway keeps a streamed response whole when the client
  reads the body late.
- **A patched control-plane image.** The image moves to Debian 13 and picks up fixed OpenSSL,
  Python `cryptography`, Pillow, and kubectl versions.

## Qualification

This release was built and checked by the release workflow and its vulnerability scans. It was not
qualified on paid cloud or GPU infrastructure. Review
[compatibility and qualification](https://docs.infercrane.com/compatibility) and the
[feature qualification matrix](https://docs.infercrane.com/testing/feature-qualification-matrix) for
the evidence attached to a specific model, runtime, accelerator, and provider combination.

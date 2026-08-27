---
title: InferCrane v1.0.0-rc.1
description: Public beta release of the InferCrane inference operations platform.
sidebarTitle: v1.0.0-rc.1 public beta
---

# InferCrane v1.0.0-rc.1 — Public Beta

`v1.0.0-rc.1` is the first public beta of InferCrane, the open-source inference operations platform
for deploying new model serving or adopting inference that a team already runs.

## Install

```bash
brew install infercrane/tap/infercrane
python -m pip install 'infercrane==1.0.0rc1'
npm install '@infercrane/sdk@1.0.0-rc.1'
```

The [GitHub prerelease](https://github.com/infercrane/infercrane/releases/tag/v1.0.0-rc.1)
provides native CLI archives, checksums, SPDX SBOMs, attestations, a Homebrew formula, SDK packages,
and Terraform provider ZIPs for macOS and Linux on amd64 and arm64.

## What is included

- model-to-endpoint workload initialization, validation, planning, deployment, and durable operation
  inspection;
- adoption of existing vLLM, SGLang, LiteLLM, and OpenAI-compatible endpoints without transferring
  provider-resource ownership;
- one stable OpenAI-compatible application endpoint across revisions and provider bindings;
- request inspection, Doctor findings, events, sourced cost evidence, capacity recommendations,
  autoscaling decisions, and durable recovery workflows;
- benchmark, replay, quality, reliability, and cost evidence for deterministic Release Guard
  promotion, rejection, monitoring, and rollback;
- CLI, control API, Python and TypeScript SDKs, Terraform provider, GitHub Action, terminal workspace,
  and a separately deployed browser console;
- Apache-2.0 licensing, multi-architecture artifacts, checksums, SBOMs, provenance, and
  GitHub attestations.

## Distribution status

The CLI, SDK prereleases, and Terraform GitHub artifacts are public. The GHCR image is release-gated
separately and is not part of this candidate until its vulnerability scan passes. Terraform Registry
installation remains separate from the downloadable provider artifacts. The hosted InferCrane Cloud
experience remains waitlist access; it does not limit use of the open-source release.

Software behavior and exact infrastructure qualification remain separate. Review
[compatibility and qualification](https://docs.infercrane.com/compatibility) and the
[feature qualification matrix](https://docs.infercrane.com/testing/feature-qualification-matrix)
for the evidence attached to a specific model, runtime, accelerator, and provider combination.

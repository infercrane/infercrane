---
title: InferCrane v2.0.2
description: Publishes the InferCrane control-plane container image for amd64 and arm64.
sidebarTitle: v2.0.2
---

# InferCrane v2.0.2

`v2.0.2` publishes the control-plane container image that [`v2.0.1`](/release-notes-v2.0.1) held
back. The CLI, SDKs, and Terraform provider are unchanged apart from their version.

## Install

```bash
brew upgrade infercrane
python -m pip install 'infercrane==2.0.2'
npm install '@infercrane/sdk@2.0.2'
docker pull ghcr.io/infercrane/infercrane:v2.0.2
```

## What changed

- **Container image.** The `v2.0.1` amd64 image failed the release vulnerability scan: the Google
  Cloud CLI's x86_64 archive bundles its own Python. The image now removes that interpreter and runs
  `gcloud` on the image's Python, and installs the AWS CLI from its source release on the same
  Python instead of AWS's prebuilt bundle, which also carries its own interpreter.

## Qualification

This release was built and checked by the release workflow and its vulnerability scans. It was not
qualified on paid cloud or GPU infrastructure. Review
[compatibility and qualification](https://docs.infercrane.com/compatibility) for the evidence
attached to a specific model, runtime, accelerator, and provider combination.

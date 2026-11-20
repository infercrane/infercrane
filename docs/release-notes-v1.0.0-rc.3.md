---
title: InferCrane v1.0.0-rc.3
description: Second public beta of InferCrane, with optimization runs, any Hugging Face model, AMD Instinct, GPU profiling, and a simpler CLI.
sidebarTitle: v1.0.0-rc.3 public beta
---

# InferCrane v1.0.0-rc.3 — Public Beta

`v1.0.0-rc.3` is the second public beta. It brings everything merged since
[`v1.0.0-rc.1`](/release-notes-v1.0.0-rc.1) to the released CLI, SDKs, and Terraform provider.

The `v1.0.0-rc.2` tag was never published: its build failed the release vulnerability scan on the
Go 1.26.7 standard library. This candidate is built with Go 1.26.11.

## Install

```bash
brew install infercrane/tap/infercrane
python -m pip install \
  'https://github.com/infercrane/infercrane/releases/download/v1.0.0-rc.3/infercrane-1.0.0rc3-py3-none-any.whl'
npm install '@infercrane/sdk@1.0.0-rc.3'
```

If you installed `v1.0.0-rc.1` with Homebrew, run `brew upgrade infercrane`.

The [GitHub prerelease](https://github.com/infercrane/infercrane/releases/tag/v1.0.0-rc.3)
provides native CLI archives, checksums, SPDX SBOMs, attestations, a Homebrew formula, SDK packages,
and Terraform provider ZIPs for macOS and Linux on amd64 and arm64.

## What is new

- **Optimization runs.** `infercrane optimize` measures setups side by side on the same traffic,
  checks answer quality, waits for your spend approval, and rolls out a winner only when you say so.
  `optimize watch` follows a run live and `optimize report` shows what it changed in cost, speed, and
  answer quality. A run resumes after losing a measured server.
- **Any open Hugging Face model.** Deploy and optimize public models beyond the reviewed catalog;
  `infercrane models check OWNER/NAME` reads a model's config and reports what it needs. The
  checkpoint catalog now spans several publishers, including quantized builds.
- **AMD Instinct GPUs.** Deploy and optimize on AMD Instinct accelerators, with reviewed engine
  switches and GPU metrics from AMD's exporter.
- **GPU profiling.** `infercrane profile` captures and analyzes a serving replica's GPU profile,
  shows where GPU time goes, and compares captures before and after a change.
- **Move off a closed API.** `infercrane migrate` captures your closed API's traffic, measures an
  open model you run against it on cost and answer quality, and switches traffic in stages with a
  rollback at each step.
- **Bring a model you trained elsewhere.** Serve and optimize a model you fine-tuned in your own
  training run.
- **Connect servers on your own machine.** `infercrane agent run` connects vLLM, SGLang, llama.cpp,
  Ollama, and LM Studio servers through an outbound-only agent.
- **Sign in from the browser.** `infercrane login` signs in through the dashboard and stores the key
  in the OS keychain; results link back to the dashboard and `infercrane open` jumps there.
- **More clouds.** Optimization runs on SkyPilot clouds, starting with Vast.ai, and RunPod uses its
  REST API v2.
- **A simpler CLI.** `infercrane` opens on the everyday commands in five sections, with every other
  command behind `infercrane help --all`. Unknown commands suggest the closest one, errors say what
  to do next, and human output is styled like the dashboard in a terminal while scripts and
  `--output json` stay plain.

## Distribution status

Distribution is unchanged from `v1.0.0-rc.1`: the CLI, SDK prereleases, and Terraform GitHub
artifacts are public, and the GHCR image is release-gated separately until its vulnerability scan
passes.

This candidate was built and checked by the release workflow. It was not qualified on paid cloud or
GPU infrastructure; review [compatibility and qualification](https://docs.infercrane.com/compatibility)
and the [feature qualification matrix](https://docs.infercrane.com/testing/feature-qualification-matrix)
for the evidence attached to a specific model, runtime, accelerator, and provider combination.

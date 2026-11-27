---
title: InferCrane v2.1.0
description: Optimization techniques with a shared evidence contract, runs on an agent's endpoint, a signed proof report, a proof gate for pull requests, and a Grafana pack.
sidebarTitle: v2.1.0
---

# InferCrane v2.1.0

`v2.1.0` adds optimization techniques that share one evidence contract, optimization runs on an
endpoint an InferCrane agent serves, a signed proof report, a proof gate for pull requests, and a
Grafana and Prometheus pack.

## Install

```bash
brew upgrade infercrane
python -m pip install 'infercrane==2.1.0'
npm install '@infercrane/sdk@2.1.0'
docker pull ghcr.io/infercrane/infercrane:v2.1.0
```

## What changed

### Optimization

- **Technique contract.** Every optimization technique declares the evidence its results must
  carry. Runs and reports show each setup's technique, result and claim, and a gain is shown only
  after a fresh machine confirms it. Read the [technique contract](/features/technique-contract).
- **Plans from the current setup.** The planner starts from the endpoint's current setup and the
  load it holds now, ranks speculation by the cache room the engine reports, and orders its choices
  by similar measured results. Read [measured results](/features/results-store).
- **Measurements you can compare.** A run measures how much its machines differ and uses that in
  every decision. A window that is not valid on fresh servers is measured once more, and otherwise
  the gain stays unknown. Read [optimization](/features/optimization).
- **Answer quality on a task set.** Answers can be scored item by item on a task set. A setup that
  changes answers must show they held within 1 point, and a confirmed setup's answers are pooled
  over both pairs of servers. Read [semantic quality](/features/semantic-quality).
- **New techniques.** Published FP8 checkpoints are measured beside FP8 at load,
  [kernels tuned for your shapes](/features/kernel-tuning),
  [speculation that changes with load](/features/adaptive-speculation), and
  [drafts trained on your traffic](/features/trained-drafts). Each is judged by the same rules.
- **Runtimes.** vLLM 0.34.0 and SGLang 0.5.23 defaults are recorded from their source. AMD
  Instinct accelerators are listed and marked as not qualified.

### Agent

- **Optimization runs on an agent's endpoint.** The agent measures each setup in your own cluster,
  and the control plane ranks, confirms and guards the setups by the same rules as its own runs. The
  winner rolls out guarded on the agent-managed endpoint. Read
  [the agent](/architecture/agent#optimization-runs-on-an-agent%E2%80%99s-endpoint).

### Proof and integrations

- **Proof report.** One signed before-and-after document for your finished runs, from the API or
  `infercrane report proof`, and checked with `infercrane report proof verify`. Read [the proof report](/features/proof-report).
- **Proof gate.** The GitHub Action's `proof-gate` mode runs InferCrane's proof when a pull request
  changes a model, an engine setting or a serving setup, comments the verdict, and fails the check
  on a regression. `infercrane optimize compare` and `infercrane optimize gate` do the same from the
  command line. Read [GitHub Actions](/integrations/github-actions).
- **Grafana and Prometheus.** A dashboard, scrape configuration and alert rules for Release Guard
  verdicts, canary traffic, rollbacks and optimization results. Read
  [Grafana and Prometheus](/integrations/grafana).

### Fixes

- A control plane named in `INFERCRANE_URL` no longer inherits the dashboard saved for another
  context. Read [Sign in the CLI](/features/cli-login).
- The benchmark index states why the September 2026 H200 screen is no longer cited, with the
  receipts of its re-run. Read [benchmarks](/benchmarks).

## Upgrade

The control plane runs new forward-only database migrations on start. Back up the database before
you upgrade, as for every release.

## Qualification

This release passed the repository's full local gate and its storage suite before tagging, and is
built and scanned by the release workflow. Measured results for a specific model, runtime,
accelerator and workload are on the evidence pages they link to; nothing here generalises them.
Review [compatibility and qualification](https://docs.infercrane.com/compatibility).

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/infercrane-logo-dark.svg">
    <img alt="InferCrane" src="docs/assets/infercrane-logo-light.svg" width="420">
  </picture>
</p>

<p align="center">
  <strong>Open models. Better execution.</strong>
</p>

<p align="center">
  Write your application once. Run it on qualified open-weight Model APIs, your own cloud,<br>
  or a measured serving configuration—without changing its OpenAI-compatible contract.
</p>

<p align="center">
  <a href="LICENSE"><img alt="License: Apache-2.0" src="https://img.shields.io/badge/license-Apache--2.0-111111.svg"></a>
  <a href="https://github.com/infercrane/infercrane/actions/workflows/quality.yml"><img alt="Quality checks" src="https://github.com/infercrane/infercrane/actions/workflows/quality.yml/badge.svg?branch=main"></a>
  <a href="https://docs.infercrane.com"><img alt="Documentation" src="https://img.shields.io/badge/docs-infercrane.com-235ee7.svg"></a>
</p>

<p align="center">
  <a href="https://console.infercrane.com"><strong>Open the console</strong></a>
  · <a href="#choose-how-to-start">Choose how to start</a>
  · <a href="#install">Install the CLI</a>
  · <a href="https://docs.infercrane.com/quickstart">Documentation</a>
</p>

<p align="center">
  <img alt="InferCrane plans a model deployment and persists its durable operation" src="docs/images/product/github-product-demo.gif" width="960">
  <br>
  <sub>Model APIs · BYOC deployments · workload optimization · agent sandboxes</sub>
</p>

InferCrane is the open-source, evidence-gated release system for open-weight inference. Deploy or
adopt vLLM and SGLang, measure serving changes against your workload, and promote only proven
winners behind one stable API.

```text
Applications and agents
          │
          ▼
One OpenAI-compatible InferCrane endpoint
          │
          ├── activate a qualified Model API when capacity is published
          ├── deploy open weights into your cloud or Kubernetes
          ├── optimize an existing workload
          └── run agent tasks in a persistent sandbox
          │
          ▼
vLLM · SGLang · custom OCI
AWS · GCP · Kubernetes · RunPod · existing infrastructure
```

## Choose how to start

| What you need | Start here | What InferCrane does |
|---|---|---|
| Use managed capacity | [Model APIs](https://console.infercrane.com/model-apis) | Shows only currently qualified offers; an empty catalog never creates a false availability promise. |
| Deploy open weights | [Deploy a model](https://console.infercrane.com/build?mode=deploy) | Resolve the model, workload, runtime, accelerator, provider, scaling policy, and cost boundary before creating compute. |
| Improve a workload | [Optimize](https://console.infercrane.com/optimization) | Profile traffic, search bounded candidates, measure on exact hardware, and keep only qualified wins. |
| Isolate an agent | [Sandboxes](https://console.infercrane.com/sandboxes) | Persistent workspace, streaming commands, files, private previews, sleep/resume, and scoped model access. |
| Keep existing inference | [Connect an endpoint](https://console.infercrane.com/onboarding/connect) | Adopt a compatible endpoint without transferring infrastructure ownership. |

Nothing billable starts from a recommendation. Deployment and optimization stop at a review
boundary; provider mutation or benchmark spend requires explicit approval.

### Measured optimization, not a preset

For `Qwen/Qwen3.8-27B-FP8` on one H200, InferCrane selected this measured winner:

| Exact workload | Control | Winner | Change |
|---|---:|---:|---:|
| Aggregate output throughput, concurrency 12 | 810.4 tok/s | 1,393.6 tok/s | **1.72×** |
| Median per-request output speed | 76.1 tok/s | 141.0 tok/s | **+85%** |
| Median TTFT | 1,591 ms | 739 ms | **−54%** |

InferCrane also retained candidates that failed quality, latency, cost, or reproducibility gates
instead of turning an attractive microbenchmark into a release claim.

Those numbers belong only to the recorded 4K-input/512-output workload and exact pinned hardware
and runtime tuple. See the [decision record](docs/testing/qwen38-openrouter-launch-decision-2026-09-23.md)
and [raw evidence](docs/testing/evidence/qwen38-public-decode-saturation-modal-2026-09-23T044530Z.json),
or browse the compact [benchmark index](BENCHMARKS.md).

## Install

Install the `v1.0.0-rc.1` public beta CLI with Homebrew or use a matching release artifact:

```bash
brew install infercrane/tap/infercrane
python -m pip install \
  'https://github.com/infercrane/infercrane/releases/download/v1.0.0-rc.1/infercrane-1.0.0rc1-py3-none-any.whl'
npm install '@infercrane/sdk@1.0.0-rc.1'
```

Release archives and Terraform provider binaries are available from the
[`v1.0.0-rc.1` prerelease](https://github.com/infercrane/infercrane/releases/tag/v1.0.0-rc.1).
To run the complete GPU-free product proof without creating cloud resources:

```bash
git clone https://github.com/infercrane/infercrane.git
cd infercrane
make demo
```

The proof connects an OpenAI-compatible worker, sends and inspects a request, creates an isolated
candidate, records a deterministic Release Guard rejection, verifies that production traffic did
not move, and removes its disposable stack.

`main` contains work scheduled for the next prerelease. The hosted documentation follows `main`;
use the release notes and assets together when you need a versioned installation.

## Why InferCrane

Starting a model server can be one command. Operating it while models, runtimes, accelerators,
providers, scaling policies, and revisions change is the longer-lived problem.

- **Start where you are:** deploy vLLM, SGLang, or a custom OCI workload—or adopt a compatible
  endpoint you already operate—across AWS, GCP, Kubernetes, and RunPod.
- **Use qualified managed capacity when available:** offers appear only after their supplier,
  pricing, protocol, and capacity boundaries are installed; otherwise the catalog fails closed.
- **Keep the application stable:** route infrastructure and revision changes behind one
  OpenAI-compatible endpoint instead of teaching every application about the serving topology.
- **Give agents an isolated workspace:** run commands, move files, inspect a private preview, and
  sleep or resume the computer without exposing a long-lived model credential to the guest.
- **Make long operations durable:** persist intent before provider mutation, survive CLI and worker
  disconnects, and reattach to the same operation rather than guessing what completed.
- **Prove changes before traffic moves:** evaluate isolated candidates with benchmark, replay,
  quality, reliability, and sourced cost evidence. Missing evidence is never silently treated as a
  successful release.

## The release loop

A candidate does not receive production traffic merely because its health endpoint returns `200`.
InferCrane keeps the active revision serving while the candidate is measured and records one of
three explicit Release Guard decisions: `ACCEPT`, `REJECT`, or `WAIT`. An `ACCEPT` decision permits
an explicit promotion; it does not move traffic by itself.

```text
Stable endpoint ───────────────────────────────▶ active revision
                                                     ▲
                                                     │ explicit promotion after ACCEPT
new serving plan ▶ isolated candidate ▶ evidence ▶ ACCEPT / REJECT / WAIT
                                                     └─ reject or wait: active unchanged
```

The evidence and decision remain inspectable after the operation finishes. If this is an
operational gap your team recognizes, [star InferCrane](https://github.com/infercrane/infercrane)
and tell us which model, runtime, accelerator, and provider tuple should be qualified next.

## Why not just…

- **Raw vLLM and Kubernetes:** the serving engine and manifests do not by themselves provide an
  evidence-gated release lifecycle. InferCrane adds deterministic promotion, rejection, rollback,
  and a persisted record of what changed.
- **LiteLLM:** it is an excellent routing layer. InferCrane additionally owns deployment lifecycle,
  evidence-gated promotion, and rollback; it can also connect to an existing LiteLLM endpoint
  without taking infrastructure ownership.
- **A managed inference platform:** it is often the right choice when a team wants the provider to
  operate its infrastructure. InferCrane is for teams that want the control plane, capacity, and
  billing boundary to remain in infrastructure they own.
- **Scripts and CI:** scripts can deploy a revision. InferCrane standardizes the durable
  reject/promote/rollback decision and the evidence attached to it.

See the [detailed comparison](docs/compare.mdx), including the boundaries InferCrane does not own.

The local proof needs no GPU or cloud account. Real-provider support remains exact-tuple qualified;
InferCrane reports missing model/runtime/hardware evidence as unknown instead of turning it into a
compatibility claim.

<details>
<summary><strong>See the system boundary</strong></summary>

```text
Applications and agents
          │
          ▼
Stable OpenAI-compatible endpoint
          │
          ▼
InferCrane: route · observe · optimize · release · recover
          │
          ├── deploy new inference
          ├── adopt an existing workload
          └── govern a model API or gateway
          │
          ▼
vLLM · SGLang · custom OCI · experimental Dynamo
AWS · GCP · Kubernetes · RunPod · existing infrastructure
```

</details>

## Start with the job you need to do

| Goal | InferCrane workflow |
|---|---|
| Put a model into production | Initialize a workload, review the serving plan, deploy, then call its stable endpoint. |
| Adopt existing inference | Connect vLLM, SGLang, LiteLLM, or another compatible endpoint without transferring lifecycle ownership. |
| Ship a safer revision | Benchmark and replay an isolated candidate, attach quality evidence, then let Release Guard promote or reject it. |
| Understand production failures | Trace queue wait, attempts, runtime, revision, latency, saturation, and durable operations without storing prompt content. |
| Optimize performance and cost | Propose serving configurations, measure comparable candidates on real hardware, and persist only qualified evidence. |
| Survive infrastructure delays | Submit idempotent durable operations that continue after the CLI or control plane process disconnects. |

<p align="center">
  <img alt="Connect an existing inference endpoint with InferCrane" src="docs/images/showcase/connect-existing.gif" width="820">
</p>

Local fixtures prove application and lifecycle behavior. They do not prove GPU performance, cloud
capacity, runtime compatibility, or model quality. InferCrane keeps those evidence boundaries
explicit.

## From model to endpoint

Start with a curated recipe:

```bash
infercrane workload init ./support --recipe qwen3-8b
cd support
infercrane workload plan
infercrane workload deploy --wait
```

Or bring another compatible immutable model identity:

```bash
infercrane workload init ./agent-model --model mistralai/Mistral-7B-Instruct-v0.3
cd agent-model
infercrane workload plan
infercrane workload deploy --wait
```

Recipes are reproducible configuration starting points, not benchmark claims or an allowlist.
Evidence remains bound to the exact model commit, runtime, accelerator, provider, cache state, and
workload.

Already operating a workload? Connect it first:

```bash
infercrane connect https://vllm.internal/v1 --as support-production
infercrane doctor support-production
infercrane observe support-production
```

InferCrane can observe an existing endpoint before it manages traffic or infrastructure. Provider
credentials and request content do not enter the browser console.

## Optimize, then prove

InferCrane separates a modeled proposal from measured and qualified evidence:

```bash
infercrane optimize propose llama-3.1-8b-instruct \
  --provider aws \
  --region eu-central-1 \
  --gpu L40S \
  --objective interactive \
  --write-dir .infercrane/candidates
```

Uncataloged open-weight models enter through an untuned, fail-closed baseline:

```bash
infercrane optimize propose OWNER/MODEL \
  --model-revision IMMUTABLE_40_TO_64_HEX_COMMIT \
  --provider runpod-pods --gpu H100 --gpu-count 1 \
  --runtimes vllm,sglang --objective throughput
```

The generic path makes no memory-fit, license, quality, kernel, or performance claim. Measure an
isolated candidate with repeated AIPerf runs before Release Guard can move traffic.

```text
model + hardware + workload + SLO + cost target
                       │
                       ▼
              candidate serving plans
                       │
                       ▼
          AIPerf + replay + quality evidence
                       │
                       ▼
            performance · errors · cost
                       │
                 ┌─────┴─────┐
                 ▼           ▼
              promote      reject
```

InferCrane composes replaceable execution technology instead of rebuilding it. vLLM, SGLang, and
custom OCI are current execution paths. Dynamo is experimental. TensorRT-LLM, LMCache, NIXL, and
external optimizers remain capability boundaries until their exact adapters and hardware tuples are
qualified. InferCrane owns serving-plan identity, durable operations, comparable evidence, routing
policy, promotion, and rollback.

## Production properties

- **Stable endpoint identity:** applications do not change when the serving plan changes.
- **Bounded overload:** admission limits, explicit `429` and `Retry-After`, one end-to-end deadline,
  and bounded retries prevent unlimited queue growth.
- **Request-path isolation:** gateways route from immutable in-memory snapshots and never query
  PostgreSQL on the inference request path.
- **Durable operations:** deployment, scaling, deletion, and release work is idempotent,
  restart-safe, cancellable, and inspectable.
- **Release evidence:** benchmark, replay, quality, reliability, and sourced cost evidence can block
  a candidate before traffic moves.
- **Content-free operations:** request evidence records operational metadata without persisting
  prompts or model outputs.
- **Explicit ownership:** existing runtimes, gateways, training systems, sandboxes, and clouds stay
  replaceable behind versioned contracts.

Read the [architecture](https://docs.infercrane.com/architecture/system),
[system invariants](docs/architecture/invariants.md), and
[data flows](docs/architecture/data-flows.md) for the complete design.

## Interfaces

| Interface | Status and purpose |
|---|---|
| CLI and control API | Primary deployment, operation, evidence, and administration interfaces. |
| OpenAI-compatible gateway | Capability-gated Chat, Completions, Embeddings, Responses, and online batch paths. The pinned vLLM profile currently qualifies Chat plus model-compatible Completions and Embeddings; unsupported capabilities fail before upstream transmission. |
| Python and TypeScript SDKs | Public beta release artifacts: the Python wheel attached to `v1.0.0-rc.1` and `@infercrane/sdk@1.0.0-rc.1`. Generated from the checked OpenAPI contract. |
| Terraform provider | Logical deployment lifecycle with guarded updates and import. Release binaries and source are public; Registry publication is pending. |
| Terminal workspace | Fleet attention, evidence inspection, and state-valid guarded actions. |
| Browser console | Company-operated service for Model APIs, BYOC planning, optimization campaigns, usage, access, and workspace-scoped sandboxes. Its source is not part of the Apache-2.0 core. |
| Read-only MCP server | Closed-world operational inspection without deployment, scaling, promotion, deletion, budget, or secret tools. |

## Qualification status

InferCrane `v1.0.0-rc.1` is the first public beta. The stable `v1.0.0` release will promote the exact
qualified product contract after the prerelease cycle; no earlier development tag should be treated
as a supported public release.

- Local race, PostgreSQL, fault-injection, Docker, Kind, KWOK, package, migration, security, and
  documentation gates are automated.
- AWS has exact-tuple real GPU evidence for vLLM, SGLang, custom OCI, model identity, requests,
  bounded benchmarks, durable deletion, and final zero managed-resource inventory.
- GCP GPU, real GPU Kubernetes/KServe, additional model/runtime/GPU tuples, and several distributed
  optimization paths still require separate real-infrastructure evidence.
- No benchmark is generalized beyond the exact tuple and workload that produced it.

See the authoritative [compatibility and qualification policy](docs/compatibility.md),
[AWS evidence](docs/testing/aws-real-evidence.md), and
[feature qualification matrix](docs/testing/feature-qualification-matrix.md) before relying on an
exact provider, runtime, model, or accelerator combination.

## Deploy, observe, improve

InferCrane selects a reviewed compatible starting configuration for a declared
workload objective, deploys it behind a stable endpoint, and uses content-free
production monitoring to decide what evidence to collect next. Request-level
signals can narrow the bottleneck to queueing, prefill, or decode; only an exact
target-GPU profile can open the custom-kernel path.

Kernel opportunities search the pinned runtime, FlashInfer, CUTLASS/CuTe, and
Triton before proposing handwritten CUDA. Candidate code is built outside the
API process in an isolated worker and must pass correctness, exact-GPU, AIPerf,
quality, cost, and Release Guard gates before promotion. See
[`docs/architecture/model-agnostic-optimization-engine.md`](docs/architecture/model-agnostic-optimization-engine.md).

## Documentation

- [Five-minute quickstart](https://docs.infercrane.com/quickstart)
- [Product concepts](https://docs.infercrane.com/concepts)
- [Build new inference](https://docs.infercrane.com/showcase/build-inference)
- [Connect existing inference](https://docs.infercrane.com/showcase/connect-existing)
- [Safe releases](https://docs.infercrane.com/showcase/safe-rollouts)
- [Provider setup](https://docs.infercrane.com/provider-setup)
- [Production operations](https://docs.infercrane.com/production)
- [Python SDK](https://docs.infercrane.com/integrations/python)
- [TypeScript SDK](https://docs.infercrane.com/integrations/typescript)
- [Terraform provider](https://docs.infercrane.com/integrations/terraform)
- [Security](SECURITY.md)
- [Support](SUPPORT.md)

Mintlify generates [`llms.txt`](https://docs.infercrane.com/llms.txt) and
[`llms-full.txt`](https://docs.infercrane.com/llms-full.txt) from the public documentation. Every
public documentation page is also available as Markdown by appending `.md` to its URL.

## Contributing

Contributions are welcome. Start with [CONTRIBUTING.md](CONTRIBUTING.md), follow the
[Code of Conduct](CODE_OF_CONDUCT.md), and sign commits with `git commit -s`. Changes must include
tests and relevant documentation. Durable architecture, security, storage, and dependency changes
must update their authoritative public documentation.

## Security and support

Never disclose credentials, prompts, model responses, private endpoints, or suspected
vulnerabilities in a public issue. Use the private reporting process in [SECURITY.md](SECURITY.md).
Questions and reproducible defects follow [SUPPORT.md](SUPPORT.md).

## License

InferCrane Community is available under the [Apache License 2.0](LICENSE). Hosted and enterprise products
are separate distributions and are not licensed by this repository. Release archives also include
[third-party notices](THIRD_PARTY_NOTICES.md) and a release-specific SPDX SBOM. The InferCrane name
and crane logo remain subject to the [trademark policy](TRADEMARKS.md).

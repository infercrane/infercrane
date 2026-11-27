<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/infercrane-logo-dark.svg">
    <img alt="InferCrane" src="docs/assets/infercrane-logo-light.svg" width="420">
  </picture>
</p>

<p align="center">
  <strong>Same model. Less compute. Proven on your own traffic.</strong>
</p>

<p align="center">
  InferCrane makes the open-weight models you already serve faster on your own GPUs, and proves<br>
  every change on your traffic before your users see it. It refuses speedups that change your answers.
</p>

<p align="center">
  <a href="LICENSE"><img alt="License: Apache-2.0" src="https://img.shields.io/badge/license-Apache--2.0-111111.svg"></a>
  <a href="https://github.com/infercrane/infercrane/releases/latest"><img alt="Latest release" src="https://img.shields.io/github/v/release/infercrane/infercrane?color=235ee7"></a>
  <a href="https://github.com/infercrane/infercrane/actions/workflows/quality.yml"><img alt="Quality checks" src="https://github.com/infercrane/infercrane/actions/workflows/quality.yml/badge.svg?branch=main"></a>
  <a href="https://docs.infercrane.com"><img alt="Documentation" src="https://img.shields.io/badge/docs-infercrane.com-235ee7.svg"></a>
</p>

<p align="center">
  <a href="#results-and-receipts"><strong>Results and receipts</strong></a>
  · <a href="#install">Install</a>
  · <a href="docs/integrations/kubernetes-install.mdx">Run it on your GPU cluster</a>
  · <a href="https://docs.infercrane.com/quickstart">Documentation</a>
</p>

<p align="center">
  <img alt="InferCrane plans a model deployment and persists its durable operation" src="docs/images/product/github-product-demo.gif" width="960">
  <br>
  <sub>Measure side by side · confirm on fresh servers · check the answers · roll out with a fallback</sub>
</p>

## Every change proven before it ships

Every inference vendor says it's fast. InferCrane shows it on your traffic. It runs beside the vLLM
or SGLang servers you already have, on a laptop, a Kubernetes cluster or a cloud account, and every
change goes through the same four steps:

1. **Measured against your current setup.** Setups take turns under the same traffic, and a copy of
   your current setup is measured the same way, so the run knows its own noise instead of assuming
   it. When setups run on separate servers, every comparison also carries an allowance for the
   difference between machines.
2. **Again on fresh servers.** A gain counts only when it holds on machines that didn't produce it.
3. **Answers checked.** The candidate answers a graded task set. A setup that changes answers must
   hold within 1 point, or it is refused, however fast it is.
4. **Rolled out with a fallback.** A second server takes traffic first, your current server stays
   up as the fallback, and traffic switches back automatically if the guard sees a regression.

A result the evidence doesn't show is reported as "not shown", never as a win. Prompts and answers
stay in your account.

InferCrane doesn't replace your engine. It runs [vLLM](https://github.com/vllm-project/vllm) and
[SGLang](https://github.com/sgl-project/sglang) as they are, drives load with
[AIPerf](https://github.com/ai-dynamo/aiperf), and trains drafts with
[speculators](https://github.com/vllm-project/speculators). See [how a run measures and confirms a gain](docs/features/optimization.mdx)
and [how answers are checked](docs/features/semantic-quality.mdx).

## Results and receipts

Each number links to its receipt: the rules written before the run, the machines, and every
server's results.

| Result | Measured on | Status |
|---|---|---|
| A speculation draft trained on chat traffic served **54% more output** at one request at a time and **13% more** at 32 at once, with answers held within 1 point. The published community draft served 41% and 6% more. ([receipt](docs/features/trained-drafts.mdx)) | Qwen3-8B with FP8 weights on one NVIDIA H100, confirmed on a second H100; vLLM 0.34.0; 26 Nov 2026 | Lab-proven, coming to InferCrane. The trainer image and prompt capture aren't built yet |
| On an NVIDIA L4 at 32 requests at once, both drafts were **slower** than no speculation (−3% and −7%), so speculation stays off there. ([receipt](docs/features/trained-drafts.mdx)) | Qwen3-8B FP8 on two L4s; vLLM 0.34.0 | Measured. This is why every result is per GPU and per load |
| The model publisher's documented speculation setting served **32.1% more output** than its plain command at 32 requests at once, and **33.1% more** on two fresh servers. A copy of the current setup measured −0.1% and was rejected as no measurable difference. p95 time to first token was 31.4% worse on the fresh servers. ([receipt](docs/testing/qwen38-27b-fp8-h100-campaign-2026-11-23.md)) | Qwen3.8-27B FP8, one NVIDIA H100 per setup, short real-text chat; 23 Nov 2026 | Measured by the product's judge; proposed, not activated |
| FP8 weights with an FP8 KV cache served **62% to 159% more output** than vLLM's defaults, but scored 85.07% against 86.17% on 1,000 graded tasks (−1.10 points, interval −2.20 to −0.002), so the change was **refused**. ([receipt](docs/testing/qwen3-8b-fp8-l4-confirmation-2026-11-26.md); published FP8 checkpoints: [receipt](docs/testing/qwen3-8b-fp8-checkpoints-l4-2026-11-26.md)) | Qwen3-8B on NVIDIA L4, three fresh servers per setup; vLLM 0.34.0; 26 Nov 2026 | Refused by the run's rules, written before it started; InferCrane's 1-point rule reads it the same way |
| A September screen measured **1.72×** from one speculation setting, on synthetic prompts that repeat one sentence. Re-run on real text with three fresh servers per setup, the same change ranged from 22% slower to 68% faster, and the publisher's documented setup was faster than September's in every case. ([receipt](docs/testing/qwen38-27b-fp8-h200-rerun-2026-11-23.md)) | Qwen3.8-27B FP8 on NVIDIA H200; vLLM 0.34.0 and SGLang 0.5.20; 23 Nov 2026 | **Withdrawn** and re-measured |

What the trained-draft result does and doesn't cover:
- Each speedup is the smaller of two H100s' measurements, each beyond a measured noise floor under
  2.5%. The comparison with the published draft (`RedHatAI/Qwen3-8B-speculator.eagle3`) was measured
  on the first H100 only.
- Answers: 85.5% correct without speculation and 86.4% with the trained draft on 500 GSM8K and MMLU
  items, three servers per setup (95% interval of the difference −0.2 to +2.1 points). Greedy text
  differs even without speculation on this GPU, so exact text isn't the test; the task score is.
- One model, one GPU type, answers up to 256 tokens, 3 draft tokens. The prompts are OpenAssistant
  chat (thinking off) standing in for a customer's traffic: trained on its training split, measured
  on its held-out split. Other models, GPUs and kinds of traffic weren't measured.

## Choose how to start

| What you need | Start here | What InferCrane does |
|---|---|---|
| Speed up a model you already serve | [Install on your GPU cluster](docs/integrations/kubernetes-install.mdx) | Runs an agent beside your vLLM server, measures candidates on your traffic, and rolls out with your server kept as the fallback. Qualified on EKS with one NVIDIA A10G. |
| Improve a workload from the console | [Optimize](https://console.infercrane.com/optimization) | Profile traffic, measure candidates on exact hardware, and keep only proven wins. |
| Deploy open weights | [Deploy a model](https://console.infercrane.com/build?mode=deploy) | Resolve the model, workload, runtime, accelerator, provider, scaling policy, and cost boundary before creating compute. |
| Gate model and setting changes in CI | [Proof gate GitHub Action](#the-regression-gate-for-model-and-setting-changes) | Proves a pull request's serving change against the base branch and fails the check on a regression. |
| Keep existing inference | [Connect an endpoint](https://console.infercrane.com/onboarding/connect) | Adopt a compatible endpoint without transferring infrastructure ownership. |
| Isolate an agent | [Sandboxes](https://console.infercrane.com/sandboxes) | Persistent workspace, streaming commands, files, private previews, sleep/resume, and scoped model access. |
| Use managed capacity | [Model APIs](https://console.infercrane.com/model-apis) | Shows only currently qualified offers; an empty catalog never creates a false availability promise. |

Nothing billable starts from a recommendation. Deployment and optimization stop at a review
boundary; provider mutation or benchmark spend requires explicit approval.

## Install

Install the `v2.1.0` CLI with Homebrew or use a matching release artifact:

```bash
brew install infercrane/tap/infercrane
python -m pip install \
  'https://github.com/infercrane/infercrane/releases/download/v2.1.0/infercrane-2.1.0-py3-none-any.whl'
npm install '@infercrane/sdk@2.1.0'
```

Release archives and Terraform provider binaries are available from the
[`v2.1.0` release](https://github.com/infercrane/infercrane/releases/tag/v2.1.0).
To run the complete GPU-free product proof without creating cloud resources:

```bash
git clone https://github.com/infercrane/infercrane.git
cd infercrane
make demo
```

The proof connects an OpenAI-compatible worker, sends and inspects a request, creates an isolated
candidate, records a deterministic Release Guard rejection, verifies that production traffic did
not move, and removes its disposable stack.

`main` contains work scheduled for the next release. The hosted documentation follows `main`;
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

## The regression gate for model and setting changes

Teams change models, engine settings, and serving setups through pull requests. The proof gate
GitHub Action proves each such change before it merges: it measures the pull request's spec against
the spec on the base branch, in turns on one kind of machine, against the noise measured in that same
window, and checks the changed setup's answers against the current setup's. It comments one verdict
on the pull request and fails the check on a regression. The bar is the noise measured in that same
run, not a fixed latency threshold.

```yaml
- uses: infercrane/infercrane/actions/infercrane@v2.1.0
  with:
    mode: proof-gate
    spec: deploy/qwen.yaml
    max-cost-usd: "20"
```

| Verdict | Check |
|---|---|
| `PASS`: no worse beyond the measured noise, and the answers held | passes |
| `WATCH`: no speed regression, answers in the watch band | neutral by default |
| `BLOCK`: worse beyond the noise, more failures, shorter answers, or blocked answers | fails |
| `UNKNOWN`: anything the signed report does not show | fails; unknown never passes |

A gain is claimed only when it holds again on fresh servers. The verdict is computed from the run's
signed report, so anyone can recompute it offline with `infercrane optimize gate --file REPORT`.
See [GitHub Actions](docs/integrations/github-actions.mdx), or run
`examples/proof-gate/dry-run.sh` to see every verdict without a GPU.

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
| Gate model and setting changes in CI | Let the proof gate measure a pull request's change against the current setup, check its answers, comment the verdict, and fail the check on a regression. |
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
| GitHub Action | The regression gate for model and setting changes (`proof-gate`), plus plan, protected apply, and exact release checks. |
| OpenAI-compatible gateway | Capability-gated Chat, Completions, Embeddings, Responses, and online batch paths. The pinned vLLM profile currently qualifies Chat plus model-compatible Completions and Embeddings; unsupported capabilities fail before upstream transmission. |
| Python and TypeScript SDKs | Release artifacts: the Python wheel attached to `v2.1.0` and `@infercrane/sdk@2.1.0`. Generated from the checked OpenAPI contract. |
| Terraform provider | Logical deployment lifecycle with guarded updates and import. Release binaries and source are public; Registry publication is pending. |
| Terminal workspace | Fleet attention, evidence inspection, and state-valid guarded actions. |
| Browser console | Company-operated service for Model APIs, BYOC planning, optimization campaigns, usage, access, and workspace-scoped sandboxes. Its source is not part of the Apache-2.0 core. |
| Read-only MCP server | Closed-world operational inspection without deployment, scaling, promotion, deletion, budget, or secret tools. |

## Qualification status

InferCrane `v2.1.0` is the current stable release and the supported line. The `v1.0.0-rc.*` tags were
public betas; no earlier development tag should be treated as a supported public release.

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

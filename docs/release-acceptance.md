# v0.1 release acceptance record

The resumable operator harness reduces the normal workflow to a few guarded commands:

```bash
./scripts/release-acceptance.sh local
./scripts/release-acceptance.sh preflight
./scripts/release-acceptance.sh elastic --approve-paid-resources
./scripts/release-acceptance.sh serverless --approve-paid-resources
./scripts/release-acceptance.sh qualify --approve-paid-resources
./scripts/release-acceptance.sh cleanup
./scripts/release-acceptance.sh report
```

For release sign-off, prefer the higher-level orchestrator:

```bash
make qualify-local
make qualify-rc  # requires a clean commit and the configured paid-provider credentials
```

It separates elastic benchmark/autoscaling/Guard, serverless, and both disruption suites into
independently resumable stages and writes
`.infercrane/qualification/COMMIT/qualification.json`. A stage marker is accepted only for the
same clean commit; a dirty worktree always reruns local stages.

`preflight` is read-only. Paid workflows require the explicit approval flag, generate stable names
and idempotency keys, and retain sanitized evidence under the ignored
`.infercrane/acceptance/` directory. `cleanup` resumes those same run-owned deployments instead of
guessing resource names. Each new run pins the candidate commit in its state and report so later
harness edits cannot misattribute earlier infrastructure evidence. Every recorded stage streams
progress to the terminal while keeping machine-readable stdout in `NAME.log` and the human timeline
in `NAME-progress.log`; it also prints UTC start time, exit status, and elapsed seconds. The harness automates smoke coverage; the controlled disruption and timed
provider observations below remain release gates and may not be inferred from a successful smoke
run.

Only one paid acceptance command may run from a checkout at a time. The harness stores its PID and
run ID in `.infercrane/acceptance/.paid.lock`, rejects a concurrent paid mutation, and safely
recovers a stale lock after its owner no longer exists. Prefer the narrow `elastic`, `serverless`,
or fault stage while debugging; reserve `qualify` for a locally green frozen candidate.

`qualify` is the guarded end-to-end path. It runs the elastic benchmark, verifies provider-backed
scale-up and scale-down, records a deterministic Release Guard rejection without provisioning the
bad candidate, deletes elastic capacity, then verifies Serverless cold/warm traffic, scale-to-zero,
and a second cold request. Elastic and Serverless smoke also verify a named function call and strict
structured JSON response through the logical OpenAI-compatible endpoint. Named function choice is
supported by default in vLLM. Automatic tool selection is not implied because vLLM requires an
explicitly enabled, model-specific parser for `tool_choice: auto`. The harness never permits more than the deployment's configured maximum of two
workers and stops on the first missing observation. Paid subcommands install a failure trap that
captures direct provider identifiers and host/image metadata before deleting run-owned resources;
running `cleanup` afterward is still a safe idempotent final check. This preserves bootstrap
diagnostics after the billable resource is removed without persisting provider credentials.

RunPod does not publish a fixed Pod allocation-time guarantee. Its [container guidance](https://docs.runpod.io/tutorials/introduction/containers/docker-commands)
recommends versioned images and explains host-side image caching. The harness therefore separates its
operator timeout from a performance claim: readiness and scale-up allow 45 minutes by default and
can be overridden with `INFERCRANE_ACCEPTANCE_READY_TIMEOUT_SECONDS` and
`INFERCRANE_ACCEPTANCE_SCALE_UP_TIMEOUT_SECONDS`. A long `INIT` provider state is capacity or image
startup evidence, not vLLM readiness. InferCrane launches elastic replicas from a digest-pinned
image derived from the official vLLM image with only RunPod's required SSH bootstrap added, so a
new replica does not install vLLM or its CUDA dependency stack at boot. The GHCR package must be
public so a fresh provider worker can pull it without repository credentials.
Do not extend a timeout repeatedly while provider state is ambiguous; inspect the existing intent
and resource before deciding whether to resume or clean up.

This document is the evidence index for the first public release. A checkbox is not evidence:
record the command, UTC time, final commit, sanitized log path, and provider resource identifiers
for every manual run. Never mark a real-infrastructure gate from a fake worker or mocked provider
test.

## Run metadata

Fill these fields immediately before qualification:

```text
candidate commit:
candidate tag:
operator:
started (UTC):
RunPod account/team:
RunPod region:
elastic deployment name:
serverless deployment name:
evidence directory:
```

Use unique deployment names and explicit, stable idempotency keys for the entire run. Before every
retry, inspect InferCrane state and RunPod pods/endpoints. Resume an existing operation or reuse its
idempotency key; do not submit a replacement while the original provider request is unresolved.
Never paste API keys, Hugging Face tokens, prompts, or generated output into evidence logs.

## Automated and package gates

| Gate | Required evidence | Status / artifact |
|---|---|---|
| Repository verification | Clean checkout; `make verify` | pending |
| Dead code | `make deadcode` | pending |
| Vulnerability audit | `make audit` and reviewed findings | pending |
| Docker integration | `make test-container` | pending |
| Local stack | OpenAI request, SSE stream, concurrent requests, restart recovery | pending |
| Release archives | darwin/linux × amd64/arm64 archives | pending |
| Supply chain | SHA-256 file, archive SBOMs, image SBOM and provenance | pending |
| Container | amd64 and arm64 production image; no development fakes or compiler | pending |
| Homebrew | final formula has published archive URLs/checksums; clean-machine install | pending |
| Documentation | strict site build, link review, security contact, examples, issue forms | pending |
| Demo | reproducible terminal recording no longer than 60 seconds | pending |

## Gate 0 — elastic lifecycle

The real RunPod run must prove all rows. Record InferCrane operation IDs, revision IDs, replica
external keys, and RunPod pod IDs.

| Acceptance | Required observation | Status / artifact |
|---|---|---|
| Preflight | `doctor --cloud`; read-only RunPod inventory and GPU stock captured before mutation | pending |
| CLI disconnect | client exits during provisioning; the same durable operation completes | pending |
| Provision restart | control plane restarts mid-provision; the original operation resumes | pending |
| Exactly-once intent | one RunPod pod for each persisted replica external key after replay | pending |
| vLLM readiness | immutable artifact loads and expected model is served | pending |
| OpenAI compatibility | authenticated logical endpoint returns a valid chat completion | pending |
| Streaming | valid SSE chunks and `[DONE]`; first bytes arrive before completion | pending |
| Autoscaling | persisted evidence and provider inventory show `1 → N → 1` | pending |
| Safe draining | withdrawn replica receives no new request and active stream completes | pending |
| Immutable revision | active and candidate specs/artifacts have distinct immutable identities | pending |
| Update | healthy candidate is guarded, routed, and old capacity terminates after drain | pending |
| Bad update | deterministic Release Guard rejection; active revision remains routed | pending |
| Generation safety | active stream stays on its selected router generation during publish | pending |
| Delete restart | control plane restarts mid-delete and resumes the same cleanup operation | pending |
| Zero leaks | final InferCrane inventory and RunPod inventory contain no run-owned resource | pending |

## Public CLI and API

Exercise `init`, `doctor`, `deploy`, `apply`, `plan`, `status`, `status --watch`, `events`, `inspect`,
`explain`, and `delete` from a clean client configuration. For every command that returns data,
capture human output and parse `--output json` with `jq`. Confirm the CLI only contacts the
authenticated control-plane URL and never receives database credentials. Confirm Ctrl-C merely
disconnects a watcher/waiter and does not cancel or corrupt the persisted operation.

Verify plan output describes field changes, candidate provisioning, validation, routing, draining,
and termination. A missing provider price must remain explicitly unavailable; never replace it with
an estimate lacking a trustworthy source and timestamp.

## Telemetry, artifacts, guard, and explanations

| Acceptance | Required observation | Status / artifact |
|---|---|---|
| ModelArtifact | mutable Hugging Face reference resolves to an immutable commit | pending |
| Privacy | no prompt or generated output is stored or exported by default | pending |
| Dimensions | deployment, revision, replica, provider, runtime, compute mode, operation | pending |
| Measurements | TTFT, latency, token counts/throughput, stream/runtime errors, queue evidence | pending |
| Release Guard | persisted policy, inputs, decision, metrics, and deterministic reason codes | pending |
| Explain deployment | degraded state and blocking operation reproduced from persisted state | pending |
| Explain scaling | latest persisted scale/no-scale decision and signals reproduced | pending |
| Explain rollout | exact persisted guard evaluation and rejection reasons reproduced | pending |
| Explain cold start | available timing evidence and unavailable boundaries clearly separated | pending |

Shadow traffic is not a v0.1 acceptance requirement. If used in a future release, it must be
explicit, bounded, privacy-documented, and cost-visible; InferCrane must never duplicate requests
silently.

## Demo A — elastic

From a clean config: install, run doctor, deploy `Qwen/Qwen3-8B`, send buffered and streaming
requests, benchmark, autoscale `1 → N`, explain scaling, create a deliberately bad candidate,
observe Release Guard reject it, inspect the persisted explanation, roll back or clean up, delete,
then capture zero-resource provider inventory.

## Demo B — RunPod Serverless

Start from a validated immutable vLLM template and zero workers. Capture one cold request, one warm
request, scale-to-zero, a second cold request, streaming, client cancellation, durable events,
accounting, deletion, orphan reconciliation, and final zero-resource inventory. Provider APIs may
not expose capacity/container/artifact/model-load boundaries; record them as unavailable rather than
inferring timestamps.

For orphan reconciliation, exercise the create-response-loss boundary (or an equivalent controlled
interruption after provider acceptance but before identity persistence). The cancellation/delete
operation must recover the endpoint by the deterministic replica external-key name, persist no
false deletion while the endpoint remains visible, and finish only after RunPod inventory confirms
absence. Do not create an extra endpoint to simulate this case.

For timing interpretation, RunPod [documents](https://docs.runpod.io/serverless/endpoints/job-states) that a Serverless cold start includes container
startup, model loading, and initialization, and marks a worker unhealthy if initialization exceeds
its configured limit ([seven minutes by default](https://docs.runpod.io/serverless/workers/overview) in the current platform documentation). Flex workers
are expected to scale to zero and cold-start again. These are provider semantics, not InferCrane
latency guarantees; record the measured values and only the timing boundaries actually exposed.

## Benchmark evidence

Run `infercrane benchmark DEPLOYMENT` only against the real immutable deployment. Attach the
persisted result and exact redacted reproduction command. Record artifact identity, vLLM and AIPerf
versions, runtime arguments, RunPod region, GPU/type/count, revision, compute mode, workload,
TTFT/TPOT, throughput, errors, available GPU telemetry, trustworthy cost metadata, and UTC time.
Do not upload benchmark records by default and do not publish comparative performance claims from a
single run.

## Durable Session decision

Durable Session identity is deferred to v0.2. v0.1 does not promise session affinity or durable KV
state and does not integrate LMCache. This timeboxed preview is optional and is excluded because it
would add unqualified lifecycle state before the release gates are complete.

## Final zero-leak and release sign-off

After both demos, reconcile orphans, inspect RunPod pods and Serverless endpoints directly, and
record that every run-owned identifier is absent. Do not tag while provider deletion is pending or
inventory is ambiguous. Replace Homebrew placeholders with checksums from the final archives, rerun
clean-machine installation, and ensure the draft release points to the audited commit.

Known limitations in the release notes must include RunPod-only infrastructure, vLLM-only runtime,
no Kubernetes, no automatic cloud/GPU/runtime selection, grounded-but-provider-limited cold-start
substage visibility, no guaranteed durable KV state, and no unqualified performance or pricing
claim.

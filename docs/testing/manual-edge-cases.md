# Manual edge-case qualification

Local fixtures prove InferCrane logic only. This document defines the paid or external-system procedures required for provider, GPU, and runtime qualification. Do not execute these procedures during ordinary local development.

## Evidence requirements for every run

- Record the exact InferCrane commit, provider/runtime versions, immutable image and model identities, region, hardware, timestamps, and acceptance run ID.
- Preserve operation events, provider inventory before/after, API responses, runtime logs, request IDs, and cleanup evidence.
- Abort if ownership labels are missing or the cleanup target cannot be resolved exactly.
- Finish with both InferCrane and provider-native inventories proving zero run-owned billable resources.

## RunPod: elastic lifecycle and disruption

- Sources: [RunPod Pod management](https://docs.runpod.io/sdks/graphql/manage-pods) and the repository's `scripts/release-acceptance.sh` fault stages.
- Why local simulation is insufficient: stock, image transfer, provider create/delete visibility, network publication, and GPU/runtime readiness are provider behavior.
- Safe procedure: configure the key file and immutable serverless template, choose a unique run ID, then run `scripts/qualify-provider-manual.sh run --approve-paid-resources`. Reuse the same run ID after a terminal or control-plane interruption. The orchestrator runs baseline qualification, elastic disconnect/restart/lost-response/generation-drain/delete-boundary faults, serverless faults, and guarded cleanup.
- Risk/cost: paid GPU Pods and Serverless workers. Run only in an isolated RunPod project with a defined spend ceiling.
- Expected behavior: one Pod per replica intent; a lost create response is adopted; CLI/control-plane loss does not duplicate capacity; the old revision remains routed until candidate readiness; active streams fence drain; delete converges despite restart/eventual visibility.
- Evidence: all generated acceptance reports, operation/event exports, request transcripts, immutable image/model identity, before/after RunPod inventory, and a direct console/API confirmation of zero run-owned Pods/endpoints.
- Cleanup: `scripts/qualify-provider-manual.sh cleanup`, followed by direct provider inventory. Never delete an unlabelled or non-run-owned resource.

## RunPod Serverless: cold/warm/zero/cancellation/adoption

- Sources: [RunPod Serverless endpoint operations](https://docs.runpod.io/serverless/endpoints/operation-reference) and [endpoint overview](https://docs.runpod.io/serverless/endpoints/overview).
- Why local simulation is insufficient: endpoint existence, worker readiness, queue state, FlashBoot/cache behavior, scale-to-zero delay, and eventual deletion are controlled by RunPod.
- Safe procedure: use the `serverless-faults` stage through the qualification orchestrator above, or a fresh run ID with `scripts/release-acceptance.sh serverless-faults --approve-paid-resources`. Send one cold request, one warm request, wait for provider-confirmed zero workers, send a second cold request, and cancel one bounded stream from the client. Exercise the fault proxy's lost-create-response checkpoint only against the run-owned endpoint.
- Risk/cost: endpoint and transient worker billing; cancellation must use a normal bounded request, never an exhaustion payload.
- Expected behavior: one durable endpoint is adopted after the lost response, no duplicate endpoint appears, both cold cycles succeed, warm latency is separately recorded, cancellation reaches the upstream context and durable telemetry, and delete remains pending until provider absence is observed.
- Evidence: endpoint ID/template/GPU/worker bounds, worker counts with observation timestamps, cold-start boundaries actually exposed, request/cancellation IDs, accounting records, deletion polling, and final zero inventory.
- Cleanup: run the matching acceptance cleanup with the same run ID, then independently verify no run-owned endpoint remains.

## AWS EC2 BYOC: ambiguous create, capacity, IAM, networking, and delete

- Source: [EC2 API idempotency](https://docs.aws.amazon.com/ec2/latest/devguide/ec2-api-idempotency.html) and the repository's portable-provider acceptance harness.
- Why local simulation is insufficient: `RunInstances` client-token semantics, IAM propagation, EC2 eventual consistency, subnet/ENI exhaustion, capacity errors, security groups, and termination timing require an AWS account.
- Safe procedure: prepare the private AWS environment/spec/key files described in `docs/release-acceptance.md`; use a dedicated account/project, subnet, security group, instance profile, and budget alarm; then run `scripts/portable-provider-acceptance.sh aws --approve-paid-resources` with a unique `INFERCRANE_ACCEPTANCE_RUN_ID`. Qualify separately under an intentionally insufficient-capacity accelerator/AZ only if AWS confirms the request is non-destructive. Interrupt the client after request transmission, resume with the same ID, rotate/expire credentials between observations, and restart during termination.
- Risk/cost: EC2 GPU, storage, public IPv4/NAT, and image/model transfer. Spot interruption is out of scope unless the spec deliberately selects Spot.
- Expected behavior: stable zonal client token, at most one matching instance, configuration-mismatched discovery rejected, capacity/quota/auth/network failures normalized and retryable only when safe, no ready state until model identity is proven, and termination/inventory converge.
- Evidence: CloudTrail request/client token, instance tags and immutable configuration, operation attempts, runtime readiness transcript, VPC reachability evidence, termination observations, cost tags, and exact pre/post inventory.
- Cleanup: use the portable harness cleanup and independently query run-owned EC2 instances, volumes, ENIs, and any harness-created security objects.

### Model-diverse AWS performance evidence

Run model families independently so a failure or cost boundary remains attributable:

```bash
export INFERCRANE_V1_PROVIDER_ENV_FILE=/secure/path/aws.env
export INFERCRANE_V1_API_KEY_FILE=/secure/path/worker-key

scripts/aws-performance-qualification.sh mistral --approve-paid-resources
scripts/aws-performance-qualification.sh deepseek --approve-paid-resources
scripts/aws-performance-qualification.sh granite --approve-paid-resources
```

Mistral qualifies the catalog-declared tool path plus structured decoding; DeepSeek Distill and
Granite qualify chat, streaming, workload shapes, and lifecycle without inventing tool support. Each
run must archive the seven-profile AIPerf matrix, the fixed-workload concurrency sweep, immutable
model/runtime/image identities, startup waterfall, and matching before/after EC2 and EBS inventory.
Do not combine rows across different
model identities or workload digests into one ranking.

## GCP Compute BYOC: adoption, quota, IAM, networking, and delete

- Why local simulation is insufficient: the fixture proves intent-digest adoption and bounded command handling, but Compute Engine name reuse, regional quota, service-account propagation, VPC/firewall behavior, operation polling, and eventual deletion are controlled by GCP.
- Safe procedure: use a dedicated GCP project with a budget alert, isolated VPC/subnet, least-privilege service account, immutable image, and unique acceptance run ID. Run `scripts/portable-provider-acceptance.sh gcp --approve-paid-resources`. Interrupt the client after instance insertion, resume the same operation, and verify the existing same-intent VM is adopted. Separately pre-create a same-name VM with a different InferCrane intent digest and verify adoption fails closed. Restart the control plane during deletion and poll both the zonal operation and instance inventory until absence is proven.
- Risk/cost: GPU VM, persistent disk, external IP/NAT, and model transfer. Execute only in an isolated project and retain every command/API response for review.
- Expected behavior: at most one VM per durable replica intent; configuration- or digest-mismatched resources are never adopted; auth/quota/capacity/network failures remain visible; readiness requires the expected model identity; delete remains pending until provider absence is observed.
- Evidence: project/zone, machine and accelerator type, image digest, service account, labels and intent digest, operation IDs, readiness transcript, restart timeline, billing labels, and before/after instance/disk/address inventory.
- Cleanup: delete only resources carrying the exact run ownership labels, then independently verify zero run-owned VMs, disks, addresses, and forwarding rules.

## Kubernetes/KServe: controller staleness, UID reuse, finalizers, eviction

- Sources: [Kubernetes API concepts](https://kubernetes.io/docs/reference/using-api/api-concepts), [finalizers](https://kubernetes.io/docs/concepts/overview/working-with-objects/finalizers/), [Deployment status](https://kubernetes.io/docs/reference/kubernetes-api/workload-resources/deployment-v1/), and [KServe CRD API](https://kserve.github.io/website/docs/reference/crd-api).
- Why local simulation is insufficient: the Kind gate proves API semantics without GPU scheduling, real KServe storage initialization, node eviction, CNI/service propagation, or a production ingress.
- Safe procedure: first run `make test-kubernetes-kind`. For a real isolated GPU cluster, prepare the Kubernetes provider files and run `scripts/portable-provider-acceptance.sh kubernetes --approve-paid-resources`. During a candidate, cordon/drain only the dedicated test node, delete/recreate one run-owned workload with the same name, temporarily make its image unavailable, and add a benign test finalizer before deletion. Never disturb shared namespaces/nodes.
- Risk/cost: dedicated GPU cluster capacity; node drain and finalizers can disrupt other workloads unless the cluster and namespace are isolated.
- Expected behavior: readiness requires matching UID/ownership and current `observedGeneration`; Pending/ImagePullBackOff/CrashLoop/Unknown never route; API conflicts retry without overwriting foreign fields; accepted deletion remains pending through the finalizer; removed/recreated UID is not confused with the old resource.
- Evidence: YAML including UID/resourceVersion/generation/observedGeneration/conditions, InferCrane events, route snapshots, pod events/logs, finalizer lifecycle, and namespace inventory before/after.
- Cleanup: remove only the test finalizer, run harness cleanup, and prove the namespace contains no run-owned resources.

## NVIDIA Dynamo: graph ownership, transfer, cache, and performance

- Sources: [Dynamo DGD reference](https://docs.nvidia.com/dynamo/reference/api/kubernetes/dynamo-graph-deployment), [model deployment](https://docs.nvidia.com/dynamo/dev/kubernetes/model-deployment/introduction), and [KV cache offloading](https://docs.nvidia.com/dynamo/dev/kubernetes/kv-cache-offloading/overview).
- Why local simulation is insufficient: fixtures and the test-only Kind CRD prove InferCrane's parent ownership and API behavior, not the Dynamo operator, GPU scheduling, NIXL data movement, runtime compatibility, cache allocation, or workload performance.
- Safe procedure: use an isolated GPU namespace and immutable qualified Dynamo/runtime image. Record empty run-owned DGD inventory, deploy the aggregated baseline, verify the stable endpoint and exact served model, then create a disaggregated candidate with KV-aware routing. Run the same AIPerf workload against both, confirm Release Guard uses only fresh comparable evidence, and promote only a passing candidate. Repeat with aggregated vLLM KVBM after verifying requested memory leaves runtime headroom and the named PVC has sufficient capacity. Kill one worker, restart the operator, interrupt the CLI after apply, and verify one parent is adopted without duplicate children. Delete through InferCrane and confirm zero run-owned DGD parents and provider children.
- Evidence to collect: InferCrane operation/revision IDs, canonical topology digest, DGD YAML/status and observed generation, Dynamo/operator versions, runtime image digest, model commit, GPU topology, NIXL/cache configuration, AIPerf input digest/results, route generation, stream continuity, Release Guard decision, and final zero inventory.
- Cost/risk: paid multi-GPU capacity; disaggregation may be slower for the selected workload; incorrectly sized host/disk cache can starve the runtime. Use bounded concurrency and a dedicated namespace/cluster.
- Expected behavior: only the exact parent intent is adopted; stale or failed graph status never routes; the active stable endpoint does not move until comparable Release Guard evidence passes; rejected candidates and deleted graphs converge to zero owned resources.
- Cleanup: delete through InferCrane, wait for the parent DGD to disappear, then independently verify that no run-owned DGD, DCD, Pod, Service, PVC, or scheduler child remains.

## vLLM: real protocol, overload, worker loss, and drain

- Why local simulation is insufficient: CUDA OOM, NCCL/startup, tokenizer/model compatibility, runtime response conformance, tool/structured behavior, and disconnect handling require the real pinned image/model on GPU.
- Safe procedure: through RunPod, AWS, or Kubernetes qualification, verify `/health` and `/v1/models` identity; buffered Chat, SSE Chat, Responses, Embeddings, tool calling, and structured output only where the runtime profile claims them; AIPerf under bounded concurrency; a slow client; client disconnect; runtime process restart; and candidate promotion while a long stream remains active.
- Risk/cost: GPU time and load. Keep prompts/output bounded and never induce deliberate OOM on shared infrastructure.
- Expected behavior: unsupported protocols fail closed; no streamed request is retried; disconnect cancels upstream; old generation remains until its last request releases; worker loss is visible and healthy capacity takes precedence; runtime/model mismatch never routes.
- Evidence: runtime/image/model digests, protocol transcripts, request IDs, vLLM logs/metrics, generation counters, drain timing, and resource cleanup.
- Cleanup: delete the owning deployment and prove provider/cluster absence.

## NVIDIA DCGM: real hardware telemetry

- Source: [NVIDIA DCGM Exporter metrics](https://docs.nvidia.com/datacenter/dcgm/latest/reference/dcgm-exporter-metrics.html).
- Why local simulation is insufficient: the parser and evidence lifecycle can be proven from
  fixtures, but metric availability, units, GPU labels, XID semantics, MIG behavior, and exporter
  compatibility depend on the physical GPU, driver, DCGM, and deployment topology.
- Safe procedure: on an isolated qualified GPU workload, expose DCGM Exporter only to an operator
  or protected collector network. Confirm its workload labels, then run
  `infercrane telemetry collect dcgm DEPLOYMENT --url URL --selector key=value --replica REPLICA --ttl 2m`.
  Compare the normalized values with the same raw snapshot. Stop the exporter and verify each value
  becomes stale and loses its rendered numeric value after two minutes.
- Risk/cost: no extra GPU load beyond ordinary telemetry, but the test needs an already-running GPU
  workload. Never make the exporter publicly reachable.
- Expected behavior: the snapshot binds to the active revision, selectors exclude unrelated GPUs,
  units and aggregation match the documented contract, and stale evidence never looks live.
- Evidence: immutable workload revision, GPU/DCGM/driver versions, exact selector, saved raw scrape,
  API response, monitoring response before and after expiry, and confirmation that prompt/output
  content and credentials are absent.

## OpenCost: real Kubernetes allocation and billing reconciliation

- Source: [OpenCost Allocation API](https://opencost.io/docs/integrations/api/) and
  [OpenCost specification](https://opencost.io/docs/specification/).
- Why local simulation is insufficient: fixtures prove parsing, exact selection, persistence, and
  report semantics, but not cluster labeling, pricing configuration, idle-cost policy, or agreement
  with the provider invoice.
- Safe procedure: on a non-production namespace with OpenCost already installed, port-forward its
  API; query one completed 24-hour controller allocation; save the JSON; run
  `infercrane finops collect opencost DEPLOYMENT --file allocation.json --window 24h --allocation EXACT_KEY --currency CODE`;
  then compare the persisted source window and hourly rate with the OpenCost UI/API.
- Cost/risk: read-only API access and local control-plane persistence; no provider mutation.
- Expected behavior: only the exact allocation is recorded, the active revision is bound, retry is
  idempotent, changed evidence conflicts, and no currency conversion or savings claim appears.
- Evidence: OpenCost version/configuration, query parameters, exact allocation key, response digest,
  InferCrane revision/evidence IDs, report digest, and provider invoice comparison when available.

## Model revisions: semantic output quality

- Research signal: production operators report that an infrastructure-healthy model or runtime
  rollout can still regress correctness, groundedness, hallucination rate, or task success.
- Why local simulation is insufficient: these outcomes depend on the customer's model, prompts,
  domain, expected answers, safety policy, and evaluation rubric. Generic latency/error fixtures
  cannot establish semantic correctness.
- Safe procedure: before promotion, run a versioned customer-owned offline evaluation dataset
  against active and candidate revisions. Record the evaluator and dataset identity, compare task
  metrics, review regressions, and require explicit human approval. Do not shadow live prompts or
  retain request/output bodies without separate informed privacy approval.
- Expected behavior: Release Guard continues to evaluate only trustworthy readiness, error,
  performance, compatibility, and sourced-cost evidence. Semantic evaluation remains a separate
  explicit release prerequisite; no LLM decides promotion.
- Evidence: immutable model/runtime identities, evaluation dataset and evaluator versions, aggregate
  results, privacy approval if production content was used, and the human promotion decision.

## SGLang and custom OCI: Runtime Contract V1

- Why local simulation is insufficient: the hermetic contract cannot prove GPU model compatibility, production probe semantics, metrics, cancellation, or graceful shutdown for the exact image.
- Safe procedure: use the AWS or Kubernetes portable harness with the immutable SGLang example and one customer-controlled digest-pinned custom OCI image. Verify the declared health/models/metrics paths, expected model, every claimed protocol, disconnect cancellation, SIGTERM/shutdown grace, and generation drain. Keep min=max because SGLang autoscaling is not qualified.
- Risk/cost: one GPU per runtime candidate and image/model transfer.
- Expected behavior: only explicitly declared capabilities route; probe/model mismatch remains unhealthy; runtime arguments are passed as argv without shell interpretation; cancellation and shutdown finish within the declared bounds; deletion reaches zero.
- Evidence: OCI digest, normalized workload contract, probe/protocol transcripts, process logs, termination timing, and final provider inventory.
- Cleanup: portable harness cleanup followed by provider and namespace inventory.

## Streaming/draining: generation safety under real latency

- Why local simulation is insufficient: real proxy buffering, client TCP behavior, runtime disconnect propagation, and long-token generation timing differ from fixtures.
- Safe procedure: start one bounded long-running SSE request and record its first chunk; promote a healthy candidate or scale down its worker; prove the old provider resource remains while the stream is active; cancel or allow completion; prove the route generation retires only afterward. Repeat with control-plane and router restart during the stream.
- Risk/cost: two revisions may overlap temporarily. Set an explicit maximum stream duration and incremental cost limit.
- Expected behavior: the stream remains pinned, receives no mixed-generation chunks, is never replayed, and capacity deletion starts only after release; a client cancellation is recorded once.
- Evidence: timestamped SSE transcript, route/generation snapshots, active/retiring counters, provider resource timeline, cancellation telemetry, and cleanup inventory.

## vLLM: security advisories and runtime upgrade

- Sources: [GHSA-rxc4-3w6r-4v47 / CVE-2025-48956](https://github.com/vllm-project/vllm/security/advisories/GHSA-rxc4-3w6r-4v47) and [GHSA-94f4-hr76-p5j6](https://github.com/vllm-project/vllm/security/advisories/GHSA-94f4-hr76-p5j6).
- Why local simulation is insufficient: the candidate now pins vLLM 0.22.0, outside both affected ranges, but proving the replacement requires a real GPU, immutable image bytes, model load, protocols, metrics, cancellation, and drain behavior. Do not send an exploit or memory-exhaustion payload to any worker.
- Safe procedure: deploy the pinned 0.22.0 image by digest to one isolated, non-production replica with provider firewall rules allowing only the InferCrane data plane; verify the version without printing credentials, readiness, model identity, buffered/streaming/tool/structured requests, cancellation, metrics, and deletion. Confirm the public InferCrane gateway returns 431 for a bounded over-limit header. Verify the provider worker is not directly reachable from an untrusted network and startup logs contain no credential value.
- Risk/cost: one GPU replica and image/model transfer; no destructive or exploit-sized request is permitted.
- Expected behavior: only the gateway is public, headers are bounded, runtime compatibility gates pass, and cleanup reaches zero owned resources.
- Evidence: immutable image digest, vLLM version, network policy/security-group evidence, gateway response, protocol transcripts, operation events, and final provider inventory.

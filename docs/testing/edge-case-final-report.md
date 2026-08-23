# Edge-case hardening final report

## Executive Summary

- Failure patterns researched: 16 production failure classes, covering distributed operations, PostgreSQL transactions, reconciliation, autoscaling, routing/streaming, releases, serverless, provider APIs, security, admission, Kubernetes, AWS, RunPod, runtimes, resource exhaustion, and upgrades.
- Hypotheses investigated: 53 ledgered high-value hypotheses.
- New tests added: 36 named regression, stress, and fuzz checks, plus a broad public module journey and an immutable runtime-publication contract.
- Bugs reproduced: 34 locally reproducible defects plus one defect reproduced only on real AWS.
- Bugs fixed: 35, each retained in the regression suite or pending exact real-runtime requalification where stated.
- Critical bugs remaining: 0 known locally reproducible defects.
- High bugs remaining: 0 known locally reproducible defects.
- Real-infrastructure edge cases remaining: provider, GPU-runtime, network, multi-host, and long-duration behavior listed in [manual edge cases](/testing/manual-edge-cases). The vLLM 0.22.0 security upgrade remains unqualified until the exact pinned image passes the real GPU gate.

These results prove the tested InferCrane logic against local, PostgreSQL, Docker, Kind, deterministic provider-fixture, race, stress, and fuzz conditions. They do not prove external-provider semantics or production readiness.

## Most Important Bugs Found

### Lost lease ownership could become a permanent operation failure

- Failure scenario: lease maintenance fails, cancels the handler, and the stale worker persists a terminal failure instead of yielding to recovery.
- Source/inspiration: fenced-worker failure patterns in distributed controllers.
- Reproduction: `TestWorkerDoesNotFinalizeAfterLeaseMaintenanceFailure` failed before the fix.
- Root cause: the worker treated lease-maintenance cancellation like a handler failure.
- Fix: stop without a terminal mutation when ownership cannot be maintained; let lease recovery resume the durable operation.
- Regression test: `internal/operations/worker_test.go`.

### Async inference could execute twice after its lease expired

- Failure scenario: a paid, long request exceeds the one-minute lease and another worker reclaims it.
- Source/inspiration: durable queue visibility-timeout and fencing failures.
- Reproduction: production worker inspection plus forced-expiry PostgreSQL tests proved no heartbeat existed.
- Root cause: async execution acquired a lease once but never renewed it.
- Fix: fenced heartbeats cancel execution on ownership uncertainty and prevent stale completion writes.
- Regression test: `internal/asyncinference/service_lease_test.go`.

### Endpoint promotion could drain an active stream too early

- Failure scenario: an Endpoint route changes while a request is pinned to the old generation, but that generation is not marked retiring.
- Source/inspiration: generation-safe proxy drain invariants.
- Reproduction: strengthened endpoint promotion test observed zero retiring requests before the fix.
- Root cause: endpoint replacement did not retire withdrawn generations.
- Fix: retire replaced/removed generations while retaining any generation still published by a concrete deployment.
- Regression test: `TestEndpointPromotionPinsExistingRequestGeneration` and route state-machine fuzzing.

### Release Guard promotion had a check/use race

- Failure scenario: a previously accepted candidate is promoted after the endpoint candidate or active binding changes concurrently.
- Source/inspiration: stale-controller and optimistic-concurrency failure patterns.
- Reproduction: PostgreSQL regression changed candidate state between evidence and activation.
- Root cause: evidence validation and endpoint update were separate operations without a row lock.
- Fix: lock the endpoint and require the current candidate plus the latest PASS against the current active binding in the activation transaction.
- Regression test: `internal/store/endpoints_test.go`.

### Signed Inference Passports were accepted without cryptographic binding

- Failure scenario: an invalid signature, wrong revision, wrong tenant, or altered spec could be stored as trusted evidence.
- Source/inspiration: signed-evidence substitution and replay failures.
- Reproduction: invalid-signature and wrong-revision PostgreSQL tests succeeded before the fix.
- Root cause: non-empty signature metadata was stored without Ed25519 verification and domain binding.
- Fix: verify signature/schema and bind tenant, deployment, revision, and specification to authoritative database state.
- Regression test: passport cases in `internal/store/store_test.go`.

### Provider adoption could bind the wrong infrastructure

- Failure scenario: AWS adopted an instance with a mismatched immutable configuration; GCP adopted a same-name VM without proving intent; long SkyPilot names collided after truncation.
- Source/inspiration: AWS idempotency guidance and lost-create-response recovery.
- Reproduction: adapter fixtures supplied mismatched resources and production-length identities.
- Root cause: discovery identity was treated as sufficient proof of intent.
- Fix: compare immutable AWS configuration, persist/verify a GCP intent digest, and preserve uniqueness with a deterministic hash suffix.
- Regression tests: AWS, GCP, and SkyPilot provider tests.

### External and subprocess error paths leaked or retained unbounded data

- Failure scenario: RunPod/SkyPilot responses expose credentials or force unbounded memory retention; runtime redirects forward a worker credential to another origin.
- Source/inspiration: hostile-provider boundaries and HTTP credential redirect behavior.
- Reproduction: reflected keys, 9 MiB subprocess output, and a local 307 credential receiver.
- Root cause: error bodies and subprocess output were trusted; runtime inspection followed redirects.
- Fix: shared bounded/redacted evidence, an 8 MiB subprocess cap, and redirect-disabled runtime inspection.
- Regression tests: RunPod, SkyPilot, provider-runner, and vLLM inspector tests.

### Webhook SSRF filtering allowed special-purpose address space

- Failure scenario: a webhook hostname resolves to `100.64.0.0/10` or another reserved range not classified private by the standard helper.
- Source/inspiration: RFC 6598 and IANA special-purpose registries.
- Reproduction: `100.64.0.1` passed the prior classifier.
- Root cause: the deny policy relied primarily on `IP.IsPrivate`.
- Fix: explicitly reject shared, protocol-assignment, documentation, benchmark, multicast, and reserved IPv4/IPv6 ranges unless private destinations are deliberately enabled.
- Regression test: `TestProhibitedRejectsSharedAndReservedDestinations`.

### Independent tenants/deployments could starve behind one failure

- Failure scenario: one unavailable autoscaling signal, quota refill error, or reconciler lookup failure aborts the entire control-loop pass.
- Source/inspiration: controller work-queue failure isolation.
- Reproduction: selective fixtures showed later work was never evaluated.
- Root cause: loop bodies returned on the first item error.
- Fix: process all independent items, aggregate errors afterward, and still stop promptly for context cancellation.
- Regression tests: autoscale, request-quota, and reconciler isolation cases.

### Invalid async work was queued and retried

- Failure scenario: a request without the endpoint model identity enters the durable encrypted
  queue, consumes three attempts, and only then fails at the gateway.
- Source/inspiration: queue admission must reject permanently invalid work before persistence.
- Reproduction: the black-box endpoint accepted a missing-model payload and recorded all retry
  attempts.
- Root cause: protocol-native validation happened only during execution.
- Fix: validate the supported protocol, JSON object, required model, and endpoint/model equality
  before encryption and durable submission.
- Regression test: `TestAsyncInferenceRejectsInvalidProtocolAndEndpointModelBeforeQueueing` plus the
  public module journey.

### Public CLI automation contracts were inconsistent

- Failure scenario: target commands advertised automation but printed human tables for JSON, a
  positional capacity argument silently stopped flag parsing, and unavailable evidence encoded
  collections as `null`.
- Source/inspiration: hostile/careless operator testing through public commands.
- Reproduction: the black-box CLI produced each incompatible output.
- Root cause: older commands did not share output/argument validation, and empty decision slices
  were not normalized.
- Fix: consistent output validation and help, strict positional rejection, and stable empty arrays
  without fabricated evidence.
- Regression tests: target/capacity/help CLI cases, decision collection-contract test, and the
  public module journey.

### Runtime publication could mislabel old vLLM bytes

- Failure scenario: the workflow publishes a requested newer vLLM tag while the Dockerfile still
  builds the old unconditional base digest.
- Source/inspiration: immutable artifact/version binding and the applicable vLLM header advisory.
- Reproduction: static data-flow inspection proved the workflow build argument had no consumer.
- Root cause: version selection existed only in image metadata.
- Fix: candidate publication now requires an immutable official base digest and verifies the
  installed package version before layering or tagging.
- Regression test: `scripts/test-runtime-image-contract.sh`. Changing the production default still
  requires real GPU qualification.

### PostgreSQL dependency health could race fresh-volume initialization

- Failure scenario: Compose reports a fresh PostgreSQL volume healthy, starts InferCrane, and the
  control plane receives `connection refused` while PostgreSQL replaces its temporary
  initialization server with the final TCP server.
- Source/inspiration: official PostgreSQL image initialization and dependency-readiness behavior.
- Reproduction: the whole-product module journey passed the PostgreSQL dependency and then failed
  its first database connection on a clean volume.
- Root cause: `pg_isready` omitted a host, so it tested the temporary Unix-socket server instead of
  the TCP endpoint used by InferCrane.
- Fix: every PostgreSQL Compose health check probes `127.0.0.1` explicitly.
- Regression: production Compose and acceptance-safety checks require the TCP probe, and the module
  journey passes from a new volume.

## Failure Classes Proven Safe Locally

- Durable mutations are semantically idempotent under retry, concurrent submission, cancellation ordering, worker restart, lease expiry, and database-clock differences.
- Provider adapters fail closed on mismatched adoption, semantic-invalid success, stale Kubernetes status, malformed/truncated responses, eventual delete, and bounded/redacted diagnostics.
- Routing preserves generation pinning and drain safety across promotion/removal sequences; state-machine fuzzing exercised 435,137 generated executions without another invariant failure.
- Admission and quota accounting preserve exact limits under concurrent acquire/release, cancellation, policy reduction, queue timeout, and per-tenant refill failure.
- Release state rejects stale promotion evidence, preserves immutable revisions, verifies passport signatures and bindings, and cleans rejected candidates in deterministic workflows.
- Public mutation APIs reject unknown/trailing JSON, authentication and authorization remain tenant-scoped, public credentials are not forwarded upstream, and webhook destinations reject non-public address classes.
- Public product journeys cover endpoint identity and plans, admission, secrets, governed external
  fallback, alerts, encrypted async inference, sessions, replay, capacity/cost evidence, SLOs,
  recommendations, Burst Guard, Inference Lab, cleanup ownership, and oversized HTTP headers without
  direct database access.
- Store/event/resource boundaries prevent unbounded public history and provider/subprocess diagnostic growth; 64 concurrent PostgreSQL operation claims produced exactly one owner per operation.
- The rebuilt race suite, dependency audits, vet, generated-contract checks, Docker integration, disposable Kind lifecycle, migration-prefix/restore checks, and local acceptance gates are the final local verification boundary.

See the [failure research ledger](/testing/failure-research-ledger) for the per-hypothesis source, applicability, reproduction, and disposition.

## Real Infrastructure Tests Still Required

- RunPod: elastic allocation, lost create acknowledgement, image pull/runtime readiness, restart/disconnect, eventual deletion, and clean billable inventory.
- AWS: EC2 client-token behavior, insufficient capacity/quota, IAM propagation, VPC/ENI reachability, GPU startup, and termination visibility.
- GCP: same-intent adoption, same-name conflict rejection, quota/IAM/VPC behavior, operation polling, GPU startup, and deletion inventory.
- Kubernetes: real GPU scheduling, eviction/node drain, ingress/service propagation, KServe initialization/status, UID replacement, and finalizer behavior.
- vLLM: patched runtime qualification, real model identity, protocols, tool/structured output, overload, cancellation, CUDA/NCCL/OOM behavior, and long-stream drain.
- SGLang/custom OCI: exact image/runtime capability, probe, metrics, cancellation, shutdown, and generation-drain qualification.
- Serverless: cold/warm/zero/cold timing, worker-state lag, cancellation, lost-create-response adoption, billing accounting, and eventual endpoint deletion.
- Streaming/draining: real TCP/proxy buffering, slow clients, router/control-plane restart during a long generation, and deletion only after the final stream releases.

The safe procedures, evidence requirements, cost/risk boundaries, and cleanup steps are specified in [manual edge cases](/testing/manual-edge-cases).

## Research Sources

Primary sources that materially influenced the work include:

- [AWS EC2 API idempotency](https://docs.aws.amazon.com/ec2/latest/devguide/ec2-api-idempotency.html)
- [AWS Builders' Library: Making retries safe with idempotent APIs](https://d1.awsstatic.com/builderslibrary/pdfs/making-retries-safe-with-idempotent-apis-malcolm-featonby.pdf)
- [PostgreSQL COMMIT](https://www.postgresql.org/docs/current/sql-commit.htm) and [protocol flow](https://www.postgresql.org/docs/current/protocol-flow.html)
- [Kubernetes API concepts](https://kubernetes.io/docs/reference/using-api/api-concepts), [finalizers](https://kubernetes.io/docs/concepts/overview/working-with-objects/finalizers/), and [Deployment status](https://kubernetes.io/docs/reference/kubernetes-api/workload-resources/deployment-v1/)
- [KServe CRD API](https://kserve.github.io/website/docs/reference/crd-api) and [debugging guide](https://kserve.github.io/website/docs/developer-guide/debugging)
- [RunPod Serverless endpoint operations](https://docs.runpod.io/serverless/endpoints/operation-reference) and [overview](https://docs.runpod.io/serverless/endpoints/overview)
- [Go vulnerability GO-2024-2963](https://pkg.go.dev/vuln/GO-2024-2963)
- [vLLM GHSA-rxc4-3w6r-4v47](https://github.com/vllm-project/vllm/security/advisories/GHSA-rxc4-3w6r-4v47)
- [PostgreSQL CVE-2025-12818](https://www.postgresql.org/support/security/CVE-2025-12818/)
- [RFC 6598](https://www.rfc-editor.org/info/rfc6598/) and the [IANA special-purpose address registries](https://www.iana.org/assignments/iana-ipv4-special-registry/iana-ipv4-special-registry.xhtml)

## Final Verdict

EDGE-CASE HARDENING COMPLETE — REAL-INFRASTRUCTURE ADVERSARIAL TESTS REMAIN

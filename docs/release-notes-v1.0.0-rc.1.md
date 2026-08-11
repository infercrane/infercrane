# InferCrane v1.0.0-rc.1 release notes

`v1.0.0-rc.1` is the automated implementation candidate for InferCrane's portable inference
release and operations control plane. It is not a stable or publicly qualified release until the
consolidated real-provider workflow is complete.

## Product surface

- Durable asynchronous deployment, update, scale, rollback, and delete operations.
- Provider Contract V1 adapters for RunPod elastic/serverless, AWS EC2 BYOC, namespaced Kubernetes,
  and governed OpenAI-compatible external capacity.
- Runtime Contract V1 profiles for vLLM, SGLang, and immutable custom OCI workloads.
- Release Guard V2, bounded explicit validation, restart-safe rollback monitoring, and signed
  Inference Passports.
- Reproducible AIPerf history, cold-start evidence, capacity/SLO recommendations, and deterministic
  explanations.
- CLI/TUI, embedded operational dashboard, OpenAPI, Python and TypeScript SDKs, Terraform provider,
  and GitHub delivery action.

## v1 hardening

- DeploymentSpec is explicitly `infercrane.dev/v1`; legacy headerless specs continue to load as v1.
- Every historical migration prefix is upgraded in PostgreSQL tests. Migration checksums, gaps,
  unknown future migrations, and concurrent startup are verified fail-closed.
- The base production Compose stack is provider-neutral. RunPod, AWS, and Kubernetes configuration
  is supplied through explicit overlays.
- The production image runs as a non-root user and includes checksum-pinned AWS CLI v2 and `kubectl`
  boundaries on amd64 and arm64. Development fakes remain outside the runtime target.
- Release archives use the exact RC version and include checksums, SPDX SBOMs, native command smoke
  tests, and a generated Homebrew formula.

## Qualification state

| Evidence | State |
| --- | --- |
| Automated local/Docker/Kind/package gates | Pending final evidence commit |
| RunPod elastic and Serverless | Deferred to consolidated manual qualification |
| AWS EC2 BYOC vLLM/SGLang/custom OCI | Deferred to consolidated manual qualification |
| Kubernetes GPU vLLM/SGLang/custom OCI | Deferred to consolidated manual qualification |
| Public performance or cost claim | None |

## Known limitations

- Provider and runtime combinations not listed by `infercrane integrations` are unqualified.
- AWS support is the narrow private-network EC2 BYOC adapter, not SageMaker or an automatic AWS
  abstraction. Kubernetes is namespace-scoped Deployment/Service or standard KServe ownership,
  not a custom operator or Kubernetes distribution.
- RunPod is the only provider-native Serverless adapter in this RC.
- Advanced Dynamo, llm-d, and distributed KV/cache topologies are not qualified product paths.
- Recommendations are evidence-based and read-only; InferCrane does not autonomously choose or buy
  hardware.
- Cost remains unavailable without a sourced, timestamped provider signal.
- Cold-start substages hidden by a provider remain unavailable.
- Durable Session identity does not guarantee durable KV state and remains outside this RC.
- SDK, Terraform Registry, GitHub Marketplace, Homebrew, image, and release publication are deferred.

See [Upgrade and compatibility](/upgrade), [Release packaging](/release-packaging), and
[Release acceptance](/release-acceptance).

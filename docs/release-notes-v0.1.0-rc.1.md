# InferCrane v0.1.0-rc.1 release notes

Status: **qualification pending — do not publish these notes as a completed release**

InferCrane provides production-oriented vLLM inference lifecycle management on RunPod without
requiring Kubernetes. The primary workflow is:

```console
infercrane deploy Qwen/Qwen3-8B
```

This candidate includes durable lifecycle operations, bounded autoscaling, immutable revisions,
guarded promotion and rollback, RunPod Serverless, reproducible AIPerf integration, grounded
cold-start evidence, and deterministic operational explanations. Release Guard decisions are
policy-driven, persisted, and auditable; no LLM decides whether a revision is promoted.

## Qualification evidence

Replace every pending field from the sanitized artifacts in
[the release acceptance record](release-acceptance.md). Do not infer a result from unit tests or
fake workers.

| Evidence | Required value |
|---|---|
| Final commit | pending |
| Tag | `v0.1.0-rc.1` (pending) |
| Automated verification | pending command log |
| Elastic RunPod lifecycle | pending operation/revision/pod identifiers and sanitized log |
| RunPod Serverless lifecycle | pending endpoint identifiers and sanitized log |
| Zero-resource inventory | pending final provider inventory |
| AIPerf benchmark | pending benchmark ID, methodology, result, and reproduction command |
| Release Guard rejection | pending evaluation ID, policy, metrics, and deterministic reasons |
| Archive checksums/SBOMs | pending release artifacts |
| Container digest/SBOM/provenance | pending immutable digest artifact |
| Homebrew clean install | pending formula/checksum/install log |
| Terminal demonstration | pending recording no longer than 60 seconds |

No performance multiplier, latency advantage, cost saving, or provider price is claimed until the
corresponding reproducible evidence is attached above.

## Security and privacy defaults

- Prompt and generated-output content are not persisted by default.
- Benchmark records remain in the operator's PostgreSQL database and are not uploaded by default.
- Shadow traffic is not implemented; user requests are never duplicated silently.
- Public CLI lifecycle workflows use the authenticated control-plane API and do not receive
  PostgreSQL or provider credentials.

## Known limitations

- Infrastructure support is RunPod only. Elastic provisioning uses SkyPilot; serverless uses
  provider-native RunPod Serverless.
- The inference runtime is vLLM only. InferCrane does not select a cloud, GPU, or runtime
  automatically.
- Kubernetes, AWS, other GPU clouds, SGLang, LMCache, distributed KV, and managed compute are not
  included.
- Cold-start classification and gateway first-response timing are grounded, but RunPod does not
  expose trustworthy per-request capacity, container, artifact, model-load, runtime-readiness,
  time-to-ready, or true first-token substage timestamps. Those values remain unavailable.
- Durable Session identity is deferred to v0.2. v0.1 does not promise session affinity or durable
  KV state.
- Provider pricing is unavailable unless a trustworthy measured source and timestamp are present;
  InferCrane does not fabricate cost estimates.
- Real production limits still depend on the completed acceptance and soak evidence linked above.

## Upgrade and rollback

This is the first public release candidate. Back up PostgreSQL before upgrading, retain the exact
image digest and archive checksum, and verify embedded forward-only migrations in a staging copy.
Rollback the process/image only after confirming schema compatibility; use InferCrane revision
rollback for model/runtime candidates rather than rewriting database state.

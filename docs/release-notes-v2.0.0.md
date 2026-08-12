# InferCrane v2.0.0

v2.0 adds Context Passport logical session identity, fail-closed delegated request-survival contracts,
and policy-bounded Burst Guard. Context Passport does not guarantee durable KV, and Burst Guard cannot
bypass the existing external privacy and hard-budget policy.

The stable release also includes the edge-case hardening recorded in the
[final report](/testing/edge-case-final-report): durable lease fencing, semantic idempotency, safer
provider adoption, generation-safe routing, isolated control loops, bounded diagnostics, stricter API
parsing, webhook SSRF protection, and cryptographically bound Inference Passports.

Real-provider and GPU-runtime qualification remains environment-specific. Follow the
[manual edge-case procedures](/testing/manual-edge-cases) before using a provider/runtime combination
for a critical workload.

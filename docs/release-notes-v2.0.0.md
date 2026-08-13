# InferCrane v2.0.0

v2.0 adds a project-based inference workflow, one read-only operations view, revision-bound signed
semantic evaluation evidence, safe environment promotion, artifact-cache intent and observation,
and a read-only MCP server for coding agents. It also adds Context Passport logical session identity,
fail-closed delegated request-survival contracts, and policy-bounded Burst Guard. Context Passport
does not guarantee durable KV, and Burst Guard cannot bypass the existing external privacy and
hard-budget policy.

The local project path is now explicit:

```bash
infercrane workload init my-model --model mistralai/Mistral-7B-Instruct-v0.3
cd my-model
infercrane workload validate
infercrane workload plan
infercrane workload deploy
```

For custom OCI projects, `build` and `deploy` remain separate mutation boundaries. Local images are
not treated as deployable artifacts: a pushed workload must resolve to an immutable registry digest
before deployment. vLLM and SGLang model projects use their qualified runtime adapters and do not
require an InferCrane-managed image build.

Release Guard can require signed aggregate quality evidence from an external evaluator. Evidence is
bound to one deployment revision, evaluator version, suite version, artifact digest, and sample count.
InferCrane compares only compatible evidence and makes the threshold decision deterministically; it
does not inspect prompts or let an LLM decide promotion.

The stable release also includes the edge-case hardening recorded in the
[final report](/testing/edge-case-final-report): durable lease fencing, semantic idempotency, safer
provider adoption, generation-safe routing, isolated control loops, bounded diagnostics, stricter API
parsing, webhook SSRF protection, and cryptographically bound Inference Passports.

Real-provider and GPU-runtime qualification remains environment-specific. Follow the
[manual edge-case procedures](/testing/manual-edge-cases) before using a provider/runtime combination
for a critical workload.

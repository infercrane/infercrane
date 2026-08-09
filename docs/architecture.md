# Architecture

For the engineering-level component map, flows and invariants, see
[docs/architecture](architecture/system.mdx).

InferCrane separates its control plane from its request data plane.

The control plane owns logical deployments, targets, desired state, observed state,
routing configuration, reconciliation, provider resource identities, and events. PostgreSQL
is the source of truth and supports multiple gateway replicas.

The gateway owns the stable OpenAI-compatible endpoint. It resolves a logical model alias,
rewrites it to the upstream served model name, and forwards the request to a deployment-local
router. It does not select workers.

The router owns per-request worker selection. Stage 1 integrates the upstream Apache-2.0
vLLM Router as a supervised external process. InferCrane does not embed or fork its routing
algorithms. A router generation is reconstructible from persisted desired state.

Provisioners are adapters. Stage 1 implements existing targets. Stage 2 adds SkyPilot.
Additional schedulers and runtime platforms are outside the v0.1 architecture.

The product is a single Go binary. Package boundaries keep the gateway data plane independent
of PostgreSQL reads, and provider/runtime/router integrations sit behind small interfaces. The
standard library HTTP stack supplies streaming and connection pooling.

## Failure model

The last valid gateway routing snapshot remains usable if reconciliation or PostgreSQL is
temporarily unavailable. Router circuit breakers provide immediate worker exclusion; health
reconciliation persists observed failures and updates effective membership. Router failure
returns a bounded OpenAI-compatible 503 until supervision restores it.

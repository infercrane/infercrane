---
title: Security
description: Authentication, tenant isolation, secret references, network boundaries, and secure deployment guidance.
---

# Security

InferCrane authenticates the control and data planes with bearer credentials and tenant-scopes public API reads and writes. Use separate, least-privilege provider credentials and rotate them through your secret manager. Production PostgreSQL must use TLS; back up and restore it as the lifecycle source of truth.

For the path from a local proof to a shared endpoint, pair this page with
[Share a development endpoint safely](/runbooks/shared-development),
[endpoint admission and distributed tenant quotas](/features/admission-async),
[protocol qualification](/features/protocols#qualify-streaming-cancellation-quotas-and-retries-together),
and the [Request Inspector contract](/features/adoption-diagnostics#inspect-one-request).

The identity boundary is a tenant. Principals are service accounts with a role ceiling and
explicit action scopes. A scope can remove a role permission but cannot add one. Credentials are
shown once, stored only as SHA-256 digests, and support rotation and revocation.
Existing legacy principals are migrated to their previous explicit action set; new secret and
external-capacity permissions are never granted implicitly during upgrade.

Gateway authentication uses an in-memory last-known-good credential snapshot so an established data
plane does not add PostgreSQL latency or fail immediately during a database outage. Rotation and
revocation take effect on the next successful snapshot refresh. If immediate revocation is required
while PostgreSQL is unreachable, restore authoritative database access or fence/restart the affected
gateway instances; InferCrane cannot infer a revocation it cannot read.

Secret objects are references, not a secret store. For example:

```bash
export OPENROUTER_API_KEY='...'
infercrane secret create openrouter --from-env OPENROUTER_API_KEY
```

PostgreSQL stores the resolver (`env`) and reference (`OPENROUTER_API_KEY`), never the environment
value. Resolved values stay in process memory and are excluded from API responses, logs, audit
payloads and qualification evidence. Operators should inject referenced variables from their
existing secret manager and restrict the control-plane process environment.

## Validate provider access without enabling traffic

Infrastructure adapters expose provider-specific read-only checks:

```bash
infercrane doctor --cloud       # SkyPilot/provider credentials
infercrane doctor --serverless  # RunPod template and API access
infercrane doctor --aws         # AWS identity and configuration reads
infercrane doctor --gcp         # GCP identity and Compute API reads
infercrane doctor --kubernetes  # Kubernetes API and RBAC reads
```

For an external model API, create only a reference-backed connection and inspect its redacted
metadata:

```bash
export OPENROUTER_API_KEY='injected-by-your-secret-manager'
infercrane provider connect openrouter-main \
  --adapter openrouter \
  --model openai/gpt-oss-120b \
  --from-env OPENROUTER_API_KEY
infercrane provider list --output json
```

Creating and listing the connection does not send a prompt, test request, or authorize spend. It
proves that InferCrane can resolve the named reference, not that the upstream credential or model is
valid. There is no generic no-traffic API credential probe in the current release. Validate real
provider access through that provider's documented metadata/auth endpoint or an explicitly approved,
budgeted staging request. Never put a credential in a URL, CLI argument value, DeploymentSpec,
ticket, log, or qualification artifact.

Prompt and output content are not recorded by default. Request telemetry stores identifiers, dimensions, status, timing, and token counts. AIPerf uses metrics-only record exports; InferCrane does not upload benchmark data. Shadow traffic is not implemented, so requests are never duplicated silently.

Durable async inference is the explicit exception. It remains disabled until an operator injects
`INFERCRANE_ASYNC_ENCRYPTION_KEY`, and every submission must acknowledge encrypted content storage.
Request and result bodies are stored only as AES-256-GCM ciphertext bound to tenant and job identity.
Completion webhooks are signed, HTTPS-only, redirect-free and resolved through an SSRF-hardened
transport. Webhook payloads contain results, so their destinations are part of the application data
boundary. Synchronous inference remains content-free by default.

External fallback is disabled by default. Enabling it requires a persisted acknowledgement that
prompt and output data can leave controlled infrastructure, plus atomic hard request and worst-case
cost reservations. Selection happens before transmission; InferCrane does not replay a stream or
retry after bytes may have reached an external provider.

The complete commands are in [Keep sensitive inputs self-hosted](/integrations/overview#keep-sensitive-inputs-on-self-hosted-infrastructure)
and [Stable endpoints](/features/endpoints#add-an-authenticated-managed-api). A provider connection
alone enables neither traffic nor spend. If data residency cannot be proven, keep a one-binding
`manual` plan containing only self-hosted capacity. If trustworthy external pricing or a defensible
worst-case per-request reservation is unavailable, do not pass `--enable-external`; cost limits are
authorization ceilings, not provider invoice evidence.

AWS BYOC uses STS role assumption for each provider call and passes temporary credentials only to
the child AWS CLI process. EC2 receives no public IP. The worker retrieves its API key from AWS
Secrets Manager through a narrowly scoped instance profile; the control plane stores the secret ARN,
not the value. Require an external ID, restrict trust and permissions, and use tag conditions in
production.

The Kubernetes adapter always uses an explicit context and namespace. Its reference Role contains no
wildcards and cannot read Secrets, mutate RBAC, or create Pods directly. Workers read one named Secret
through their service account; the control plane stores only its name and key. Strict server-side
apply preserves field ownership and never force-steals conflicts. InferCrane deletes only exact names
with matching durable ownership labels and annotations.

Run the container as its non-root user, pin immutable image tags, restrict network access, and protect `/metrics` according to your environment. Report vulnerabilities through the repository's private GitHub security-advisory flow; do not open a public issue.

# Security

InferCrane authenticates the control and data planes with bearer credentials and tenant-scopes public API reads and writes. Use separate, least-privilege provider credentials and rotate them through your secret manager. Production PostgreSQL must use TLS; back up and restore it as the lifecycle source of truth.

The v0.3 identity boundary is a tenant. Principals are service accounts with a role ceiling and
explicit action scopes. A scope can remove a role permission but cannot add one. Credentials are
shown once, stored only as SHA-256 digests, and support rotation and revocation.
Existing pre-v0.3 principals are migrated to their previous explicit action set; new secret and
external-capacity permissions are never granted implicitly during upgrade.

Secret objects are references, not a secret store. For example:

```bash
export OPENROUTER_API_KEY='...'
infercrane secret create openrouter --from-env OPENROUTER_API_KEY
```

PostgreSQL stores the resolver (`env`) and reference (`OPENROUTER_API_KEY`), never the environment
value. Resolved values stay in process memory and are excluded from API responses, logs, audit
payloads and qualification evidence. Operators should inject referenced variables from their
existing secret manager and restrict the control-plane process environment.

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

Run the container as its non-root user, pin immutable image tags, restrict network access, and protect `/metrics` according to your environment. Report vulnerabilities privately as described in the repository [security policy](https://github.com/infercrane/infercrane/blob/main/SECURITY.md).

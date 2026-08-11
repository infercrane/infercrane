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

External fallback is disabled by default. Enabling it requires a persisted acknowledgement that
prompt and output data can leave controlled infrastructure, plus atomic hard request and worst-case
cost reservations. Selection happens before transmission; InferCrane does not replay a stream or
retry after bytes may have reached an external provider.

AWS BYOC uses STS role assumption for each provider call and passes temporary credentials only to
the child AWS CLI process. EC2 receives no public IP. The worker retrieves its API key from AWS
Secrets Manager through a narrowly scoped instance profile; the control plane stores the secret ARN,
not the value. Require an external ID, restrict trust and permissions, and use tag conditions in
production.

Run the container as its non-root user, pin immutable image tags, restrict network access, and protect `/metrics` according to your environment. Report vulnerabilities privately as described in the repository [security policy](https://github.com/infercrane/infercrane/blob/main/SECURITY.md).

# Security

InferCrane authenticates the control and data planes with bearer credentials and tenant-scopes public API reads and writes. Use separate, least-privilege provider credentials and rotate them through your secret manager. Production PostgreSQL must use TLS; back up and restore it as the lifecycle source of truth.

Prompt and output content are not recorded by default. Request telemetry stores identifiers, dimensions, status, timing, and token counts. AIPerf uses metrics-only record exports; InferCrane does not upload benchmark data. Shadow traffic is not implemented, so requests are never duplicated silently.

Run the container as its non-root user, pin immutable image tags, restrict network access, and protect `/metrics` according to your environment. Report vulnerabilities privately as described in the repository [security policy](https://github.com/infercrane/infercrane/blob/main/SECURITY.md).

# Dependencies

| Project | Purpose | License | Integration |
|---|---|---|---|
| [vLLM Router 0.1.15](https://github.com/vllm-project/router) | Request routing | Apache-2.0 | Pinned upstream process packaged in the production image |
| [vLLM](https://github.com/vllm-project/vllm) | Inference runtime | Apache-2.0 | External worker runtime |
| [SkyPilot](https://github.com/skypilot-org/skypilot) | GPU provisioning | Apache-2.0 | External CLI |
| [pgx](https://github.com/jackc/pgx) | PostgreSQL connectivity | MIT | Go driver |
| [yaml.v3](https://pkg.go.dev/gopkg.in/yaml.v3) | Deployment files | Apache-2.0 / MIT | Go library |
| [Cobra](https://github.com/spf13/cobra) | CLI command tree, help, suggestions, and completion | Apache-2.0 | Go library |

InferCrane does not fork or embed vLLM, vLLM Router, or SkyPilot. Release automation must pin
external tool versions and audit module/container transitive licenses and vulnerabilities.
No AGPL, SSPL, BSL, Commons Clause, or other source-available dependency is approved for the
core.

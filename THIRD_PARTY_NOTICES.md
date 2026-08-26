# Third-party notices

InferCrane is distributed under the MIT License. The release binary and optional
integration workflows also rely on third-party software under their own licenses.

The authoritative machine-readable inventory for each release is the SPDX SBOM
published beside that release. Source distributions retain dependency metadata in
`go.mod`, `go.sum`, and the separate module manifests under `integrations/` and
`sdk/`.

Notable components used by the core binary include:

| Component | License | Source |
| --- | --- | --- |
| Cobra | Apache-2.0 | https://github.com/spf13/cobra |
| pgx | MIT | https://github.com/jackc/pgx |
| yaml.v3 | Apache-2.0 and MIT | https://github.com/go-yaml/yaml |

Optional runtimes, providers, command-line tools, and services such as vLLM,
SGLang, vLLM Router, SkyPilot, AWS, Google Cloud, Kubernetes, LiteLLM, OpenRouter,
and Terraform are separate works. They are not relicensed by InferCrane. Operators
must follow the terms that apply to the exact components and services they choose.

See [docs/dependencies.md](docs/dependencies.md) for the maintained dependency and
integration ownership matrix.

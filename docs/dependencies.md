# Dependencies

| Project | Purpose | License | Integration |
|---|---|---|---|
| [vLLM Router 0.1.15](https://github.com/vllm-project/router) | Request routing | Apache-2.0 | Pinned upstream process packaged in the production image |
| [vLLM](https://github.com/vllm-project/vllm) | Inference runtime | Apache-2.0 | External worker runtime; CUDA image `vllm/vllm-openai` and ROCm image `vllm/vllm-openai-rocm`, both pinned by digest and pulled by the provider |
| [SGLang](https://github.com/sgl-project/sglang) | Inference runtime | Apache-2.0 | External worker runtime; CUDA and ROCm (`-rocm720-mi30x`, `-mi35x`) images of `lmsysorg/sglang`, pinned by digest and pulled by the provider |
| [AMD GPU device plugin and node labeller](https://github.com/ROCm/k8s-device-plugin) | Exposes AMD GPUs to Kubernetes as `amd.com/gpu` and labels nodes with `amd.com/gpu.product-name` | Apache-2.0 | Operator-installed in their cluster; optional; never installed by InferCrane |
| [AMD Device Metrics Exporter](https://github.com/ROCm/device-metrics-exporter) | AMD GPU telemetry read by `infercrane telemetry collect amd` | Apache-2.0 | Operator-run Prometheus endpoint; optional; only its text output is parsed |
| [SkyPilot](https://github.com/skypilot-org/skypilot) | GPU provisioning | Apache-2.0 | External CLI |
| [AWS CLI v2](https://github.com/aws/aws-cli) | Narrow EC2 BYOC API and STS boundary | Apache-2.0 | External CLI; optional |
| [kubectl](https://github.com/kubernetes/kubectl) | Namespaced Kubernetes API boundary and server-side apply | Apache-2.0 | External CLI; optional |
| [Kind](https://github.com/kubernetes-sigs/kind) | Disposable Kubernetes lifecycle qualification | Apache-2.0 | Downloaded development tool; never linked or redistributed |
| [pgx](https://github.com/jackc/pgx) | PostgreSQL connectivity | MIT | Go driver |
| [yaml.v3](https://pkg.go.dev/gopkg.in/yaml.v3) | Deployment files | Apache-2.0 / MIT | Go library |
| [Cobra](https://github.com/spf13/cobra) | CLI command tree, help, suggestions, and completion | Apache-2.0 | Go library |
| [Bubble Tea](https://github.com/charmbracelet/bubbletea) | Full-screen terminal views (`ui`, `optimize watch`) | MIT | Go library |
| [Lip Gloss](https://github.com/charmbracelet/lipgloss) | Terminal styling, cards, and layout for human CLI output | MIT | Go library |
| [colorprofile](https://github.com/charmbracelet/colorprofile), [x/term](https://github.com/charmbracelet/x), [x/ansi](https://github.com/charmbracelet/x) | Terminal color depth, TTY and width detection, and ANSI-aware text width; already used by Lip Gloss | MIT | Go libraries |
| [Terraform Plugin Framework](https://github.com/hashicorp/terraform-plugin-framework) | Terraform protocol and resource implementation | MPL-2.0 | Separate provider module; communicates only with the control API |
| [Terraform Plugin Testing](https://github.com/hashicorp/terraform-plugin-testing) | Real protocol CRUD/import acceptance | MPL-2.0 | Development-only provider test dependency |
| [Terraform CLI](https://github.com/hashicorp/terraform) | Protocol acceptance runner | BSL-1.1 for current releases | Downloaded, checksum-verified development tool; never linked, embedded, or redistributed |

InferCrane does not fork or embed vLLM, vLLM Router, SGLang, or SkyPilot. The ROCm runtime images
bundle ROCm and AMD's AITER kernels (MIT) under their own licenses; InferCrane does not build or
redistribute them. Release automation must pin
external tool versions and audit module/container transitive licenses and vulnerabilities.
The Python SDK has no runtime dependency. The TypeScript SDK has no runtime dependency; TypeScript
and Node type declarations are development-only build dependencies.
No AGPL, SSPL, BSL, Commons Clause, or other source-available dependency is approved for the
core. The current Terraform CLI is a separately executed, development-only qualification tool;
InferCrane does not redistribute it. Operators may use an OpenTofu-compatible workflow after that
integration has been independently qualified.

OpenRouter and generic OpenAI-compatible APIs are remote integrations, not linked dependencies or
resold services. Their terms, data handling, and charges remain between the operator and provider.
LiteLLM is connected through its operator-managed OpenAI-compatible endpoint; InferCrane does not
fork, embed, install, or redistribute it. Its repository licenses content outside the separately
identified enterprise area under MIT; any future managed-process integration must pin and audit the
exact distributed paths rather than treating the whole repository as uniformly licensed. E2B,
Modal, Kubernetes sandbox implementations, MLflow,
Kubeflow, SkyPilot training pipelines, and similar systems are external composition owners rather
than InferCrane dependencies. Their licenses, credentials, isolation, data handling, execution, and
charges remain with the operator and the chosen system.

Candidate integration licenses and ownership decisions are tracked in
[Integration ownership and license matrix](/integrations/ownership). Listing a
candidate there does not register or qualify it as a supported dependency.

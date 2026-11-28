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
| [jsonschema-go](https://github.com/google/jsonschema-go) | Validates tool-call arguments against the JSON Schema an eval set declares, in the built-in answer check | MIT | Go library; already in the module graph through the MCP Go SDK. Schemas come from the customer's eval set; remote `$ref` is refused, so validation never fetches anything |
| [Cobra](https://github.com/spf13/cobra) | CLI command tree, help, suggestions, and completion | Apache-2.0 | Go library |
| [Bubble Tea](https://github.com/charmbracelet/bubbletea) | Full-screen terminal views (`ui`, `optimize watch`) | MIT | Go library |
| [Lip Gloss](https://github.com/charmbracelet/lipgloss) | Terminal styling, cards, and layout for human CLI output | MIT | Go library |
| [colorprofile](https://github.com/charmbracelet/colorprofile), [x/term](https://github.com/charmbracelet/x), [x/ansi](https://github.com/charmbracelet/x) | Terminal color depth, TTY and width detection, and ANSI-aware text width; already used by Lip Gloss | MIT | Go libraries |
| [Terraform Plugin Framework](https://github.com/hashicorp/terraform-plugin-framework) | Terraform protocol and resource implementation | MPL-2.0 | Separate provider module; communicates only with the control API |
| [Terraform Plugin Testing](https://github.com/hashicorp/terraform-plugin-testing) | Real protocol CRUD/import acceptance | MPL-2.0 | Development-only provider test dependency |
| [Terraform CLI](https://github.com/hashicorp/terraform) | Protocol acceptance runner | BSL-1.1 for current releases | Downloaded, checksum-verified development tool; never linked, embedded, or redistributed |

## Runtime image overrides

The control-plane image pins its tools in the root `Dockerfile`. kubectl is `v1.37.3`, verified
against the `.sha256` files Kubernetes publishes on `dl.k8s.io`. Three Python packages are replaced
after pip resolves the pinned tools, because a pinned tool declares a range that only allows
vulnerable versions:

| Package | Installed | Resolver would install | Held back by | Drop the override when |
|---|---|---|---|---|
| Pillow | 13.0.0 | 12.2.0 | AIPerf 0.12.0 (`~=12.2.0`) and vastai (`==12.2.0` in every release) | AIPerf and vastai both allow 13.0 |
| aiohttp | 3.14.5 | 3.13.5 | AIPerf 0.12.0 (`~=3.13.3`) | the AIPerf pin moves to 0.14 |
| cryptography | 50.0.2 | 46.0.5 | vastai 1.3.0 (`==46.0.5`) and runpod 1.9.1 (`<47`), both held back by the aiohttp cap | pip picks 50.0 or later on its own |

Known findings still in the image:

- urllib3 2.6.3 in the AWS CLI environment. Every AWS CLI v2 release through 2.42.5 pins
  `urllib3<=2.6.3`, and earlier urllib3 upgrades needed changes to the CLI's bundled botocore, so
  the image does not replace it.
- golang.org/x/net v0.57.0 inside kubectl. v1.37.3 is the newest stable kubectl; the fixed x/net is
  only in the v1.38 pre-releases. Most of these HTTP/2 advisories affect servers; InferCrane runs
  kubectl only as a client of the operator's API server and never as `kubectl proxy`.

## GPU profiling tools

[GPU profiling](/features/gpu-profiling) reads the output of profilers the operator already runs. None of
them is linked into, embedded in, or redistributed with InferCrane; the Go analysis reads their files.
Licences were checked against each project's repository on 2026-11-17.

| Project | Purpose | License | Integration |
|---|---|---|---|
| [PyTorch profiler (Kineto)](https://github.com/pytorch/kineto) | Traces vLLM and SGLang write from `/start_profile` and `/stop_profile`, on NVIDIA (CUPTI) and AMD (ROCm) | BSD-3-Clause | Runs inside the operator's engine; InferCrane parses its Chrome-trace JSON |
| [rocprofiler-sdk (rocprofv3)](https://github.com/ROCm/rocprofiler-sdk) | AMD kernel, HIP API, and memory-copy traces | MIT | Operator-run CLI; InferCrane parses its CSV output |
| [NVIDIA Nsight Systems](https://developer.nvidia.com/nsight-systems) | Optional NVIDIA timelines | NVIDIA proprietary licence | Optional and operator-installed only; InferCrane parses `nsys stats` CSV exports and, in Accelerator Lab, runs an `nsys` binary already present in a reviewed runtime image |
| [NVIDIA DCGM Exporter](https://github.com/NVIDIA/dcgm-exporter) | Continuous GPU counters | Apache-2.0 (source); NVIDIA's container images are also under the NVIDIA Software License Agreement | Operator-run exporter; InferCrane reads its Prometheus text |
| [Holistic Trace Analysis](https://github.com/facebookresearch/HolisticTraceAnalysis) | Definitions of temporal breakdown, idle time, kernel launch, and communication overlap | MIT | Definitions re-implemented in Go; no code copied, linked, or run |
| [Perfetto UI](https://github.com/google/perfetto) | Full trace viewer | Apache-2.0 | The dashboard opens `ui.perfetto.dev` in a new tab on request and posts it the capture's excerpt; nothing is embedded or redistributed |
| [speedscope](https://github.com/jlfwong/speedscope) | Flame-graph interaction reference | MIT | Design reference for the dashboard's flame graph; no code used |

Evaluated and not integrated: [Parca Agent](https://github.com/parca-dev/parca-agent) with
[parcagpu](https://github.com/parca-dev/parcagpu) (both Apache-2.0) for continuous eBPF and CUPTI
profiling, because it needs a privileged node DaemonSet and an injected CUDA library; and Zymtrace, a
proprietary continuous GPU profiler, which stays an integration option.

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

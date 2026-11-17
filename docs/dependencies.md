# Dependencies

| Project | Purpose | License | Integration |
|---|---|---|---|
| [vLLM Router 0.1.15](https://github.com/vllm-project/router) | Request routing | Apache-2.0 | Pinned upstream process packaged in the production image |
| [vLLM](https://github.com/vllm-project/vllm) | Inference runtime | Apache-2.0 | External worker runtime |
| [SkyPilot](https://github.com/skypilot-org/skypilot) | GPU provisioning | Apache-2.0 | External CLI |
| [AWS CLI v2](https://github.com/aws/aws-cli) | Narrow EC2 BYOC API and STS boundary | Apache-2.0 | External CLI; optional |
| [kubectl](https://github.com/kubernetes/kubectl) | Namespaced Kubernetes API boundary and server-side apply | Apache-2.0 | External CLI; optional |
| [Kind](https://github.com/kubernetes-sigs/kind) | Disposable Kubernetes lifecycle qualification | Apache-2.0 | Downloaded development tool; never linked or redistributed |
| [pgx](https://github.com/jackc/pgx) | PostgreSQL connectivity | MIT | Go driver |
| [yaml.v3](https://pkg.go.dev/gopkg.in/yaml.v3) | Deployment files | Apache-2.0 / MIT | Go library |
| [Cobra](https://github.com/spf13/cobra) | CLI command tree, help, suggestions, and completion | Apache-2.0 | Go library |
| [Terraform Plugin Framework](https://github.com/hashicorp/terraform-plugin-framework) | Terraform protocol and resource implementation | MPL-2.0 | Separate provider module; communicates only with the control API |
| [Terraform Plugin Testing](https://github.com/hashicorp/terraform-plugin-testing) | Real protocol CRUD/import acceptance | MPL-2.0 | Development-only provider test dependency |
| [Terraform CLI](https://github.com/hashicorp/terraform) | Protocol acceptance runner | BSL-1.1 for current releases | Downloaded, checksum-verified development tool; never linked, embedded, or redistributed |

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

InferCrane does not fork or embed vLLM, vLLM Router, or SkyPilot. Release automation must pin
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

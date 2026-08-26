# Runtime image license inventory

The InferCrane runtime image preserves component-native license and notice files and publishes an
SPDX SBOM for the exact image digest. This file identifies the major components added on top of the
base image and where their redistributed license material can be inspected.

| Component | Pinned input | License | Material in the image |
| --- | --- | --- | --- |
| InferCrane and linked Go modules | release tag and `go.sum` | Apache-2.0 and dependency-specific terms | `/usr/share/licenses/infercrane` |
| Python / Debian base | immutable image digest | Python, Debian package-specific, and dependency-specific terms | `/usr/local/lib/python3.12`, `/usr/share/doc` |
| vLLM Router | `VLLM_ROUTER_VERSION` | Apache-2.0 | package metadata plus `/usr/share/licenses/infercrane/LICENSE` |
| SkyPilot with RunPod support | `SKYPILOT_VERSION` | Apache-2.0 | Python distribution license files |
| NVIDIA AIPerf | `AIPERF_VERSION` | Apache-2.0 | Python distribution license and attribution files |
| Hugging Face Hub and Xet support | `HUGGINGFACE_HUB_VERSION` | Apache-2.0 and dependency-specific terms | isolated Python distribution license files |
| AWS CLI v2 | `AWS_CLI_VERSION` and archive checksum | Apache-2.0 and bundled dependency terms | `/usr/share/licenses/infercrane/runtime-components/aws-cli-LICENSE.txt` |
| Google Cloud CLI | `GCLOUD_CLI_VERSION` and archive checksum | Apache-2.0 and bundled dependency terms | `/usr/local/google-cloud-sdk/LICENSE` and its `third_party` license tree |
| kubectl | `KUBECTL_VERSION` and binary checksum | Apache-2.0 | `/usr/share/licenses/infercrane/LICENSE` plus this inventory |

The SBOM and component-native files are authoritative for the exact built image. This inventory is
not permission to use a separately downloaded runtime, model, provider service, or cloud API; those
components retain their own terms.

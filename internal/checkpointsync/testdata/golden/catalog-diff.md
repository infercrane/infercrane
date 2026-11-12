# Checkpoint catalog diff

- Snapshot fetched: 2026-11-11T12:00:00Z
- Snapshot digest: `sha256:3360aeda5c4ed09f9509f0eb2eaabfaf62c6d3ee7e3a8c84a46d51af0d253e39`
- Policy digest: `sha256:04a9159f339bbc2d79b7e902fd5c11ee23a7b59d149d67e21b00802a9cae1094`
- Entries: 12 (5 candidates, 2 pending review)
- Changes: 9 added, 1 revised, 0 changed, 1 removed, 1 unchanged
- Selection: RedHatAI since 2026-08-01 or 10000+ downloads; amd since 2026-08-01 or 10000+ downloads; nvidia since 2026-08-01 or 10000+ downloads; stepfun-ai collections only; collection `nvidia/test-modelopt-checkpoints-0123456789abcdef` (6 models)

Every entry is unmeasured. Publisher tables are priors, not InferCrane evidence.

## Review required

Approve a publisher or format in the policy, then re-run `plan` on the same snapshot.

| Entry | Format | Hardware | Reasons |
| --- | --- | --- | --- |
| [RedHatAI/Qwen3.8-27B-NVFP4](https://huggingface.co/RedHatAI/Qwen3.8-27B-NVFP4) @ `c3c3c3c3c3c3` | llm-compressor mixed-nvfp4-fp8 | nvidia (unknown) | new publisher RedHatAI requires review; new format quantized_checkpoint llm-compressor/mixed-nvfp4-fp8 for RedHatAI requires review |
| [stepfun-ai/Step-Test-NVFP4](https://huggingface.co/stepfun-ai/Step-Test-NVFP4) @ `a8a8a8a8a8a8` | modelopt nvfp4 | nvidia blackwell (runnable) | new publisher stepfun-ai requires review |

## Candidates

Approved, licensed, runnable quantized checkpoints. Proposals still treat them as unmeasured.

| Entry | Base | Format | Hardware | Runtimes | Engine requirements |
| --- | --- | --- | --- | --- | --- |
| [nvidia/DeepSeek-V4.1-Flash-NVFP4](https://huggingface.co/nvidia/DeepSeek-V4.1-Flash-NVFP4) @ `3431dde3247c` | deepseek-ai/DeepSeek-V4.1-Flash (exact) | modelopt nvfp4 | nvidia blackwell (runnable) | sglang, vllm | none declared |
| [nvidia/Hidden-Signal-8B-FP8](https://huggingface.co/nvidia/Hidden-Signal-8B-FP8) @ `b9b9b9b9b9b9` | nvidia/Nemotron-Old-30B-BF16 (publisher_declared) | modelopt fp8 | nvidia hopper, blackwell (runnable) | sglang, vllm | none declared |
| [nvidia/Nemotron-Old-30B-FP8](https://huggingface.co/nvidia/Nemotron-Old-30B-FP8) @ `e7e7e7e7e7e7` | nvidia/Nemotron-Old-30B-BF16 (publisher_declared) | modelopt fp8 | nvidia hopper, blackwell (runnable) | sglang, vllm | none declared |
| [nvidia/Old-Listed-NVFP4](https://huggingface.co/nvidia/Old-Listed-NVFP4) @ `f7f7f7f7f7f7` | zai-org/GLM-5.3-Flash (publisher_declared) | modelopt nvfp4 | nvidia blackwell (runnable) | sglang, vllm | none declared |
| [nvidia/Qwen3.8-27B-NVFP4](https://huggingface.co/nvidia/Qwen3.8-27B-NVFP4) @ `482ca0f38322` | Qwen/Qwen3.8-27B (publisher_declared) | modelopt mixed-nvfp4-fp8 | nvidia blackwell (runnable) | sglang, vllm | none declared |

## Added

| Entry | Base | Format | Hardware | Selected by | Eligibility |
| --- | --- | --- | --- | --- | --- |
| [amd/GLM-5.3-Flash-Quark-MXFP4](https://huggingface.co/amd/GLM-5.3-Flash-Quark-MXFP4) @ `d4d4d4d4d4d4` | zai-org/GLM-5.3-Flash | quark mxfp4 | amd cdna4 (unsupported_hardware) | created 2026-10-25 (on or after 2026-08-01) | catalog only: targets amd hardware the product cannot run yet; tool quark cannot seed product candidates yet |
| [nvidia/DeepSeek-V4.1-Flash-1017-NVFP4](https://huggingface.co/nvidia/DeepSeek-V4.1-Flash-1017-NVFP4) @ `3ae9c0d3fbbe` | deepseek-ai/DeepSeek-V4.1-Flash-1017 | modelopt nvfp4 | nvidia blackwell (runnable) | created 2026-10-30 (on or after 2026-08-01) | catalog only: every reviewed runtime needs an unreleased or blocked engine change |
| [nvidia/GLM-5.3-Flash-FP4-Local](https://huggingface.co/nvidia/GLM-5.3-Flash-FP4-Local) @ `a1a1a1a1a1a1` | zai-org/GLM-5.3-Flash | modelopt mixed-nvfp4-fp8 | nvidia blackwell (runnable) | created 2026-10-08 (on or after 2026-08-01) | catalog only: license "other" is unresolved |
| [nvidia/Hidden-Signal-8B-FP8](https://huggingface.co/nvidia/Hidden-Signal-8B-FP8) @ `b9b9b9b9b9b9` | nvidia/Nemotron-Old-30B-BF16 | modelopt fp8 | nvidia hopper, blackwell (runnable) | 15000 downloads in 30 days (at least 10000); listed in collection nvidia/test-modelopt-checkpoints-0123456789abcdef | candidate |
| [nvidia/Nemotron-Old-30B-FP8](https://huggingface.co/nvidia/Nemotron-Old-30B-FP8) @ `e7e7e7e7e7e7` | nvidia/Nemotron-Old-30B-BF16 | modelopt fp8 | nvidia hopper, blackwell (runnable) | 250000 downloads in 30 days (at least 10000) | candidate |
| [nvidia/Old-Listed-NVFP4](https://huggingface.co/nvidia/Old-Listed-NVFP4) @ `f7f7f7f7f7f7` | zai-org/GLM-5.3-Flash | modelopt nvfp4 | nvidia blackwell (runnable) | listed in collection nvidia/test-modelopt-checkpoints-0123456789abcdef | candidate |
| [nvidia/Qwen3.8-27B-DSpark](https://huggingface.co/nvidia/Qwen3.8-27B-DSpark) @ `b2b2b2b2b2b2` | Qwen/Qwen3.8-27B | speculator dspark | nvidia hopper, blackwell (runnable) | created 2026-10-20 (on or after 2026-08-01) | catalog only: speculator: speculative-decoding candidates need acceptance and output-parity gates first |
| [RedHatAI/Qwen3.8-27B-NVFP4](https://huggingface.co/RedHatAI/Qwen3.8-27B-NVFP4) @ `c3c3c3c3c3c3` | Qwen/Qwen3.8-27B | llm-compressor mixed-nvfp4-fp8 | nvidia (unknown) | created 2026-09-18 (on or after 2026-08-01) | catalog only: review pending; target hardware is not reviewed; no reviewed runtime |
| [stepfun-ai/Step-Test-NVFP4](https://huggingface.co/stepfun-ai/Step-Test-NVFP4) @ `a8a8a8a8a8a8` | stepfun-ai/Step-Test | modelopt nvfp4 | nvidia blackwell (runnable) | listed in collection nvidia/test-modelopt-checkpoints-0123456789abcdef | catalog only: review pending |

## New revisions

A new revision of a reviewed publisher and format passes an automated re-review unless its kind, base, format, hardware vendor, or license changed.

| Repository | From | To |
| --- | --- | --- |
| nvidia/DeepSeek-V4.1-Flash-NVFP4 | 3431dde3247c | c48dca0146b5 current: catalog only: every reviewed runtime needs an unreleased or blocked engine change<br>3431dde3247c superseded: candidate |

## Changed

None.

## Removed

| Entry | Reason |
| --- | --- |
| [nvidia/GLM-5.3-Flash-NVFP4](https://huggingface.co/nvidia/GLM-5.3-Flash-NVFP4) @ `09b04e5e74bc` | repository unavailable upstream (status 404) |

## Skipped repositories

Repositories the selection rule picked that did not become entries.

### library is not served by an LLM engine (1)

nvidia/Cosmos3-Nano-FP8

### listed in a reviewed collection, but its organization is not a publisher in the policy (1)

black-forest-labs/FLUX.3-Test-NVFP4

### no declared base model (1)

RedHatAI/GLM-5.3-Flash

### not a text-generation model (1)

nvidia/bigvgan-test-22khz

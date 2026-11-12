# Checkpoint catalog diff

- Snapshot fetched: 2026-11-11T12:00:00Z
- Snapshot digest: `sha256:1f7bc009bceb7ed8fc17f23eb1c2ed1017261dcfed99937a799c06345ad19b9b`
- Policy digest: `sha256:2bd0abcf7b2cd5b504595f00a6207d8b3968d6613c3fcb2132636f66f1c00704`
- Entries: 8 (2 candidates, 1 pending review)
- Changes: 5 added, 1 revised, 0 changed, 1 removed, 1 unchanged

Every entry is unmeasured. Publisher tables are priors, not InferCrane evidence.

## Review required

Approve a publisher or format in the policy, then re-run `plan` on the same snapshot.

| Entry | Format | Hardware | Reasons |
| --- | --- | --- | --- |
| [RedHatAI/Qwen3.8-27B-NVFP4](https://huggingface.co/RedHatAI/Qwen3.8-27B-NVFP4) @ `c3c3c3c3c3c3` | llm-compressor mixed-nvfp4-fp8 | nvidia (unknown) | new publisher RedHatAI requires review; new format quantized_checkpoint llm-compressor/mixed-nvfp4-fp8 for RedHatAI requires review |

## Candidates

Approved, licensed, runnable quantized checkpoints. Proposals still treat them as unmeasured.

| Entry | Base | Format | Hardware | Runtimes | Engine requirements |
| --- | --- | --- | --- | --- | --- |
| [nvidia/DeepSeek-V4.1-Flash-NVFP4](https://huggingface.co/nvidia/DeepSeek-V4.1-Flash-NVFP4) @ `3431dde3247c` | deepseek-ai/DeepSeek-V4.1-Flash (exact) | modelopt nvfp4 | nvidia blackwell (runnable) | sglang, vllm | none declared |
| [nvidia/Qwen3.8-27B-NVFP4](https://huggingface.co/nvidia/Qwen3.8-27B-NVFP4) @ `482ca0f38322` | Qwen/Qwen3.8-27B (publisher_declared) | modelopt mixed-nvfp4-fp8 | nvidia blackwell (runnable) | sglang, vllm | none declared |

## Added

| Entry | Base | Format | Hardware | Eligibility |
| --- | --- | --- | --- | --- |
| [amd/GLM-5.3-Flash-Quark-MXFP4](https://huggingface.co/amd/GLM-5.3-Flash-Quark-MXFP4) @ `d4d4d4d4d4d4` | zai-org/GLM-5.3-Flash | quark mxfp4 | amd cdna4 (unsupported_hardware) | catalog only: targets amd hardware the product cannot run yet; tool quark cannot seed product candidates yet |
| [nvidia/DeepSeek-V4.1-Flash-1017-NVFP4](https://huggingface.co/nvidia/DeepSeek-V4.1-Flash-1017-NVFP4) @ `3ae9c0d3fbbe` | deepseek-ai/DeepSeek-V4.1-Flash-1017 | modelopt nvfp4 | nvidia blackwell (runnable) | catalog only: every reviewed runtime needs an unreleased or blocked engine change |
| [nvidia/GLM-5.3-Flash-FP4-Local](https://huggingface.co/nvidia/GLM-5.3-Flash-FP4-Local) @ `a1a1a1a1a1a1` | zai-org/GLM-5.3-Flash | modelopt mixed-nvfp4-fp8 | nvidia blackwell (runnable) | catalog only: license "other" is unresolved |
| [nvidia/Qwen3.8-27B-DSpark](https://huggingface.co/nvidia/Qwen3.8-27B-DSpark) @ `b2b2b2b2b2b2` | Qwen/Qwen3.8-27B | speculator dspark | nvidia hopper, blackwell (runnable) | catalog only: speculator: speculative-decoding candidates need acceptance and output-parity gates first |
| [RedHatAI/Qwen3.8-27B-NVFP4](https://huggingface.co/RedHatAI/Qwen3.8-27B-NVFP4) @ `c3c3c3c3c3c3` | Qwen/Qwen3.8-27B | llm-compressor mixed-nvfp4-fp8 | nvidia (unknown) | catalog only: review pending; target hardware is not reviewed; no reviewed runtime |

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

Repositories in a publisher's listing window that did not become entries.

### library is not served by an LLM engine (1)

nvidia/Cosmos3-Nano-FP8

### no declared base model (1)

RedHatAI/GLM-5.3-Flash

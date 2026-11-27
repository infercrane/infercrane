# Rules: rung 2, fused-MoE tuning from shapes alone on one L40S (26 Nov 2026)

Written before any server or GPU of this run started. A change means a new rules file.

## Where this sits

The kernel-tuning ladder in the maintainer's run notes: rung 1 ($0) estimated
the fused-MoE layer at about 87–92% of a decode step's bytes for Qwen3-30B-A3B (32 and 128 at
once) and 61–73% for Qwen3.6-35B-A3B (`tools/kernel-tune/amdahl.py`, an estimate). This rung
measures the kernel alone, on the GPU rung 3 serves on, without loading any model. Tuned
configs are per GPU, so it cannot stand in for an H100 result, and an H100 result cannot stand
in for it.

## Question

Does vLLM 0.34.0's own Triton fused-MoE kernel, tuned for an L40S at rung 3's step sizes,
beat the engine's own choice by enough to project at least 2% end to end for Qwen3-30B-A3B at
32 or 128 requests at once?

## Setup

| | |
|---|---|
| Engine | `vllm/vllm-openai@sha256:20fdb089454afbb324efa4110a2c025ff7b27cc1a086e07aa580b18df8ad1ce1` (v0.34.0), the engine's own Python |
| GPU | Modal `L40S`, one, 4 cores, 16 GiB; no model weights |
| Shapes (block FP8 128×128, hidden 2,048, top-8) | E=128, N=768 (Qwen3-30B-A3B and Qwen3-Coder-30B-A3B; rung 3's) and E=256, N=512 (Qwen3.6-35B-A3B; for comparison with the H100 run) |
| Step sizes | decode up to 128 (loads of 128 and 32 at once), prefill up to 2,048 (vLLM 0.34.0's step budget below 70 GB), plus the keys that keep the engine's default exact |
| Plan | `KERNEL_TUNE_PLAN=moe-shapes-l40s` |

First the container records whether vLLM ships a file for each shape on this GPU
(`get_config_file_name`, its configs folder). If it does, the baseline is that file; if not,
the engine's built-in default. Either way the baseline is what the engine would run.

## Measurement (per shape, `tune_moe.py`)

- The engine's own choice timed twice per step size (start and end), giving that step size's
  spread. Coordinate search from it and the previous winner, two passes, 45 s per step size,
  420 s per shape.
- Kernel alone, CUDA graph of 10 calls, each call reading the next of 4 weight copies (so no
  call finds its experts in the L2 cache), 5 warm-up replays, 30 timed replays, median.
  Weights and activations uniform in [-1, 1] (weights scaled by 1/8 before FP8). Uniform random
  routing, as vLLM's own tuner uses.
- Clocks, power, temperature and use logged during each shape (Modal does not allow locking
  clocks).
- A setting is kept only when it is at least 3% faster than the engine's choice and faster by
  more than twice that step size's spread, has no NaN or Inf, has an error against an FP32
  reference at most 1.25× the engine's own (+0.001), and neither time is below 90% of the
  memory floor at 864 GB/s. Then the file as the engine reads it is checked at the step sizes
  between keys; a slower one gets the engine's default.

## Pass bar

For each shape and load, the projected end-to-end speedup is Amdahl's with rung 1's share at
that load and the kernel gain kept at the step size nearest the load (0 when the engine's
setting was kept).

- **Rung 3 is asked for** only if the projection for Qwen3-30B-A3B is at least **1.02x** at 32
  or 128 at once. A projection is not a result: the judge measures it in rung 3.
- The kernel planner's gate (1.05x) is reported beside it.
- If neither load reaches 1.02x, the ladder stops for this model on an L40S and that is the
  result.

## What gets reported

Per shape: whether vLLM ships a file for it on an L40S, every step size's engine and best
times, spread, kept or why not, the share of the memory floor each reaches, the reference
check, the hidden step sizes, GPU clocks and power, and the projections. Kernel-level only;
never a serving claim.

## Money

Modal list price for an L40S is **assumed at $1.95/h** (modal.com did not resolve from this
machine on 26 Nov; confirm before launch). With 4 cores and 16 GiB: $2.27/h. Cap: 25 minutes
after the container starts, **$0.98**; `shapes --budget-usd 1` refuses less. Estimate: about
17 minutes, about $0.65.

## Dry run

`KERNEL_TUNE_PLAN=moe-shapes-l40s kernel_tune_modal.py dry`: the same path with the tuner's
stand-in timings, $0, not a measurement.

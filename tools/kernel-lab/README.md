# InferCrane Kernel Lab PoC

This directory contains bounded custom-kernel experiments. The first candidate
fuses residual addition and RMSNorm, an operator pattern shared by multiple
open-weight transformer families. It is not tied to the Qwen model name.

Run the dependency-free semantic check on a laptop:

```bash
python3 tools/kernel-lab/fused_residual_rmsnorm.py \
  --self-test --rows 7 --hidden-size 1024
```

The result is `cpu-emulated-correctness`. It proves the mathematical contract,
not GPU compilation or performance.

On an NVIDIA worker with compatible PyTorch and Triton installations:

```bash
python3 tools/kernel-lab/fused_residual_rmsnorm.py \
  --gpu-benchmark --rows 128 --hidden-size 1024
```

That path checks the Triton output against PyTorch and reports a CUDA
microbenchmark. A microbenchmark still cannot qualify a deployment. Integrate
the candidate into the pinned runtime image, replay the same workload with
AIPerf, run semantic-quality gates, and compare TTFT, TPOT, goodput, error rate,
and cost before promotion.

The handwritten CUDA C++ candidate is kept separate and visible:

```bash
python3 tools/kernel-lab/fused_residual_rmsnorm_cuda.py \
  --gpu-benchmark --rows 128 --hidden-size 1024
```

CuPy provides only NVRTC compilation and launch. The operator in
`CUDA_SOURCE` is handwritten CUDA and is compared with the Triton candidate and
vLLM's pinned vendor implementation. Its H100 screen passed correctness but
lost to both baselines, so it is a rejected candidate rather than serving code.

Create an operator-level experiment plan from the documentation fixture:

```bash
infercrane optimize kernel-plan \
  --file internal/kernelplanner/testdata/qwen3-0.6b-fixture.json
```

Replace fixture evidence with an exact Nsight Systems/Compute or PyTorch
profiler capture and add `--require-measured` for a paid GPU campaign.

## Generated kernels

`packs/<operator-family>/` holds the human-reviewed contract (`pack.json`) and
the hidden evaluator (`evaluate.py`) used by `infercrane-kernel-adapter`. The
evaluator reports only which stage failed, so its test inputs never reach the
model. On a GPU it also reports roofline utilization: the time the operator's
minimum DRAM traffic would take at peak bandwidth, divided by the measured time.
It reports 0 when that is unknown. `run-isolated-evaluation.sh` is the reference
no-network launcher, and `brezel-runner/` is the fixed source-screening
entrypoint for Brezel.
`target-runtimes.json` holds reviewed launch data for profiling and
qualification. A pack's `integration/` directory routes a serving runtime to the
kernel, and its `test_integration.py` runs without a GPU. The Go test suite runs
each pack's `test_*.py` files.

An integration declares the exact runtime versions it was reviewed against in
`runtime_versions`. The adapter refuses any other version before generation or
GPU spend, and the integration refuses it again when the server starts. The
residual-RMSNorm integration registers a vLLM IR provider and supports vLLM
0.22.1 only. vLLM IR is deprecated and vLLM 0.34.0 removes its op-priority
setting, so this pack needs a CustomOp-based integration, reviewed and qualified,
before any vLLM 0.34 or later pin. Versions between 0.22.1 and 0.34.0 stay
unsupported until someone reviews and tests them.

Check a kernel on a laptop without a GPU with
`screen-kernel.sh FAMILY KERNEL.py`. It runs the pack's evaluator on CPU with
Triton's interpreter in the `cpu-screen/` image, and `packs/*/testdata/` holds a
correct kernel and two wrong ones to confirm the suite catches mistakes. Set
`INFERCRANE_KERNEL_CPU_SCREEN` to `run-cpu-screen.sh` so the adapter rejects
wrong drafts before any GPU time is spent.

## Serving timeline (opt-in)

`infercrane-kernel-adapter timeline` records a short Nsight Systems timeline of
the serving process under load. It reads the same profile request JSON as
`profile`, and it is off unless the runtime's catalog entry has a reviewed
`timeline` block and its image ships the `nsys` CLI:

```json
"timeline": {
  "nsys": "/opt/nvidia/nsight-systems/bin/nsys",
  "profiler_args": ["--profiler-config.profiler=cuda"],
  "env": ["VLLM_WORKER_MULTIPROC_METHOD=spawn"]
}
```

For SGLang, leave `profiler_args` out and set
`"profile_request": {"activities": ["CUDA_PROFILER"]}`. The reviewed catalog has
no `timeline` block yet. The published `vllm/vllm-openai` image does not include
nsys, and whether `lmsysorg/sglang` runtime tags do is unconfirmed. Use a derived
image with a pinned digest. The adapter checks `nsys --version` inside the image
before starting a server and fails with that reason when the tool is missing.

The server runs under
`nsys profile --trace=cuda,nvtx --sample=none --cpuctxsw=none --capture-range=cudaProfilerApi --capture-range-end=stop-shutdown`
in the same hardened container as `profile`. CPU sampling stays off, so the
capture needs no extra Linux capability. AIPerf drives load, a 5-second warmup
passes, and then `/start_profile` and `/stop_profile` open and close the CUDA
profiler range. The window is 20 seconds by default and can be set from 5 to 120
seconds with `INFERCRANE_KERNEL_TIMELINE_SECONDS`. `nsys stats` then runs in the
same image with no network and writes `cuda_gpu_kern_sum`, `cuda_api_sum`, and
`cuda_gpu_mem_time_sum` CSVs. The adapter returns an
`infercrane.serving-timeline/v1` summary:

- the capture window and the summed kernel and memory-operation time
- `gpu_idle_fraction`: 1 − (kernel + memory time) / (window × GPUs). Overlapping
  streams count twice, so this is a lower bound.
- `kernel_launch_overhead_share`: time in kernel and graph launch calls / (window
  × GPUs). This assumes one launching worker per GPU.
- `communication_kernel_share`: the share of kernel time in NCCL and runtime
  all-reduce kernels, which includes time waiting for peers.
- the ten busiest kernels, with names truncated to 200 characters

A report that nsys does not export leaves its fields `null`. The `.nsys-rep`
digest is returned as `artifact_digest`, and the file stays on the GPU host.
The tests use the CSV fixtures in `internal/kerneltarget/testdata/nsys`. No
capture has run on a GPU yet, and multi-GPU behavior is unverified: the first
worker's `cudaProfilerStop` ends the session. Nothing in a campaign calls
`timeline` yet.

Add a pack for an operator only after writing its contract and hidden suite by
hand. Do not let a model write either one. See
[Generate a custom kernel](../../docs/features/optimization.mdx).

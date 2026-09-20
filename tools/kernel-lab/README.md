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

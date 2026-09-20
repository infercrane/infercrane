#!/usr/bin/env python3
"""Handwritten CUDA candidate for fused residual-add + RMSNorm.

This module intentionally keeps the CUDA source visible and small.  CuPy is
used only as the NVRTC compiler/launcher; the operator itself is handwritten
CUDA.  The benchmark compares the same out-of-place contract as the Triton
candidate and separately compares vLLM's pinned in-place vendor operator.

The result is a kernel screening receipt, not serving qualification.
"""

from __future__ import annotations

import json


CUDA_SOURCE = r"""
extern "C" __device__ __forceinline__ float bf16_to_float(unsigned short value) {
    return __uint_as_float(((unsigned int)value) << 16);
}

extern "C" __device__ __forceinline__ unsigned short float_to_bf16(float value) {
    unsigned int bits = __float_as_uint(value);
    // Round-to-nearest-even before discarding the low 16 bits.
    bits += 0x7fffu + ((bits >> 16) & 1u);
    return (unsigned short)(bits >> 16);
}

extern "C" __device__ __forceinline__ float warp_sum(float value) {
    value += __shfl_down_sync(0xffffffffu, value, 16);
    value += __shfl_down_sync(0xffffffffu, value, 8);
    value += __shfl_down_sync(0xffffffffu, value, 4);
    value += __shfl_down_sync(0xffffffffu, value, 2);
    value += __shfl_down_sync(0xffffffffu, value, 1);
    return value;
}

extern "C" __global__ void fused_residual_rmsnorm_bf16(
    const unsigned short* __restrict__ x,
    const unsigned short* __restrict__ residual,
    const unsigned short* __restrict__ weight,
    unsigned short* __restrict__ output,
    unsigned short* __restrict__ residual_output,
    int hidden_size,
    float epsilon
) {
    const int row = (int)blockIdx.x;
    const int thread = (int)threadIdx.x;
    const int row_offset = row * hidden_size;
    float square_sum = 0.0f;

    // The same loop supports arbitrary hidden sizes while preserving a fast
    // four-elements-per-thread path for Qwen3's hidden size of 1024.
    for (int column = thread; column < hidden_size; column += (int)blockDim.x) {
        const int index = row_offset + column;
        const float fused = bf16_to_float(x[index]) + bf16_to_float(residual[index]);
        residual_output[index] = float_to_bf16(fused);
        square_sum = fmaf(fused, fused, square_sum);
    }

    square_sum = warp_sum(square_sum);
    __shared__ float warp_sums[32];
    __shared__ float inverse_rms;
    const int lane = thread & 31;
    const int warp = thread >> 5;
    const int warp_count = ((int)blockDim.x + 31) >> 5;
    if (lane == 0) {
        warp_sums[warp] = square_sum;
    }
    __syncthreads();

    if (warp == 0) {
        float block_sum = lane < warp_count ? warp_sums[lane] : 0.0f;
        block_sum = warp_sum(block_sum);
        if (lane == 0) {
            inverse_rms = rsqrtf(block_sum / (float)hidden_size + epsilon);
        }
    }
    __syncthreads();

    for (int column = thread; column < hidden_size; column += (int)blockDim.x) {
        const int index = row_offset + column;
        // Recompute from the BF16 inputs so normalization uses the same FP32
        // fused value as the reference rather than the rounded residual output.
        const float fused = bf16_to_float(x[index]) + bf16_to_float(residual[index]);
        const float scale = bf16_to_float(weight[column]);
        output[index] = float_to_bf16(fused * inverse_rms * scale);
    }
}
"""


class CUDARMSNorm:
    """Compiled CUDA launch wrapper bound to the caller's PyTorch stream."""

    def __init__(self) -> None:
        import cupy as cp

        self.cp = cp
        module = cp.RawModule(
            code=CUDA_SOURCE,
            options=("--std=c++17", "--use_fast_math"),
            name_expressions=("fused_residual_rmsnorm_bf16",),
        )
        self.kernel = module.get_function("fused_residual_rmsnorm_bf16")

    def arrays(self, *tensors):
        import torch

        # Viewing BF16 as uint16 avoids depending on CuPy's high-level BF16
        # dtype support.  The CUDA kernel interprets the exact storage bits.
        return tuple(self.cp.from_dlpack(tensor.view(torch.uint16)) for tensor in tensors)

    def launch(self, arrays, rows: int, hidden_size: int, epsilon: float, threads: int = 256) -> None:
        import torch

        stream = self.cp.cuda.ExternalStream(torch.cuda.current_stream().cuda_stream)
        with stream:
            self.kernel(
                (rows,),
                (threads,),
                (*arrays, self.cp.int32(hidden_size), self.cp.float32(epsilon)),
            )


def gpu_benchmark(
    rows: int,
    hidden_size: int,
    seed: int,
    epsilon: float,
    triton_candidate=None,
) -> dict[str, object]:
    import importlib.metadata

    import torch
    import triton
    from vllm import _custom_ops as ops

    if not torch.cuda.is_available():
        raise RuntimeError("an NVIDIA CUDA device is required")
    if hidden_size < 1 or hidden_size > 65536:
        raise ValueError("hidden size is outside the reviewed CUDA candidate boundary")

    torch.manual_seed(seed)
    x = torch.randn((rows, hidden_size), device="cuda", dtype=torch.bfloat16)
    residual = torch.randn_like(x)
    weight = torch.randn((hidden_size,), device="cuda", dtype=torch.bfloat16)
    output = torch.empty_like(x)
    residual_output = torch.empty_like(x)

    expected_residual = x + residual
    expected = (
        expected_residual.float()
        * torch.rsqrt(expected_residual.float().square().mean(dim=-1, keepdim=True) + epsilon)
        * weight.float()
    ).to(x.dtype)

    implementation = CUDARMSNorm()
    arrays = implementation.arrays(x, residual, weight, output, residual_output)
    implementation.launch(arrays, rows, hidden_size, epsilon)
    torch.cuda.synchronize()
    torch.testing.assert_close(output, expected, atol=5e-2, rtol=5e-2)
    torch.testing.assert_close(residual_output, expected_residual, atol=2e-2, rtol=2e-2)

    cuda_ms = triton.testing.do_bench(
        lambda: implementation.launch(arrays, rows, hidden_size, epsilon)
    )

    triton_ms = None
    if triton_candidate is not None:
        triton_output = torch.empty_like(x)
        triton_residual_output = torch.empty_like(x)
        triton_candidate._launch_triton(
            x, residual, weight, triton_output, triton_residual_output, epsilon
        )
        torch.testing.assert_close(triton_output, expected, atol=5e-2, rtol=5e-2)
        torch.testing.assert_close(triton_residual_output, expected_residual, atol=2e-2, rtol=2e-2)
        triton_ms = triton.testing.do_bench(
            lambda: triton_candidate._launch_triton(
                x, residual, weight, triton_output, triton_residual_output, epsilon
            )
        )

    vendor_input = x.clone()
    vendor_residual = residual.clone()
    ops.fused_add_rms_norm(vendor_input, vendor_residual, weight, epsilon)
    torch.testing.assert_close(vendor_input, expected, atol=5e-2, rtol=5e-2)
    torch.testing.assert_close(vendor_residual, expected_residual, atol=2e-2, rtol=2e-2)
    vendor_ms = triton.testing.do_bench(
        lambda: ops.fused_add_rms_norm(vendor_input, vendor_residual, weight, epsilon)
    )

    approximate_bytes = (4 * rows * hidden_size + hidden_size) * 2
    return {
        "schema_version": "infercrane.cuda-kernel-gpu-microbenchmark/v1",
        "kernel": "fused-residual-rmsnorm-bf16",
        "implementation": "handwritten-cuda-nvrtc",
        "evidence_class": "gpu-microbenchmark",
        "device": torch.cuda.get_device_name(),
        "rows": rows,
        "hidden_size": hidden_size,
        "threads": 256,
        "seed": seed,
        "cuda_kernel_ms": cuda_ms,
        "triton_kernel_ms": triton_ms,
        "vllm_vendor_ms": vendor_ms,
        "speedup_vs_triton": triton_ms / cuda_ms if triton_ms else None,
        "speedup_vs_vllm_vendor": vendor_ms / cuda_ms,
        "approximate_effective_gb_s": approximate_bytes / (cuda_ms * 1e6),
        "torch_version": torch.__version__,
        "cupy_version": importlib.metadata.version("cupy-cuda12x"),
        "vllm_version": importlib.metadata.version("vllm"),
        "correctness_passed": True,
        "performance_qualified": False,
        "qualification_boundary": "microbenchmark only; runtime integration and paired endpoint evidence remain required",
    }


def main() -> int:
    import argparse

    parser = argparse.ArgumentParser()
    parser.add_argument("--gpu-benchmark", action="store_true", required=True)
    parser.add_argument("--rows", type=int, default=8)
    parser.add_argument("--hidden-size", type=int, default=1024)
    parser.add_argument("--seed", type=int, default=17)
    parser.add_argument("--epsilon", type=float, default=1e-6)
    arguments = parser.parse_args()
    print(json.dumps(gpu_benchmark(arguments.rows, arguments.hidden_size, arguments.seed, arguments.epsilon), indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

#!/usr/bin/env python3
"""InferCrane fused residual-add + RMSNorm kernel candidate.

The dependency-free self-test validates the mathematical contract on a laptop.
When PyTorch, Triton, and an NVIDIA GPU are present, --gpu-benchmark validates
the Triton implementation against PyTorch and emits a microbenchmark receipt.
Neither path is end-to-end serving qualification.
"""

from __future__ import annotations

import argparse
import json
import math
import random
from typing import Sequence

try:
    import torch
    import triton
    import triton.language as tl
except ImportError:
    torch = None
    triton = None
    tl = None


def reference(
    x: Sequence[Sequence[float]],
    residual: Sequence[Sequence[float]],
    weight: Sequence[float],
    epsilon: float,
) -> tuple[list[list[float]], list[list[float]]]:
    """Clear, allocation-heavy definition used as the local oracle."""
    residual_out = [
        [x_value + residual_value for x_value, residual_value in zip(x_row, residual_row)]
        for x_row, residual_row in zip(x, residual)
    ]
    output = []
    for row in residual_out:
        inverse_rms = 1.0 / math.sqrt(sum(value * value for value in row) / len(row) + epsilon)
        output.append([value * inverse_rms * scale for value, scale in zip(row, weight)])
    return output, residual_out


def emulated_fused(
    x: Sequence[Sequence[float]],
    residual: Sequence[Sequence[float]],
    weight: Sequence[float],
    epsilon: float,
) -> tuple[list[list[float]], list[list[float]]]:
    """CPU emulation following the dataflow of one fused GPU program row."""
    output: list[list[float]] = []
    residual_out: list[list[float]] = []
    for row_index in range(len(x)):
        fused_row: list[float] = []
        square_sum = 0.0
        for column in range(len(weight)):
            value = x[row_index][column] + residual[row_index][column]
            fused_row.append(value)
            square_sum += value * value
        inverse_rms = 1.0 / math.sqrt(square_sum / len(weight) + epsilon)
        output.append([fused_row[column] * inverse_rms * weight[column] for column in range(len(weight))])
        residual_out.append(fused_row)
    return output, residual_out


if triton is not None:

    @triton.jit
    def _fused_residual_rmsnorm_kernel(
        x_ptr,
        residual_ptr,
        weight_ptr,
        output_ptr,
        residual_output_ptr,
        hidden_size: tl.constexpr,
        epsilon: tl.constexpr,
        block_size: tl.constexpr,
    ):
        row = tl.program_id(axis=0)
        offsets = tl.arange(0, block_size)
        mask = offsets < hidden_size
        row_offsets = row * hidden_size + offsets

        x = tl.load(x_ptr + row_offsets, mask=mask, other=0.0).to(tl.float32)
        residual = tl.load(residual_ptr + row_offsets, mask=mask, other=0.0).to(tl.float32)
        fused = x + residual
        mean_square = tl.sum(fused * fused, axis=0) / hidden_size
        inverse_rms = tl.rsqrt(mean_square + epsilon)
        weight = tl.load(weight_ptr + offsets, mask=mask, other=0.0).to(tl.float32)

        tl.store(residual_output_ptr + row_offsets, fused, mask=mask)
        tl.store(output_ptr + row_offsets, fused * inverse_rms * weight, mask=mask)


def triton_fused_residual_rmsnorm(x, residual, weight, epsilon: float):
    if triton is None or torch is None:
        raise RuntimeError("PyTorch and Triton are required for the GPU path")
    if not x.is_cuda or not residual.is_cuda or not weight.is_cuda:
        raise RuntimeError("GPU path requires CUDA tensors")
    if x.shape != residual.shape or x.shape[-1] != weight.numel():
        raise ValueError("x, residual, and weight shapes do not match")
    x = x.contiguous()
    residual = residual.contiguous()
    weight = weight.contiguous()
    output = torch.empty_like(x)
    residual_output = torch.empty_like(x)
    _launch_triton(x, residual, weight, output, residual_output, epsilon)
    return output, residual_output


def _launch_triton(x, residual, weight, output, residual_output, epsilon: float, num_warps: int = 4):
    """Launch only the kernel; callers own allocations for fair timing."""
    hidden_size = x.shape[-1]
    rows = x.numel() // hidden_size
    block_size = triton.next_power_of_2(hidden_size)
    _fused_residual_rmsnorm_kernel[(rows,)](
        x,
        residual,
        weight,
        output,
        residual_output,
        hidden_size=hidden_size,
        epsilon=epsilon,
        block_size=block_size,
        num_warps=num_warps,
    )


def self_test(rows: int, hidden_size: int, seed: int, epsilon: float) -> dict[str, object]:
    random_generator = random.Random(seed)
    x = [[random_generator.uniform(-3, 3) for _ in range(hidden_size)] for _ in range(rows)]
    residual = [[random_generator.uniform(-3, 3) for _ in range(hidden_size)] for _ in range(rows)]
    weight = [random_generator.uniform(0.25, 1.75) for _ in range(hidden_size)]
    expected, expected_residual = reference(x, residual, weight, epsilon)
    actual, actual_residual = emulated_fused(x, residual, weight, epsilon)
    max_absolute_error = max(
        abs(actual[row][column] - expected[row][column])
        for row in range(rows)
        for column in range(hidden_size)
    )
    max_residual_error = max(
        abs(actual_residual[row][column] - expected_residual[row][column])
        for row in range(rows)
        for column in range(hidden_size)
    )
    passed = max_absolute_error <= 1e-12 and max_residual_error <= 1e-12
    return {
        "schema_version": "infercrane.kernel-local-check/v1",
        "kernel": "fused-residual-rmsnorm",
        "evidence_class": "cpu-emulated-correctness",
        "rows": rows,
        "hidden_size": hidden_size,
        "seed": seed,
        "max_absolute_error": max_absolute_error,
        "max_residual_error": max_residual_error,
        "passed": passed,
        "performance_qualified": False,
    }


def gpu_benchmark(rows: int, hidden_size: int, seed: int, epsilon: float) -> dict[str, object]:
    if torch is None or triton is None:
        raise RuntimeError("install PyTorch and Triton for the GPU benchmark")
    if not torch.cuda.is_available():
        raise RuntimeError("an NVIDIA CUDA device is required for the GPU benchmark")
    torch.manual_seed(seed)
    x = torch.randn((rows, hidden_size), device="cuda", dtype=torch.bfloat16)
    residual = torch.randn_like(x)
    weight = torch.randn((hidden_size,), device="cuda", dtype=torch.bfloat16)

    expected_residual = x + residual
    expected = (
        expected_residual.float()
        * torch.rsqrt(expected_residual.float().square().mean(dim=-1, keepdim=True) + epsilon)
        * weight.float()
    ).to(x.dtype)
    actual, actual_residual = triton_fused_residual_rmsnorm(x, residual, weight, epsilon)
    torch.testing.assert_close(actual, expected, atol=5e-2, rtol=5e-2)
    torch.testing.assert_close(actual_residual, expected_residual, atol=2e-2, rtol=2e-2)

    def eager_baseline():
        combined = x + residual
        return (
            combined.float()
            * torch.rsqrt(combined.float().square().mean(dim=-1, keepdim=True) + epsilon)
            * weight.float()
        ).to(x.dtype), combined

    @torch.compile(fullgraph=True)
    def compiled_baseline(x_value, residual_value, weight_value):
        combined = x_value + residual_value
        normalized = (
            combined.float()
            * torch.rsqrt(combined.float().square().mean(dim=-1, keepdim=True) + epsilon)
            * weight_value.float()
        ).to(x_value.dtype)
        return normalized, combined

    compiled_expected, compiled_residual = compiled_baseline(x, residual, weight)
    torch.testing.assert_close(compiled_expected, expected, atol=5e-2, rtol=5e-2)
    torch.testing.assert_close(compiled_residual, expected_residual, atol=2e-2, rtol=2e-2)
    output_buffer = torch.empty_like(x)
    residual_buffer = torch.empty_like(x)
    launch_variants_ms = {}
    for num_warps in (4, 8, 16):
        _launch_triton(x, residual, weight, output_buffer, residual_buffer, epsilon, num_warps)
        launch_variants_ms[str(num_warps)] = triton.testing.do_bench(
            lambda num_warps=num_warps: _launch_triton(
                x, residual, weight, output_buffer, residual_buffer, epsilon, num_warps
            )
        )
    selected_num_warps = min(launch_variants_ms, key=launch_variants_ms.get)
    custom_kernel_ms = launch_variants_ms[selected_num_warps]
    custom_wrapper_ms = triton.testing.do_bench(
        lambda: triton_fused_residual_rmsnorm(x, residual, weight, epsilon)
    )
    eager_ms = triton.testing.do_bench(eager_baseline)
    compiled_ms = triton.testing.do_bench(lambda: compiled_baseline(x, residual, weight))
    approximate_bytes = (4 * rows * hidden_size + hidden_size) * 2
    return {
        "schema_version": "infercrane.kernel-gpu-microbenchmark/v1",
        "kernel": "fused-residual-rmsnorm",
        "evidence_class": "gpu-microbenchmark",
        "device": torch.cuda.get_device_name(),
        "torch_version": torch.__version__,
        "triton_version": triton.__version__,
        "rows": rows,
        "hidden_size": hidden_size,
        "seed": seed,
        "pytorch_eager_ms": eager_ms,
        "torch_compile_ms": compiled_ms,
        "custom_wrapper_ms": custom_wrapper_ms,
        "custom_kernel_ms": custom_kernel_ms,
        "launch_variants_ms": launch_variants_ms,
        "selected_num_warps": int(selected_num_warps),
        "speedup_vs_pytorch_eager": eager_ms / custom_kernel_ms,
        "speedup_vs_torch_compile": compiled_ms / custom_kernel_ms,
        "approximate_effective_gb_s": approximate_bytes / (custom_kernel_ms * 1e6),
        "correctness_passed": True,
        "performance_qualified": False,
        "qualification_boundary": "paired end-to-end AIPerf and quality evidence are still required",
    }


def main() -> int:
    parser = argparse.ArgumentParser()
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--self-test", action="store_true")
    mode.add_argument("--gpu-benchmark", action="store_true")
    parser.add_argument("--rows", type=int, default=7)
    parser.add_argument("--hidden-size", type=int, default=1024)
    parser.add_argument("--seed", type=int, default=17)
    parser.add_argument("--epsilon", type=float, default=1e-6)
    arguments = parser.parse_args()
    if arguments.rows < 1 or arguments.hidden_size < 1 or arguments.epsilon <= 0:
        parser.error("rows, hidden-size, and epsilon must be positive")
    if arguments.self_test:
        receipt = self_test(arguments.rows, arguments.hidden_size, arguments.seed, arguments.epsilon)
    else:
        receipt = gpu_benchmark(arguments.rows, arguments.hidden_size, arguments.seed, arguments.epsilon)
    print(json.dumps(receipt, indent=2, sort_keys=True))
    return 0 if receipt.get("passed", receipt.get("correctness_passed", False)) else 1


if __name__ == "__main__":
    raise SystemExit(main())

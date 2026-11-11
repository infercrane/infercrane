#!/usr/bin/env python3
"""Hidden evaluator for generated residual-add + RMSNorm kernels.

Runs inside the isolated GPU container. It reads one evaluation request on
stdin and prints stage results and timings. It reports only which stage failed,
so test inputs never reach the model writing the kernel. The checks follow the
published failure modes of generated kernels: bypasses for narrow test values,
uninitialized-memory wins, and batch-dependent output.
"""

from __future__ import annotations

import importlib.util
import json
import os
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

STAGES = ("smoke", "shape_sweep", "numerical_stability", "determinism", "edge_cases", "sanitizer")
SHAPES = ((1, 1024), (8, 1024), (32, 768), (128, 1024), (512, 4096), (3, 64))
VERIFICATION_SHAPES = ((5, 1536), (17, 2048), (64, 512), (257, 1024))
SANITIZER_CHILD_FLAG = "--sanitizer-child"
EPSILON = 1e-6
# INFERCRANE_EVAL_DEVICE=cpu with TRITON_INTERPRET=1 runs the same checks on a
# laptop. It proves correctness only: the sanitizer and timing need a GPU and
# are reported as not run, never as passed.
DEVICE = os.environ.get("INFERCRANE_EVAL_DEVICE", "cuda")
CPU_MODE = DEVICE == "cpu"
if CPU_MODE:  # Triton's interpreter is slow, so the CPU suites are smaller.
    SHAPES = ((1, 64), (3, 64), (5, 96), (8, 256), (32, 128))
    VERIFICATION_SHAPES = ((2, 512), (7, 192), (17, 256))
HIDDEN = 256 if CPU_MODE else 1024
ROWS = 8 if CPU_MODE else 32


class StageFailure(Exception):
    pass


def load_candidate(source: str):
    directory = Path(tempfile.mkdtemp(prefix="kernel-eval-"))
    path = directory / "kernel.py"
    path.write_text(source, encoding="utf-8")
    spec = importlib.util.spec_from_file_location("generated_kernel", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module.fused_residual_rmsnorm


def reference(torch, x, residual, weight, epsilon):
    residual_out = x + residual
    wide = residual_out.float()
    output = wide * torch.rsqrt(wide.square().mean(dim=-1, keepdim=True) + epsilon) * weight.float()
    return output.to(x.dtype), residual_out


def inputs(torch, rows, hidden, seed, kind="normal"):
    generator = torch.Generator(device=DEVICE).manual_seed(seed)

    def draw(shape):
        return torch.randn(shape, device=DEVICE, dtype=torch.float32, generator=generator)

    x, residual, weight = draw((rows, hidden)), draw((rows, hidden)), draw((hidden,))
    if kind == "large":
        x, residual = x * 1e3, residual * 1e3
    elif kind == "small":
        x, residual = x * 1e-3, residual * 1e-3
    elif kind == "sparse":
        mask = torch.rand((rows, hidden), device=DEVICE, generator=generator) > 0.9
        x, residual = x * mask, residual * mask
    return x.bfloat16(), residual.bfloat16(), weight.bfloat16()


def check(torch, kernel, x, residual, weight, stage):
    source_x, source_residual, source_weight = x.clone(), residual.clone(), weight.clone()
    output, residual_out = kernel(x, residual, weight, EPSILON)
    expected, expected_residual = reference(torch, x, residual, weight, EPSILON)
    if output.shape != expected.shape or residual_out.shape != expected_residual.shape or output.dtype != x.dtype:
        raise StageFailure(stage)
    if not (torch.equal(x, source_x) and torch.equal(residual, source_residual) and torch.equal(weight, source_weight)):
        raise StageFailure(stage)
    try:
        torch.testing.assert_close(output, expected, atol=5e-2, rtol=5e-2)
        torch.testing.assert_close(residual_out, expected_residual, atol=2e-2, rtol=2e-2)
    except AssertionError as error:
        raise StageFailure(stage) from error


def dirty_memory(torch):
    """Leave recognizable garbage so reading uninitialized output is caught."""
    for size in (1 << 12, 1 << 16, 1 << 20) if CPU_MODE else (1 << 16, 1 << 20, 1 << 24):
        block = torch.full((size,), 7.25, device=DEVICE, dtype=torch.float32)
        del block
    if not CPU_MODE:
        torch.cuda.synchronize()


def check_determinism(torch, kernel, seed):
    x, residual, weight = inputs(torch, ROWS, HIDDEN, seed)
    dirty_memory(torch)
    first, first_residual = kernel(x, residual, weight, EPSILON)
    dirty_memory(torch)
    second, second_residual = kernel(x, residual, weight, EPSILON)
    if not (torch.equal(first, second) and torch.equal(first_residual, second_residual)):
        raise StageFailure("determinism")
    # A row's bits must not depend on the rows batched with it.
    other_x, other_residual, _ = inputs(torch, ROWS, HIDDEN, seed + 1)
    for index in (0, ROWS // 4, ROWS - 1):
        alone, _ = kernel(x[index:index + 1], residual[index:index + 1], weight, EPSILON)
        if not torch.equal(alone[0], first[index]):
            raise StageFailure("determinism")
        mixed_x, mixed_residual = other_x.clone(), other_residual.clone()
        mixed_x[index], mixed_residual[index] = x[index], residual[index]
        mixed, _ = kernel(mixed_x, mixed_residual, weight, EPSILON)
        if not torch.equal(mixed[index], first[index]):
            raise StageFailure("determinism")


def check_edge_cases(torch, kernel):
    zero = torch.zeros((4, HIDDEN), device=DEVICE, dtype=torch.bfloat16)
    weight = torch.ones((HIDDEN,), device=DEVICE, dtype=torch.bfloat16)
    check(torch, kernel, zero, zero.clone(), weight, "edge_cases")
    check(torch, kernel, zero + 1, zero - 1, weight, "edge_cases")
    check(torch, kernel, zero + 1, zero.clone(), weight * 0, "edge_cases")


def run_sanitizer(request_bytes: bytes) -> bool:
    """A missing tool fails closed; memory safety is never assumed."""
    tool = shutil.which("compute-sanitizer")
    if tool is None:
        return False
    command = [tool, "--tool", "memcheck", "--error-exitcode", "1", sys.executable, os.path.abspath(__file__), SANITIZER_CHILD_FLAG]
    completed = subprocess.run(command, input=request_bytes, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=600)
    return completed.returncode == 0


def load_baseline(torch):
    """The pinned runtime's own kernel is the only fair comparison."""
    try:
        from vllm import _custom_ops as ops
    except ImportError as error:
        raise StageFailure("baseline runtime kernel unavailable") from error

    def baseline(x, residual, weight):
        ops.fused_add_rms_norm(x.clone(), residual.clone(), weight, EPSILON)

    return baseline


def median_microseconds(triton, function):
    return triton.testing.do_bench(function) * 1000


def sanitizer_child(source: str) -> None:
    import torch

    kernel = load_candidate(source)
    x, residual, weight = inputs(torch, 8, 1024, 11)
    kernel(x, residual, weight, EPSILON)


def main() -> int:
    request_bytes = sys.stdin.buffer.read()
    request = json.loads(request_bytes)
    if SANITIZER_CHILD_FLAG in sys.argv[1:]:
        sanitizer_child(request["source"])
        return 0

    import torch
    import triton

    stages = {stage: False for stage in STAGES}
    result = {"correctness": stages, "baseline_median_us": 0.0, "candidate_median_us": 0.0, "roofline_utilization": 0.0, "cost_usd": 0.0}
    verification = bool(request.get("verification"))
    seed = 104729 if verification else 17
    shapes = VERIFICATION_SHAPES if verification else SHAPES
    stage = "load"
    try:
        kernel = load_candidate(request["source"])
        stage = "smoke"
        x, residual, weight = inputs(torch, 1, HIDDEN, seed)
        check(torch, kernel, x, residual, weight, stage)
        stages[stage] = True

        stage = "shape_sweep"
        for rows, hidden in shapes:
            x, residual, weight = inputs(torch, rows, hidden, seed + rows)
            check(torch, kernel, x, residual, weight, stage)
        stages[stage] = True

        stage = "numerical_stability"
        for kind in ("large", "small", "sparse"):
            x, residual, weight = inputs(torch, ROWS, HIDDEN, seed + 3, kind)
            check(torch, kernel, x, residual, weight, stage)
        stages[stage] = True

        stage = "determinism"
        check_determinism(torch, kernel, seed)
        stages[stage] = True

        stage = "edge_cases"
        check_edge_cases(torch, kernel)
        stages[stage] = True

        if not CPU_MODE:
            stage = "sanitizer"
            stages[stage] = run_sanitizer(request_bytes)
            if not stages[stage]:
                result["failed_stage"] = stage

            stage = "timing"
            x, residual, weight = inputs(torch, 128, 1024, seed)
            baseline = load_baseline(torch)
            result["baseline_median_us"] = median_microseconds(triton, lambda: baseline(x, residual, weight))
            result["candidate_median_us"] = median_microseconds(triton, lambda: kernel(x, residual, weight, EPSILON))
    except StageFailure:
        result["failed_stage"] = stage
    except Exception:  # Generated code may fail in any way; report only the stage.
        result["failed_stage"] = stage
    print(json.dumps(result))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

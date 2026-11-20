#!/usr/bin/env python3
"""Hidden evaluator for generated mixture-of-experts expert kernels.

Runs inside the isolated GPU container. It reads one evaluation request on
stdin and prints stage results and timings. It reports only which stage failed,
so test inputs never reach the model writing the kernel. Besides the reference
math it checks the failure modes of generated MoE kernels: ignoring the router
weights, swapping gate and up, skipping experts that received no tokens or all
of them, reading uninitialized memory, and results that depend on the other
tokens in the batch.
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
# (tokens, experts, hidden, intermediate, top_k). Small decode batches come
# first; hidden and intermediate sizes include ones no power-of-two tile covers.
SHAPES = (
    (1, 8, 512, 256, 2),
    (4, 16, 1024, 512, 4),
    (8, 64, 2048, 768, 8),
    (16, 128, 2048, 768, 8),
    (64, 128, 2048, 768, 8),
    (3, 8, 1000, 200, 1),
    (33, 32, 1024, 384, 6),
)
VERIFICATION_SHAPES = ((2, 32, 1536, 512, 4), (7, 64, 2048, 768, 8), (48, 16, 896, 320, 2), (129, 128, 2048, 768, 8))
SANITIZER_CHILD_FLAG = "--sanitizer-child"
# INFERCRANE_EVAL_DEVICE=cpu with TRITON_INTERPRET=1 runs the same checks on a
# laptop. It proves correctness only: the sanitizer and timing need a GPU and
# are reported as not run, never as passed.
DEVICE = os.environ.get("INFERCRANE_EVAL_DEVICE", "cuda")
CPU_MODE = DEVICE == "cpu"
if CPU_MODE:  # Triton's interpreter is slow, so the CPU suites are smaller.
    SHAPES = ((1, 4, 64, 32, 2), (3, 4, 96, 40, 1), (5, 8, 64, 48, 3), (8, 8, 128, 64, 2))
    VERIFICATION_SHAPES = ((2, 6, 80, 24, 2), (7, 8, 64, 32, 4))
BASE = (8, 8, 128, 64, 2) if CPU_MODE else (16, 64, 1024, 512, 8)
# Decode at four requests on Qwen3-30B-A3B's expert shape: the batch where the
# engine's own kernel is reported weakest.
TIMING = (4, 128, 2048, 768, 8)
ATOL, RTOL = 3e-2, 3e-2


class StageFailure(Exception):
    pass


def load_candidate(source: str):
    directory = Path(tempfile.mkdtemp(prefix="kernel-eval-"))
    path = directory / "kernel.py"
    path.write_text(source, encoding="utf-8")
    spec = importlib.util.spec_from_file_location("generated_kernel", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module.fused_moe_experts


def reference(torch, hidden_states, w13, w2, topk_weights, topk_ids):
    """FP32 math, rounded to BF16 once, one expert at a time."""
    tokens, hidden = hidden_states.shape
    intermediate = w2.shape[2]
    x = hidden_states.float()
    output = torch.zeros((tokens, hidden), device=hidden_states.device, dtype=torch.float32)
    for expert in torch.unique(topk_ids).tolist():
        rows, slots = (topk_ids == expert).nonzero(as_tuple=True)
        gate_up = x[rows] @ w13[expert].float().t()
        activated = torch.nn.functional.silu(gate_up[:, :intermediate]) * gate_up[:, intermediate:]
        output.index_add_(0, rows, (activated @ w2[expert].float().t()) * topk_weights[rows, slots].unsqueeze(1))
    return output.to(hidden_states.dtype)


def route(torch, logits, top_k):
    weights, ids = torch.topk(torch.softmax(logits, dim=-1), top_k, dim=-1)
    return (weights / weights.sum(dim=-1, keepdim=True)).float(), ids.to(torch.int32)


def inputs(torch, shape, seed, kind="normal"):
    tokens, experts, hidden, intermediate, top_k = shape
    generator = torch.Generator(device=DEVICE).manual_seed(seed)

    def draw(size, scale=1.0):
        return torch.randn(size, device=DEVICE, dtype=torch.float32, generator=generator) * scale

    x = draw((tokens, hidden))
    w13 = draw((experts, 2 * intermediate, hidden), hidden ** -0.5)
    w2 = draw((experts, hidden, intermediate), intermediate ** -0.5)
    logits = draw((tokens, experts))
    if kind == "large":
        x = x * 1e2
    elif kind == "small":
        x = x * 1e-3
    elif kind == "sparse":
        x = x * (torch.rand((tokens, hidden), device=DEVICE, generator=generator) > 0.9)
    elif kind == "hot":  # Every token picks the same experts; the rest get none.
        logits = torch.zeros_like(logits)
        logits[:, :top_k] = torch.arange(top_k, 0, -1, device=DEVICE, dtype=torch.float32)
    weights, ids = route(torch, logits, top_k)
    return x.bfloat16().contiguous(), w13.bfloat16(), w2.bfloat16(), weights, ids


def check(torch, kernel, case, stage):
    sources = [tensor.clone() for tensor in case]
    output = kernel(*case)
    expected = reference(torch, *case)
    if output.shape != expected.shape or output.dtype != expected.dtype:
        raise StageFailure(stage)
    if not all(torch.equal(tensor, source) for tensor, source in zip(case, sources)):
        raise StageFailure(stage)
    try:
        torch.testing.assert_close(output.float(), expected.float(), atol=ATOL, rtol=RTOL)
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
    case = inputs(torch, BASE, seed)
    dirty_memory(torch)
    first = kernel(*case)
    dirty_memory(torch)
    second = kernel(*case)
    if not torch.equal(first, second):
        raise StageFailure("determinism")
    # A token's result must not depend on the tokens batched with it.
    x, w13, w2, weights, ids = case
    other = inputs(torch, BASE, seed + 1)
    tokens = x.shape[0]
    for index in (0, tokens // 2, tokens - 1):
        alone = kernel(x[index:index + 1].contiguous(), w13, w2, weights[index:index + 1].contiguous(), ids[index:index + 1].contiguous())
        mixed_x, mixed_weights, mixed_ids = other[0].clone(), other[3].clone(), other[4].clone()
        mixed_x[index], mixed_weights[index], mixed_ids[index] = x[index], weights[index], ids[index]
        mixed = kernel(mixed_x, w13, w2, mixed_weights, mixed_ids)
        for row in (alone[0], mixed[index]):
            try:
                torch.testing.assert_close(row.float(), first[index].float(), atol=1e-2, rtol=1e-2)
            except AssertionError as error:
                raise StageFailure("determinism") from error


def check_edge_cases(torch, kernel, seed):
    x, w13, w2, weights, ids = inputs(torch, BASE, seed)
    # Router weights of zero must give zero: a kernel that ignores them fails.
    check(torch, kernel, (x, w13, w2, torch.zeros_like(weights), ids), "edge_cases")
    check(torch, kernel, (torch.zeros_like(x), w13, w2, weights, ids), "edge_cases")
    # Every token on the same experts, and the last expert index in use.
    check(torch, kernel, inputs(torch, BASE, seed + 2, "hot"), "edge_cases")
    tokens, top_k = ids.shape
    last = torch.cat([torch.full((tokens, 1), w13.shape[0] - 1, device=DEVICE, dtype=torch.int32), torch.arange(top_k - 1, device=DEVICE, dtype=torch.int32).repeat(tokens, 1)], dim=1)
    check(torch, kernel, (x, w13, w2, weights, last.contiguous()), "edge_cases")
    # One token, one expert.
    single = inputs(torch, (1, BASE[1], BASE[2], BASE[3], 1), seed + 3)
    check(torch, kernel, single, "edge_cases")


def run_sanitizer(request_bytes: bytes) -> bool:
    """A missing tool fails closed; memory safety is never assumed."""
    tool = shutil.which("compute-sanitizer")
    if tool is None:
        return False
    command = [tool, "--tool", "memcheck", "--error-exitcode", "1", sys.executable, os.path.abspath(__file__), SANITIZER_CHILD_FLAG]
    completed = subprocess.run(command, input=request_bytes, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=900)
    return completed.returncode == 0


def load_baseline():
    """The pinned runtime's own Triton experts are the only fair comparison."""
    try:
        from vllm.model_executor.layers.fused_moe.fused_moe import fused_experts
    except ImportError as error:
        raise StageFailure("baseline runtime kernel unavailable") from error
    return fused_experts


def median_microseconds(triton, function):
    return triton.testing.do_bench(function) * 1000


def minimum_bytes(tokens, hidden, intermediate, experts_used, element_bytes, top_k):
    """Bytes every correct kernel must move: each routed expert's weights once,
    the tokens in and out, and the router's weights (FP32) and ids (INT32)."""
    weights = experts_used * (2 * intermediate * hidden + hidden * intermediate) * element_bytes
    activations = 2 * tokens * hidden * element_bytes
    routing = tokens * top_k * (4 + 4)
    return weights + activations + routing


def roofline_utilization(bytes_moved, median_us, peak_gbps):
    """Fraction of the DRAM roofline reached: the fastest possible time for
    these bytes at peak bandwidth, divided by the measured time.

    Small-batch expert matrix multiplies are memory-bound, so this is their
    roofline. 0.0 means unknown. A value above 1 cannot come from DRAM (the
    weights were cache-resident), so it is reported as unknown."""
    if not (bytes_moved > 0 and median_us > 0 and peak_gbps > 0):
        return 0.0
    utilization = (bytes_moved / (peak_gbps * 1e3)) / median_us
    return utilization if utilization <= 1.0 else 0.0


def peak_dram_gbps(triton):
    """Peak DRAM bandwidth from the driver's memory clock and bus width, or 0.0."""
    try:
        return float(triton.testing.get_dram_gbps())
    except Exception:  # Older Triton or an unknown device: leave it unknown.
        return 0.0


def sanitizer_child(source: str) -> None:
    import torch

    kernel = load_candidate(source)
    kernel(*inputs(torch, (8, 16, 512, 256, 4), 11))


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
        check(torch, kernel, inputs(torch, (1,) + BASE[1:], seed), stage)
        stages[stage] = True

        stage = "shape_sweep"
        for index, shape in enumerate(shapes):
            check(torch, kernel, inputs(torch, shape, seed + index), stage)
        stages[stage] = True

        stage = "numerical_stability"
        for kind in ("large", "small", "sparse"):
            check(torch, kernel, inputs(torch, BASE, seed + 3, kind), stage)
        stages[stage] = True

        stage = "determinism"
        check_determinism(torch, kernel, seed)
        stages[stage] = True

        stage = "edge_cases"
        check_edge_cases(torch, kernel, seed)
        stages[stage] = True

        if not CPU_MODE:
            stage = "sanitizer"
            stages[stage] = run_sanitizer(request_bytes)
            if not stages[stage]:
                result["failed_stage"] = stage

            stage = "timing"
            case = inputs(torch, TIMING, seed)
            baseline = load_baseline()
            result["baseline_median_us"] = median_microseconds(triton, lambda: baseline(*case))
            result["candidate_median_us"] = median_microseconds(triton, lambda: kernel(*case))
            tokens, _, hidden, intermediate, top_k = TIMING
            experts_used = int(torch.unique(case[4]).numel())
            result["roofline_utilization"] = roofline_utilization(
                minimum_bytes(tokens, hidden, intermediate, experts_used, case[0].element_size(), top_k), result["candidate_median_us"], peak_dram_gbps(triton)
            )
    except StageFailure:
        result["failed_stage"] = stage
    except Exception:  # Generated code may fail in any way; report only the stage.
        result["failed_stage"] = stage
    print(json.dumps(result))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

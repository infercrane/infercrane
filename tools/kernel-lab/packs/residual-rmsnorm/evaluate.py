#!/usr/bin/env python3
"""Hidden evaluator for generated residual-add + RMSNorm kernels.

Runs inside the isolated GPU container. It reads one evaluation request on
stdin and prints stage results and timings. It reports only which stage failed,
so test inputs never reach the model writing the kernel. The checks follow the
published failure modes of generated kernels: bypasses for narrow test values,
uninitialized-memory wins, and batch-dependent output.

Hardening (Wafer's reward-hack field guide; KernelBench-Verified):
- The generated module runs in this process, so it could rebind the checks.
  Before it loads, the evaluator records this file's and the pack's bytes and
  the identity of every function the checks call, its own and torch's; any
  change afterwards fails the "integrity" stage.
- Correctness also runs at the shapes the profiler saw on the customer's
  traffic (request "shapes", rows x hidden) and at row counts next to them that
  a hidden seed picks, which no prompt shows.
- Timing is fair to the runtime's own kernel: both run captured in a CUDA graph
  (vLLM serves decode that way), the baseline in place with no copies, and each
  call reads the next of several input sets larger than twice the L2 cache.
- A candidate faster than the bytes it must move allow at the GPU's peak memory
  bandwidth fails the "physics_floor" stage: the timing is wrong, not fast.
Every new failure is reported as failed_stage, with all correctness false.
"""

from __future__ import annotations

import hashlib
import importlib.util
import json
import os
import random
import re
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


# The profiler's shape strings: "rows×hidden dtype; rows×hidden dtype; hidden dtype".
SHAPE_PART = re.compile(r"^\s*(\d+)[×x](\d+)(?:\s+\S+)?\s*$")
MAX_TRAFFIC_SHAPES = 8
MAX_ROWS, MAX_HIDDEN = 65536, 65536
FLOOR_FACTOR = 0.9  # a time below 90% of the memory floor is a timing fault
L2_BYTES = 50 << 20  # H100; larger caches are covered by the factor of two below


def traffic_shapes(shapes) -> list:
    """(rows, hidden) of each profiled shape whose first input is rows x hidden."""
    out = []
    for text in shapes or []:
        if not isinstance(text, str) or len(text) > 512:
            continue
        match = SHAPE_PART.match(text.split(";")[0])
        if not match:
            continue
        rows, hidden = int(match.group(1)), int(match.group(2))
        if 0 < rows <= MAX_ROWS and 0 < hidden <= MAX_HIDDEN and (rows, hidden) not in out:
            out.append((rows, hidden))
        if len(out) == MAX_TRAFFIC_SHAPES:
            break
    return out


def hidden_neighbours(shapes, seed) -> list:
    """Row counts next to each traffic shape that only the evaluator's seed knows."""
    rng = random.Random(seed)
    return [(max(1, rows + rng.choice((-7, -3, -1, 1, 3, 7))), hidden) for rows, hidden in shapes]


def file_digest(path) -> str:
    try:
        return hashlib.sha256(Path(path).read_bytes()).hexdigest()
    except OSError:
        return ""


def integrity_snapshot(torch=None) -> dict:
    """What the checks depend on: the evaluator's and the pack's bytes, and the
    identity of every function they call."""
    here = Path(__file__)
    snapshot = {"file:evaluate.py": file_digest(here), "file:pack.json": file_digest(here.with_name("pack.json"))}
    # This module's own namespace, however it was loaded (as __main__ in a run).
    for name, value in sorted(globals().items()):
        if not name.startswith("__"):
            snapshot["evaluate." + name] = id(value)
    if torch is not None:
        targets = {"torch.equal": torch.equal, "torch.allclose": torch.allclose, "torch.rsqrt": torch.rsqrt, "torch.randn": torch.randn,
                   "torch.rand": torch.rand, "torch.isfinite": torch.isfinite, "torch.Generator": torch.Generator,
                   "Tensor.float": torch.Tensor.float, "Tensor.clone": torch.Tensor.clone, "Tensor.equal": torch.Tensor.equal,
                   "Tensor.__eq__": torch.Tensor.__eq__, "Tensor.abs": torch.Tensor.abs, "Tensor.max": torch.Tensor.max, "Tensor.mean": torch.Tensor.mean}
        for name in ("synchronize", "CUDAGraph", "graph", "Event"):
            if hasattr(torch.cuda, name):
                targets["cuda." + name] = getattr(torch.cuda, name)
        if hasattr(torch, "testing") and hasattr(torch.testing, "assert_close"):
            targets["testing.assert_close"] = torch.testing.assert_close
        for name, value in targets.items():
            snapshot[name] = id(value)
    return snapshot


def integrity_changes(before: dict, after: dict) -> list:
    return sorted(name for name in set(before) | set(after) if before.get(name) != after.get(name))


def below_memory_floor(bytes_moved, median_us, peak_gbps) -> bool:
    """A time the GPU's memory cannot deliver: below FLOOR_FACTOR of the bytes
    the kernel must move at peak bandwidth. Unknown peak decides nothing."""
    if not (bytes_moved > 0 and median_us > 0 and peak_gbps > 0):
        return False
    return median_us < FLOOR_FACTOR * bytes_moved / (peak_gbps * 1e3)


def rotation_sets(rows, hidden, element_bytes) -> int:
    """Input sets a timed graph rotates through so they exceed twice the L2."""
    per_set = minimum_bytes(rows, hidden, element_bytes)
    return max(2, min(256, -(-2 * L2_BYTES // max(1, per_set))))


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
    """The pinned runtime's own kernel is the only fair comparison. It works in
    place, so it is timed in place: copying its inputs first would charge it
    for traffic the candidate never pays."""
    try:
        from vllm import _custom_ops as ops
    except ImportError as error:
        raise StageFailure("baseline runtime kernel unavailable") from error

    def baseline(x, residual, weight):
        ops.fused_add_rms_norm(x, residual, weight, EPSILON)

    return baseline


def median_microseconds(triton, function):
    return triton.testing.do_bench(function) * 1000


def graph_median_us(torch, call, sets, calls=20, repeats=50):
    """Median microseconds of one call, captured in a CUDA graph of `calls`
    calls, call i reading input set i mod len(sets)."""
    call(sets[0])
    torch.cuda.synchronize()
    graph = torch.cuda.CUDAGraph()
    stream = torch.cuda.Stream()
    with torch.cuda.stream(stream), torch.cuda.graph(graph, stream=stream):
        for index in range(calls):
            call(sets[index % len(sets)])
    torch.cuda.synchronize()
    for _ in range(3):
        graph.replay()
    torch.cuda.synchronize()
    begin, end = torch.cuda.Event(enable_timing=True), torch.cuda.Event(enable_timing=True)
    times = []
    for _ in range(repeats):
        begin.record()
        graph.replay()
        end.record()
        end.synchronize()
        times.append(begin.elapsed_time(end) * 1000 / calls)
    graph.reset()
    times.sort()
    return times[len(times) // 2]


def minimum_bytes(rows, hidden, element_bytes):
    """Bytes every correct kernel must move: read x, residual and weight, then
    write output and residual_out."""
    return (4 * rows * hidden + hidden) * element_bytes


def roofline_utilization(bytes_moved, median_us, peak_gbps):
    """Fraction of the DRAM roofline reached: the fastest possible time for
    these bytes at peak bandwidth, divided by the measured time.

    The operator is memory-bound, so this is its roofline. 0.0 means unknown.
    A value above 1 cannot come from DRAM (the data was cache-resident), so it
    is reported as unknown rather than clamped into a false stop."""
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
    traffic = [] if CPU_MODE else traffic_shapes(request.get("shapes"))
    shapes = (VERIFICATION_SHAPES if verification else SHAPES) + tuple(traffic) + tuple(hidden_neighbours(traffic, seed * 7919))
    before = integrity_snapshot(torch)
    stage = "load"
    try:
        kernel = load_candidate(request["source"])
        stage = "integrity"
        if integrity_changes(before, integrity_snapshot(torch)):
            raise StageFailure("the candidate changed what the checks rely on")
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
            # The customer's most common profiled shape when known, else the pack's.
            rows, hidden = traffic[0] if traffic else (128, 1024)
            element = 2  # BF16
            sets = [inputs(torch, rows, hidden, seed + index) for index in range(rotation_sets(rows, hidden, element))]
            baseline = load_baseline(torch)
            result["baseline_median_us"] = graph_median_us(torch, lambda tensors: baseline(*tensors), sets)
            result["candidate_median_us"] = graph_median_us(torch, lambda tensors: kernel(*tensors, EPSILON), sets)
            bytes_moved, peak = minimum_bytes(rows, hidden, element), peak_dram_gbps(triton)
            stage = "physics_floor"
            if below_memory_floor(bytes_moved, result["candidate_median_us"], peak):
                raise StageFailure("faster than the GPU's memory allows")
            result["roofline_utilization"] = roofline_utilization(bytes_moved, result["candidate_median_us"], peak)
            stage = "integrity"
            if integrity_changes(before, integrity_snapshot(torch)):
                raise StageFailure("the candidate changed what the checks rely on")
    except StageFailure:
        result["failed_stage"] = stage
    except Exception:  # Generated code may fail in any way; report only the stage.
        result["failed_stage"] = stage
    if result.get("failed_stage") in ("integrity", "physics_floor"):
        # Nothing measured after a rebound check or an impossible time is evidence.
        result["correctness"] = {name: False for name in STAGES}
        result["baseline_median_us"] = result["candidate_median_us"] = result["roofline_utilization"] = 0.0
    print(json.dumps(result))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

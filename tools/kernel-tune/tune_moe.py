"""Tunes vLLM's Triton fused-MoE kernel for one GPU and one model's MoE shape, at the step
sizes a workload's loads reach, and writes the result as a tuned-config set
(internal/kerneltuning, domain.KernelConfigSet): data the engine reads, never code.

Runs inside the pinned engine image on the GPU it tunes for. For every step size (key):

1. Time the engine's own choice: what vLLM resolves with no tuned folder (its shipped file
   for this shape and GPU, or its built-in default), twice, before and after the search,
   to know the key's own run-to-run spread.
2. Search tile settings by coordinate descent from seeds (the engine's choice, the previous
   key's winner), one setting at a time over the reviewed values, two passes, within a
   per-key and a global time budget.
3. Keep the winner only when it is faster than the engine's choice by more than
   max(3%, twice the spread) and passes the checks below; otherwise write the engine's own
   default for that key, so the file reproduces what the engine would have done.

Checks on every kept setting, none of which the search sees: no NaN or Inf; error against
an FP32 reference no larger than the engine's own (x1.25); a time no faster than the
memory-traffic floor allows (a faster time is a timing fault, not a gain); then the file as
the engine reads it (nearest key) against the engine's choice at step sizes between the
keys ("hidden" sizes); a hidden size that is slower adds a key with the engine's default.

The timings are the kernel alone, with CUDA graphs, uniform random routing like vLLM's own
tuner (benchmark_moe.py), and each call of a graph reading another copy of the weights, so no
call finds its experts in the L2 cache. They choose what to
measure end to end; they never claim a serving gain.

`--stand-in` replaces the GPU with a fixed timing function so the whole path, files and
digest included, runs on a laptop for $0. Nothing it reports is a measurement.
"""

from __future__ import annotations

import argparse
import contextlib
import hashlib
import json
import math
import os
import random
import re
import sys
import time
from dataclasses import dataclass, field

TUNER = "infercrane-fused-moe-tuner-v1"
OPERATOR = "fused-moe"
MOUNT_PATH = "/opt/infercrane/kernel-configs"

# Step sizes, as internal/kerneltuning/keys.go plans them (parity is tested).
DECODE_GRID = (1, 2, 4, 8, 16, 24, 32, 48, 64, 96, 128, 192, 256, 384, 512, 768, 1024, 1536)
PREFILL_GRID = (2048, 3072, 4096, 6144, 8192, 12288, 16384, 24576, 32768)
# get_default_config, block-quantized FP8 on CUDA: the tiles change above these (kerneltuning).
DEFAULT_BOUNDARIES = {("vllm", "0.34.0", "fp8_w8a8+block"): (4, 8, 16, 64), ("vllm", "0.22.1", "fp8_w8a8+block"): (16, 64)}

# Reviewed tile values (internal/kerneltuning/configset.go accepts a superset).
SPACE = {
    "BLOCK_SIZE_M": (16, 32, 64, 128),
    "BLOCK_SIZE_N": (32, 64, 128, 256),
    "BLOCK_SIZE_K": (64, 128, 256),
    "GROUP_SIZE_M": (1, 16, 32),
    "num_warps": (4, 8),
    "num_stages": (2, 3, 4, 5),
}
ORDER = ("BLOCK_SIZE_N", "BLOCK_SIZE_K", "num_warps", "num_stages", "BLOCK_SIZE_M", "GROUP_SIZE_M")
FIELDS = ("BLOCK_SIZE_M", "BLOCK_SIZE_N", "BLOCK_SIZE_K", "GROUP_SIZE_M", "SPLIT_K", "num_warps", "num_stages")

MIN_GAIN = 0.03          # a kept setting is at least 3% faster at kernel level ...
SPREAD_FACTOR = 2.0      # ... and more than twice the engine choice's own spread
ERROR_FACTOR = 1.25      # reference error no worse than the engine's own, by this factor
FLOOR_FACTOR = 0.9       # a time below 90% of the memory floor is a timing fault
# H100 SXM HBM3 peak memory bandwidth, bytes per second (NVIDIA H100 datasheet: 3.35 TB/s).
# L40S: 864 GB/s and L4: 300 GB/s (NVIDIA datasheets).
PEAK_BANDWIDTH = {"NVIDIA_H100_80GB_HBM3": 3.35e12, "NVIDIA_H200": 4.8e12, "NVIDIA_L40S": 864e9, "NVIDIA_L4": 300e9}


# ----------------------------------------------------------------------------- pure parts

@dataclass(frozen=True)
class Shape:
    experts: int
    intermediate: int
    dtype: str
    block_n: int
    block_k: int
    hidden: int
    topk: int

    def dtype_class(self) -> str:
        return self.dtype + ("+block" if self.block_n and self.block_k else "")


def shape_from_config(config: dict, tensor_parallel: int = 1) -> Shape:
    """internal/kerneltuning ShapeFromModelConfig, plus the hidden size and top-k the
    kernel's inputs need."""
    fields = config.get("text_config") or config

    def integer(*names: str) -> int:
        for name in names:
            for source in (fields, config):
                value = source.get(name)
                if isinstance(value, int) and value > 0:
                    return value
        return 0

    experts = integer("num_experts", "n_routed_experts", "num_local_experts")
    intermediate = integer("moe_intermediate_size") or integer("intermediate_size")
    if not experts or not intermediate or intermediate % tensor_parallel:
        raise ValueError("the model has no routed experts whose size splits over the GPUs")
    quantization = fields.get("quantization_config") or config.get("quantization_config") or {}
    if quantization.get("quant_method") != "fp8" or len(quantization.get("weight_block_size") or []) != 2:
        raise ValueError("only block-quantized FP8 fused-MoE files are reviewed")
    block_n, block_k = quantization["weight_block_size"]
    return Shape(experts, intermediate // tensor_parallel, "fp8_w8a8", block_n, block_k, integer("hidden_size"), integer("num_experts_per_tok", "moe_topk"))


FILE_NAME = re.compile(r"^E=(\d{1,4}),N=(\d{1,6}),device_name=([A-Za-z0-9_.-]{1,64})(?:,dtype=([a-z0-9_]{1,16}))?(?:,block_shape=\[(\d{1,4}),(\d{1,4})\])?\.json$")


def shape_from_file_name(name: str, hidden: int, topk: int) -> tuple[Shape, str]:
    """The shape and device a file name the engine looked for names (kerneltuning.ParseFileName)."""
    match = FILE_NAME.match(name)
    if not match or match.group(4) != "fp8_w8a8" or not match.group(5):
        raise ValueError(f"{name!r} is not a block-quantized FP8 fused-MoE file name")
    shape = Shape(int(match.group(1)), int(match.group(2)), "fp8_w8a8", int(match.group(5)), int(match.group(6)), hidden, topk)
    if file_name(shape, match.group(3)) != name:
        raise ValueError(f"{name!r} is not in the engine's own form")
    return shape, match.group(3)


def file_name(shape: Shape, device: str) -> str:
    """vLLM get_config_file_name; kerneltuning.FileName."""
    name = f"E={shape.experts},N={shape.intermediate},device_name={device}"
    if shape.dtype:
        name += f",dtype={shape.dtype}"
    if shape.block_n and shape.block_k:
        name += f",block_shape=[{shape.block_n},{shape.block_k}]"
    return name + ".json"


def engine_device_name(reported: str) -> str:
    """vLLM get_device_name_as_file_name, H200 family folded as get_config_file_name does."""
    name = re.sub(r"[\s/]+", "_", reported.strip())
    return "NVIDIA_H200" if "H200" in name.split("_") else name


def plan_keys(runtime: str, version: str, shape: Shape, loads: list[dict]) -> dict:
    """kerneltuning.Keys: the step sizes to tune and the ones that keep the default exact."""
    boundaries = DEFAULT_BOUNDARIES.get((runtime, version, shape.dtype_class()))
    if boundaries is None:
        raise ValueError(f"kernel tuning is not reviewed for {runtime} {version} {shape.dtype_class()}")
    decode = max(load["running"] * (1 + load.get("draft_tokens", 0)) for load in loads)
    batched = max(load.get("batched_tokens", 0) for load in loads)
    tune: set[int] = set()
    for size in DECODE_GRID:
        tune.add(size)
        if size >= decode:
            break
    tune.update(size for size in PREFILL_GRID if size <= batched)
    if batched:
        tune.add(batched)
    default = {size for edge in boundaries for size in (edge, edge + 1)} - tune
    return {"tune": sorted(tune), "default": sorted(default)}


def nearest_key(keys: list[int], tokens: int) -> int:
    """The entry vLLM picks: min(keys, key=|key - tokens|), first (smallest) on a tie."""
    return min(sorted(keys), key=lambda key: abs(key - tokens))


def canonical_file(entries: dict[int, dict]) -> bytes:
    """kerneltuning.CanonicalFile: ascending keys, the engine's field order, no spaces."""
    ordered = {}
    for key in sorted(entries):
        config = entries[key]
        ordered[str(key)] = {name: config[name] for name in FIELDS if name in config}
    return json.dumps(ordered, separators=(",", ":")).encode()


def set_digest(kernel_set: dict) -> str:
    """kerneltuning.Digest: Go's json.Marshal of the named fields, in struct order."""
    files = sorted(({"name": f["name"], "sha256": f["sha256"]} for f in kernel_set["files"]), key=lambda f: f["name"])
    body = {"operator": kernel_set["operator"], "runtime": kernel_set["runtime"], "runtime_version": kernel_set["runtime_version"],
            "device": kernel_set["device"], "tuner": kernel_set["tuner"], "files": files}
    return "sha256:" + hashlib.sha256(json.dumps(body, separators=(",", ":")).encode()).hexdigest()


def build_set(runtime: str, version: str, device: str, files: dict[str, dict[int, dict]]) -> dict:
    out = {"operator": OPERATOR, "runtime": runtime, "runtime_version": version, "device": device, "tuner": TUNER, "files": []}
    for name in sorted(files):
        content = canonical_file(files[name])
        out["files"].append({"name": name, "content": content.decode(), "sha256": hashlib.sha256(content).hexdigest()})
    out["digest"] = set_digest(out)
    return out


def error_bound(known: list[float]) -> float:
    """The reference error a setting may show where the engine's own setting cannot run:
    the worst the engine showed where it did run (x1.25 + 0.001), or 0.1 before any."""
    return ERROR_FACTOR * max(known) + 1e-3 if known else 0.1


def worth_keeping(engine_us: float, engine_spread: float, best_us: float) -> bool:
    """A tuned setting replaces the engine's only when faster by more than the noise."""
    return best_us < engine_us * (1 - max(MIN_GAIN, SPREAD_FACTOR * engine_spread))


def aligned(config: dict, shape: Shape) -> bool:
    """kerneltuning's rule: a tile and the quantization block divide one another."""
    ok = lambda tile, block: tile % block == 0 or block % tile == 0  # noqa: E731
    return not (shape.block_n and shape.block_k) or (ok(config["BLOCK_SIZE_N"], shape.block_n) and ok(config["BLOCK_SIZE_K"], shape.block_k))


def neighbours(config: dict, shape: Shape, name: str) -> list[dict]:
    out = []
    for value in SPACE[name]:
        if value != config.get(name):
            candidate = {k: v for k, v in config.items() if k != "SPLIT_K"}
            candidate[name] = value
            # vLLM 0.34.0 invoke_fused_moe_triton_kernel runs a block-quantized kernel with
            # BLOCK_SIZE_K at most the quantization block: a larger one times the same kernel.
            if name == "BLOCK_SIZE_K" and shape.block_n and shape.block_k and value > min(shape.block_n, shape.block_k):
                continue
            if aligned(candidate, shape):
                out.append(candidate)
    return out


def config_key(config: dict) -> tuple:
    return tuple(config.get(name) for name in FIELDS if name != "SPLIT_K")


def memory_floor_us(shape: Shape, tokens: int, distinct_experts: float, peak: float) -> float:
    """The least time the kernel can take: each touched expert's FP8 weights and block
    scales read once, the activations read and the output written once."""
    per_expert = (2 * shape.intermediate * shape.hidden + shape.hidden * shape.intermediate)  # FP8: one byte a weight
    if shape.block_n and shape.block_k:
        per_expert += 4 * (math.ceil(2 * shape.intermediate / shape.block_n) * math.ceil(shape.hidden / shape.block_k)
                           + math.ceil(shape.hidden / shape.block_n) * math.ceil(shape.intermediate / shape.block_k))
    activations = tokens * shape.hidden * 2 * 2  # BF16 in and out
    return (distinct_experts * per_expert + activations) / peak * 1e6


def expected_distinct(experts: int, draws: int) -> float:
    return experts * (1 - (1 - 1 / experts) ** draws)


def hidden_sizes(keys: list[int]) -> list[int]:
    """Step sizes between keys, which the search never timed: the midpoint of each gap."""
    ordered = sorted(keys)
    return [(a + b) // 2 for a, b in zip(ordered, ordered[1:]) if b - a > 1]


# ----------------------------------------------------------------------------- measuring

@dataclass
class Bench:
    """Times one setting at one step size. `None` is the engine's own choice."""
    shape: Shape
    stand_in: bool
    peak: float = PEAK_BANDWIDTH["NVIDIA_H100_80GB_HBM3"]
    repeats: int = 30
    graph_calls: int = 10
    compiled: set = field(default_factory=set)

    def time(self, config: dict | None, tokens: int, seed: int = 0) -> float:
        if self.stand_in:
            return stand_in_time(self.shape, config, tokens, seed, self.peak)
        return gpu_time(self.shape, config, tokens, self.repeats, self.graph_calls, seed)

    def check(self, config: dict, tokens: int, bound: float) -> dict:
        """`bound` is the error allowed when the engine's own setting cannot run here."""
        if self.stand_in:
            fits = stand_in_fits(None, tokens)
            return {"finite": True, "error": 0.01, "engine_error": 0.01 if fits else None, "passed": fits or 0.01 <= bound}
        return gpu_check(self.shape, config, tokens, bound)

    def engine_choice(self, tokens: int) -> dict:
        if self.stand_in:
            return stand_in_default(tokens)
        from vllm.model_executor.layers.fused_moe.fused_moe import try_get_optimal_moe_config
        w1 = (self.shape.experts, 2 * self.shape.intermediate, self.shape.hidden)
        w2 = (self.shape.experts, self.shape.hidden, self.shape.intermediate)
        with setting(None):  # the engine's own choice, never a leftover override
            return dict(try_get_optimal_moe_config(w1, w2, self.shape.topk, self.shape.dtype, tokens, block_shape=[self.shape.block_n, self.shape.block_k]))


def stand_in_default(tokens: int) -> dict:
    """vLLM 0.34.0 get_default_config for block FP8 on CUDA, written out."""
    block_n = 64 if tokens <= 8 else 128
    stages = 4 if tokens <= 4 else 3
    return {"BLOCK_SIZE_M": 16 if tokens <= 64 else 64, "BLOCK_SIZE_N": block_n, "BLOCK_SIZE_K": 128,
            "GROUP_SIZE_M": 1 if tokens <= 16 else 32, "SPLIT_K": 1, "num_warps": 4, "num_stages": stages}


STAND_IN_SHARED = 101376  # an L40S's per-block limit, as rung 2's first run reported it


def stand_in_fits(config: dict | None, tokens: int) -> bool:
    """A made-up shared-memory rule so a dry run meets settings that do not fit: stages x
    (A tile + B tile) bytes, plus 8 KiB, against an L40S's limit. The stand-in's own default
    also fails at 1 to 4 tokens, only to exercise that path: on the L40S runs so far the
    engine's default did not fail; a leaked override did (see `setting`)."""
    config = config or stand_in_default(tokens)
    need = config["num_stages"] * (config["BLOCK_SIZE_M"] * config["BLOCK_SIZE_K"] + config["BLOCK_SIZE_K"] * config["BLOCK_SIZE_N"]) + 8192
    return need <= STAND_IN_SHARED and not (config is not None and tokens <= 4 and config == stand_in_default(tokens))


def stand_in_time(shape: Shape, config: dict | None, tokens: int, seed: int, peak: float = PEAK_BANDWIDTH["NVIDIA_H100_80GB_HBM3"]) -> float:
    """A fixed, made-up cost surface: the default is a little off above 64 tokens. Not a
    measurement."""
    if peak < PEAK_BANDWIDTH["NVIDIA_H100_80GB_HBM3"] and not stand_in_fits(config, tokens):
        raise RuntimeError("OutOfResources (stand-in): shared memory")
    config = config or stand_in_default(tokens)
    # About 70% of an H100's memory bandwidth at the default's best, like a real kernel.
    base = memory_floor_us(shape, tokens, expected_distinct(shape.experts, tokens * shape.topk), peak) / 0.7
    rows_per_expert = max(1.0, tokens * shape.topk / shape.experts)
    waste = config["BLOCK_SIZE_M"] / max(16.0, rows_per_expert)
    cost = base * (1 + 0.04 * math.log2(waste)) * (1 + 0.02 * abs(math.log2(config["BLOCK_SIZE_N"] / 64)))
    cost *= 1 + 0.01 * abs(config["num_stages"] - 3) + (0.03 if config["num_warps"] == 8 and tokens < 512 else 0)
    jitter = random.Random(hash((config_key(config), tokens, seed)) & 0xFFFF).uniform(-0.002, 0.002)
    return cost * (1 + jitter)


# Weight copies a timed graph rotates through, so no call finds the experts it reads still in
# the L2 cache from the call before (NVIDIA's CUTLASS measurement guide: rotate inputs through
# at least twice the L2 size). At one token a step only 8 experts (about 25 MB) are read, and
# an H100 holds 50 MB of L2; serving reads another layer's weights in between.
WEIGHT_COPIES = 4


def _inputs(shape: Shape, tokens: int, seed: int):
    import torch
    torch.manual_seed(seed)
    device = "cuda"
    fp8 = torch.float8_e4m3fn
    e, n, k = shape.experts, shape.intermediate, shape.hidden
    # Uniform values in [-1, 1] (NVIDIA's CUTLASS measurement guide: zeros and narrow values
    # draw less power and time faster than real data).
    def uniform(*size, dtype=torch.float16):
        return torch.rand(*size, device=device, dtype=dtype) * 2 - 1

    w1 = (uniform(e, 2 * n, k) / 8).to(fp8)
    w2 = (uniform(e, k, n) / 8).to(fp8)
    w1_scale = torch.rand(e, math.ceil(2 * n / shape.block_n), math.ceil(k / shape.block_k), device=device, dtype=torch.float32) * 1e-2 + 1e-3
    w2_scale = torch.rand(e, math.ceil(k / shape.block_n), math.ceil(n / shape.block_k), device=device, dtype=torch.float32) * 1e-2 + 1e-3
    x = uniform(tokens, k, dtype=torch.bfloat16)
    return x, w1, w2, w1_scale, w2_scale


class HarnessError(RuntimeError):
    """A fault of this harness. Never recorded as a finding about the engine."""


@contextlib.contextmanager
def setting(config: dict | None):
    """Runs the engine's fused-MoE kernel with `config`, or with the engine's own choice when
    None. vLLM 0.34.0's override_config sets a module global and puts the old value back only
    when its body returns, so a setting that raised (Triton OutOfResources) stayed in force,
    and every "engine's own choice" timed after it ran that setting instead: rung 2's second
    L40S run (26 Nov) recorded the engine's default as failing at every step size for that
    reason. This puts it back in every case and refuses to run the engine's own choice, or
    to start a setting, while any override is in force."""
    import vllm.model_executor.layers.fused_moe as fused_moe

    if fused_moe.get_config() is not None:
        raise HarnessError(f"an MoE override is in force before a timing: {fused_moe.get_config()}")
    if config is None:
        yield
        return
    try:
        with fused_moe.override_config(config):
            yield
    finally:
        fused_moe._config = None  # override_config has no try/finally


def _quant_config(shape: Shape, w1_scale, w2_scale):
    import torch
    from vllm.model_executor.layers.fused_moe.config import FusedMoEQuantConfig
    return FusedMoEQuantConfig.make(quant_dtype=torch.float8_e4m3fn, block_shape=[shape.block_n, shape.block_k], w1_scale=w1_scale, w2_scale=w2_scale)


_cache: dict = {}


def gpu_time(shape: Shape, config: dict | None, tokens: int, repeats: int, graph_calls: int, seed: int) -> float:
    """Median microseconds of one fused_experts call, captured in a CUDA graph of
    graph_calls calls, with fresh routing copied in before every replay."""
    import torch
    from vllm.model_executor.layers.fused_moe import fused_topk
    from vllm.model_executor.layers.fused_moe.fused_moe import fused_experts

    key = (tokens, seed)
    if key not in _cache:
        _cache.clear()
        copies = []
        for copy in range(WEIGHT_COPIES):
            x, w1, w2, w1_scale, w2_scale = _inputs(shape, tokens, seed * WEIGHT_COPIES + copy)
            copies.append((x, w1, w2, _quant_config(shape, w1_scale, w2_scale)))
        gating = torch.randn(repeats, tokens, shape.experts, device="cuda", dtype=torch.float32)
        _cache[key] = (copies, gating, torch.empty(tokens, shape.experts, device="cuda", dtype=torch.float32))
    copies, gating, current = _cache[key]

    def run(copy: int):
        x, w1, w2, quant = copies[copy % len(copies)]
        # A fresh context per call: a generator-based context manager enters once.
        with setting(config):
            weights, ids, _ = fused_topk(x, current, shape.topk, renormalize=True)
            return fused_experts(x, w1, w2, weights, ids, quant_config=quant)

    current.copy_(gating[0])
    run(0)
    torch.cuda.synchronize()
    graph = torch.cuda.CUDAGraph()
    stream = torch.cuda.Stream()
    with torch.cuda.stream(stream):
        with torch.cuda.graph(graph, stream=stream):
            for call in range(graph_calls):
                run(call)
    torch.cuda.synchronize()
    for _ in range(5):  # warm-up replays, not timed
        graph.replay()
    torch.cuda.synchronize()
    times = []
    begin, finish = torch.cuda.Event(enable_timing=True), torch.cuda.Event(enable_timing=True)
    for index in range(repeats):
        current.copy_(gating[index])
        torch.cuda.synchronize()
        begin.record()
        graph.replay()
        finish.record()
        finish.synchronize()
        times.append(begin.elapsed_time(finish) * 1000 / graph_calls)
    graph.reset()
    times.sort()
    return times[len(times) // 2]


def gpu_check(shape: Shape, config: dict, tokens: int, bound: float = math.inf) -> dict:
    """The setting's output against an FP32 reference on fresh inputs, beside the engine's
    own choice's error on the same inputs."""
    import torch
    from vllm.model_executor.layers.fused_moe import fused_topk
    from vllm.model_executor.layers.fused_moe.fused_moe import fused_experts

    seed = 7919 + tokens
    x, w1, w2, w1_scale, w2_scale = _inputs(shape, tokens, seed)
    quant = _quant_config(shape, w1_scale, w2_scale)
    gating = torch.randn(tokens, shape.experts, device="cuda", dtype=torch.float32)
    weights, ids, _ = fused_topk(x, gating, shape.topk, renormalize=True)
    try:
        with setting(None):
            engine = fused_experts(x, w1, w2, weights, ids, quant_config=quant).float()
    except HarnessError:
        raise
    except Exception:  # noqa: BLE001 - the engine's own setting does not run here; judge by the bound
        engine = None
    with setting(config):
        tuned = fused_experts(x, w1, w2, weights, ids, quant_config=quant).float()
    rows = min(tokens, 8)

    def dequant(weight, scale, out_block, in_block):
        expanded = scale.repeat_interleave(out_block, 0)[: weight.shape[0]].repeat_interleave(in_block, 1)[:, : weight.shape[1]]
        return weight.float() * expanded

    reference = torch.zeros(rows, shape.hidden, device="cuda")
    for row in range(rows):
        for slot in range(shape.topk):
            expert = int(ids[row, slot])
            gate_up = x[row].float() @ dequant(w1[expert], w1_scale[expert], shape.block_n, shape.block_k).T
            act = torch.nn.functional.silu(gate_up[: shape.intermediate]) * gate_up[shape.intermediate:]
            reference[row] += float(weights[row, slot]) * (act @ dequant(w2[expert], w2_scale[expert], shape.block_n, shape.block_k).T)
    scale = reference.abs().max().item() or 1.0
    engine_error = None if engine is None else (engine[:rows] - reference).abs().max().item() / scale
    tuned_error = (tuned[:rows] - reference).abs().max().item() / scale
    finite = bool(torch.isfinite(tuned).all().item())
    allowed = bound if engine_error is None else ERROR_FACTOR * engine_error + 1e-3
    return {"finite": finite, "error": round(tuned_error, 6), "engine_error": None if engine_error is None else round(engine_error, 6),
            "passed": finite and tuned_error <= allowed}


# ----------------------------------------------------------------------------- search

def tune(shape: Shape, device: str, runtime: str, version: str, loads: list[dict], bench: Bench,
         budget_seconds: float, per_key_seconds: float, log=print) -> dict:
    plan = plan_keys(runtime, version, shape, loads)
    peak = PEAK_BANDWIDTH.get(device)
    started = time.monotonic()
    entries: dict[int, dict] = {}
    report = {"plan": plan, "keys": {}, "hidden": {}, "stand_in": bench.stand_in, "device": device, "shape": shape.__dict__,
              "peak_bandwidth": peak, "budget_seconds": budget_seconds}
    previous_best: dict | None = None
    known_errors: list[float] = []  # the engine's own reference errors at keys where it ran
    for tokens in plan["default"]:
        entries[tokens] = bench.engine_choice(tokens)
    for tokens in plan["tune"]:
        engine_config = bench.engine_choice(tokens)
        row = {"engine": engine_config, "kept": "engine"}
        if time.monotonic() - started > budget_seconds:
            row["reason"] = "global time budget spent"
            entries[tokens] = engine_config
            report["keys"][str(tokens)] = row
            continue
        key_started = time.monotonic()

        def safe_time(config: dict | None, seed: int = 0) -> tuple[float, str | None]:
            try:
                return bench.time(config, tokens, seed), None
            except HarnessError:
                raise
            except Exception as failure:  # noqa: BLE001 - a setting that cannot run here is a finding, not a crash
                return math.inf, f"{type(failure).__name__}: {str(failure)[:160]}"

        first, first_error = safe_time(None)
        timed: dict[tuple, float] = {}

        def measure(config: dict) -> float:
            name = config_key(config)
            if name not in timed:
                try:
                    timed[name] = bench.time(config, tokens)
                except HarnessError:
                    raise
                except Exception as failure:  # noqa: BLE001 - a setting that does not compile or run is skipped
                    timed[name] = math.inf
                    error = f"{type(failure).__name__}: {str(failure)[:120]}"
                    row.setdefault("failed", []).append({"config": config, "error": error, "does_not_fit": "OutOfResources" in error})
            return timed[name]

        seeds = [{k: v for k, v in engine_config.items() if k != "SPLIT_K"}] + ([previous_best] if previous_best else [])
        best = min(seeds, key=measure)
        for _ in range(2):
            for name in ORDER:
                if time.monotonic() - key_started > per_key_seconds:
                    break
                for candidate in neighbours(best, shape, name):
                    if measure(candidate) < measure(best):
                        best = candidate
        second, second_error = safe_time(None, seed=1)
        best_us = measure(best)
        draws = tokens * shape.topk
        floor = memory_floor_us(shape, tokens, expected_distinct(shape.experts, draws), peak) if peak else None
        if first_error or second_error:
            # The engine's own setting does not run on this GPU at this step size. Its time and
            # spread are unknown, so no gain is computed; a searched setting that runs and
            # passes the reference check replaces it, because the default would fail here.
            row.update(engine_error=first_error or second_error, engine_us=None, best=best, best_us=round(best_us, 3) if best_us < math.inf else None,
                       gain=None, settings_timed=len(timed), seconds=round(time.monotonic() - key_started, 1), memory_floor_us=round(floor, 3) if floor else None)
            if best_us == math.inf:
                row["reason"] = "neither the engine's setting nor any searched setting runs on this GPU at this step size"
                entries[tokens] = engine_config
            elif floor and best_us < FLOOR_FACTOR * floor:
                row["reason"] = "a time below the memory floor: timing fault, key left at the engine's choice"
                entries[tokens] = engine_config
            else:
                check = bench.check(best, tokens, error_bound(known_errors))
                row["check"] = check
                if check["passed"]:
                    entries[tokens], row["kept"] = best, "replaces-failing-default"
                    previous_best = best
                else:
                    row["reason"] = "failed the reference check"
                    entries[tokens] = engine_config
            report["keys"][str(tokens)] = row
            log(f"tokens {tokens}: the engine's own setting does not run ({row['engine_error'][:80]}); kept {row['kept']}")
            continue
        engine_us = (first + second) / 2
        spread = abs(first - second) / engine_us
        row.update(engine_us=round(engine_us, 3), engine_spread=round(spread, 4), best=best, best_us=round(best_us, 3),
                   gain=round(engine_us / best_us - 1, 4) if best_us < math.inf else None, settings_timed=len(timed),
                   seconds=round(time.monotonic() - key_started, 1),
                   memory_floor_us=round(floor, 3) if floor else None,
                   engine_floor_share=round(floor / engine_us, 3) if floor else None, best_floor_share=round(floor / best_us, 3) if floor and best_us < math.inf else None)
        if floor and min(engine_us, best_us) < FLOOR_FACTOR * floor:
            row["reason"] = "a time below the memory floor: timing fault, key left at the engine's choice"
            entries[tokens] = engine_config
        elif not worth_keeping(engine_us, spread, best_us):
            row["reason"] = "not faster than the engine's choice beyond the noise"
            entries[tokens] = engine_config
        else:
            check = bench.check(best, tokens, error_bound(known_errors))
            row["check"] = check
            if check.get("engine_error") is not None:
                known_errors.append(check["engine_error"])
            if check["passed"]:
                entries[tokens], row["kept"] = best, "tuned"
                previous_best = best
            else:
                row["reason"] = "failed the reference check"
                entries[tokens] = engine_config
        report["keys"][str(tokens)] = row
        log(f"tokens {tokens}: engine {engine_us:.1f} us, best {best_us:.1f} us, kept {row['kept']}")
    # The file as the engine reads it, at sizes the search never timed.
    keys = sorted(entries)
    for tokens in hidden_sizes(keys):
        if time.monotonic() - started > budget_seconds * 1.15:
            report["hidden"][str(tokens)] = {"reason": "time budget spent"}
            continue
        used = entries[nearest_key(keys, tokens)]
        try:
            engine_us = bench.time(None, tokens, seed=2)
        except HarnessError:
            raise
        except Exception as failure:  # noqa: BLE001
            engine_us, engine_error = math.inf, f"{type(failure).__name__}: {str(failure)[:160]}"
        else:
            engine_error = None
        try:
            file_us = bench.time(used, tokens, seed=2)
        except HarnessError:
            raise
        except Exception as failure:  # noqa: BLE001
            file_us = math.inf
            report["hidden"][str(tokens)] = {"file_error": f"{type(failure).__name__}: {str(failure)[:160]}", "engine_error": engine_error}
            continue
        slower = engine_us < math.inf and file_us > engine_us * (1 + MIN_GAIN)
        report["hidden"][str(tokens)] = {"engine_us": round(engine_us, 3) if engine_us < math.inf else None, "engine_error": engine_error,
                                         "file_us": round(file_us, 3), "slower": slower}
        if slower and tokens <= 32768 and len(entries) < 64:
            entries[tokens] = bench.engine_choice(tokens)
            report["hidden"][str(tokens)]["added_engine_default"] = True
    report["kept_tuned"] = sorted(int(k) for k, row in report["keys"].items() if row.get("kept") == "tuned")
    report["engine_fails_at"] = sorted(int(k) for k, row in report["keys"].items() if row.get("engine_error"))
    report["replaces_failing_default"] = sorted(int(k) for k, row in report["keys"].items() if row.get("kept") == "replaces-failing-default")
    report["seconds"] = round(time.monotonic() - started, 1)
    return {"entries": entries, "report": report}


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--model-config", help="the model's config.json")
    parser.add_argument("--shape", help="E,N,hidden,topk of a block-FP8 (128x128) MoE, instead of a model config")
    parser.add_argument("--file-name", help="the file the engine looked for and did not find; its shape is tuned (default: the model config's)")
    parser.add_argument("--loads", required=True, help='JSON list of {"running", "draft_tokens", "batched_tokens"}')
    parser.add_argument("--runtime", default="vllm")
    parser.add_argument("--version", default="0.34.0")
    parser.add_argument("--device", help="engine device name; read from the GPU when omitted")
    parser.add_argument("--budget-seconds", type=float, default=600)
    parser.add_argument("--per-key-seconds", type=float, default=45)
    parser.add_argument("--stand-in", action="store_true")
    parser.add_argument("--out", required=True, help="folder for kernel-configs.json, tuning-report.json and the files")
    args = parser.parse_args()
    if args.shape:
        experts, intermediate, hidden, topk = (int(part) for part in args.shape.split(","))
        shape = Shape(experts, intermediate, "fp8_w8a8", 128, 128, hidden, topk)
    else:
        with open(args.model_config) as handle:
            shape = shape_from_config(json.load(handle))
    device = args.device
    if not device:
        import torch
        device = engine_device_name(torch.cuda.get_device_name(0))
    if args.file_name:
        shape, named = shape_from_file_name(args.file_name, shape.hidden, shape.topk)
        if named != device:
            raise SystemExit(f"{args.file_name} names {named}, this GPU is {device}")
    bench = Bench(shape, args.stand_in, PEAK_BANDWIDTH.get(device, PEAK_BANDWIDTH["NVIDIA_H100_80GB_HBM3"]))
    result = tune(shape, device, args.runtime, args.version, json.loads(args.loads), bench, args.budget_seconds, args.per_key_seconds)
    write(result, shape, device, args.runtime, args.version, args.out)


def write(result: dict, shape: Shape, device: str, runtime: str, version: str, out: str) -> dict:
    kernel_set = build_set(runtime, version, device, {file_name(shape, device): result["entries"]})
    os.makedirs(os.path.join(out, "files"), exist_ok=True)
    for entry in kernel_set["files"]:
        with open(os.path.join(out, "files", entry["name"]), "w") as handle:
            handle.write(entry["content"])
    with open(os.path.join(out, "kernel-configs.json"), "w") as handle:
        json.dump(kernel_set, handle, indent=1)
    with open(os.path.join(out, "tuning-report.json"), "w") as handle:
        json.dump(result["report"], handle, indent=1)
    return kernel_set


if __name__ == "__main__":
    sys.exit(main())

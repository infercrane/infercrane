#!/usr/bin/env python3
"""Screen an InferCrane-authored Qwen3.8 MTP state-commit kernel on H200."""

from __future__ import annotations

import hashlib
import importlib.metadata
import importlib.util
import json
import math
import pathlib
import statistics
import time
from dataclasses import dataclass

import torch
import triton
import triton.language as tl


EXPECTED_SOURCE_SHA256 = "e814dc1e68071c7c089734cc99eac0d1c00f8d00041d352f862e9dd2bd5775c5"
SOURCE_RELATIVE = pathlib.Path("kernels/ops/mamba/mamba_state_scatter_triton.py")
HOTSPOT_FRACTION = 0.05480692269096207
MIN_ENDPOINT_UPSIDE = 0.03


@dataclass(frozen=True)
class Variant:
    name: str
    block: int
    chunks: int
    warps: int


VARIANTS = (
    Variant("ic-b512-c4-w4", 512, 4, 4),
    Variant("ic-b512-c8-w4", 512, 8, 4),
    Variant("ic-b1024-c2-w4", 1024, 2, 4),
    Variant("ic-b1024-c4-w4", 1024, 4, 4),
    Variant("ic-b1024-c8-w4", 1024, 8, 4),
    Variant("ic-b1024-c16-w4", 1024, 16, 4),
    Variant("ic-b2048-c2-w8", 2048, 2, 8),
    Variant("ic-b2048-c4-w8", 2048, 4, 8),
)


def digest(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def upstream_path() -> pathlib.Path:
    spec = importlib.util.find_spec("sglang")
    if spec is None or not spec.submodule_search_locations:
        raise RuntimeError("installed SGLang package is unavailable")
    return pathlib.Path(next(iter(spec.submodule_search_locations))) / SOURCE_RELATIVE


@triton.jit
def _infercrane_state_commit_kernel(
    src,
    dst,
    destinations,
    accepted_steps,
    elements: tl.constexpr,
    src_layer_stride,
    src_request_stride,
    src_step_stride,
    dst_layer_stride,
    dst_request_stride,
    source_requests,
    source_steps,
    destination_requests,
    BLOCK: tl.constexpr,
    CHUNKS: tl.constexpr,
):
    """Copy several adjacent state chunks after decoding request metadata once."""
    request = tl.program_id(0)
    layer = tl.program_id(1).to(tl.int64)
    group = tl.program_id(2).to(tl.int64)
    step = tl.load(accepted_steps + request).to(tl.int64)
    destination = tl.load(destinations + request).to(tl.int64)
    if not (
        (step >= 0)
        & (step < source_steps)
        & (request < source_requests)
        & (destination >= 0)
        & (destination < destination_requests)
    ):
        return
    src_base = (
        layer * src_layer_stride
        + request.to(tl.int64) * src_request_stride
        + step * src_step_stride
    )
    dst_base = layer * dst_layer_stride + destination * dst_request_stride
    start = group * BLOCK * CHUNKS
    for chunk in tl.static_range(CHUNKS):
        offsets = start + chunk * BLOCK + tl.arange(0, BLOCK)
        mask = offsets < elements
        values = tl.load(src + src_base + offsets, mask=mask, other=0.0)
        tl.store(dst + dst_base + offsets, values, mask=mask)


def candidate(dst, src, destinations, accepted_steps, variant: Variant) -> None:
    if not (dst.is_cuda and src.is_cuda and dst.device == src.device):
        raise ValueError("state tensors must share a CUDA device")
    if not dst.is_contiguous() or not src.is_contiguous():
        raise ValueError("state tensors must be contiguous")
    if dst.shape[0] != src.shape[0] or dst.shape[2:] != src.shape[3:]:
        raise ValueError("state geometry differs")
    if destinations.shape != accepted_steps.shape or destinations.ndim != 1:
        raise ValueError("indices must be equal-length vectors")
    requests = accepted_steps.shape[0]
    if requests == 0:
        return
    elements = dst.numel() // (dst.shape[0] * dst.shape[1])
    destinations = destinations.to(torch.int32).contiguous()
    accepted_steps = accepted_steps.to(torch.int32).contiguous()
    grid = (
        requests,
        dst.shape[0],
        triton.cdiv(elements, variant.block * variant.chunks),
    )
    _infercrane_state_commit_kernel[grid](
        src,
        dst,
        destinations,
        accepted_steps,
        elements,
        src.stride(0),
        src.stride(1),
        src.stride(2),
        dst.stride(0),
        dst.stride(1),
        src.shape[1],
        src.shape[2],
        dst.shape[1],
        BLOCK=variant.block,
        CHUNKS=variant.chunks,
        num_warps=variant.warps,
        num_stages=1,
    )


def inputs(layers, requests, steps, entry, *, cache=None, adversarial=False):
    torch.manual_seed(26_032_026 + layers + requests + math.prod(entry))
    cache = cache or max(2 * requests, 4)
    src = torch.randn(
        layers, requests, steps, *entry, device="cuda", dtype=torch.float32
    )
    dst = torch.randn(layers, cache, *entry, device="cuda", dtype=torch.float32)
    destinations = torch.arange(requests, device="cuda", dtype=torch.int32) % cache
    accepted_steps = torch.arange(requests, device="cuda", dtype=torch.int32) % steps
    if adversarial and requests >= 4:
        accepted_steps[0], accepted_steps[1] = -1, steps
        destinations[2], destinations[3] = -1, cache
    return src, dst, destinations, accepted_steps


def parity(upstream, variant, values):
    src, initial, destinations, accepted_steps = values
    reference = initial.clone()
    actual = initial.clone()
    upstream(reference, src, destinations, accepted_steps)
    candidate(actual, src, destinations, accepted_steps, variant)
    torch.cuda.synchronize()
    return {
        "passed": bool(torch.equal(reference, actual)),
        "max_abs": float((reference - actual).abs().max()),
    }


def deterministic(variant, values):
    src, initial, destinations, accepted_steps = values
    first, second = initial.clone(), initial.clone()
    candidate(first, src, destinations, accepted_steps, variant)
    candidate(second, src, destinations, accepted_steps, variant)
    torch.cuda.synchronize()
    return {"passed": bool(torch.equal(first, second))}


def elapsed_us(function, iterations=12):
    for _ in range(4):
        function()
    torch.cuda.synchronize()
    samples = []
    for _ in range(7):
        start = torch.cuda.Event(enable_timing=True)
        end = torch.cuda.Event(enable_timing=True)
        start.record()
        for _ in range(iterations):
            function()
        end.record()
        end.synchronize()
        samples.append(start.elapsed_time(end) * 1000 / iterations)
    return statistics.median(samples)


def main() -> None:
    if not torch.cuda.is_available():
        raise SystemExit("CUDA is required")
    capability = torch.cuda.get_device_capability()
    if capability[0] != 9:
        raise SystemExit(f"Hopper is required; got SM{capability[0]}{capability[1]}")
    path = upstream_path()
    upstream_digest = digest(path.read_bytes())
    if upstream_digest != EXPECTED_SOURCE_SHA256:
        raise SystemExit(f"refusing unexpected SGLang source: {upstream_digest}")
    from sglang.kernels.ops.mamba.mamba_state_scatter_triton import (
        fused_mamba_state_scatter_with_mask as upstream,
    )

    checks = {
        "smoke": inputs(2, 2, 4, (3, 17)),
        "shape_sweep": inputs(3, 5, 4, (7, 257)),
        "numerical_stability": inputs(2, 4, 4, (5, 129)),
        "adversarial_and_edge": inputs(2, 8, 4, (5, 129), adversarial=True),
        "single_element": inputs(1, 1, 1, (1,)),
    }
    rows = []
    for variant in VARIANTS:
        correctness = {name: parity(upstream, variant, value) for name, value in checks.items()}
        correctness["determinism"] = deterministic(variant, checks["shape_sweep"])
        rows.append(
            {
                "name": variant.name,
                "block": variant.block,
                "chunks": variant.chunks,
                "warps": variant.warps,
                "correctness": correctness,
                "all_correctness_passed": all(v["passed"] for v in correctness.values()),
                "measurements_us": {},
            }
        )
    qualified = [row for row in rows if row["all_correctness_passed"]]
    if not qualified:
        raise SystemExit("all authored candidates failed correctness")

    baseline = {}
    # Qwen3.8: 48 linear-attention layers and [48, 128, 128] fp32 state.
    for concurrency in (4, 8, 12, 16):
        case = f"c{concurrency}-t4"
        values = inputs(48, concurrency, 4, (48, 128, 128), cache=32)
        baseline[case] = elapsed_us(
            lambda current=values: upstream(
                current[1], current[0], current[2], current[3]
            )
        )
        for row in qualified:
            variant = next(item for item in VARIANTS if item.name == row["name"])
            row["measurements_us"][case] = elapsed_us(
                lambda selected=variant, current=values: candidate(
                    current[1], current[0], current[2], current[3], selected
                )
            )
        del values
        torch.cuda.empty_cache()

    for row in rows:
        if not row["all_correctness_passed"]:
            row["weighted_speedup_vs_upstream"] = 0.0
            row["projected_endpoint_upside_fraction"] = 0.0
            continue
        log_ratio = sum(
            weight * math.log(row["measurements_us"][case] / baseline[case])
            for case, weight in (("c12-t4", .35), ("c16-t4", .65))
        )
        speedup = math.exp(-log_ratio)
        row["weighted_speedup_vs_upstream"] = speedup
        row["projected_endpoint_upside_fraction"] = (
            HOTSPOT_FRACTION * (1 - 1 / speedup) if speedup > 1 else 0.0
        )
    selected = max(qualified, key=lambda row: row["weighted_speedup_vs_upstream"])
    receipt = {
        "schema_version": "infercrane.dev/authored-state-commit-kernel-screen/v1",
        "created_unix": int(time.time()),
        "boundary": "Authored Triton microbenchmark; endpoint promotion requires full workload qualification.",
        "method": "profile -> five-stage correctness -> benchmark -> Amdahl gate",
        "gpu": {"name": torch.cuda.get_device_name(), "compute_capability": f"{capability[0]}.{capability[1]}"},
        "software": {
            "torch": torch.__version__,
            "triton": importlib.metadata.version("triton"),
            "sglang_source_sha256": upstream_digest,
            "authored_source_sha256": digest(pathlib.Path(__file__).read_bytes()),
        },
        "geometry": {
            "linear_attention_layers": 48,
            "state_shape": [48, 128, 128],
            "state_dtype": "float32",
            "verification_steps": 4,
            "concurrency": [4, 8, 12, 16],
        },
        "hotspot_fraction": HOTSPOT_FRACTION,
        "minimum_endpoint_upside_fraction": MIN_ENDPOINT_UPSIDE,
        "upstream_measurements_us": baseline,
        "variants": rows,
        "selected": selected["name"],
        "selected_speedup": selected["weighted_speedup_vs_upstream"],
        "projected_endpoint_upside_fraction": selected["projected_endpoint_upside_fraction"],
        "endpoint_candidate_eligible": selected["projected_endpoint_upside_fraction"] >= MIN_ENDPOINT_UPSIDE,
    }
    receipt["receipt_sha256"] = digest(
        json.dumps(receipt, sort_keys=True, separators=(",", ":")).encode()
    )
    print(json.dumps(receipt, indent=2, sort_keys=True))


if __name__ == "__main__":
    main()

#!/usr/bin/env python3
"""Bounded H200 search for Qwen3.8's recurrent GDN update kernel.

This is a screening harness, not a promotion mechanism. It benchmarks exact
Qwen3.8 head geometry under packed c1-c16 decode and four-token verification,
checks output/state parity, and emits a content-addressed receipt. A winning
microbenchmark must still pass the full workload and quality gates.
"""

from __future__ import annotations

import hashlib
import importlib.metadata
import importlib.util
import json
import math
import pathlib
import statistics
import tempfile
import time
from dataclasses import dataclass

import torch


EXPECTED_SOURCE_SHA256 = "0cf7b4dbed432cec721c60b69629a4cb53d1600b865699f461da82618098b505"
SOURCE_RELATIVE = pathlib.Path(
    "kernels/ops/attention/fla/fused_sigmoid_gating_recurrent.py"
)


@dataclass(frozen=True)
class Variant:
    name: str
    bv_cap: int
    num_warps: int
    num_stages: int = 3


VARIANTS = (
    Variant("upstream-bv32-w1", 32, 1),
    Variant("bv16-w1", 16, 1),
    Variant("bv64-w1", 64, 1),
    Variant("bv128-w1", 128, 1),
    Variant("bv32-w2", 32, 2),
    Variant("bv32-w4", 32, 4),
    Variant("bv64-w2", 64, 2),
    Variant("bv64-w4", 64, 4),
)


def sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def source_path() -> pathlib.Path:
    spec = importlib.util.find_spec("sglang")
    if spec is None or not spec.submodule_search_locations:
        raise RuntimeError("installed SGLang package is unavailable")
    return pathlib.Path(next(iter(spec.submodule_search_locations))) / SOURCE_RELATIVE


def variant_source(base: str, variant: Variant) -> str:
    replacements = {
        "BK, BV = triton.next_power_of_2(K), min(triton.next_power_of_2(V), 32)": (
            "BK, BV = triton.next_power_of_2(K), "
            f"min(triton.next_power_of_2(V), {variant.bv_cap})"
        ),
        "num_stages = 3": f"num_stages = {variant.num_stages}",
        "num_warps = 1": f"num_warps = {variant.num_warps}",
    }
    updated = base
    for old, new in replacements.items():
        if updated.count(old) != 1:
            raise RuntimeError(f"expected source expression exactly once: {old}")
        updated = updated.replace(old, new)
    return updated


def load_variant(base: str, variant: Variant):
    directory = pathlib.Path(tempfile.mkdtemp(prefix="infercrane-gdn-"))
    path = directory / f"{variant.name.replace('-', '_')}.py"
    path.write_text(variant_source(base, variant))
    module_name = f"infercrane_gdn_{variant.name.replace('-', '_')}"
    spec = importlib.util.spec_from_file_location(module_name, path)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"cannot load {variant.name}")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module.fused_sigmoid_gating_delta_rule_update, sha256(path.read_bytes())


def tensors(concurrency: int, steps: int) -> dict[str, torch.Tensor]:
    torch.manual_seed(23_092_026 + concurrency * 100 + steps)
    device = torch.device("cuda")
    key_heads, value_heads, dimension = 16, 48, 128
    tokens = concurrency * steps
    return {
        "A_log": -torch.rand(value_heads, dtype=torch.float32, device=device) - 1.0,
        "a": torch.randn(1, tokens, value_heads, dtype=torch.bfloat16, device=device) * 0.1,
        "dt_bias": torch.randn(value_heads, dtype=torch.float32, device=device) * 0.1,
        "q": torch.randn(1, tokens, key_heads, dimension, dtype=torch.bfloat16, device=device),
        "k": torch.randn(1, tokens, key_heads, dimension, dtype=torch.bfloat16, device=device),
        "v": torch.randn(1, tokens, value_heads, dimension, dtype=torch.bfloat16, device=device),
        "b": torch.randn(1, tokens, value_heads, dtype=torch.bfloat16, device=device) * 0.1,
        "state": torch.randn(
            concurrency,
            value_heads,
            dimension,
            dimension,
            dtype=torch.float32,
            device=device,
        )
        * 0.01,
        "indices": torch.arange(concurrency, dtype=torch.int32, device=device),
        "cu_seqlens": torch.arange(
            0,
            tokens + 1,
            steps,
            dtype=torch.int32,
            device=device,
        ),
    }


def invoke(function, values: dict[str, torch.Tensor], state: torch.Tensor):
    return function(
        A_log=values["A_log"],
        a=values["a"],
        dt_bias=values["dt_bias"],
        softplus_beta=1.0,
        softplus_threshold=20.0,
        q=values["q"],
        k=values["k"],
        v=values["v"],
        b=values["b"],
        initial_state_source=state,
        initial_state_indices=values["indices"],
        use_qk_l2norm_in_kernel=True,
        cu_seqlens=values["cu_seqlens"],
    )


def elapsed_us(function, values: dict[str, torch.Tensor]) -> float:
    state = values["state"].clone()
    for _ in range(12):
        invoke(function, values, state)
    torch.cuda.synchronize()
    samples = []
    for _ in range(7):
        start = torch.cuda.Event(enable_timing=True)
        end = torch.cuda.Event(enable_timing=True)
        start.record()
        for _ in range(50):
            invoke(function, values, state)
        end.record()
        end.synchronize()
        samples.append(start.elapsed_time(end) * 1000 / 50)
    return statistics.median(samples)


def parity(reference, candidate, values: dict[str, torch.Tensor]) -> dict[str, float | bool]:
    reference_state = values["state"].clone()
    candidate_state = values["state"].clone()
    reference_output = invoke(reference, values, reference_state)
    candidate_output = invoke(candidate, values, candidate_state)
    torch.cuda.synchronize()
    output_delta = float((reference_output.float() - candidate_output.float()).abs().max())
    state_delta = float((reference_state - candidate_state).abs().max())
    return {
        "passed": bool(output_delta <= 1e-5 and state_delta <= 1e-5),
        "output_max_abs": output_delta,
        "state_max_abs": state_delta,
    }


def main() -> None:
    if not torch.cuda.is_available():
        raise SystemExit("CUDA is required")
    capability = torch.cuda.get_device_capability()
    if capability[0] != 9:
        raise SystemExit(f"this receipt is H200/Hopper-specific; got SM{capability[0]}{capability[1]}")
    path = source_path()
    raw = path.read_bytes()
    digest = sha256(raw)
    if digest != EXPECTED_SOURCE_SHA256:
        raise SystemExit(f"refusing unexpected SGLang GDN source: {digest}")
    base = raw.decode()
    loaded = {variant.name: load_variant(base, variant) for variant in VARIANTS}
    reference = loaded[VARIANTS[0].name][0]
    cases = [
        (concurrency, steps)
        for steps in (1, 4)
        for concurrency in ((1, 4, 8, 12, 16) if steps == 1 else (4, 8, 12, 16))
    ]
    inputs = {f"c{c}-t{t}": tensors(c, t) for c, t in cases}
    results = []
    for variant in VARIANTS:
        function, variant_digest = loaded[variant.name]
        measurements = {}
        parity_results = {}
        for concurrency, steps in cases:
            case = f"c{concurrency}-t{steps}"
            values = inputs[case]
            parity_results[case] = parity(reference, function, values)
            measurements[case] = elapsed_us(function, values)
        baseline_measurements = results[0]["measurements_us"] if results else measurements
        weighted_ratio = math.exp(
            sum(
                weight
                * math.log(measurements[case] / baseline_measurements[case])
                for case, weight in (("c16-t1", 0.30), ("c16-t4", 0.70))
            )
        )
        results.append(
            {
                "name": variant.name,
                "bv_cap": variant.bv_cap,
                "num_warps": variant.num_warps,
                "num_stages": variant.num_stages,
                "source_sha256": variant_digest,
                "measurements_us": measurements,
                "parity": parity_results,
                "all_parity_passed": all(row["passed"] for row in parity_results.values()),
                "weighted_speedup_vs_upstream": 1 / weighted_ratio,
            }
        )
    qualified = [row for row in results if row["all_parity_passed"]]
    selected = max(qualified, key=lambda row: row["weighted_speedup_vs_upstream"])
    receipt = {
        "schema_version": "infercrane.dev/gdn-kernel-screen/v1",
        "created_unix": int(time.time()),
        "boundary": (
            "Exact Qwen3.8 geometry microbenchmark on H200. Selection does not "
            "authorize endpoint promotion without workload parity and cost qualification."
        ),
        "gpu": {
            "name": torch.cuda.get_device_name(),
            "compute_capability": f"{capability[0]}.{capability[1]}",
        },
        "software": {
            "torch": torch.__version__,
            "triton": importlib.metadata.version("triton"),
            "sglang_source_sha256": digest,
        },
        "geometry": {
            "key_heads": 16,
            "value_heads": 48,
            "key_dimension": 128,
            "value_dimension": 128,
            "packed_concurrency": [1, 4, 8, 12, 16],
            "verification_steps": [1, 4],
        },
        "variants": results,
        "selected_microbenchmark": selected["name"],
        "endpoint_candidate_eligible": selected["weighted_speedup_vs_upstream"] >= 1.03,
    }
    receipt["receipt_sha256"] = hashlib.sha256(
        json.dumps(receipt, sort_keys=True, separators=(",", ":")).encode()
    ).hexdigest()
    print(json.dumps(receipt, indent=2, sort_keys=True))


if __name__ == "__main__":
    main()

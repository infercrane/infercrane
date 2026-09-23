"""Pure helpers for the Qwen3.8 workload-optimization campaign.

This module intentionally has no Modal dependency so its routing assumptions,
measurement reductions, and evidence conversion can be tested locally before
renting a GPU.
"""

from __future__ import annotations

import hashlib
import json
import math
import re
from typing import Any, Iterable


MODEL_SLUG = "qwen/qwen3.8-27b"
MODEL_ID = "Qwen/Qwen3.8-27B-FP8"
MODEL_REVISION = "017b9c7af6b5689d5dd426a76e0bc077eb5ca20a"
PUBLIC_TRACE = {
    "name": "A Year in LLM Serving: Workload Evolution, Caching and Load-Balancing",
    "url": "https://data.agentic-system.org/#chutes",
    "artifact": "https://harvardsys-datasets.s3.us-east-1.amazonaws.com/2026_chutes_anonymized/chutes_trace.parquet",
    "license": "CC-BY-4.0",
    "citation": "Nixon et al., arXiv:2608.13573 (2026)",
}

_SAMPLING = {
    "temperature": 0.7,
    "top_p": 0.8,
    "top_k": 20,
    "min_p": 0.0,
    "presence_penalty": 1.5,
    "seed": 20260923,
    "enable_thinking": False,
}

WORKLOADS = {
    "public-interactive": {
        "id": "agentic-system-chutes-public-interactive-v1",
        "source": PUBLIC_TRACE,
        "input_tokens": 3919,
        "output_tokens": 295,
        "concurrency_lanes": [1, 4, 8],
        "streaming": True,
        "slo": {
            "id": "infercrane-interactive-aggressive-v1",
            "source": "pre_registered_internal_launch_objective",
            "max_ttft_ms": 800.0,
            "max_itl_ms": 12.0,
        },
        "sampling": _SAMPLING,
        "boundary": (
            "Median token shape from InferCrane's deterministic sample of the released trace. "
            "It is day-zero screening input, not a customer replay."
        ),
    },
    "public-long-prefill": {
        "id": "agentic-system-chutes-public-long-prefill-v1",
        "source": PUBLIC_TRACE,
        "input_tokens": 24832,
        "output_tokens": 295,
        "concurrency_lanes": [1, 4],
        "streaming": True,
        "slo": {
            "id": "infercrane-long-prefill-aggressive-v1",
            "source": "pre_registered_internal_launch_objective",
            "max_ttft_ms": 3000.0,
            "max_itl_ms": 12.0,
        },
        "sampling": _SAMPLING,
        "boundary": "P95 input and median output token shape from the released trace sample.",
    },
    "public-decode-heavy": {
        "id": "agentic-system-chutes-public-decode-heavy-v1",
        "source": PUBLIC_TRACE,
        "input_tokens": 3919,
        "output_tokens": 1377,
        "concurrency_lanes": [1, 4],
        "streaming": True,
        "slo": {
            "id": "infercrane-decode-heavy-aggressive-v1",
            "source": "pre_registered_internal_launch_objective",
            "max_ttft_ms": 800.0,
            "max_itl_ms": 12.0,
        },
        "sampling": _SAMPLING,
        "boundary": "Median input and P95 output token shape from the released trace sample.",
    },
    "public-decode-saturation": {
        "id": "agentic-system-chutes-public-decode-saturation-v1",
        "source": PUBLIC_TRACE,
        "input_tokens": 3919,
        "output_tokens": 1377,
        "concurrency_lanes": [4, 8, 12, 16],
        "streaming": True,
        "slo": {
            "id": "infercrane-provider-capacity-v1",
            "source": "pre_registered_provider_capacity_objective",
            "max_ttft_ms": 5000.0,
            "max_itl_ms": 20.0,
        },
        "sampling": _SAMPLING,
        "boundary": (
            "Capacity diagnostic using the public trace's median input and P95 output shape. "
            "It finds the economic saturation frontier; it is not a claim about arrival mix."
        ),
    },
    "agent-prefix-reuse": {
        "id": "agentic-system-agent-prefix-reuse-derived-v1",
        "source": {
            "name": "FreeInference agent-serving aggregate findings and live telemetry",
            "url": "https://data.agentic-system.org/#freeinference-kv",
            "telemetry": "https://openinfra.freeinference.org/api/dashboard",
            "release_state": "aggregate_findings_published_trace_planned",
        },
        "input_tokens": 95856,
        "uncached_input_tokens": 6329,
        "output_tokens": 502,
        "concurrency_lanes": [1, 4],
        "streaming": True,
        "slo": {
            "id": "infercrane-agent-session-aggressive-v1",
            "source": "pre_registered_internal_launch_objective",
            "max_ttft_ms": 2000.0,
            "max_itl_ms": 12.0,
        },
        "sampling": _SAMPLING,
        "session": {
            "append_fraction": 0.9659,
            "mutation_fraction": 0.0341,
            "turns": 8,
        },
        "boundary": (
            "Synthetic session workload derived from published aggregate findings and public "
            "fleet telemetry. It is not a replay of the unreleased agent trace."
        ),
    },
}

# Backward-compatible default for the competitive target calculation. OpenRouter
# is an external validation channel; it does not define the optimization corpus.
WORKLOAD = WORKLOADS["public-interactive"]


def percentile(values: Iterable[float], quantile: float) -> float:
    ordered = sorted(float(value) for value in values)
    if not ordered:
        raise ValueError("percentile requires at least one value")
    if not 0 <= quantile <= 1:
        raise ValueError("quantile must be between zero and one")
    position = (len(ordered) - 1) * quantile
    lower = math.floor(position)
    upper = math.ceil(position)
    if lower == upper:
        return ordered[lower]
    return ordered[lower] + (ordered[upper] - ordered[lower]) * (position - lower)


def canonical_digest(value: Any) -> str:
    body = json.dumps(value, sort_keys=True, separators=(",", ":")).encode()
    return "sha256:" + hashlib.sha256(body).hexdigest()


def merge_prometheus_measurements(
    aggregate: dict[str, float], measurement: dict[str, float]
) -> dict[str, float]:
    """Merge Prometheus deltas without turning gauges into fake counters.

    Counters and histogram components are additive across disjoint benchmark
    lanes. Gauges describe the latest runtime state and must replace the prior
    observation. The full metric key, including labels, remains intact.
    """

    merged = dict(aggregate)
    for key, value in measurement.items():
        name = key.split("{", 1)[0]
        if name.endswith(("_total", "_sum", "_count", "_bucket")):
            merged[key] = merged.get(key, 0.0) + value
        else:
            merged[key] = value
    return merged


def _kernel_family(name: str) -> str:
    lowered = name.lower()
    families = (
        ("gated_deltanet", ("deltanet", "gdn", "linear_attn", "mamba")),
        ("normalization", ("rmsnorm", "layernorm", "norm_kernel")),
        ("attention", ("attention", "flash_attn", "flashinfer", "fmha")),
        ("gemm", ("gemm", "matmul", "cutlass", "cublas", "wgmma", "nvjet")),
        ("moe", ("moe", "expert", "fused_moe")),
        ("sampling", ("sampling", "topk", "topp", "softmax")),
        ("memory", ("memcpy", "memset", "copy_kernel")),
    )
    for family, markers in families:
        if any(marker in lowered for marker in markers):
            return family
    return "other"


def summarize_torch_trace(trace: dict[str, Any], *, top_k: int = 20) -> dict[str, Any]:
    """Reduce a Chrome/PyTorch trace into measured GPU hotspot evidence."""

    events = trace.get("traceEvents") if isinstance(trace, dict) else None
    if not isinstance(events, list):
        raise ValueError("torch trace must contain a traceEvents list")
    kernels: dict[str, float] = {}
    families: dict[str, float] = {}
    kernel_event_count = 0
    for event in events:
        if not isinstance(event, dict) or event.get("ph") != "X":
            continue
        category = str(event.get("cat") or "").lower()
        name = str(event.get("name") or "")
        duration = float(event.get("dur") or 0)
        args = event.get("args") or {}
        device_like = (
            "kernel" in category
            or category in {"gpu", "cuda_kernel"}
            or "stream" in args
            or "device" in args and "cuda" in str(args.get("device", "")).lower()
        )
        runtime_api = any(marker in category for marker in ("runtime", "driver"))
        if not device_like or runtime_api or duration <= 0 or not name:
            continue
        kernel_event_count += 1
        kernels[name] = kernels.get(name, 0.0) + duration
        family = _kernel_family(name)
        families[family] = families.get(family, 0.0) + duration
    total_us = sum(kernels.values())
    if total_us <= 0:
        raise ValueError("torch trace contains no measurable GPU kernel events")

    def rows(values: dict[str, float], limit: int | None = None) -> list[dict[str, Any]]:
        ordered = sorted(values.items(), key=lambda row: (-row[1], row[0]))
        if limit is not None:
            ordered = ordered[:limit]
        return [
            {
                "name": name,
                "device_time_us": duration,
                "device_time_fraction": duration / total_us,
            }
            for name, duration in ordered
        ]

    return {
        "schema_version": "infercrane.dev/gpu-hotspot-profile/v1",
        "total_gpu_kernel_time_us": total_us,
        "kernel_events": kernel_event_count,
        "top_kernels": rows(kernels, top_k),
        "families": rows(families),
    }


def custom_kernel_gate(
    profile: dict[str, Any],
    *,
    minimum_hotspot_fraction: float = 0.05,
    minimum_endpoint_upside: float = 0.03,
) -> dict[str, Any]:
    """Fail closed unless a measured hotspot can materially move the endpoint."""

    families = profile.get("families") or []
    if not families:
        raise ValueError("kernel gate requires a measured hotspot profile")
    hotspot = families[0]
    fraction = float(hotspot["device_time_fraction"])
    # Amdahl ceiling if the hotspot were made infinitely fast. Real expected
    # upside is established later by a candidate microbenchmark and endpoint run.
    ceiling = fraction
    eligible = fraction >= minimum_hotspot_fraction and ceiling >= minimum_endpoint_upside
    kernel_names = [
        str(row.get("name") or "").lower()
        for row in profile.get("top_kernels") or []
        if _kernel_family(str(row.get("name") or "")) == hotspot["name"]
    ]
    existing_markers = {
        "deep_gemm": "DeepGEMM",
        "cutlass": "CUTLASS/FlashInfer",
        "cublas": "cuBLASLt",
        "flashinfer": "FlashInfer",
        "nvjet": "NVJet",
    }
    existing_implementations = sorted(
        {
            implementation
            for name in kernel_names
            for marker, implementation in existing_markers.items()
            if marker in name
        }
    )
    if not eligible:
        decision = "reject_immaterial"
        custom_generation_state = "not_authorized"
    elif existing_implementations:
        decision = "benchmark_existing_implementations_first"
        custom_generation_state = "deferred_until_existing_candidates_are_measured"
    else:
        decision = "build_bounded_custom_candidate"
        custom_generation_state = "authorized_for_screening_only"
    return {
        "schema_version": "infercrane.dev/custom-kernel-gate/v1",
        "eligible": eligible,
        "hotspot_family": hotspot["name"],
        "measured_device_time_fraction": fraction,
        "maximum_endpoint_upside_fraction": ceiling,
        "minimum_hotspot_fraction": minimum_hotspot_fraction,
        "minimum_endpoint_upside_fraction": minimum_endpoint_upside,
        "decision": decision,
        "existing_optimized_implementations": existing_implementations,
        "custom_generation_state": custom_generation_state,
        "boundary": (
            "Eligibility authorizes a bounded kernel candidate only. Promotion still requires "
            "numerical parity, target-GPU microbenchmarks, end-to-end workload evidence, and cost gates."
        ),
    }


def parse_sglang_startup(log_text: str) -> dict[str, Any]:
    """Extract SGLang's own startup phase accounting from a pinned run log."""

    match = re.search(
        r"Engine startup timings \(s\): load_weight=(?P<load>[0-9.]+), "
        r"kv_cache_allocation=(?P<kv>[0-9.]+), scheduler_e2e=(?P<scheduler>[0-9.]+), "
        r"cuda_graph=\{prefill=(?P<prefill>[0-9.]+), decode=(?P<decode>[0-9.]+), "
        r"target_verify=(?P<verify>[0-9.]+), draft_prefill=(?P<draft_prefill>[0-9.]+), "
        r"draft_decode=(?P<draft_decode>[0-9.]+), draft_extend=(?P<draft_extend>[0-9.]+)\}, "
        r"tokenizer_e2e=(?P<tokenizer>[0-9.]+)",
        log_text,
    )
    if not match:
        return {"available": False}
    values = {name: float(value) for name, value in match.groupdict().items()}
    return {
        "available": True,
        "load_weight_seconds": values["load"],
        "kv_cache_allocation_seconds": values["kv"],
        "scheduler_e2e_seconds": values["scheduler"],
        "tokenizer_e2e_seconds": values["tokenizer"],
        "cuda_graph_seconds": {
            "prefill": values["prefill"],
            "decode": values["decode"],
            "target_verify": values["verify"],
            "draft_prefill": values["draft_prefill"],
            "draft_decode": values["draft_decode"],
            "draft_extend": values["draft_extend"],
        },
    }


def encoded_token_count(encoded: Any) -> int:
    """Count token IDs from tokenizer list, tensor, or multimodal mapping output."""
    if isinstance(encoded, dict) or hasattr(encoded, "keys"):
        encoded = encoded["input_ids"]
    if hasattr(encoded, "tolist"):
        encoded = encoded.tolist()
    if encoded and isinstance(encoded[0], (list, tuple)):
        if len(encoded) != 1:
            raise ValueError("token count expects one encoded conversation")
        encoded = encoded[0]
    return len(encoded)


def synthetic_prompt_content(
    *, seed: str, repeats: int, variant: int, shared_prefix: str = ""
) -> str:
    """Build deterministic prompts without accidental cross-request caching."""
    fingerprint = hashlib.sha256(f"infercrane-request-{variant}".encode()).hexdigest()
    if shared_prefix:
        return (
            shared_prefix
            + f"\nTurn fingerprint {fingerprint}. "
            + (seed * repeats)
            + "Diagnose the failure and propose a verified patch."
        )
    return (
        f"Request fingerprint {fingerprint}. "
        + (seed * repeats)
        + "Diagnose the failure and propose a verified patch."
    )


def _usd_per_million(raw: str | float | int | None) -> float | None:
    if raw in (None, ""):
        return None
    return float(raw) * 1_000_000


def openrouter_snapshot(payload: dict[str, Any], *, captured_at: str) -> dict[str, Any]:
    """Normalize the official endpoint API and derive routing targets.

    We retain only public endpoint measurements. The API credential used to
    fetch them is never included in the snapshot.
    """

    data = payload.get("data") or {}
    endpoints = []
    for raw in data.get("endpoints") or []:
        throughput = raw.get("throughput_last_30m") or {}
        latency = raw.get("latency_last_30m") or {}
        pricing = raw.get("pricing") or {}
        endpoint = {
            "provider": raw.get("provider_name"),
            "name": raw.get("name"),
            "quantization": raw.get("quantization"),
            "context_length": raw.get("context_length"),
            "max_completion_tokens": raw.get("max_completion_tokens"),
            "input_price_usd_per_million": _usd_per_million(pricing.get("prompt")),
            "output_price_usd_per_million": _usd_per_million(pricing.get("completion")),
            "cache_read_price_usd_per_million": _usd_per_million(pricing.get("input_cache_read")),
            "throughput_p50_tokens_per_second": throughput.get("p50"),
            "latency_p50_ms": latency.get("p50"),
            "uptime_30m_percent": raw.get("uptime_last_30m"),
            "uptime_1d_percent": raw.get("uptime_last_1d"),
            "supports_tool_choice": bool(raw.get("supports_tool_choice")),
            "supports_implicit_caching": bool(raw.get("supports_implicit_caching")),
        }
        endpoints.append(endpoint)

    stable = [
        endpoint
        for endpoint in endpoints
        if (endpoint["uptime_30m_percent"] or 0) >= 95
        and endpoint["throughput_p50_tokens_per_second"] is not None
        and endpoint["latency_p50_ms"] is not None
    ]
    if not stable:
        raise ValueError("OpenRouter returned no stable measured endpoints")

    priced = [
        endpoint
        for endpoint in stable
        if endpoint["input_price_usd_per_million"] is not None
        and endpoint["output_price_usd_per_million"] is not None
    ]
    price_floor = min(
        priced,
        key=lambda endpoint: (
            endpoint["input_price_usd_per_million"] * WORKLOAD["input_tokens"]
            + endpoint["output_price_usd_per_million"] * WORKLOAD["output_tokens"]
        ),
    )
    fastest = max(stable, key=lambda endpoint: endpoint["throughput_p50_tokens_per_second"])
    lowest_latency = min(stable, key=lambda endpoint: endpoint["latency_p50_ms"])
    wafer = next((endpoint for endpoint in endpoints if endpoint["provider"] == "Wafer"), None)

    return {
        "schema_version": "infercrane.dev/openrouter-competitive-target/v1",
        "captured_at": captured_at,
        "source": f"https://openrouter.ai/api/v1/models/{MODEL_SLUG}/endpoints",
        "model": data.get("id") or MODEL_SLUG,
        "endpoint_count": len(endpoints),
        "routing_assumptions": {
            "default": "stable providers, then request-price-weighted routing with cheaper providers favored",
            "throughput": "provider sort=throughput and :nitro favor observed output speed",
            "latency": "provider sort=latency favors observed time to first token",
            "tool_use": "tool traffic is also influenced by provider tool-call correctness",
            "cache": "eligible cached conversations may prefer the prior endpoint",
        },
        "targets": {
            "price_floor": price_floor,
            "fastest_p50": fastest,
            "lowest_latency_p50": lowest_latency,
            "wafer": wafer,
            "launch": {
                "minimum_p50_output_tokens_per_second": max(
                    100.0, fastest["throughput_p50_tokens_per_second"] * 1.10
                ),
                "maximum_p50_ttft_ms": min(400.0, lowest_latency["latency_p50_ms"] * 0.90),
                "minimum_uptime_percent": 99.95,
                "maximum_input_price_usd_per_million": price_floor["input_price_usd_per_million"],
                "maximum_output_price_usd_per_million": price_floor["output_price_usd_per_million"],
            },
        },
        "endpoints": endpoints,
    }


def summarize_lane(rows: list[dict[str, Any]], wall_seconds: float, hourly_cost_usd: float) -> dict[str, Any]:
    successful = [row for row in rows if row.get("success")]
    if not rows or not successful or wall_seconds <= 0:
        raise ValueError("lane requires successful measured requests and positive duration")
    input_tokens = sum(int(row["prompt_tokens"]) for row in successful)
    output_tokens = sum(int(row["completion_tokens"]) for row in successful)
    slo_rows = [row for row in successful if row.get("slo_pass")]
    gpu_cost = wall_seconds / 3600 * hourly_cost_usd
    return {
        "concurrency": int(rows[0]["concurrency"]),
        "requests": len(rows),
        "successful_requests": len(successful),
        "input_tokens": input_tokens,
        "output_tokens": output_tokens,
        "prompt_token_mismatch_count": sum(
            not bool(row.get("prompt_token_match")) for row in rows
        ),
        "measurement_seconds": wall_seconds,
        "slo_attainment": len(slo_rows) / len(rows),
        "ttft_p50_ms": percentile((row["ttft_ms"] for row in successful), 0.50),
        "ttft_p95_ms": percentile((row["ttft_ms"] for row in successful), 0.95),
        "itl_p50_ms": percentile((row["itl_ms"] for row in successful), 0.50),
        "itl_p95_ms": percentile((row["itl_ms"] for row in successful), 0.95),
        "per_request_output_tokens_per_second_p50": percentile(
            (row["output_tokens_per_second"] for row in successful), 0.50
        ),
        "per_request_output_tokens_per_second_p95": percentile(
            (row["output_tokens_per_second"] for row in successful), 0.95
        ),
        "aggregate_output_tokens_per_second": output_tokens / wall_seconds,
        "request_throughput_requests_per_second": len(successful) / wall_seconds,
        "slo_goodput_requests_per_second": len(slo_rows) / wall_seconds,
        "slo_qualified_output_tokens_per_second": sum(
            int(row["completion_tokens"]) for row in slo_rows
        )
        / wall_seconds,
        "gpu_seconds_per_successful_request": wall_seconds / len(successful),
        "cost_per_1m_successful_output_tokens_usd": gpu_cost * 1_000_000 / output_tokens,
    }


def project_lane_economics(
    lane: dict[str, Any],
    *,
    hourly_cost_usd: float,
    input_price_usd_per_million: float,
    output_price_usd_per_million: float,
    target_utilization: float = 0.70,
    non_gpu_revenue_fraction: float = 0.10,
    target_gross_margin: float = 0.35,
) -> dict[str, Any]:
    """Project provider economics from one measured workload lane.

    Benchmark cost assumes the GPU was busy for the full measured interval.
    Operational utilization scales that cost upward per served token. Non-GPU
    costs are intentionally reserved as a fraction of revenue rather than
    omitted from the decision.
    """

    if not 0 < target_utilization <= 1:
        raise ValueError("target_utilization must be in (0, 1]")
    if not 0 <= non_gpu_revenue_fraction < 1:
        raise ValueError("non_gpu_revenue_fraction must be in [0, 1)")
    if not 0 <= target_gross_margin < 1:
        raise ValueError("target_gross_margin must be in [0, 1)")
    input_tokens = int(lane.get("input_tokens") or 0)
    output_tokens = int(lane.get("output_tokens") or 0)
    measurement_seconds = float(lane.get("measurement_seconds") or 0)
    throughput = float(lane.get("aggregate_output_tokens_per_second") or 0)
    if input_tokens <= 0 or output_tokens <= 0 or measurement_seconds <= 0 or throughput <= 0:
        raise ValueError("lane requires positive tokens, duration, and output throughput")
    revenue = (
        input_tokens / 1_000_000 * input_price_usd_per_million
        + output_tokens / 1_000_000 * output_price_usd_per_million
    )
    measured_gpu_cost = measurement_seconds / 3600 * hourly_cost_usd
    non_gpu_cost = revenue * non_gpu_revenue_fraction
    operating_gpu_cost = measured_gpu_cost / target_utilization
    operating_cost = operating_gpu_cost + non_gpu_cost
    gross_margin = (revenue - operating_cost) / revenue if revenue > 0 else -math.inf
    break_even_denominator = revenue * (1 - non_gpu_revenue_fraction)
    break_even_utilization = (
        measured_gpu_cost / break_even_denominator if break_even_denominator > 0 else math.inf
    )
    target_denominator = revenue * (
        1 - non_gpu_revenue_fraction - target_gross_margin
    )
    target_margin_utilization = (
        measured_gpu_cost / target_denominator if target_denominator > 0 else math.inf
    )
    input_output_ratio = input_tokens / output_tokens
    revenue_per_million_output_equivalent = (
        output_price_usd_per_million
        + input_output_ratio * input_price_usd_per_million
    )
    allowed_gpu_cost_per_million = revenue_per_million_output_equivalent * (
        1 - non_gpu_revenue_fraction - target_gross_margin
    )
    required_output_tps = (
        hourly_cost_usd * 1_000_000 / (3600 * allowed_gpu_cost_per_million)
        if allowed_gpu_cost_per_million > 0
        else math.inf
    )
    return {
        "input_price_usd_per_million": input_price_usd_per_million,
        "output_price_usd_per_million": output_price_usd_per_million,
        "input_output_token_ratio": input_output_ratio,
        "measured_revenue_usd": revenue,
        "measured_gpu_cost_usd": measured_gpu_cost,
        "measured_gpu_cost_per_million_output_equivalent_usd": (
            measured_gpu_cost * 1_000_000 / output_tokens
        ),
        "target_utilization": target_utilization,
        "non_gpu_revenue_fraction": non_gpu_revenue_fraction,
        "projected_gross_margin": gross_margin,
        "break_even_utilization": break_even_utilization,
        "target_gross_margin": target_gross_margin,
        "target_margin_utilization": target_margin_utilization,
        "required_output_tokens_per_second_for_target_margin": required_output_tps,
        "measured_output_tokens_per_second": throughput,
        "passes_target_margin_at_target_utilization": gross_margin >= target_gross_margin,
        "can_reach_target_margin_at_full_utilization": target_margin_utilization <= 1,
    }


def apply_runtime_parity(
    candidate_results: list[dict[str, Any]],
    *,
    reference_candidate_id: str = "sglang-0520-control",
) -> None:
    """Attach lossless runtime parity gates using deterministic probe hashes."""
    reference = next(
        (row for row in candidate_results if row["candidate_id"] == reference_candidate_id),
        None,
    )
    reference_hashes = (
        [probe["output_sha256"] for probe in reference.get("correctness_probes", [])]
        if reference
        else []
    )
    for result in candidate_results:
        observed = [probe["output_sha256"] for probe in result.get("correctness_probes", [])]
        passed = bool(reference_hashes and observed == reference_hashes)
        result["quality"] = [
            row for row in result["quality"] if row["name"] != "runtime_parity"
        ] + [{"name": "runtime_parity", "passed": passed}]


def merge_candidate_runs(
    run_results: list[dict[str, Any]], *, hourly_cost_usd: float
) -> list[dict[str, Any]]:
    """Merge independent fresh-server runs without averaging away tail samples."""
    grouped: dict[str, list[dict[str, Any]]] = {}
    for result in run_results:
        grouped.setdefault(result["candidate_id"], []).append(result)
    merged = []
    for candidate_id, runs in grouped.items():
        recipe = runs[0]["recipe"]
        workload = runs[0]["workload"]
        if any(run["recipe"] != recipe or run["workload"] != workload for run in runs[1:]):
            raise ValueError(f"candidate {candidate_id} changed recipe or workload between runs")
        lane_rows = []
        for concurrency in workload["concurrency_lanes"]:
            samples = [
                sample
                for run in runs
                for sample in run["request_samples"]
                if sample["concurrency"] == concurrency
            ]
            wall = sum(
                lane["measurement_seconds"]
                for run in runs
                for lane in run["lanes"]
                if lane["concurrency"] == concurrency
            )
            lane = summarize_lane(samples, wall, hourly_cost_usd)
            lane["independent_runs"] = len(runs)
            lane_rows.append(lane)
        gate_names = {
            gate["name"] for run in runs for gate in run.get("quality", [])
        }
        quality = [
            {
                "name": name,
                "passed": all(
                    next(
                        (gate["passed"] for gate in run.get("quality", []) if gate["name"] == name),
                        False,
                    )
                    for run in runs
                ),
            }
            for name in sorted(gate_names)
        ]
        samples = [sample for run in runs for sample in run["request_samples"]]
        successful = sum(bool(sample["success"]) for sample in samples)
        probes = runs[0].get("correctness_probes", [])
        if any(run.get("correctness_probes", []) != probes for run in runs[1:]):
            quality.append({"name": "repeat_determinism", "passed": False})
        else:
            quality.append({"name": "repeat_determinism", "passed": True})
        combined = dict(runs[0])
        combined.update(
            {
                "quality": quality,
                "lanes": lane_rows,
                "request_samples": samples,
                "independent_runs": len(runs),
                "error_rate": 1 - successful / len(samples),
                "prompt_token_mismatch_rate": sum(
                    not bool(sample.get("prompt_token_match")) for sample in samples
                )
                / len(samples),
                "run_receipts": [canonical_digest(run) for run in runs],
            }
        )
        merged.append(combined)
    return sorted(merged, key=lambda row: row["candidate_id"])


def build_evidence(
    candidate_results: list[dict[str, Any]],
    *,
    workload: dict[str, Any] = WORKLOAD,
    evidence_level: str = "screening",
) -> dict[str, Any]:
    if not candidate_results:
        raise ValueError("campaign requires candidate results")
    workload_digest = canonical_digest(workload)
    required_gates = [
        "api_models",
        "stream_usage",
        "stream_done",
        "stream_finish_reason",
        "buffered_usage",
        "structured_output",
        "tool_call",
        "invalid_request_rejected",
        "runtime_parity",
    ]
    minimums = {
        "screening": {"requests": 12, "runs": 1, "seconds": 0.0},
        "qualification": {"requests": 100, "runs": 2, "seconds": 300.0},
        "public": {"requests": 300, "runs": 3, "seconds": 600.0},
    }
    if evidence_level not in minimums:
        raise ValueError(f"unsupported evidence level {evidence_level}")
    floor = minimums[evidence_level]
    if evidence_level != "screening":
        required_gates.append("repeat_determinism")
    hardware_identities = {
        "modal:"
        + ",".join(
            f"{device['name']}:{device['compute_capability']}"
            for device in result["gpu_inventory"]
        )
        for result in candidate_results
    }
    if len(hardware_identities) != 1:
        raise ValueError("all candidates must run on the same exact hardware identity")
    candidates = []
    for result in candidate_results:
        quality = result["quality"]
        gate_map = {row["name"]: bool(row["passed"]) for row in quality}
        lanes = []
        for lane in result["lanes"]:
            lanes.append(
                {
                    "concurrency": lane["concurrency"],
                    "requests": lane["requests"],
                    "successful_requests": lane["successful_requests"],
                    "input_tokens": lane["input_tokens"],
                    "output_tokens": lane["output_tokens"],
                    "independent_runs": int(result.get("independent_runs", 1)),
                    "measurement_seconds": lane["measurement_seconds"],
                    "slo_attainment": lane["slo_attainment"],
                    "ttft_p95_ms": lane["ttft_p95_ms"],
                    "itl_p95_ms": lane["itl_p95_ms"],
                    "aggregate_output_tokens_per_second": lane["aggregate_output_tokens_per_second"],
                    "request_throughput_requests_per_second": lane["request_throughput_requests_per_second"],
                    "slo_goodput_requests_per_second": lane["slo_goodput_requests_per_second"],
                    "slo_qualified_output_tokens_per_second": lane[
                        "slo_qualified_output_tokens_per_second"
                    ],
                    "gpu_seconds_per_successful_request": lane[
                        "gpu_seconds_per_successful_request"
                    ],
                    "cost_per_1m_successful_output_tokens_usd": lane[
                        "cost_per_1m_successful_output_tokens_usd"
                    ],
                    "economics": lane.get("economics"),
                }
            )
        candidates.append(
            {
                "id": result["candidate_id"],
                "runtime_id": result["runtime_id"],
                "origin": "infercrane_generated",
                "evidence_level": evidence_level,
                "evidence_state": "qualified" if evidence_level != "screening" else "reproduced",
                "recipe_digest": canonical_digest(result["recipe"]),
                "workload_digest": workload_digest,
                "quality_passed": all(gate_map.get(gate, False) for gate in required_gates),
                "quality_gates": [
                    {"name": gate, "passed": gate_map.get(gate, False)} for gate in required_gates
                ],
                "error_rate": result["error_rate"],
                "prompt_token_mismatch_rate": result["prompt_token_mismatch_rate"],
                "lanes": lanes,
            }
        )
    return {
        "schema_version": "infercrane.dev/optimization-evidence/v1",
        "model_identity": f"{MODEL_ID}@{MODEL_REVISION}",
        "hardware_identity": hardware_identities.pop(),
        "workload_id": workload["id"],
        "workload_digest": workload_digest,
        "required_concurrency_lanes": workload["concurrency_lanes"],
        "slo": {
            "scope": "each_concurrency_lane",
            "max_ttft_ms": float(workload["slo"]["max_ttft_ms"]),
            "max_itl_ms": float(workload["slo"]["max_itl_ms"]),
            "min_successful_fraction": 0.99,
            "max_error_rate": 0.01,
            "max_prompt_token_mismatch_rate": 0.0,
            "required_quality_gates": required_gates,
            "minimum_requests_per_lane": floor["requests"],
            "minimum_independent_runs": floor["runs"],
            "minimum_measurement_seconds_per_lane": floor["seconds"],
        },
        "selection_policy": {
            "id": "workload-qualified-price-performance-screen-v1",
            "version": 1,
            "objective": "minimum_cogs_under_slo",
            "lane_aggregation": "arithmetic_mean",
            "evidence_level": evidence_level,
            "qualification_before_scoring": True,
            "cost_tie_breaker": True,
        },
        "candidates": candidates,
    }


def competitive_report(
    candidate_results: list[dict[str, Any]], target_snapshot: dict[str, Any]
) -> dict[str, Any]:
    launch = target_snapshot["targets"]["launch"]
    rows = []
    for result in candidate_results:
        lane_one = next(lane for lane in result["lanes"] if lane["concurrency"] == 1)
        p50_speed = lane_one["per_request_output_tokens_per_second_p50"]
        p50_ttft = lane_one["ttft_p50_ms"]
        rows.append(
            {
                "candidate_id": result["candidate_id"],
                "p50_output_tokens_per_second": p50_speed,
                "p50_ttft_ms": p50_ttft,
                "beats_launch_speed_target": p50_speed
                >= launch["minimum_p50_output_tokens_per_second"],
                "beats_launch_latency_target": p50_ttft <= launch["maximum_p50_ttft_ms"],
                "quality_passed": all(row["passed"] for row in result["quality"]),
                "screening_only": True,
            }
        )
    return {
        "schema_version": "infercrane.dev/openrouter-competitive-report/v1",
        "model": MODEL_SLUG,
        "target_captured_at": target_snapshot["captured_at"],
        "comparison_boundary": (
            "Modal loopback screening is directional. Only OpenRouter-observed production "
            "traffic can establish leaderboard placement."
        ),
        "targets": launch,
        "candidates": rows,
    }

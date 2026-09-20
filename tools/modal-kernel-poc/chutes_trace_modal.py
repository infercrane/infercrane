"""Derive content-free InferCrane workload profiles without a local download.

The 91 GB Chutes trace remains in public S3. Modal reads only selected Parquet
row groups and six numeric/timestamp columns, then returns a compact JSON
summary. Prompt text is not present in the source dataset or the result.
"""

from __future__ import annotations

import json
import math
from datetime import datetime, timedelta, timezone

import modal


APP_NAME = "infercrane-public-trace-profiler"
BUCKET = "harvardsys-datasets"
OBJECT_KEY = "2026_chutes_anonymized/chutes_trace.parquet"
SOURCE_URL = f"https://{BUCKET}.s3.us-east-1.amazonaws.com/{OBJECT_KEY}"
TRACE_ROWS = 6_122_413_756
COLUMNS = ["started_at", "completed_at", "it", "ot", "ct", "ttft"]

app = modal.App(APP_NAME)
image = modal.Image.debian_slim(python_version="3.12").pip_install("pyarrow==21.0.0")


def _percentile(values: list[float], quantile: float) -> float:
    if not values:
        raise ValueError("cannot calculate a percentile without samples")
    ordered = sorted(values)
    index = max(0, min(len(ordered) - 1, math.ceil(quantile * len(ordered)) - 1))
    return float(ordered[index])


def _distribution(values: list[float], unit: str) -> dict[str, object]:
    finite = [float(value) for value in values if math.isfinite(float(value)) and float(value) >= 0]
    if not finite:
        raise ValueError(f"no valid {unit} samples")
    return {
        "unit": unit,
        "samples": len(finite),
        "percentiles": {
            "p50": _percentile(finite, 0.50),
            "p90": _percentile(finite, 0.90),
            "p95": _percentile(finite, 0.95),
            "p99": _percentile(finite, 0.99),
        },
    }


def _seconds_between(start: object, completed: object) -> float | None:
    if start is None or completed is None:
        return None
    if isinstance(start, datetime) and isinstance(completed, datetime):
        return (completed - start).total_seconds()
    if isinstance(start, timedelta) and isinstance(completed, timedelta):
        return (completed - start).total_seconds()
    try:
        return float(completed) - float(start)
    except (TypeError, ValueError):
        return None


def _selected_row_groups(total: int, count: int) -> list[int]:
    if total < 1:
        raise ValueError("trace has no row groups")
    count = max(1, min(count, total))
    if count == 1:
        return [total // 2]
    # Avoid edge-only behavior while retaining deterministic coverage across
    # the full year. These are samples, not a claim of a full-trace scan.
    selected = {
        min(total - 1, max(0, round((0.1 + 0.8 * index / (count - 1)) * (total - 1))))
        for index in range(count)
    }
    return sorted(selected)


def _bounded_shape(input_tokens: float, output_tokens: float, max_context_tokens: int) -> tuple[int, int, bool]:
    input_count = max(1, int(round(input_tokens)))
    output_count = max(1, int(round(output_tokens)))
    clipped = input_count + output_count > max_context_tokens
    if clipped:
        output_count = min(output_count, max(1, max_context_tokens // 2))
        input_count = max(1, max_context_tokens - output_count)
    return input_count, output_count, clipped


def _profile(
    name: str,
    description: str,
    objective: str,
    input_tokens: float,
    output_tokens: float,
    concurrency: int,
    max_context_tokens: int,
) -> dict[str, object]:
    bounded_input, bounded_output, clipped = _bounded_shape(
        input_tokens, output_tokens, max_context_tokens
    )
    return {
        "name": name,
        "description": description,
        "objective": objective,
        "requests": max(32, concurrency * 4),
        "concurrency": concurrency,
        "input_tokens": bounded_input,
        "output_tokens": bounded_output,
        "streaming": True,
        "clipped_to_context_window": clipped,
    }


@app.function(
    image=image,
    cpu=4,
    memory=8192,
    timeout=900,
    max_containers=1,
    scaledown_window=2,
)
def profile_chutes_trace(
    row_group_count: int = 3,
    max_rows_per_group: int = 50_000,
    max_context_tokens: int = 2048,
) -> dict[str, object]:
    import pyarrow.fs as fs
    import pyarrow.parquet as pq

    if not 1 <= row_group_count <= 9:
        raise ValueError("row_group_count must be between 1 and 9")
    if not 1_000 <= max_rows_per_group <= 250_000:
        raise ValueError("max_rows_per_group must be between 1,000 and 250,000")
    if not 128 <= max_context_tokens <= 2_000_000:
        raise ValueError("max_context_tokens must be between 128 and 2,000,000")

    filesystem = fs.S3FileSystem(anonymous=True, region="us-east-1")
    parquet = pq.ParquetFile(f"{BUCKET}/{OBJECT_KEY}", filesystem=filesystem)
    metadata = parquet.metadata
    selected = _selected_row_groups(metadata.num_row_groups, row_group_count)

    inputs: list[float] = []
    outputs: list[float] = []
    cached: list[float] = []
    ttft_ms: list[float] = []
    duration_ms: list[float] = []
    cache_fraction: list[float] = []
    inspected = 0

    for row_group in selected:
        accepted = 0
        for batch in parquet.iter_batches(
            batch_size=min(8192, max_rows_per_group),
            row_groups=[row_group],
            columns=COLUMNS,
            use_threads=True,
        ):
            rows = batch.to_pydict()
            batch_length = batch.num_rows
            remaining = max_rows_per_group - accepted
            if remaining <= 0:
                break
            take = min(batch_length, remaining)
            accepted += take
            inspected += take
            for index in range(take):
                input_value = rows["it"][index]
                output_value = rows["ot"][index]
                cached_value = rows["ct"][index]
                ttft_value = rows["ttft"][index]
                if input_value is not None and float(input_value) > 0:
                    input_number = float(input_value)
                    inputs.append(input_number)
                    if cached_value is not None and float(cached_value) >= 0:
                        cached_number = float(cached_value)
                        cached.append(cached_number)
                        cache_fraction.append(min(1.0, cached_number / input_number))
                if output_value is not None and float(output_value) > 0:
                    outputs.append(float(output_value))
                duration_seconds = _seconds_between(
                    rows["started_at"][index], rows["completed_at"][index]
                )
                if duration_seconds is not None and 0 < duration_seconds < 300:
                    duration_ms.append(duration_seconds * 1000)
                # The released trace and its paper define TTFT in seconds.
                if ttft_value is not None and 0 < float(ttft_value) < 300:
                    ttft_ms.append(float(ttft_value) * 1000)
            if accepted >= max_rows_per_group:
                break

    distributions = {
        "input_tokens": _distribution(inputs, "tokens"),
        "output_tokens": _distribution(outputs, "tokens"),
        "cached_tokens": _distribution(cached, "tokens"),
        "ttft_ms": _distribution(ttft_ms, "milliseconds"),
        "duration_ms": _distribution(duration_ms, "milliseconds"),
        "cache_hit_fraction": _distribution(cache_fraction, "ratio"),
    }
    input_percentiles = distributions["input_tokens"]["percentiles"]
    output_percentiles = distributions["output_tokens"]["percentiles"]
    profiles = [
        _profile(
            "public-interactive",
            "Median token shape from the sampled public trace.",
            "latency",
            input_percentiles["p50"],
            output_percentiles["p50"],
            8,
            max_context_tokens,
        ),
        _profile(
            "public-long-prefill",
            "P95 input with median output to stress prefill.",
            "long_context",
            input_percentiles["p95"],
            output_percentiles["p50"],
            4,
            max_context_tokens,
        ),
        _profile(
            "public-decode-heavy",
            "Median input with P95 output to stress decode.",
            "long_generation",
            input_percentiles["p50"],
            output_percentiles["p95"],
            4,
            max_context_tokens,
        ),
    ]
    file_info = filesystem.get_file_info(f"{BUCKET}/{OBJECT_KEY}")
    return {
        "schema_version": "infercrane.public-workload-profile/v1",
        "evidence_class": "public-trace-derived-screening-input",
        "generated_at": datetime.now(timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z"),
        "source": {
            "name": "A Year in LLM Serving (Chutes)",
            "url": SOURCE_URL,
            "license": "CC-BY-4.0",
            "citation": "Nixon et al., A Year in LLM Serving: Workload Evolution, Caching and Load-Balancing (2026)",
            "trace_rows": int(metadata.num_rows or TRACE_ROWS),
            "trace_bytes": int(file_info.size),
        },
        "sampling": {
            "method": "deterministic 10%-to-90% row-group coverage with bounded leading batches",
            "columns": COLUMNS,
            "row_groups": selected,
            "total_row_groups": metadata.num_row_groups,
            "rows_inspected": inspected,
            "remote_only": True,
        },
        "distributions": distributions,
        "profiles": profiles,
        "methodology_boundary": (
            "The public fleet sample selects representative token shapes only. It does not represent a customer's "
            "model mix, absolute arrival rate, semantic quality, or production SLO. Controlled concurrency lanes "
            "must be qualified on the exact model, runtime, GPU, and customer replay before promotion."
        ),
    }


@app.local_entrypoint()
def main(
    row_groups: int = 3,
    rows_per_group: int = 50_000,
    max_context_tokens: int = 2048,
) -> None:
    result = profile_chutes_trace.remote(
        row_group_count=row_groups,
        max_rows_per_group=rows_per_group,
        max_context_tokens=max_context_tokens,
    )
    print("INFERCRANE_CHUTES_PROFILE=" + json.dumps(result, sort_keys=True))

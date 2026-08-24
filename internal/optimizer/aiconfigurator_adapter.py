"""Versioned, stdout-clean adapter for AIConfigurator.

InferCrane embeds and executes this file in a separately managed Python
environment. Keep the wire contract independent from AIConfigurator objects so
AISimulate can replace this implementation without changing InferCrane state.
"""

from __future__ import annotations

import contextlib
import hashlib
import io
import importlib.metadata
import json
import math
import sys
from typing import Any


INPUT_SCHEMA = "infercrane.optimizer.estimator-input/v1"
OUTPUT_SCHEMA = "infercrane.optimizer.estimator-output/v1"


def finite(value: Any) -> float | None:
    try:
        result = float(value)
    except (TypeError, ValueError):
        return None
    return result if math.isfinite(result) else None


def integer(value: Any) -> int | None:
    parsed = finite(value)
    if parsed is None or parsed < 0 or not parsed.is_integer():
        return None
    return int(parsed)


def first(row: Any, *names: str) -> Any:
    for name in names:
        value = row.get(name)
        if value is not None and finite(value) is not None:
            return value
    return None


def pool(row: Any, prefix: str) -> dict[str, int]:
    workers = integer(row.get(f"({prefix})workers"))
    tp = integer(row.get(f"({prefix})tp"))
    if not workers or not tp:
        return {}
    return {"replicas": workers, "tensor_parallelism": tp}


def normalize_row(mode: str, backend: str, row: Any) -> dict[str, Any]:
    result: dict[str, Any] = {
        "mode": "disaggregated" if mode.startswith("disagg") else "aggregated",
        "backend": backend,
        "total_gpus": integer(first(row, "total_gpus_needed", "total_gpus_used", "num_total_gpus")),
        "replicas": integer(first(row, "replicas_needed", "replicas")),
        "gpus_per_replica": integer(row.get("num_total_gpus")),
        "tensor_parallelism": integer(first(row, "tp", "(a)tp")),
        "estimated_ttft_ms": finite(row.get("ttft")),
        "estimated_tpot_ms": finite(row.get("tpot")),
        "estimated_request_latency_ms": finite(row.get("request_latency")),
        "estimated_request_rate": finite(first(row, "cluster_request_rate", "request_rate", "seq/s")),
        "estimated_output_tokens_second_per_gpu": finite(row.get("tokens/s/gpu")),
        "prefill": pool(row, "p"),
        "decode": pool(row, "d"),
    }
    if not result["replicas"]:
        total = result["total_gpus"] or result["gpus_per_replica"]
        per_replica = result["gpus_per_replica"] or total
        if total and per_replica and total % per_replica == 0:
            result["replicas"] = total // per_replica
    if not result["tensor_parallelism"] and result["mode"] == "aggregated":
        result["tensor_parallelism"] = result["gpus_per_replica"]
    return result


def main() -> None:
    request = json.load(sys.stdin)
    if request.get("schema_version") != INPUT_SCHEMA:
        raise ValueError("unsupported estimator input schema")

    # Import and model execution can be chatty. The wire protocol reserves
    # stdout for exactly one JSON object.
    diagnostics = io.StringIO()
    with contextlib.redirect_stdout(diagnostics), contextlib.redirect_stderr(diagnostics):
        import aiconfigurator
        from aiconfigurator.cli import cli_recommend

        version = str(aiconfigurator.__version__)
        required = str(request["required_version"])
        if version != required:
            raise RuntimeError(f"aiconfigurator version {version} does not match required {required}")
        plotext_version = importlib.metadata.version("plotext")
        required_plotext = str(request["required_plotext_version"])
        if plotext_version != required_plotext:
            raise RuntimeError(
                f"plotext version {plotext_version} does not match required {required_plotext}; "
                "AIConfigurator 0.11.0 is incompatible with plotext 6"
            )

        result = cli_recommend(
            model_path=str(request["model_path"]),
            system=str(request["system"]),
            target_concurrency=float(request["target_concurrency"]),
            backend=str(request["backend"]),
            database_mode=str(request.get("database_mode", "HYBRID")),
            isl=int(request["input_tokens"]),
            osl=int(request["output_tokens"]),
            ttft=float(request["ttft_ms"]),
            tpot=float(request["tpot_ms"]),
            prefix=int(request.get("prefix_tokens", 0)),
            strict_sla=True,
            enable_chunked_prefill=bool(request.get("enable_chunked_prefill", False)),
            top_n=int(request["top_n"]),
        )

    candidates: list[dict[str, Any]] = []
    for mode in sorted(result.best_configs):
        frame = result.best_configs[mode]
        if frame is None or frame.empty:
            continue
        task = result.tasks.get(mode)
        backend = str(getattr(task, "primary_backend_name", request["backend"]))
        for _, row in frame.head(int(request["top_n"])).iterrows():
            candidates.append(normalize_row(mode, backend, row))

    canonical = json.dumps(candidates, sort_keys=True, separators=(",", ":"))
    response = {
        "schema_version": OUTPUT_SCHEMA,
        "source": "aiconfigurator",
        "source_version": version,
        "evidence_class": "modeled",
        "model_path": request["model_path"],
        "system": request["system"],
        "backend": request["backend"],
        "result_digest": "sha256:" + hashlib.sha256(canonical.encode()).hexdigest(),
        "candidates": candidates,
    }
    json.dump(response, sys.stdout, sort_keys=True, separators=(",", ":"), allow_nan=False)
    sys.stdout.write("\n")


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        json.dump(
            {
                "schema_version": OUTPUT_SCHEMA,
                "error": type(error).__name__,
                "message": str(error)[:1000],
            },
            sys.stdout,
            sort_keys=True,
            separators=(",", ":"),
        )
        sys.stdout.write("\n")
        raise SystemExit(2)

"""Paired AIPerf qualification of Qwen3-0.6B runtime recipes on Modal H100.

The model is pinned and downloaded while the image is built so paid GPU time is
reserved for serving and measurement.  The campaign compares vLLM's default
compiled runtime with its eager diagnostic baseline under identical traffic.
"""

from __future__ import annotations

import json
import math
import os
import shutil
import subprocess
import time
import urllib.request
from pathlib import Path

import modal


APP_NAME = "infercrane-qwen-aiperf-poc"
MODEL_ID = "Qwen/Qwen3-0.6B"
MODEL_REVISION = "c1899de289a04d12100db370d81485cdf75e47ca"
MODEL_DIR = "/models/qwen3-0.6b"
QWEN17_MODEL_ID = "Qwen/Qwen3-1.7B"
QWEN17_MODEL_REVISION = "70d244cc86ccca08cf5af4e1e306ecf908b1ad5e"
QWEN17_MODEL_DIR = "/models/qwen3-1.7b"
PORT = 8000
SGLANG_VERSION = "0.5.10.post1"

app = modal.App(APP_NAME)
image = (
    modal.Image.debian_slim(python_version="3.12")
    .pip_install("vllm==0.22.1", "aiperf==0.12.0")
    .run_commands(
        "python -c \"from huggingface_hub import snapshot_download; "
        f"snapshot_download(repo_id='{MODEL_ID}', revision='{MODEL_REVISION}', "
        f"local_dir='{MODEL_DIR}')\""
    )
)
qwen17_image = (
    modal.Image.debian_slim(python_version="3.12")
    .pip_install("vllm==0.22.1", "aiperf==0.12.0")
    .run_commands(
        "python -c \"from huggingface_hub import snapshot_download; "
        f"snapshot_download(repo_id='{QWEN17_MODEL_ID}', revision='{QWEN17_MODEL_REVISION}', "
        f"local_dir='{QWEN17_MODEL_DIR}')\""
    )
)
sglang_image = (
    # SGLang 0.5.10 JIT-compiles its default CUDA RoPE path for Qwen3, so its
    # honest default recipe needs a toolkit image rather than a runtime-only
    # image.  CUDA 12.8 matches the cu128 PyTorch wheel pinned by SGLang.
    modal.Image.from_registry("nvidia/cuda:12.8.1-devel-ubuntu22.04", add_python="3.12")
    .pip_install(f"sglang[srt]=={SGLANG_VERSION}", "aiperf==0.12.0")
    .apt_install("libnuma1")
    .run_commands(
        "python -c \"from huggingface_hub import snapshot_download; "
        f"snapshot_download(repo_id='{MODEL_ID}', revision='{MODEL_REVISION}', "
        f"local_dir='{MODEL_DIR}')\""
    )
)


def _wait_for_server(process: subprocess.Popen, log_path: Path, timeout_seconds: int = 240) -> None:
    deadline = time.monotonic() + timeout_seconds
    url = f"http://127.0.0.1:{PORT}/v1/models"
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise RuntimeError(
                f"server exited with {process.returncode}:\n{log_path.read_text(errors='replace')[-12000:]}"
            )
        try:
            with urllib.request.urlopen(url, timeout=2) as response:
                if response.status == 200:
                    return
        except Exception:
            time.sleep(1)
    raise TimeoutError(f"server did not become ready:\n{log_path.read_text(errors='replace')[-12000:]}")


def _warm_server(model_id: str = MODEL_ID) -> None:
    body = json.dumps(
        {
            "model": model_id,
            "messages": [{"role": "user", "content": "Reply with the word ready."}],
            "max_tokens": 8,
            "temperature": 0,
        }
    ).encode()
    request = urllib.request.Request(
        f"http://127.0.0.1:{PORT}/v1/chat/completions",
        data=body,
        headers={"Content-Type": "application/json"},
    )
    with urllib.request.urlopen(request, timeout=180) as response:
        if response.status != 200:
            raise RuntimeError(f"vLLM warmup returned {response.status}")


def _percentile(values: list[float], percentile: float) -> float | None:
    if not values:
        return None
    ordered = sorted(values)
    index = max(0, min(len(ordered) - 1, math.ceil(percentile * len(ordered)) - 1))
    return ordered[index]


def _milliseconds(metric: dict) -> float:
    value = float(metric["value"])
    unit = str(metric.get("unit", "ms")).strip().lower()
    if unit in ("", "ms", "millisecond", "milliseconds"):
        return value
    if unit in ("s", "sec", "second", "seconds"):
        return value * 1000
    if unit in ("us", "µs", "μs", "microsecond", "microseconds"):
        return value / 1000
    if unit in ("ns", "nanosecond", "nanoseconds"):
        return value / 1_000_000
    raise ValueError(f"unsupported AIPerf latency unit {unit!r}")


def _parse_record_file(path: Path) -> dict[str, object]:
    ttft: list[float] = []
    tpot: list[float] = []
    latency: list[float] = []
    output_tokens = 0.0
    requests = succeeded = failed = 0
    earliest = latest = 0
    for line in path.read_text().splitlines():
        row = json.loads(line)
        metadata = row.get("metadata", {})
        if metadata.get("benchmark_phase") not in (None, "", "profiling"):
            continue
        requests += 1
        start = int(metadata.get("request_start_ns") or 0)
        end = int(metadata.get("request_end_ns") or 0)
        if start and (not earliest or start < earliest):
            earliest = start
        latest = max(latest, end)
        if row.get("error") is not None:
            failed += 1
            continue
        succeeded += 1
        metrics = row.get("metrics", {})
        if "time_to_first_token" in metrics:
            ttft.append(_milliseconds(metrics["time_to_first_token"]))
        if "request_latency" in metrics:
            latency.append(_milliseconds(metrics["request_latency"]))
        tpot_metric = metrics.get("time_per_output_token") or metrics.get("inter_token_latency")
        if tpot_metric:
            tpot.append(_milliseconds(tpot_metric))
        if "output_token_count" in metrics:
            output_tokens += float(metrics["output_token_count"]["value"])
    duration = (latest - earliest) / 1_000_000_000 if latest > earliest else 0
    return {
        "requests": requests,
        "succeeded": succeeded,
        "failed": failed,
        "duration_seconds": duration,
        "request_throughput": succeeded / duration if duration else None,
        "output_token_throughput": output_tokens / duration if duration else None,
        "ttft_p50_ms": _percentile(ttft, 0.50),
        "ttft_p95_ms": _percentile(ttft, 0.95),
        "tpot_p50_ms": _percentile(tpot, 0.50),
        "tpot_p95_ms": _percentile(tpot, 0.95),
        "latency_p50_ms": _percentile(latency, 0.50),
        "latency_p95_ms": _percentile(latency, 0.95),
        "output_tokens": output_tokens,
    }


def _confidence(values: list[float]) -> dict[str, float | int] | None:
    if len(values) < 2:
        return None
    mean = sum(values) / len(values)
    variance = sum((value - mean) ** 2 for value in values) / (len(values) - 1)
    standard_deviation = math.sqrt(variance)
    t_critical = {2: 12.706, 3: 4.303, 4: 3.182, 5: 2.776}.get(len(values), 2.571)
    margin = t_critical * standard_deviation / math.sqrt(len(values))
    return {
        "samples": len(values),
        "mean": mean,
        "standard_deviation": standard_deviation,
        "coefficient_of_variation": standard_deviation / abs(mean) if mean else 0,
        "lower_95": mean - margin,
        "upper_95": mean + margin,
    }


def _parse_artifacts(directory: Path) -> dict[str, object]:
    files = sorted(directory.rglob("infercrane.jsonl"))
    nested = [path for path in files if path.parent != directory]
    if nested:
        files = nested
    if not files:
        files = sorted(directory.rglob("profile_export.jsonl"))
    if not files:
        raise RuntimeError(f"AIPerf produced no record export under {directory}")
    runs = [_parse_record_file(path) for path in files]
    totals = {
        "requests": sum(int(run["requests"]) for run in runs),
        "succeeded": sum(int(run["succeeded"]) for run in runs),
        "failed": sum(int(run["failed"]) for run in runs),
        "duration_seconds": sum(float(run["duration_seconds"]) for run in runs),
        "output_tokens": sum(float(run["output_tokens"]) for run in runs),
    }
    duration = totals["duration_seconds"]
    totals["request_throughput"] = totals["succeeded"] / duration if duration else None
    totals["output_token_throughput"] = totals["output_tokens"] / duration if duration else None
    for metric in (
        "ttft_p50_ms",
        "ttft_p95_ms",
        "tpot_p50_ms",
        "tpot_p95_ms",
        "latency_p50_ms",
        "latency_p95_ms",
    ):
        values = [float(run[metric]) for run in runs if run[metric] is not None]
        totals[metric] = sum(values) / len(values) if values else None
    confidence = {}
    for metric in ("request_throughput", "output_token_throughput", "ttft_p95_ms", "tpot_p95_ms", "latency_p95_ms"):
        values = [float(run[metric]) for run in runs if run[metric] is not None]
        interval = _confidence(values)
        if interval:
            confidence[metric] = interval
    return {"aggregate": totals, "confidence": confidence, "runs": runs}


def _run_aiperf(recipe: str, concurrency: int) -> dict[str, object]:
    artifact_dir = Path(f"/tmp/aiperf-{recipe}-{concurrency}")
    shutil.rmtree(artifact_dir, ignore_errors=True)
    request_count = max(32, concurrency * 2)
    command = [
        "aiperf",
        "profile",
        "--model",
        MODEL_ID,
        "--tokenizer",
        MODEL_DIR,
        "--url",
        f"http://127.0.0.1:{PORT}",
        "--endpoint-type",
        "chat",
        "--streaming",
        "--request-timeout-seconds",
        "30",
        "--request-count",
        str(request_count),
        "--concurrency",
        str(concurrency),
        "--random-seed",
        "17",
        "--synthetic-input-tokens-mean",
        "128",
        "--synthetic-input-tokens-stddev",
        "0",
        "--output-tokens-mean",
        "64",
        "--output-tokens-stddev",
        "0",
        "--extra-inputs",
        '{"ignore_eos":true}',
        "--num-profile-runs",
        "3",
        "--warmup-request-count",
        str(max(4, concurrency)),
        "--profile-run-cooldown-seconds",
        "1",
        "--ui",
        "none",
        "--export-level",
        "records",
        "--artifact-dir",
        str(artifact_dir),
        "--profile-export-prefix",
        "infercrane",
        "--no-auto-plot",
        "--no-gpu-telemetry",
        "--no-server-metrics",
    ]
    started = time.monotonic()
    try:
        completed = subprocess.run(
            command, check=False, capture_output=True, text=True, timeout=240
        )
    except subprocess.TimeoutExpired as exc:
        stdout = (
            exc.stdout.decode(errors="replace")
            if isinstance(exc.stdout, bytes)
            else (exc.stdout or "")
        )
        stderr = (
            exc.stderr.decode(errors="replace")
            if isinstance(exc.stderr, bytes)
            else (exc.stderr or "")
        )
        raise RuntimeError(
            f"AIPerf exceeded the 240-second lane bound for {recipe} at concurrency "
            f"{concurrency}\nSTDOUT:\n{stdout[-12000:]}\nSTDERR:\n{stderr[-12000:]}"
        ) from exc
    if completed.returncode:
        raise RuntimeError(
            f"AIPerf failed ({completed.returncode})\nSTDOUT:\n{completed.stdout[-12000:]}"
            f"\nSTDERR:\n{completed.stderr[-12000:]}"
        )
    parsed = _parse_artifacts(artifact_dir)
    parsed.update(
        {
            "recipe": recipe,
            "concurrency": concurrency,
            "request_count_per_run": request_count,
            "profile_runs": 3,
            "wall_seconds": time.monotonic() - started,
            "workload": {"input_tokens": 128, "output_tokens": 64, "streaming": True},
            "command": " ".join(command),
        }
    )
    return parsed


def _run_trace_aiperf(
    recipe: str,
    model_id: str,
    model_dir: str,
    profile: dict[str, object],
) -> dict[str, object]:
    profile_name = str(profile["name"])
    concurrency = int(profile["concurrency"])
    request_count = int(profile["requests"])
    input_tokens = int(profile["input_tokens"])
    output_tokens = int(profile["output_tokens"])
    artifact_dir = Path(f"/tmp/aiperf-{recipe}-{profile_name}")
    shutil.rmtree(artifact_dir, ignore_errors=True)
    command = [
        "aiperf",
        "profile",
        "--model",
        model_id,
        "--tokenizer",
        model_dir,
        "--url",
        f"http://127.0.0.1:{PORT}",
        "--endpoint-type",
        "chat",
        "--streaming",
        "--request-timeout-seconds",
        "180",
        "--request-count",
        str(request_count),
        "--concurrency",
        str(concurrency),
        "--random-seed",
        "17",
        "--synthetic-input-tokens-mean",
        str(input_tokens),
        "--synthetic-input-tokens-stddev",
        "0",
        "--output-tokens-mean",
        str(output_tokens),
        "--output-tokens-stddev",
        "0",
        "--extra-inputs",
        '{"ignore_eos":true}',
        "--num-profile-runs",
        "2",
        "--warmup-request-count",
        str(max(4, concurrency)),
        "--profile-run-cooldown-seconds",
        "1",
        "--ui",
        "none",
        "--export-level",
        "records",
        "--artifact-dir",
        str(artifact_dir),
        "--profile-export-prefix",
        "infercrane",
        "--no-auto-plot",
        "--no-gpu-telemetry",
        "--no-server-metrics",
    ]
    started = time.monotonic()
    try:
        completed = subprocess.run(
            command, check=False, capture_output=True, text=True, timeout=600
        )
    except subprocess.TimeoutExpired as exc:
        stdout = (
            exc.stdout.decode(errors="replace")
            if isinstance(exc.stdout, bytes)
            else (exc.stdout or "")
        )
        stderr = (
            exc.stderr.decode(errors="replace")
            if isinstance(exc.stderr, bytes)
            else (exc.stderr or "")
        )
        raise RuntimeError(
            f"AIPerf exceeded the 600-second trace lane bound for {model_id}/{profile_name}"
            f"\nSTDOUT:\n{stdout[-12000:]}\nSTDERR:\n{stderr[-12000:]}"
        ) from exc
    if completed.returncode:
        raise RuntimeError(
            f"AIPerf failed ({completed.returncode}) for {model_id}/{profile_name}"
            f"\nSTDOUT:\n{completed.stdout[-12000:]}\nSTDERR:\n{completed.stderr[-12000:]}"
        )
    parsed = _parse_artifacts(artifact_dir)
    parsed.update(
        {
            "recipe": recipe,
            "profile": profile_name,
            "concurrency": concurrency,
            "request_count_per_run": request_count,
            "profile_runs": 2,
            "wall_seconds": time.monotonic() - started,
            "workload": {
                "input_tokens": input_tokens,
                "output_tokens": output_tokens,
                "streaming": True,
            },
            "command": " ".join(command),
        }
    )
    return parsed


def _run_trace_recipe(
    model_id: str,
    model_dir: str,
    profiles: list[dict[str, object]],
) -> list[dict[str, object]]:
    required_model_len = max(
        int(profile["input_tokens"]) + int(profile["output_tokens"])
        for profile in profiles
    )
    # AIPerf's synthetic token target does not include every chat-template
    # control token. Reserve bounded headroom so the benchmark tests the chosen
    # shape instead of accidentally exercising the server's overflow guard.
    max_model_len = max(2048, min(8192, required_model_len + 512))
    recipe = "vllm-compiled"
    log_path = Path(f"/tmp/vllm-{model_id.rsplit('/', 1)[-1]}.log")
    command = [
        "python",
        "-m",
        "vllm.entrypoints.openai.api_server",
        "--model",
        model_dir,
        "--served-model-name",
        model_id,
        "--dtype",
        "bfloat16",
        "--host",
        "127.0.0.1",
        "--port",
        str(PORT),
        "--max-model-len",
        str(max_model_len),
        "--gpu-memory-utilization",
        "0.80",
        "--generation-config",
        "vllm",
        "--no-enable-log-requests",
    ]
    server_environment = os.environ.copy()
    server_environment["VLLM_USE_DEEP_GEMM"] = "0"
    with log_path.open("w") as log:
        process = subprocess.Popen(
            command, stdout=log, stderr=subprocess.STDOUT, env=server_environment
        )
    try:
        _wait_for_server(process, log_path)
        _warm_server(model_id)
        return [
            _run_trace_aiperf(recipe, model_id, model_dir, profile)
            for profile in profiles
        ]
    finally:
        process.terminate()
        try:
            process.wait(timeout=30)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=10)


def _run_public_reference_aiperf(recipe: str) -> dict[str, object]:
    """Reproduce the workload published in AIPerf's comprehensive guide."""
    artifact_dir = Path(f"/tmp/aiperf-{recipe}")
    shutil.rmtree(artifact_dir, ignore_errors=True)
    command = [
        "aiperf",
        "profile",
        "--model",
        MODEL_ID,
        "--tokenizer",
        MODEL_DIR,
        "--url",
        f"http://127.0.0.1:{PORT}",
        "--endpoint-type",
        "chat",
        "--streaming",
        "--use-server-token-count",
        "--request-count",
        "1000",
        "--concurrency",
        "100",
        "--random-seed",
        "17",
        "--synthetic-input-tokens-mean",
        "1000",
        "--synthetic-input-tokens-stddev",
        "0",
        "--output-tokens-mean",
        "500",
        "--output-tokens-stddev",
        "0",
        "--extra-inputs",
        '{"min_tokens":500,"ignore_eos":true}',
        "--warmup-request-count",
        "100",
        "--ui",
        "none",
        "--export-level",
        "records",
        "--artifact-dir",
        str(artifact_dir),
        "--profile-export-prefix",
        "infercrane",
        "--no-auto-plot",
        "--no-gpu-telemetry",
        "--no-server-metrics",
    ]
    started = time.monotonic()
    completed = subprocess.run(command, check=False, capture_output=True, text=True, timeout=900)
    if completed.returncode:
        raise RuntimeError(
            f"AIPerf public reference failed ({completed.returncode})\nSTDOUT:\n{completed.stdout[-12000:]}"
            f"\nSTDERR:\n{completed.stderr[-12000:]}"
        )
    parsed = _parse_artifacts(artifact_dir)
    parsed.update(
        {
            "recipe": recipe,
            "concurrency": 100,
            "request_count_per_run": 1000,
            "profile_runs": 1,
            "wall_seconds": time.monotonic() - started,
            "workload": {"input_tokens": 1000, "output_tokens": 500, "streaming": True},
            "command": " ".join(command),
            "public_reference_output_token_throughput": 22521.42,
            "public_reference_request_throughput": 45.70,
            "comparison_boundary": "published AIPerf guide does not identify its GPU/runtime; directional only",
        }
    )
    return parsed


def _run_recipe(recipe: str, extra_server_args: list[str]) -> list[dict[str, object]]:
    log_path = Path(f"/tmp/vllm-{recipe}.log")
    command = [
        "python",
        "-m",
        "vllm.entrypoints.openai.api_server",
        "--model",
        MODEL_DIR,
        "--served-model-name",
        MODEL_ID,
        "--dtype",
        "bfloat16",
        "--host",
        "127.0.0.1",
        "--port",
        str(PORT),
        "--max-model-len",
        "2048",
        "--gpu-memory-utilization",
        "0.70",
        "--generation-config",
        "vllm",
        "--no-enable-log-requests",
        *extra_server_args,
    ]
    server_environment = os.environ.copy()
    # vLLM 0.22.1 can mis-detect its vendored DeepGEMM as usable on H100 and
    # then fail while warming an ordinary BF16 model.  DeepGEMM is irrelevant
    # to this non-FP8 dense model; use the upstream issue's documented bypass.
    server_environment["VLLM_USE_DEEP_GEMM"] = "0"
    with log_path.open("w") as log:
        process = subprocess.Popen(command, stdout=log, stderr=subprocess.STDOUT, env=server_environment)
    try:
        _wait_for_server(process, log_path)
        _warm_server()
        return [_run_aiperf(recipe, concurrency) for concurrency in (1, 8, 32)]
    finally:
        process.terminate()
        try:
            process.wait(timeout=30)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=10)


def _run_sglang_recipe(recipe: str, extra_server_args: list[str] | None = None) -> list[dict[str, object]]:
    """Run the same AIPerf contract against SGLang's OpenAI endpoint."""
    log_path = Path(f"/tmp/sglang-{recipe}.log")
    command = [
        "python",
        "-m",
        "sglang.launch_server",
        "--model-path",
        MODEL_DIR,
        "--served-model-name",
        MODEL_ID,
        "--dtype",
        "bfloat16",
        "--host",
        "127.0.0.1",
        "--port",
        str(PORT),
        "--context-length",
        "2048",
        "--mem-fraction-static",
        "0.70",
        *(extra_server_args or []),
    ]
    server_environment = os.environ.copy()
    server_environment["SGLANG_ENABLE_JIT_DEEPGEMM"] = "0"
    with log_path.open("w") as log:
        process = subprocess.Popen(command, stdout=log, stderr=subprocess.STDOUT, env=server_environment)
    try:
        _wait_for_server(process, log_path)
        _warm_server()
        return [_run_aiperf(recipe, concurrency) for concurrency in (1, 8, 32)]
    finally:
        process.terminate()
        try:
            process.wait(timeout=30)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=10)


def _run_public_reference_recipe(recipe: str, extra_server_args: list[str]) -> dict[str, object]:
    log_path = Path(f"/tmp/vllm-{recipe}.log")
    command = [
        "python",
        "-m",
        "vllm.entrypoints.openai.api_server",
        "--model",
        MODEL_DIR,
        "--served-model-name",
        MODEL_ID,
        "--dtype",
        "bfloat16",
        "--host",
        "127.0.0.1",
        "--port",
        str(PORT),
        "--max-model-len",
        "2048",
        "--gpu-memory-utilization",
        "0.70",
        "--generation-config",
        "vllm",
        "--no-enable-log-requests",
        *extra_server_args,
    ]
    server_environment = os.environ.copy()
    server_environment["VLLM_USE_DEEP_GEMM"] = "0"
    with log_path.open("w") as log:
        process = subprocess.Popen(command, stdout=log, stderr=subprocess.STDOUT, env=server_environment)
    try:
        _wait_for_server(process, log_path)
        _warm_server()
        return _run_public_reference_aiperf(recipe)
    finally:
        process.terminate()
        try:
            process.wait(timeout=30)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=10)


def _comparisons(campaigns: list[dict[str, object]]) -> list[dict[str, float | int]]:
    indexed = {(str(row["recipe"]), int(row["concurrency"])): row["aggregate"] for row in campaigns}
    comparisons = []
    for concurrency in (1, 8, 32):
        compiled = indexed[("compiled", concurrency)]
        eager = indexed[("eager", concurrency)]
        comparisons.append(
            {
                "concurrency": concurrency,
                "output_throughput_speedup": compiled["output_token_throughput"] / eager["output_token_throughput"],
                "request_throughput_speedup": compiled["request_throughput"] / eager["request_throughput"],
                "ttft_p95_reduction_fraction": 1 - compiled["ttft_p95_ms"] / eager["ttft_p95_ms"],
                "tpot_p95_reduction_fraction": 1 - compiled["tpot_p95_ms"] / eager["tpot_p95_ms"],
                "latency_p95_reduction_fraction": 1 - compiled["latency_p95_ms"] / eager["latency_p95_ms"],
            }
        )
    return comparisons


def _engine_comparisons(vllm_result: dict[str, object], sglang_result: dict[str, object]) -> list[dict[str, float | int]]:
    vllm_lanes = {int(row["concurrency"]): row["aggregate"] for row in vllm_result["campaigns"]}
    sglang_lanes = {int(row["concurrency"]): row["aggregate"] for row in sglang_result["campaigns"]}
    comparisons = []
    for concurrency in (1, 8, 32):
        vllm = vllm_lanes[concurrency]
        sglang = sglang_lanes[concurrency]
        comparisons.append(
            {
                "concurrency": concurrency,
                "sglang_over_vllm_output_throughput": sglang["output_token_throughput"] / vllm["output_token_throughput"],
                "sglang_over_vllm_request_throughput": sglang["request_throughput"] / vllm["request_throughput"],
                "sglang_ttft_p95_change_fraction": sglang["ttft_p95_ms"] / vllm["ttft_p95_ms"] - 1,
                "sglang_tpot_p95_change_fraction": sglang["tpot_p95_ms"] / vllm["tpot_p95_ms"] - 1,
                "sglang_latency_p95_change_fraction": sglang["latency_p95_ms"] / vllm["latency_p95_ms"] - 1,
            }
        )
    return comparisons


@app.function(
    image=image,
    gpu="H100!",
    cpu=8,
    memory=16384,
    timeout=1800,
    max_containers=1,
    scaledown_window=2,
)
def qualify_qwen() -> dict[str, object]:
    import torch

    gpu = torch.cuda.get_device_properties(0)
    campaigns = []
    campaigns.extend(_run_recipe("eager", ["--enforce-eager"]))
    campaigns.extend(_run_recipe("compiled", []))
    return {
        "schema_version": "infercrane.modal-aiperf-campaign/v1",
        "evidence_class": "paired-real-gpu-end-to-end",
        "model": MODEL_ID,
        "model_revision": MODEL_REVISION,
        "runtime": "vllm==0.22.1",
        "benchmark": "aiperf==0.12.0",
        "gpu_request": "H100!",
        "gpu": gpu.name,
        "compute_capability": f"sm{gpu.major}{gpu.minor}",
        "torch_version": torch.__version__,
        "campaigns": campaigns,
        "comparisons": _comparisons(campaigns),
        "qualification_boundary": (
            "synthetic fixed-shape performance evidence; customer replay and semantic quality remain required"
        ),
    }


@app.function(
    image=image,
    gpu="H100!",
    cpu=8,
    memory=16384,
    timeout=1200,
    max_containers=1,
    scaledown_window=2,
)
def qualify_vllm_default_qwen() -> dict[str, object]:
    import torch

    gpu = torch.cuda.get_device_properties(0)
    return {
        "schema_version": "infercrane.modal-engine-lane/v1",
        "evidence_class": "paired-real-gpu-end-to-end-screening",
        "model": MODEL_ID,
        "model_revision": MODEL_REVISION,
        "runtime": "vllm==0.22.1",
        "benchmark": "aiperf==0.12.0",
        "gpu_request": "H100!",
        "gpu": gpu.name,
        "compute_capability": f"sm{gpu.major}{gpu.minor}",
        "torch_version": torch.__version__,
        "campaigns": _run_recipe("vllm-default", []),
        "qualification_boundary": "cross-engine screening; customer replay, quality, cost, and longer qualification runs remain required",
    }


@app.function(
    image=sglang_image,
    gpu="H100!",
    cpu=8,
    memory=16384,
    timeout=1200,
    max_containers=1,
    scaledown_window=2,
)
def qualify_sglang_stable_qwen() -> dict[str, object]:
    import torch

    gpu = torch.cuda.get_device_properties(0)
    return {
        "schema_version": "infercrane.modal-engine-lane/v1",
        "evidence_class": "paired-real-gpu-end-to-end-screening",
        "model": MODEL_ID,
        "model_revision": MODEL_REVISION,
        "runtime": f"sglang=={SGLANG_VERSION}",
        "benchmark": "aiperf==0.12.0",
        "gpu_request": "H100!",
        "gpu": gpu.name,
        "compute_capability": f"sm{gpu.major}{gpu.minor}",
        "torch_version": torch.__version__,
        "campaigns": _run_sglang_recipe(
            "sglang-stable", ["--disable-piecewise-cuda-graph"]
        ),
        "server_recipe": ["--disable-piecewise-cuda-graph"],
        "recipe_reason": (
            "SGLang 0.5.10 enables experimental piecewise CUDA graphs by default; "
            "the path faulted during H100 warmup, so this lane uses the runtime's "
            "recommended stable fallback while retaining ordinary CUDA graphs"
        ),
        "qualification_boundary": "cross-engine screening; customer replay, quality, cost, and longer qualification runs remain required",
    }


@app.function(
    image=image,
    gpu="H100!",
    cpu=8,
    memory=16384,
    timeout=1200,
    max_containers=1,
    scaledown_window=2,
)
def public_reference_qwen() -> dict[str, object]:
    import torch

    gpu = torch.cuda.get_device_properties(0)
    campaign = _run_public_reference_recipe("compiled-balanced-public-reference", [])
    measured = campaign["aggregate"]
    return {
        "schema_version": "infercrane.modal-aiperf-public-reference/v1",
        "evidence_class": "real-gpu-end-to-end-public-workload-reproduction",
        "model": MODEL_ID,
        "model_revision": MODEL_REVISION,
        "runtime": "vllm==0.22.1",
        "benchmark": "aiperf==0.12.0",
        "gpu": gpu.name,
        "compute_capability": f"sm{gpu.major}{gpu.minor}",
        "campaign": campaign,
        "directional_ratio_vs_published_output_throughput": (
            measured["output_token_throughput"] / campaign["public_reference_output_token_throughput"]
        ),
        "directional_ratio_vs_published_request_throughput": (
            measured["request_throughput"] / campaign["public_reference_request_throughput"]
        ),
        "qualification_boundary": "public example omits hardware/runtime, so this is not an apples-to-apples ranking",
    }


@app.function(
    image=image,
    gpu="H100!",
    cpu=8,
    memory=16384,
    timeout=1200,
    max_containers=1,
    scaledown_window=2,
)
def throughput_reference_qwen() -> dict[str, object]:
    import torch

    gpu = torch.cuda.get_device_properties(0)
    campaign = _run_public_reference_recipe(
        "compiled-throughput-public-reference", ["--performance-mode", "throughput"]
    )
    measured = campaign["aggregate"]
    return {
        "schema_version": "infercrane.modal-aiperf-public-reference/v1",
        "evidence_class": "real-gpu-end-to-end-public-workload-reproduction",
        "model": MODEL_ID,
        "model_revision": MODEL_REVISION,
        "runtime": "vllm==0.22.1",
        "benchmark": "aiperf==0.12.0",
        "gpu": gpu.name,
        "compute_capability": f"sm{gpu.major}{gpu.minor}",
        "campaign": campaign,
        "directional_ratio_vs_published_output_throughput": (
            measured["output_token_throughput"] / campaign["public_reference_output_token_throughput"]
        ),
        "directional_ratio_vs_published_request_throughput": (
            measured["request_throughput"] / campaign["public_reference_request_throughput"]
        ),
        "qualification_boundary": "public example omits hardware/runtime, so this is not an apples-to-apples ranking",
    }


def _trace_model_result(
    model_id: str,
    model_revision: str,
    model_dir: str,
    profiles: list[dict[str, object]],
) -> dict[str, object]:
    import torch

    gpu = torch.cuda.get_device_properties(0)
    return {
        "schema_version": "infercrane.modal-public-trace-model-screen/v1",
        "evidence_class": "public-trace-shaped-real-gpu-screening",
        "model": model_id,
        "model_revision": model_revision,
        "runtime": "vllm==0.22.1",
        "benchmark": "aiperf==0.12.0",
        "gpu_request": "L40S",
        "gpu": gpu.name,
        "compute_capability": f"sm{gpu.major}{gpu.minor}",
        "torch_version": torch.__version__,
        "campaigns": _run_trace_recipe(model_id, model_dir, profiles),
        "qualification_boundary": (
            "public trace token shapes and controlled concurrency only; semantic quality, "
            "customer arrival patterns, cost, and production replay remain required"
        ),
    }


@app.function(
    image=image,
    gpu="L40S",
    cpu=8,
    memory=16384,
    timeout=1800,
    max_containers=1,
    scaledown_window=2,
)
def qualify_trace_qwen06(profiles: list[dict[str, object]]) -> dict[str, object]:
    return _trace_model_result(MODEL_ID, MODEL_REVISION, MODEL_DIR, profiles)


@app.function(
    image=qwen17_image,
    gpu="L40S",
    cpu=8,
    memory=16384,
    timeout=1800,
    max_containers=1,
    scaledown_window=2,
)
def qualify_trace_qwen17(profiles: list[dict[str, object]]) -> dict[str, object]:
    return _trace_model_result(
        QWEN17_MODEL_ID, QWEN17_MODEL_REVISION, QWEN17_MODEL_DIR, profiles
    )


def _trace_model_comparisons(results: list[dict[str, object]]) -> list[dict[str, object]]:
    indexed: dict[tuple[str, str], dict[str, object]] = {}
    for result in results:
        for campaign in result["campaigns"]:
            indexed[(str(result["model"]), str(campaign["profile"]))] = campaign[
                "aggregate"
            ]
    comparisons = []
    for profile in sorted({key[1] for key in indexed}):
        small = indexed[(MODEL_ID, profile)]
        large = indexed[(QWEN17_MODEL_ID, profile)]
        comparisons.append(
            {
                "profile": profile,
                "qwen06_over_qwen17_output_throughput": (
                    small["output_token_throughput"]
                    / large["output_token_throughput"]
                ),
                "qwen06_over_qwen17_request_throughput": (
                    small["request_throughput"] / large["request_throughput"]
                ),
                "qwen06_over_qwen17_ttft_p95": (
                    small["ttft_p95_ms"] / large["ttft_p95_ms"]
                ),
                "qwen06_over_qwen17_tpot_p95": (
                    small["tpot_p95_ms"] / large["tpot_p95_ms"]
                ),
            }
        )
    return comparisons


@app.local_entrypoint()
def main(
    mode: str = "paired",
    profile_file: str = "",
    profile_names: str = "public-interactive,public-decode-heavy",
) -> None:
    if mode == "paired":
        result = qualify_qwen.remote()
    elif mode == "public-reference":
        result = public_reference_qwen.remote()
    elif mode == "throughput-reference":
        result = throughput_reference_qwen.remote()
    elif mode == "vllm-default":
        result = qualify_vllm_default_qwen.remote()
    elif mode == "sglang-stable":
        result = qualify_sglang_stable_qwen.remote()
    elif mode == "cross-engine":
        vllm_result = qualify_vllm_default_qwen.remote()
        sglang_result = qualify_sglang_stable_qwen.remote()
        result = {
            "schema_version": "infercrane.modal-cross-engine-campaign/v1",
            "evidence_class": "paired-real-gpu-end-to-end-screening",
            "model": MODEL_ID,
            "model_revision": MODEL_REVISION,
            "hardware_identity": vllm_result["gpu"],
            "workload": {"input_tokens": 128, "output_tokens": 64, "streaming": True, "concurrency_lanes": [1, 8, 32], "profile_runs": 3},
            "engines": [vllm_result, sglang_result],
            "comparisons": _engine_comparisons(vllm_result, sglang_result),
            "selection_boundary": "same model, hardware request, and AIPerf contract; screening only, not a public or production qualification claim",
        }
    elif mode == "trace-two-models":
        if not profile_file:
            raise ValueError("trace-two-models requires --profile-file")
        document = json.loads(Path(profile_file).read_text())
        selected_names = {
            name.strip() for name in profile_names.split(",") if name.strip()
        }
        profiles = [
            profile
            for profile in document["profiles"]
            if profile["name"] in selected_names
        ]
        if len(profiles) != len(selected_names):
            found = {profile["name"] for profile in profiles}
            raise ValueError(f"profile(s) not found: {sorted(selected_names - found)}")
        qwen06_result = qualify_trace_qwen06.remote(profiles)
        qwen17_result = qualify_trace_qwen17.remote(profiles)
        models = [qwen06_result, qwen17_result]
        result = {
            "schema_version": "infercrane.modal-public-trace-two-model-screen/v1",
            "evidence_class": "public-trace-shaped-real-gpu-screening",
            "workload_source": document["source"],
            "workload_sampling": document["sampling"],
            "models": models,
            "comparisons": _trace_model_comparisons(models),
            "selection_boundary": (
                "performance-only screening on the same L40S request and public trace shapes; "
                "model quality differs and is not measured"
            ),
        }
    else:
        raise ValueError(
            "mode must be paired, public-reference, throughput-reference, "
            "vllm-default, sglang-stable, cross-engine, or trace-two-models"
        )
    print("INFERCRANE_AIPERF_RESULT=" + json.dumps(result, sort_keys=True))

"""Budget-bounded end-to-end check of the mixture-of-experts pack on Modal.

Serves Qwen3-30B-A3B-Instruct-2507 in BF16 on one H100 from the pinned vLLM
0.22.1 image and measures, in order:

1. control: vLLM's defaults, whatever MoE backend it picks;
2. triton: --moe-backend triton, vLLM's own Triton experts;
3. kernel: --moe-backend triton with the pack's integration routing every
   covered call to the pack's reference kernel (testdata/correct.py);
4. kernel-small-batches: the same, but calls above 64 tokens go back to vLLM's
   Triton experts (a test-only wrapper, to see what batch-size dispatch gives);
5. control again, to bound drift.

Each arm replays the same synthetic decode-heavy load with AIPerf at
concurrency 1, 4 and 16 and runs InferCrane's built-in answer check (the
default suite, greedy, scored as token F1 against the first control). Kernel
arms must leave the integration's marker, or their numbers are not reported as
the kernel's.

Weights are downloaded once into a Modal volume on a CPU-only function. The GPU
function stops after 55 minutes and writes partial results to the volume after
every arm. The local entrypoint cancels each call after its limit plus five
minutes of wall clock, because a container that fails while starting is retried
without limit and never reaches the function timeout.
"""

from __future__ import annotations

import json
import math
import os
import re
import shutil
import subprocess
import time
import unicodedata
import urllib.request
from datetime import datetime, timezone
from pathlib import Path

import modal

APP_NAME = "infercrane-moe-kernel-e2e"
VLLM_IMAGE = "vllm/vllm-openai@sha256:953d3a06d5e64ab582985cd7401289d3abf2a2c14ef2158e9a84313daeec77d7"
MODEL_ID = "Qwen/Qwen3-30B-A3B-Instruct-2507"
MODEL_REVISION = "0d7cf23991f47feeb3a57ecb4c9cee8ea4a17bfe"
MODEL_DIR = "/weights/qwen3-30b-a3b-instruct-2507"
RESULTS_DIR = "/weights/results"
# Local paths, read only when the image is built. In the container this file
# sits at /root, so the walk up must not index past the filesystem root.
ROOT = Path(__file__).resolve().parent.parent.parent
PACK = ROOT / "tools" / "kernel-lab" / "packs" / "moe-experts"
SUITE = ROOT / "internal" / "qualitycheck" / "suites" / "infercrane-default-v1.json"
REMOTE_PACK = "/opt/infercrane/pack"
REMOTE_SUITE = "/opt/infercrane/quality-suite.json"
AIPERF = "/opt/aiperf/bin/aiperf"
PORT = 8000
GPU_TIMEOUT_SECONDS = 3300
DOWNLOAD_TIMEOUT_SECONDS = 1200
STARTUP_GRACE_SECONDS = 300  # queueing and container start, on top of each limit
SMALL_BATCH_TOKENS = 64
CONCURRENCY = ((1, 10), (4, 16), (16, 32))  # (concurrency, requests per profile run)
INPUT_TOKENS, OUTPUT_TOKENS = 512, 128
QUALITY_SEED = 17  # internal/qualitycheck.Seed

app = modal.App(APP_NAME)
weights = modal.Volume.from_name("infercrane-moe-e2e-weights", create_if_missing=True)
download_image = modal.Image.debian_slim(python_version="3.12").pip_install("huggingface_hub[hf_xet]==1.1.4")
image = (
    modal.Image.from_registry(VLLM_IMAGE, setup_dockerfile_commands=["RUN ln -sf /usr/bin/python3 /usr/local/bin/python"])
    .entrypoint([])
    # AIPerf gets its own environment so it cannot change a package vLLM loads.
    .run_commands("uv venv /opt/aiperf --python /usr/bin/python3.12 && uv pip install --python /opt/aiperf/bin/python aiperf==0.12.0")
    .add_local_file(str(SUITE), REMOTE_SUITE)
    .add_local_dir(str(PACK), REMOTE_PACK, ignore=["__pycache__", "*.pyc"])
)

# Test-only wrapper: the pack's kernel for small batches, vLLM's Triton experts
# (fused_experts, which does not go through TritonExperts.apply) above them.
SMALL_BATCH_WRAPPER = f'''
import importlib.util, os
from vllm.model_executor.layers.fused_moe.fused_moe import fused_experts
_spec = importlib.util.spec_from_file_location("pack_correct", "{REMOTE_PACK}/testdata/correct.py")
_pack = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_pack)
_marker = os.path.join(os.environ["INFERCRANE_KERNEL_STATE"], "small-batch")
_marked = [False]

def fused_moe_experts(hidden_states, w13, w2, topk_weights, topk_ids):
    if hidden_states.shape[0] <= {SMALL_BATCH_TOKENS}:
        if not _marked[0]:
            open(_marker, "w").close()
            _marked[0] = True
        return _pack.fused_moe_experts(hidden_states, w13, w2, topk_weights, topk_ids)
    return fused_experts(hidden_states, w13, w2, topk_weights, topk_ids)
'''

ARMS = (
    ("control", [], None),
    ("triton", ["--moe-backend", "triton"], None),
    ("kernel", ["--moe-backend", "triton"], "all"),
    ("kernel-small-batches", ["--moe-backend", "triton"], "small"),
    ("control-again", [], None),
)


@app.function(image=download_image, volumes={"/weights": weights}, cpu=4, memory=16384, timeout=DOWNLOAD_TIMEOUT_SECONDS, retries=0)
def download() -> dict[str, object]:
    from huggingface_hub import snapshot_download

    marker = Path(MODEL_DIR) / ".complete"
    if marker.exists() and marker.read_text() == MODEL_REVISION:
        return {"downloaded": False}
    started = time.monotonic()
    snapshot_download(repo_id=MODEL_ID, revision=MODEL_REVISION, local_dir=MODEL_DIR, allow_patterns=["*.json", "*.safetensors", "*.txt", "*.model", "*.jinja"])
    marker.write_text(MODEL_REVISION)
    weights.commit()
    size = sum(path.stat().st_size for path in Path(MODEL_DIR).rglob("*") if path.is_file())
    return {"downloaded": True, "seconds": round(time.monotonic() - started), "bytes": size}


def _clean_env() -> dict[str, str]:
    env = {key: value for key, value in os.environ.items() if key != "PYTHONPATH"}
    # Same for every arm; DeepGEMM is irrelevant to BF16 and vLLM 0.22.1 can
    # mis-detect it on H100 (see qwen_aiperf_modal.py).
    env["VLLM_USE_DEEP_GEMM"] = "0"
    return env


def _tail(text: str, lines: int = 30) -> str:
    return "\n".join(text.strip().splitlines()[-lines:])


def _wait_for_server(process: subprocess.Popen, log_path: Path, timeout_seconds: int = 900) -> None:
    deadline = time.monotonic() + timeout_seconds
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise RuntimeError(f"server exited with {process.returncode}:\n{_tail(log_path.read_text(errors='replace'))}")
        try:
            with urllib.request.urlopen(f"http://127.0.0.1:{PORT}/v1/models", timeout=2) as response:
                if response.status == 200:
                    return
        except Exception:
            time.sleep(2)
    raise TimeoutError(f"server not ready:\n{_tail(log_path.read_text(errors='replace'))}")


def _chat(messages: list[dict[str, str]], max_tokens: int) -> str:
    body = json.dumps(
        {
            "model": MODEL_ID, "messages": messages, "temperature": 0, "top_p": 1, "seed": QUALITY_SEED,
            "max_tokens": max_tokens, "n": 1, "stream": False, "chat_template_kwargs": {"enable_thinking": False},
        }
    ).encode()
    request = urllib.request.Request(f"http://127.0.0.1:{PORT}/v1/chat/completions", data=body, headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(request, timeout=180) as response:
        return json.loads(response.read())["choices"][0]["message"]["content"] or ""


# Mirrors internal/qualitycheck/score.go: token F1 after removing reasoning,
# lowercasing, and splitting on anything that is not a letter or digit.
_THINK = re.compile(r"<think>.*?(</think>|$)", re.S)


def _tokens(answer: str) -> list[str]:
    answer = _THINK.sub(" ", answer)
    if "</think>" in answer:
        answer = answer[answer.rindex("</think>") + len("</think>"):]
    words, current = [], []
    for char in answer.lower():
        category = unicodedata.category(char)
        if category[0] == "L" or category == "Nd":  # Go: unicode.IsLetter, unicode.IsDigit
            current.append(char)
        elif current:
            words.append("".join(current))
            current = []
    if current:
        words.append("".join(current))
    return words


def _agreement(reference: str, candidate: str) -> float:
    left, right = _tokens(reference), _tokens(candidate)
    if not left and not right:
        return 1.0
    if not left or not right:
        return 0.0
    counts: dict[str, int] = {}
    for token in left:
        counts[token] = counts.get(token, 0) + 1
    common = 0
    for token in right:
        if counts.get(token, 0) > 0:
            counts[token] -= 1
            common += 1
    if not common:
        return 0.0
    precision, recall = common / len(right), common / len(left)
    return 2 * precision * recall / (precision + recall)


def _answers() -> list[str]:
    suite = json.loads(Path(REMOTE_SUITE).read_text())
    answers = []
    for prompt in suite["prompts"]:
        messages = ([{"role": "system", "content": prompt["system"]}] if prompt.get("system") else []) + [{"role": "user", "content": prompt["prompt"]}]
        answers.append(_chat(messages, int(suite.get("max_tokens") or 64)))
    return answers


def _percentile(values: list[float], percentile: float) -> float | None:
    if not values:
        return None
    ordered = sorted(values)
    return ordered[max(0, min(len(ordered) - 1, math.ceil(percentile * len(ordered)) - 1))]


def _ms(metric: dict) -> float:
    value, unit = float(metric["value"]), str(metric.get("unit", "ms")).strip().lower()
    return {"": 1, "ms": 1, "s": 1000, "sec": 1000, "us": 1e-3, "µs": 1e-3, "ns": 1e-6}.get(unit, 1) * value


def _parse_records(path: Path) -> dict[str, object]:
    ttft, itl, out_tokens, starts, ends, failed, succeeded = [], [], 0.0, [], [], 0, 0
    for line in path.read_text().splitlines():
        row = json.loads(line)
        metadata = row.get("metadata", {})
        if metadata.get("benchmark_phase") not in (None, "", "profiling"):
            continue
        starts.append(int(metadata.get("request_start_ns") or 0))
        ends.append(int(metadata.get("request_end_ns") or 0))
        if row.get("error") is not None:
            failed += 1
            continue
        succeeded += 1
        metrics = row.get("metrics", {})
        if "time_to_first_token" in metrics:
            ttft.append(_ms(metrics["time_to_first_token"]))
        token_metric = metrics.get("inter_token_latency") or metrics.get("time_per_output_token")
        if token_metric:
            itl.append(_ms(token_metric))
        if "output_token_count" in metrics:
            out_tokens += float(metrics["output_token_count"]["value"])
    starts = [value for value in starts if value]
    duration = (max(ends) - min(starts)) / 1e9 if starts and ends and max(ends) > min(starts) else 0
    return {
        "succeeded": succeeded, "failed": failed, "duration_seconds": duration,
        "output_token_throughput": out_tokens / duration if duration else None,
        "ttft_p50_ms": _percentile(ttft, 0.5), "ttft_p95_ms": _percentile(ttft, 0.95),
        "itl_p50_ms": _percentile(itl, 0.5), "itl_p95_ms": _percentile(itl, 0.95),
    }


def _aiperf(arm: str, concurrency: int, requests: int) -> dict[str, object]:
    artifacts = Path(f"/tmp/aiperf-{arm}-{concurrency}")
    shutil.rmtree(artifacts, ignore_errors=True)
    command = [
        AIPERF, "profile", "--model", MODEL_ID, "--tokenizer", MODEL_DIR, "--url", f"http://127.0.0.1:{PORT}",
        "--endpoint-type", "chat", "--streaming", "--request-timeout-seconds", "120",
        "--request-count", str(requests), "--concurrency", str(concurrency), "--random-seed", "17",
        "--synthetic-input-tokens-mean", str(INPUT_TOKENS), "--synthetic-input-tokens-stddev", "0",
        "--output-tokens-mean", str(OUTPUT_TOKENS), "--output-tokens-stddev", "0",
        "--extra-inputs", '{"ignore_eos":true}', "--num-profile-runs", "3",
        "--warmup-request-count", str(max(4, concurrency)), "--profile-run-cooldown-seconds", "1",
        "--ui", "none", "--export-level", "records", "--artifact-dir", str(artifacts),
        "--profile-export-prefix", "infercrane", "--no-auto-plot", "--no-gpu-telemetry", "--no-server-metrics",
    ]
    completed = subprocess.run(command, capture_output=True, text=True, timeout=420, env=_clean_env())
    if completed.returncode:
        raise RuntimeError(f"AIPerf failed ({completed.returncode}):\n{_tail(completed.stdout)}\n{_tail(completed.stderr)}")
    files = sorted(artifacts.rglob("infercrane.jsonl"))
    nested = [path for path in files if path.parent != artifacts]
    runs = [_parse_records(path) for path in (nested or files)]
    if not runs:
        raise RuntimeError(f"AIPerf wrote no records under {artifacts}")

    def mean(key: str) -> float | None:
        values = [float(run[key]) for run in runs if run[key] is not None]
        return sum(values) / len(values) if values else None

    return {
        "concurrency": concurrency, "requests_per_run": requests, "profile_runs": len(runs),
        "failed": sum(int(run["failed"]) for run in runs),
        **{key: mean(key) for key in ("output_token_throughput", "ttft_p50_ms", "ttft_p95_ms", "itl_p50_ms", "itl_p95_ms")},
        "runs": runs,
    }


def _run_arm(name: str, extra_args: list[str], kernel: str | None, reference_answers: list[str] | None) -> tuple[dict[str, object], list[str] | None]:
    log_path = Path(f"/tmp/vllm-{name}.log")
    env = _clean_env()
    state = Path(f"/tmp/kernel-state-{name}")
    if kernel:
        state.mkdir(parents=True, exist_ok=True)
        source = f"{REMOTE_PACK}/testdata/correct.py"
        if kernel == "small":
            source = f"/tmp/kernel-{name}.py"
            Path(source).write_text(SMALL_BATCH_WRAPPER, encoding="utf-8")
        env.update({"PYTHONPATH": f"{REMOTE_PACK}/integration", "INFERCRANE_KERNEL_SOURCE": source, "INFERCRANE_KERNEL_STATE": str(state)})
    command = [
        "vllm", "serve", MODEL_DIR, "--served-model-name", MODEL_ID, "--dtype", "bfloat16", "--host", "127.0.0.1", "--port", str(PORT),
        "--max-model-len", "4096", "--max-num-seqs", "32", "--gpu-memory-utilization", "0.92", "--generation-config", "vllm",
        "--no-enable-log-requests", *extra_args,
    ]
    arm: dict[str, object] = {"arm": name, "server_args": extra_args, "kernel": kernel}
    answers = None
    started = time.monotonic()
    with log_path.open("w") as log:
        process = subprocess.Popen(command, stdout=log, stderr=subprocess.STDOUT, env=env)
    try:
        _wait_for_server(process, log_path)
        arm["startup_seconds"] = round(time.monotonic() - started, 1)
        _chat([{"role": "user", "content": "Reply with the word ready."}], 8)
        answers = _answers()
        if reference_answers is not None:
            scores = [_agreement(reference, answer) for reference, answer in zip(reference_answers, answers)]
            arm["answer_check"] = {"score": sum(scores) / len(scores), "prompts": len(scores), "exact": sum(1 for score in scores if score == 1.0)}
        arm["load"] = [_aiperf(name, concurrency, requests) for concurrency, requests in CONCURRENCY]
    except Exception as error:  # Keep the other arms; record why this one stopped.
        arm["error"] = f"{type(error).__name__}: {str(error)[-4000:]}"
    finally:
        process.terminate()
        try:
            process.wait(timeout=60)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=20)
        log = log_path.read_text(errors="replace")
        arm["moe_log_lines"] = sorted({line.strip()[-240:] for line in log.splitlines() if "moe" in line.lower() and ("backend" in line.lower() or "using" in line.lower())})[:8]
        if kernel:
            arm["kernel_invoked"] = (state / "invoked").exists()
            if kernel == "small":
                arm["small_batch_path_invoked"] = (state / "small-batch").exists()
        arm["seconds"] = round(time.monotonic() - started, 1)
        time.sleep(5)
    return arm, answers


def _environment() -> dict[str, object]:
    probe = subprocess.run(
        ["python3", "-c", "import importlib.metadata as m, json, torch; p = torch.cuda.get_device_properties(0); "
         "print(json.dumps({'torch': torch.__version__, 'triton': m.version('triton'), 'vllm': m.version('vllm'), 'gpu': p.name, 'compute_capability': f'sm{p.major}{p.minor}'}))"],
        capture_output=True, text=True, env=_clean_env(),
    )
    smi = subprocess.run(["nvidia-smi", "--query-gpu=name,driver_version,memory.total", "--format=csv,noheader,nounits"], capture_output=True, text=True)
    aiperf = subprocess.run(["/opt/aiperf/bin/python", "-c", "import importlib.metadata as m; print(m.version('aiperf'))"], capture_output=True, text=True)
    return {"software": json.loads(probe.stdout) if probe.returncode == 0 else {"error": _tail(probe.stderr)}, "nvidia_smi": smi.stdout.strip(), "aiperf": aiperf.stdout.strip()}


@app.function(image=image, gpu="H100!", cpu=8, memory=65536, volumes={"/weights": weights}, timeout=GPU_TIMEOUT_SECONDS, retries=0, max_containers=1, scaledown_window=2)
def compare(run_id: str) -> dict[str, object]:
    started = time.monotonic()
    result: dict[str, object] = {
        "schema_version": "infercrane.kernel-e2e-check/v1",
        "evidence_class": "real-gpu-serving-comparison",
        "run_id": run_id,
        "model": {"id": MODEL_ID, "revision": MODEL_REVISION, "dtype": "bfloat16"},
        "runtime_image": VLLM_IMAGE,
        "hardware": {"gpu_request": "H100!", **_environment()},
        "workload": {"generator": "aiperf synthetic", "input_tokens": INPUT_TOKENS, "output_tokens": OUTPUT_TOKENS, "ignore_eos": True, "streaming": True, "concurrency": [c for c, _ in CONCURRENCY], "profile_runs": 3},
        "answer_check": {"suite": "infercrane-default@1", "scoring": "token F1 against the first control, greedy, seed 17, reasoning off", "watch_below": 0.95, "block_below": 0.90},
        "kernel_source": "tools/kernel-lab/packs/moe-experts/testdata/correct.py (the pack's hand-written reference kernel)",
        "small_batch_tokens": SMALL_BATCH_TOKENS,
        "arms": [],
    }
    results_path = Path(RESULTS_DIR) / f"{run_id}.json"
    results_path.parent.mkdir(parents=True, exist_ok=True)
    reference = None
    for name, args, kernel in ARMS:
        arm, answers = _run_arm(name, args, kernel, reference)
        if name == "control":
            reference = answers
        result["arms"].append(arm)
        result["gpu_seconds"] = round(time.monotonic() - started, 1)
        results_path.write_text(json.dumps(result, indent=2, sort_keys=True))
        weights.commit()
        if name == "control" and reference is None:
            break  # Without a control there is nothing to compare against.
    result["recorded_at"] = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    results_path.write_text(json.dumps(result, indent=2, sort_keys=True))
    weights.commit()
    return result


def _bounded(function: modal.Function, seconds: int, *args: object) -> dict[str, object]:
    """Waits at most `seconds` of wall clock, then cancels the call and its
    containers. The function timeout alone is not a cap: it counts only time
    spent running, and Modal retries a container that fails while starting
    without limit, retries=0 or not."""
    call = function.spawn(*args)
    try:
        return call.get(timeout=seconds)
    except (TimeoutError, modal.exception.TimeoutError):
        call.cancel(terminate_containers=True)
        raise SystemExit(f"{function.object_id}: no result after {seconds} s of wall clock; cancelled")


@app.local_entrypoint()
def main(out: str = "") -> None:
    print("download:", json.dumps(_bounded(download, DOWNLOAD_TIMEOUT_SECONDS + STARTUP_GRACE_SECONDS)))
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    print("run id:", run_id)
    result = _bounded(compare, GPU_TIMEOUT_SECONDS + STARTUP_GRACE_SECONDS, run_id)
    text = json.dumps(result, indent=2, sort_keys=True) + "\n"
    if out:
        Path(out).write_text(text, encoding="utf-8")
    print(text)

"""Development-only remote GPU servers on Modal for the Modal GPU PoC.

Modal is not an InferCrane provider. This app only gives the local PoC's
model server pods (internal/testtools/modal-gpu-proxy) a real GPU behind them:
one GPU container per lease, running the run's pinned vLLM image with exactly
the arguments the pod was started with, reachable through a Modal tunnel. The
engine image and the GPU type are the run's (models.ENGINE, models.ACCELERATOR).

Every container stops when the first of these happens:
  - the broker sets the lease's stop flag (the pod was deleted),
  - the lease is not renewed for LEASE_SECONDS (the pod, broker, or laptop died),
  - the broker's budget deadline for the lease passes,
  - the function timeout (the hard cap) ends it,
  - the broker's ephemeral app stops (the broker process exited).

The broker (broker.py) imports this module and runs the app; nothing here
reads or prints a Modal credential.
"""

from __future__ import annotations

import json
import os
import re
import subprocess
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

import modal

sys.path.insert(0, str(Path(__file__).resolve().parent))
from models import ACCELERATOR, ACCELERATOR_VARIABLE, ENGINE, ENGINE_VARIABLE, MODELS, QWEN3_8B, check_args, speculates  # noqa: E402,F401 - check_args is the broker's

APP_NAME = "infercrane-poc-modal-gpu"
VLLM_IMAGE, VLLM_VERSION = ENGINE.image, ENGINE.version


PORT = 8000
GPU = ACCELERATOR.modal_gpu
GPU_CPU, GPU_MEMORY_MIB = 4.0, 24576
# Hard cap per container. A campaign keeps every setup serving until it ranks them, and a
# gain is measured again on fresh servers, so a 27B campaign outlasts the 90 minutes an
# 8B one needed.
GPU_TIMEOUT_SECONDS = 150 * 60
# The current setup, two setups to try and the copy of the current setup.
MAX_GPUS = 4
LEASE_SECONDS = 180
STARTUP_SECONDS = 15 * 60

app = modal.App(APP_NAME)
weights = modal.Volume.from_name("infercrane-poc-modal-weights", create_if_missing=True)
leases = modal.Dict.from_name("infercrane-poc-modal-leases", create_if_missing=True)
download_image = modal.Image.debian_slim(python_version="3.12").pip_install("huggingface_hub[hf_xet]==1.1.4").add_local_python_source("models")
# The image carries the run's engine and GPU type, so the module resolves the same ones
# inside the container as on the laptop.
image = (modal.Image.from_registry(VLLM_IMAGE, setup_dockerfile_commands=["RUN ln -sf /usr/bin/python3 /usr/local/bin/python"]).entrypoint([])
         .env({ENGINE_VARIABLE: ENGINE.key, ACCELERATOR_VARIABLE: ACCELERATOR.key}).add_local_python_source("models"))


def server_command(args: list[str], model: str) -> list[str]:
    return ["vllm", "serve", MODELS[model].dir, "--host", "0.0.0.0", "--port", str(PORT), *args]


@app.function(image=download_image, volumes={"/weights": weights}, cpu=2, memory=8192, timeout=1800, retries=0)
def download(model_id: str) -> dict[str, object]:
    """Stages the pinned weights on the volume (CPU only, no GPU time)."""
    from huggingface_hub import snapshot_download

    model = MODELS[model_id]
    marker = Path(model.dir) / ".complete"
    if marker.exists() and marker.read_text() == model.revision:
        return {"downloaded": False, "path": model.dir}
    started = time.monotonic()
    snapshot_download(repo_id=model.id, revision=model.revision, local_dir=model.dir,
                      allow_patterns=["*.json", "*.safetensors", "*.txt", "*.model", "*.jinja"])
    marker.write_text(model.revision)
    weights.commit()
    files = sorted(p for p in Path(model.dir).rglob("*") if p.is_file() and ".cache" not in p.parts)
    return {"downloaded": True, "seconds": round(time.monotonic() - started), "path": model.dir,
            "bytes": sum(p.stat().st_size for p in files), "files": [p.name for p in files]}


# A stand-in server for the dry run and the stand-in campaign: same lifecycle and tunnel,
# no GPU and no model. It answers with filler words at a fixed pace, reports token usage,
# and exports the vLLM metric families the control plane and AIPerf read, so a whole
# campaign can be rehearsed for cents. A stand-in started with --speculative-config
# writes two words a step and counts drafted and accepted tokens, so the path that
# carries speculation evidence is exercised. Nothing it reports is a measurement.
FAKE_SERVER = r'''
import json, os, sys, threading, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
port, key, model = int(sys.argv[1]), sys.argv[2], sys.argv[3]
speculating = "--speculative-config" in sys.argv[4:]
STEP, PER_STEP, MAX_WORDS = 0.05, (2 if speculating else 1), 64
BUCKETS = (0.01, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0, 10.0)
lock = threading.Lock()
count = {"running": 0, "requests": 0, "prompt": 0, "generation": 0, "drafts": 0, "draft_tokens": 0, "accepted_tokens": 0}
histograms = {name: {"buckets": [0] * len(BUCKETS), "sum": 0.0, "count": 0} for name in
              ("request_queue_time_seconds", "request_prefill_time_seconds", "request_decode_time_seconds", "time_to_first_token_seconds", "e2e_request_latency_seconds")}
def observe(name, value):
    row = histograms[name]
    row["sum"] += value; row["count"] += 1
    for index, bound in enumerate(BUCKETS):
        if value <= bound: row["buckets"][index] += 1
def metrics():
    label = '{model_name="%s"}' % model
    lines = []
    with lock:
        for name, kind, value in (("num_requests_running", "gauge", count["running"]), ("num_requests_waiting", "gauge", 0), ("kv_cache_usage_perc", "gauge", 0.0),
                                  ("prompt_tokens_total", "counter", count["prompt"]), ("generation_tokens_total", "counter", count["generation"]),
                                  ("request_success_total", "counter", count["requests"]), ("num_preemptions_total", "counter", 0),
                                  ("prefix_cache_queries_total", "counter", count["prompt"]), ("prefix_cache_hits_total", "counter", 0)):
            lines += ["# TYPE vllm:%s %s" % (name, kind), "vllm:%s%s %s" % (name, label, value)]
        if speculating:
            for name in ("drafts", "draft_tokens", "accepted_tokens"):
                lines += ["# TYPE vllm:spec_decode_num_%s_total counter" % name, "vllm:spec_decode_num_%s_total%s %s" % (name, label, count[name])]
        for name, row in histograms.items():
            lines.append("# TYPE vllm:%s histogram" % name)
            for bound, total in zip(BUCKETS, row["buckets"]):
                lines.append('vllm:%s_bucket{model_name="%s",le="%s"} %s' % (name, model, bound, total))
            lines += ['vllm:%s_bucket{model_name="%s",le="+Inf"} %s' % (name, model, row["count"]), "vllm:%s_sum%s %s" % (name, label, row["sum"]), "vllm:%s_count%s %s" % (name, label, row["count"])]
    return "\n".join(lines) + "\n"
class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def log_message(self, *a): pass
    def send(self, code, body, kind="application/json"):
        data = body.encode()
        self.send_response(code); self.send_header("Content-Type", kind); self.send_header("Content-Length", str(len(data))); self.end_headers(); self.wfile.write(data)
    def do_GET(self):
        if self.path == "/health": return self.send(200, "")
        if self.path == "/metrics": return self.send(200, metrics(), "text/plain; version=0.0.4")
        if self.path == "/v1/models":
            if self.headers.get("Authorization") != "Bearer " + key: return self.send(401, "{}")
            return self.send(200, json.dumps({"object": "list", "data": [{"id": model, "object": "model"}]}))
        self.send(404, "{}")
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))) or b"{}")
        if self.headers.get("Authorization") != "Bearer " + key: return self.send(401, "{}")
        text = " ".join(str(message.get("content") or "") for message in body.get("messages") or [])
        prompt = max(1, len(text.split()) * 4 // 3)
        limit = body.get("max_completion_tokens") or body.get("max_tokens") or 16
        words = max(1, min(int(limit), MAX_WORDS))
        started = time.monotonic()
        with lock: count["running"] += 1
        try:
            usage = {"prompt_tokens": prompt, "completion_tokens": words, "total_tokens": prompt + words}
            time.sleep(STEP)  # the prompt
            # A rehearsal can ask for a silent wait before the answer, to see what the path
            # to the server does with a request that sends nothing back for a while.
            time.sleep(min(float(body.get("stand_in_hold_seconds") or 0), 120.0))
            first = time.monotonic() - started
            if body.get("stream"):
                self.send_response(200); self.send_header("Content-Type", "text/event-stream"); self.send_header("Transfer-Encoding", "chunked"); self.end_headers()
                def chunk(payload):
                    data = ("data: " + payload + "\n\n").encode(); self.wfile.write(b"%x\r\n%s\r\n" % (len(data), data)); self.wfile.flush()
                base = {"id": "stand-in", "object": "chat.completion.chunk", "model": model}
                for index in range(words):
                    if index % PER_STEP == 0 and index: time.sleep(STEP)
                    chunk(json.dumps(dict(base, choices=[{"index": 0, "delta": {"role": "assistant", "content": "dry "}, "finish_reason": None}])))
                chunk(json.dumps(dict(base, choices=[{"index": 0, "delta": {}, "finish_reason": "length" if words < int(limit) else "stop"}])))
                if (body.get("stream_options") or {}).get("include_usage"): chunk(json.dumps(dict(base, choices=[], usage=usage)))
                chunk("[DONE]"); self.wfile.write(b"0\r\n\r\n"); self.wfile.flush()
            else:
                time.sleep(STEP * (words // PER_STEP))
                self.send(200, json.dumps({"id": "stand-in", "object": "chat.completion", "model": model, "usage": usage,
                                           "choices": [{"index": 0, "message": {"role": "assistant", "content": "dry " * words}, "finish_reason": "stop"}]}))
        finally:
            elapsed = time.monotonic() - started
            with lock:
                count["running"] -= 1; count["requests"] += 1; count["prompt"] += prompt; count["generation"] += words
                if speculating:
                    steps = (words + 1) // 2
                    count["drafts"] += steps; count["draft_tokens"] += 3 * steps; count["accepted_tokens"] += words - steps
                observe("request_queue_time_seconds", 0.0); observe("request_prefill_time_seconds", first); observe("time_to_first_token_seconds", first)
                observe("request_decode_time_seconds", max(0.0, elapsed - first)); observe("e2e_request_latency_seconds", elapsed)
ThreadingHTTPServer((os.environ.get("STAND_IN_BIND", "0.0.0.0"), port), H).serve_forever()
'''


def _publish(lease_id: str, values: dict) -> dict:
    current = leases.get(f"st:{lease_id}", {}) or {}
    current.update(values)
    leases.put(f"st:{lease_id}", current)
    return current


def _tail(path: Path, lines: int = 40) -> str:
    try:
        return "\n".join(path.read_text(errors="replace").strip().splitlines()[-lines:])
    except OSError:
        return ""


def _gpu_facts() -> dict:
    """The host facts that can make one H100 serve faster than another: GPU model and
    memory, NVIDIA driver, and the CUDA version the driver supports."""
    facts = {"name": "unknown", "memory": "unknown", "driver": "unknown", "cuda": "unknown"}
    probe = subprocess.run(["nvidia-smi", "--query-gpu=name,memory.total,driver_version", "--format=csv,noheader"], capture_output=True, text=True)
    if probe.returncode == 0 and probe.stdout.strip():
        parts = [part.strip() for part in probe.stdout.strip().splitlines()[0].split(",")]
        if len(parts) == 3:
            facts["name"], facts["memory"], facts["driver"] = parts
    header = subprocess.run(["nvidia-smi"], capture_output=True, text=True)
    match = re.search(r"CUDA Version:\s*([0-9.]+)", header.stdout or "")
    if match:
        facts["cuda"] = match.group(1)
    return facts


def driver_mismatch(facts: dict, control: dict) -> str:
    """Why this host must not serve the lease, or "" when it may. A run compares setups
    across separate machines; a different NVIDIA driver alone can change speed, so every
    setup of a run is held to the driver the broker pinned."""
    required = control.get("required_driver")
    if not required:
        return ""
    if facts.get("driver") != required:
        return f"NVIDIA driver {facts.get('driver')} differs from the run's pinned driver {required}"
    return ""


def _serve(lease_id: str, args: list[str], api_key: str, model: str, dry: bool) -> dict:
    started = time.time()
    if (leases.get(f"st:{lease_id}", {}) or {}).get("container_started_at"):
        # Modal restarts a preempted Function on the same input: another machine, behind
        # another tunnel address. A setup of a campaign must not move to another machine
        # unnoticed, so the lease ends here. The pod's proxy sees the ended lease, exits,
        # and asks for a new server; the control plane sees the replica go away and come
        # back, and judges the measurement by its own rules for a replaced server.
        return _publish(lease_id, {"state": "failed", "reason": "preempted", "stopped_at": started,
                                   "error": "Modal preempted the container and started the lease again on another machine; the lease ends so the pod asks for a new server"})
    _publish(lease_id, {"state": "starting", "container_started_at": started, "dry": dry, "args": args, "runtime_image": VLLM_IMAGE})
    try:
        check_args(args, model)
    except ValueError as error:
        return _publish(lease_id, {"state": "failed", "error": str(error), "stopped_at": time.time()})
    server_env = MODELS[model].env
    _publish(lease_id, {"server_env": server_env, "model_revision": MODELS[model].revision})
    if not dry:
        facts = _gpu_facts()
        _publish(lease_id, {"gpu": f"{facts['name']}, {facts['memory']}, driver {facts['driver']}, CUDA {facts['cuda']}", "gpu_facts": facts,
                            "vllm_version": VLLM_VERSION})
        mismatch = driver_mismatch(facts, leases.get(f"ctl:{lease_id}", {}) or {})
        if mismatch:
            # Checked before vLLM starts, so a wrong host costs seconds, not a model load.
            return _publish(lease_id, {"state": "failed", "reason": "driver_mismatch", "error": mismatch, "stopped_at": time.time()})
    log_path = Path("/tmp/server.log")
    env = dict(os.environ, **server_env, VLLM_API_KEY=api_key)
    command = ["python3", "-c", FAKE_SERVER, str(PORT), api_key, model, *args] if dry else server_command(args, model)
    with log_path.open("w") as log:
        process = subprocess.Popen(command, stdout=log, stderr=subprocess.STDOUT, env=env)
    reason, error = "", ""
    try:
        deadline = time.monotonic() + STARTUP_SECONDS
        while True:
            if process.poll() is not None:
                raise RuntimeError(f"server exited with {process.returncode} while starting:\n{_tail(log_path)}")
            if time.monotonic() > deadline:
                raise TimeoutError(f"server not ready after {STARTUP_SECONDS} s:\n{_tail(log_path)}")
            if _stop_reason(lease_id, started):
                raise RuntimeError("lease ended while the server was starting")
            try:
                with urllib.request.urlopen(f"http://127.0.0.1:{PORT}/health", timeout=2) as response:
                    if response.status == 200:
                        break
            except OSError:
                pass
            time.sleep(2)
        ready = time.time()
        with modal.forward(PORT) as tunnel:
            _publish(lease_id, {"state": "ready", "url": tunnel.url, "ready_at": ready, "startup_seconds": round(ready - started, 1)})
            unreachable_since = None
            while True:
                if process.poll() is not None:
                    raise RuntimeError(f"server exited with {process.returncode} while serving:\n{_tail(log_path)}")
                try:
                    reason = _stop_reason(lease_id, started)
                    unreachable_since = None
                except Exception:  # noqa: BLE001 - Dict briefly unavailable; fail safe below
                    unreachable_since = unreachable_since or time.monotonic()
                    if time.monotonic() - unreachable_since > LEASE_SECONDS:
                        reason = "lease_store_unreachable"
                if reason:
                    break
                time.sleep(5)
    except Exception as failure:  # noqa: BLE001 - recorded on the lease, never retried
        error = f"{type(failure).__name__}: {str(failure)[-3000:]}"
    finally:
        process.terminate()
        try:
            process.wait(timeout=30)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=10)
    final = {"state": "failed" if error else "stopped", "stopped_at": time.time(), "reason": reason or "error", "error": error}
    if error:
        final["log_tail"] = _tail(log_path)
    return _publish(lease_id, final)


def _stop_reason(lease_id: str, started: float) -> str:
    control = leases.get(f"ctl:{lease_id}", {}) or {}
    now = time.time()
    if control.get("stop"):
        return "released"
    if now > float(control.get("expires_at", started + LEASE_SECONDS)):
        return "lease_expired"
    if now > float(control.get("deadline", started + GPU_TIMEOUT_SECONDS)):
        return "budget_deadline"
    return ""


@app.function(image=image, gpu=GPU, cpu=GPU_CPU, memory=GPU_MEMORY_MIB, volumes={"/weights": weights}, timeout=GPU_TIMEOUT_SECONDS,
              retries=0, max_containers=MAX_GPUS, scaledown_window=2)
def serve_gpu(lease_id: str, args: list[str], api_key: str, model: str) -> dict:
    return _serve(lease_id, args, api_key, model, dry=False)


# The stand-in. One lease for `poc_modal.py dry`, or a whole stand-in campaign: as many
# servers at once as a real run, each for as long as a campaign keeps a setup serving.
# Non-preemptible (Modal allows that for CPU Functions only, at three times the CPU price)
# and with a whole core, so a rehearsal's scripted pace is not disturbed by Modal moving the
# container or throttling it. A GPU Function cannot be made non-preemptible; _serve ends
# the lease when that happens.
DRY_CPU, DRY_MEMORY_MIB = 1.0, 1024


@app.function(image=image, cpu=DRY_CPU, memory=DRY_MEMORY_MIB, timeout=GPU_TIMEOUT_SECONDS, retries=0, max_containers=MAX_GPUS, scaledown_window=2, nonpreemptible=True)
def serve_dry(lease_id: str, args: list[str], api_key: str, model: str) -> dict:
    return _serve(lease_id, args, api_key, model, dry=True)


# The server has STARTUP_SECONDS to come up and the requests take about a minute.
CHECK_SECONDS = 18 * 60
# Families of vLLM's /metrics the control plane's engine scrape reads (docs/features/telemetry.md).
ENGINE_FAMILIES = ("vllm:num_requests_running", "vllm:num_requests_waiting", "vllm:kv_cache_usage_perc", "vllm:request_queue_time_seconds",
                   "vllm:request_prefill_time_seconds", "vllm:request_decode_time_seconds", "vllm:time_to_first_token_seconds",
                   "vllm:e2e_request_latency_seconds", "vllm:num_preemptions", "vllm:prefix_cache_queries")
# The counters speculation acceptance is read from. The publisher's page says to read them
# before trusting a speed: throughput alone cannot tell a working drafter from one that
# loaded and was ignored.
SPECULATION_COUNTERS = ("vllm:spec_decode_num_drafts", "vllm:spec_decode_num_draft_tokens", "vllm:spec_decode_num_accepted_tokens")
CHECK_SETUPS = ("current", "speculation")


def speculation_counters(metrics: str) -> dict:
    """Totals of the speculation counters in a /metrics page, summed over label sets;
    a counter the server does not export is absent, never zero."""
    totals: dict = {}
    for line in metrics.splitlines():
        if line.startswith("#") or " " not in line:
            continue
        name, value = line.rsplit(" ", 1)
        family = name.split("{", 1)[0].removesuffix("_total")
        if family in SPECULATION_COUNTERS:
            try:
                totals[family] = totals.get(family, 0.0) + float(value)
            except ValueError:
                continue
    return totals


def _call(path: str, key: str | None, body: dict | None = None) -> tuple[int, str]:
    request = urllib.request.Request(f"http://127.0.0.1:{PORT}{path}", data=json.dumps(body).encode() if body else None, method="POST" if body else "GET")
    request.add_header("Content-Type", "application/json")
    if key:
        request.add_header("Authorization", f"Bearer {key}")
    try:
        with urllib.request.urlopen(request, timeout=120) as response:
            return response.status, response.read().decode(errors="replace")
    except urllib.error.HTTPError as error:
        return error.code, error.read().decode(errors="replace")


def _healthy() -> bool:
    try:
        return _call("/health", None)[0] == 200
    except OSError:
        return False


@app.function(image=image, gpu=GPU, cpu=GPU_CPU, memory=GPU_MEMORY_MIB, volumes={"/weights": weights}, timeout=CHECK_SECONDS, retries=0,
              single_use_containers=True)
def check_gpu(model_id: str, setup: str = "current") -> dict:
    """Starts one setup of the model once and checks what a run relies on: the server
    starts, refuses a request without its key, answers one with it, calls a tool, and
    exports the vLLM metrics the control plane reads. setup is "current" (the publisher's
    command on the run's engine) or "speculation" (that command with the publisher's
    documented speculation option), which must also draft and accept tokens."""
    import secrets

    model, started = MODELS[model_id], time.monotonic()
    args = ["--served-model-name", model.id, *(model.speculation_args if setup == "speculation" else model.current_args)]
    result: dict = {"model": model.id, "revision": model.revision, "setup": setup, "args": args, "env": model.env, "engine": ENGINE.key, "runtime_image": VLLM_IMAGE,
                    "gpu_type": ACCELERATOR.key, "gpu_facts": _gpu_facts()}
    key, log_path = secrets.token_hex(16), Path("/tmp/server.log")
    with log_path.open("w") as log:
        process = subprocess.Popen(server_command(args, model.id), stdout=log, stderr=subprocess.STDOUT, env=dict(os.environ, **model.env, VLLM_API_KEY=key))
    try:
        while process.poll() is None and not _healthy():
            if time.monotonic() - started > STARTUP_SECONDS:
                raise TimeoutError(f"server not ready after {STARTUP_SECONDS} s")
            time.sleep(3)
        if process.poll() is not None:
            raise RuntimeError(f"server exited with {process.returncode} while starting")
        result["startup_seconds"] = round(time.monotonic() - started, 1)
        result["models_without_key"] = _call("/v1/models", None)[0]
        result["models_with_key"] = _call("/v1/models", key)[0]
        chat = {"model": model.id, "max_tokens": 64, "temperature": 0, "messages": [{"role": "user", "content": "Name three primary colors."}],
                "chat_template_kwargs": {"enable_thinking": False}}
        code, body = _call("/v1/chat/completions", key, chat)
        result["chat"] = {"status": code, "body": body[:600]}
        tool = {"type": "function", "function": {"name": "get_weather", "description": "Current weather for a city",
                                                 "parameters": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}}}
        code, body = _call("/v1/chat/completions", key, dict(chat, tools=[tool], messages=[{"role": "user", "content": "What is the weather in Paris?"}], max_tokens=128))
        result["tool_call"] = {"status": code, "body": body[:800]}
        if speculates(args):
            # Enough real writing for the drafter to show itself.
            code, body = _call("/v1/chat/completions", key, dict(chat, max_tokens=384, messages=[{"role": "user", "content": "Explain in plain words why the sea has tides, in about two hundred words."}]))
            result["long_answer"] = {"status": code, "body": body[:400]}
        metrics = _call("/metrics", key)[1]
        result["metrics"] = {family: any(line.startswith(family) for line in metrics.splitlines()) for family in ENGINE_FAMILIES}
        if speculates(args):
            counters = speculation_counters(metrics)
            drafted, accepted = counters.get("vllm:spec_decode_num_draft_tokens"), counters.get("vllm:spec_decode_num_accepted_tokens")
            result["speculation"] = {"counters": counters, "acceptance": round(accepted / drafted, 4) if drafted and accepted is not None else None}
    except Exception as failure:  # noqa: BLE001 - recorded, never retried
        result["error"] = f"{type(failure).__name__}: {str(failure)[-2000:]}"
    finally:
        process.terminate()
        try:
            process.wait(timeout=30)
        except subprocess.TimeoutExpired:
            process.kill()
    server_log = log_path.read_text(errors="replace")
    lines = [line for line in server_log.splitlines() if "it/s]" not in line and "s/it]" not in line]  # no progress bars
    words = ("kv cache", "maximum concurrency", "deepgemm", "deep_gemm", "loading weights took", "model loading took", "torch.compile", "error", "rust", "frontend", "panic", "init engine", "specul", "mtp", "draft", "max_num_seqs", "recurrent")
    result["log_lines"] = [line[-300:] for line in lines if any(word in line.lower() for word in words)][-60:]
    if result.get("error"):
        result["log_tail"] = "\n".join(line[-300:] for line in lines[-60:])
    result["container_seconds"] = round(time.monotonic() - started, 1)
    return result


@app.local_entrypoint()
def main(action: str = "download", model: str = QWEN3_8B.id, setup: str = "current") -> None:
    if action == "download":
        print(json.dumps(download.remote(model), indent=2))
        return
    if action != "check" or setup not in CHECK_SETUPS:
        raise SystemExit("only `download` and `check` run from the command line (check takes --setup current or speculation); the broker starts servers")
    if setup == "speculation" and not MODELS[model].publisher_speculation:
        raise SystemExit(f"{model} has no publisher-documented speculation option in models.py")
    # Spawned and cancelled at its limit: a container that fails while starting is
    # otherwise retried without end, whatever retries says.
    call = check_gpu.spawn(model, setup)
    try:
        print(json.dumps(call.get(timeout=CHECK_SECONDS + 600), indent=2))
    finally:
        call.cancel(terminate_containers=True)

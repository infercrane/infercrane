"""Development-only remote GPU servers on Modal for the Modal GPU PoC.

Modal is not an InferCrane provider. This app only gives the local PoC's
model server pods (internal/testtools/modal-gpu-proxy) a real GPU behind them:
one H100 container per lease, running the pinned vLLM image with exactly the
arguments the pod was started with, reachable through a Modal tunnel.

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
from models import MODELS, QWEN3_8B, check_args  # noqa: E402,F401 - check_args is the broker's

APP_NAME = "infercrane-poc-modal-gpu"
# vLLM 0.22.1, the same pinned image as tools/modal-kernel-poc on PR #229.
VLLM_IMAGE = "vllm/vllm-openai@sha256:953d3a06d5e64ab582985cd7401289d3abf2a2c14ef2158e9a84313daeec77d7"
VLLM_VERSION = "0.22.1"


PORT = 8000
GPU = "H100!"  # "!" pins an H100; Modal may not substitute an H200
GPU_CPU, GPU_MEMORY_MIB = 4.0, 24576
# Hard cap per container. A campaign keeps every setup serving until it ranks them, and a
# gain is measured again on fresh servers, so a 27B campaign outlasts the 90 minutes an
# 8B one needed.
GPU_TIMEOUT_SECONDS = 150 * 60
MAX_GPUS = 3
LEASE_SECONDS = 180
STARTUP_SECONDS = 15 * 60

app = modal.App(APP_NAME)
weights = modal.Volume.from_name("infercrane-poc-modal-weights", create_if_missing=True)
leases = modal.Dict.from_name("infercrane-poc-modal-leases", create_if_missing=True)
download_image = modal.Image.debian_slim(python_version="3.12").pip_install("huggingface_hub[hf_xet]==1.1.4").add_local_python_source("models")
image = modal.Image.from_registry(VLLM_IMAGE, setup_dockerfile_commands=["RUN ln -sf /usr/bin/python3 /usr/local/bin/python"]).entrypoint([]).add_local_python_source("models")


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


# A stand-in server for the dry run: same lifecycle and tunnel, no GPU.
FAKE_SERVER = r'''
import json, sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
port, key, model = int(sys.argv[1]), sys.argv[2], sys.argv[3]
class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def log_message(self, *a): pass
    def send(self, code, body, kind="application/json"):
        data = body.encode()
        self.send_response(code); self.send_header("Content-Type", kind); self.send_header("Content-Length", str(len(data))); self.end_headers(); self.wfile.write(data)
    def do_GET(self):
        if self.path == "/health": return self.send(200, "")
        if self.path == "/metrics": return self.send(200, "vllm:num_requests_waiting 0\nvllm:kv_cache_usage_perc 0\n", "text/plain")
        if self.path == "/v1/models":
            if self.headers.get("Authorization") != "Bearer " + key: return self.send(401, "{}")
            return self.send(200, json.dumps({"object": "list", "data": [{"id": model, "object": "model"}]}))
        self.send(404, "{}")
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))) or b"{}")
        if self.headers.get("Authorization") != "Bearer " + key: return self.send(401, "{}")
        if body.get("stream"):
            self.send_response(200); self.send_header("Content-Type", "text/event-stream"); self.send_header("Transfer-Encoding", "chunked"); self.end_headers()
            for chunk in ('data: {"choices":[{"index":0,"delta":{"content":"dry"}}]}\n\n', "data: [DONE]\n\n"):
                data = chunk.encode(); self.wfile.write(b"%x\r\n%s\r\n" % (len(data), data)); self.wfile.flush()
            self.wfile.write(b"0\r\n\r\n"); return
        self.send(200, json.dumps({"object": "chat.completion", "model": model, "choices": [{"index": 0, "message": {"role": "assistant", "content": "dry"}, "finish_reason": "stop"}], "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}}))
ThreadingHTTPServer(("0.0.0.0", port), H).serve_forever()
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
    command = ["python3", "-c", FAKE_SERVER, str(PORT), api_key, model] if dry else server_command(args, model)
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
def serve_h100(lease_id: str, args: list[str], api_key: str, model: str) -> dict:
    return _serve(lease_id, args, api_key, model, dry=False)


@app.function(image=image, cpu=0.5, memory=1024, timeout=20 * 60, retries=0, max_containers=2, scaledown_window=2)
def serve_dry(lease_id: str, args: list[str], api_key: str, model: str) -> dict:
    return _serve(lease_id, args, api_key, model, dry=True)


CHECK_SECONDS = 25 * 60
# Families of vLLM's /metrics the control plane's engine scrape reads (docs/features/telemetry.md).
ENGINE_FAMILIES = ("vllm:num_requests_running", "vllm:num_requests_waiting", "vllm:kv_cache_usage_perc", "vllm:request_queue_time_seconds",
                   "vllm:request_prefill_time_seconds", "vllm:request_decode_time_seconds", "vllm:time_to_first_token_seconds",
                   "vllm:e2e_request_latency_seconds", "vllm:num_preemptions", "vllm:prefix_cache_queries")


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
def check_h100(model_id: str) -> dict:
    """Starts the model's current setup once and checks what a run relies on: the server
    starts, refuses a request without its key, answers one with it, calls a tool, and
    exports the vLLM metrics the control plane reads."""
    import secrets

    model, started = MODELS[model_id], time.monotonic()
    args = ["--served-model-name", model.id, *model.current_args]
    result: dict = {"model": model.id, "revision": model.revision, "args": args, "env": model.env, "gpu_facts": _gpu_facts()}
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
        metrics = _call("/metrics", key)[1]
        result["metrics"] = {family: any(line.startswith(family) for line in metrics.splitlines()) for family in ENGINE_FAMILIES}
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
    words = ("kv cache", "maximum concurrency", "deepgemm", "deep_gemm", "loading weights took", "model loading took", "torch.compile", "error", "rust", "frontend", "panic", "init engine")
    result["log_lines"] = [line[-300:] for line in lines if any(word in line.lower() for word in words)][-60:]
    if result.get("error"):
        result["log_tail"] = "\n".join(line[-300:] for line in lines[-60:])
    result["container_seconds"] = round(time.monotonic() - started, 1)
    return result


@app.local_entrypoint()
def main(action: str = "download", model: str = QWEN3_8B.id) -> None:
    if action == "download":
        print(json.dumps(download.remote(model), indent=2))
        return
    if action != "check":
        raise SystemExit("only `download` and `check` run from the command line; the broker starts servers")
    # Spawned and cancelled at its limit: a container that fails while starting is
    # otherwise retried without end, whatever retries says.
    call = check_h100.spawn(model)
    try:
        print(json.dumps(call.get(timeout=CHECK_SECONDS + 600), indent=2))
    finally:
        call.cancel(terminate_containers=True)

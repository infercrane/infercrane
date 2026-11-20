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
import subprocess
import time
import urllib.request
from pathlib import Path

import modal

APP_NAME = "infercrane-poc-modal-gpu"
# vLLM 0.22.1, the same pinned image as tools/modal-kernel-poc on PR #229.
VLLM_IMAGE = "vllm/vllm-openai@sha256:953d3a06d5e64ab582985cd7401289d3abf2a2c14ef2158e9a84313daeec77d7"
VLLM_VERSION = "0.22.1"
MODEL_ID = "Qwen/Qwen3-8B"
MODEL_REVISION = "b968826d9c46dd6066d109eabc6255188de91218"
MODEL_DIR = f"/weights/qwen3-8b-{MODEL_REVISION[:12]}"
PORT = 8000
GPU = "H100!"  # "!" pins an H100; Modal may not substitute an H200
GPU_CPU, GPU_MEMORY_MIB = 4.0, 24576
GPU_TIMEOUT_SECONDS = 90 * 60  # hard cap per container
MAX_GPUS = 3
LEASE_SECONDS = 180
STARTUP_SECONDS = 15 * 60
# The same for every setup. vLLM 0.22.1 can mis-detect its vendored DeepGEMM as
# usable on H100 and fail while warming a BF16 model (tools/modal-kernel-poc).
SERVER_ENV = {"VLLM_USE_DEEP_GEMM": "0"}

# Arguments a pod may pass to vLLM. Anything else is refused, so a candidate can
# never ask for more GPUs, another port, or another model.
VALUE_FLAGS = {"--served-model-name", "--max-model-len", "--quantization", "--kv-cache-dtype", "--max-num-batched-tokens",
               "--max-num-seqs", "--gpu-memory-utilization", "--block-size", "--seed", "--dtype"}
SWITCH_FLAGS = {"--enable-prefix-caching", "--no-enable-prefix-caching", "--enable-chunked-prefill", "--no-enable-chunked-prefill",
                "--async-scheduling", "--calculate-kv-scales"}

app = modal.App(APP_NAME)
weights = modal.Volume.from_name("infercrane-poc-modal-weights", create_if_missing=True)
leases = modal.Dict.from_name("infercrane-poc-modal-leases", create_if_missing=True)
download_image = modal.Image.debian_slim(python_version="3.12").pip_install("huggingface_hub[hf_xet]==1.1.4")
image = modal.Image.from_registry(VLLM_IMAGE, setup_dockerfile_commands=["RUN ln -sf /usr/bin/python3 /usr/local/bin/python"]).entrypoint([])


def check_args(args: list[str], model: str) -> None:
    """Refuses arguments outside the allowlist; raises ValueError."""
    if model != MODEL_ID:
        raise ValueError(f"only {MODEL_ID} is staged on the remote volume, not {model}")
    index, served = 0, None
    while index < len(args):
        flag = args[index]
        name = flag.split("=", 1)[0]
        if name in SWITCH_FLAGS and "=" not in flag:
            index += 1
            continue
        if name not in VALUE_FLAGS:
            raise ValueError(f"vLLM argument {flag!r} is not allowed on the remote GPU")
        if "=" in flag:
            value = flag.split("=", 1)[1]
            index += 1
        else:
            if index + 1 >= len(args) or args[index + 1].startswith("--"):
                raise ValueError(f"vLLM argument {flag} needs a value")
            value = args[index + 1]
            index += 2
        if name == "--served-model-name":
            served = value
    if served != model:
        raise ValueError("--served-model-name must be the lease's model")


def server_command(args: list[str]) -> list[str]:
    return ["vllm", "serve", MODEL_DIR, "--host", "0.0.0.0", "--port", str(PORT), *args]


@app.function(image=download_image, volumes={"/weights": weights}, cpu=2, memory=8192, timeout=1800, retries=0)
def download() -> dict[str, object]:
    """Stages the pinned weights on the volume (CPU only, no GPU time)."""
    from huggingface_hub import snapshot_download

    marker = Path(MODEL_DIR) / ".complete"
    if marker.exists() and marker.read_text() == MODEL_REVISION:
        return {"downloaded": False, "path": MODEL_DIR}
    started = time.monotonic()
    snapshot_download(repo_id=MODEL_ID, revision=MODEL_REVISION, local_dir=MODEL_DIR,
                      allow_patterns=["*.json", "*.safetensors", "*.txt", "*.model", "*.jinja"])
    marker.write_text(MODEL_REVISION)
    weights.commit()
    files = sorted(p for p in Path(MODEL_DIR).rglob("*") if p.is_file() and ".cache" not in p.parts)
    return {"downloaded": True, "seconds": round(time.monotonic() - started), "path": MODEL_DIR,
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


def _gpu_name() -> str:
    probe = subprocess.run(["nvidia-smi", "--query-gpu=name,memory.total,driver_version", "--format=csv,noheader"], capture_output=True, text=True)
    return probe.stdout.strip() if probe.returncode == 0 else "unknown"


def _serve(lease_id: str, args: list[str], api_key: str, model: str, dry: bool) -> dict:
    started = time.time()
    _publish(lease_id, {"state": "starting", "container_started_at": started, "dry": dry, "args": args,
                                 "server_env": SERVER_ENV, "runtime_image": VLLM_IMAGE, "model_revision": MODEL_REVISION})
    try:
        check_args(args, model)
    except ValueError as error:
        return _publish(lease_id, {"state": "failed", "error": str(error), "stopped_at": time.time()})
    log_path = Path("/tmp/server.log")
    env = dict(os.environ, **SERVER_ENV, VLLM_API_KEY=api_key)
    command = ["python3", "-c", FAKE_SERVER, str(PORT), api_key, model] if dry else server_command(args)
    with log_path.open("w") as log:
        process = subprocess.Popen(command, stdout=log, stderr=subprocess.STDOUT, env=env)
    reason, error = "", ""
    try:
        if not dry:
            _publish(lease_id, {"gpu": _gpu_name(), "vllm_version": VLLM_VERSION})
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


@app.local_entrypoint()
def main(action: str = "download") -> None:
    if action != "download":
        raise SystemExit("only `download` runs from the command line; the broker starts servers")
    print(json.dumps(download.remote(), indent=2))

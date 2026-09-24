"""Private Modal deployment for the Qwen3.8 OpenRouter provider canary.

The application intentionally starts with a single, scale-to-zero H200. Fleet
scale-out is enabled only after the exact endpoint passes qualification,
accounting, recovery, and soak gates.
"""

from __future__ import annotations

import os
import re
import socket
import subprocess
import threading
import time
import urllib.error
import urllib.request
from pathlib import Path

import modal


DEFAULT_APP_NAME = "infercrane-qwen38-openrouter-canary"
APP_NAME = os.environ.get("INFERCRANE_MODAL_APP_NAME", DEFAULT_APP_NAME).strip()
MODEL_ID = "Qwen/Qwen3.8-27B-FP8"
PUBLIC_MODEL_ID = "qwen/qwen3.8-27b"
MODEL_REVISION = "017b9c7af6b5689d5dd426a76e0bc077eb5ca20a"
SGLANG_IMAGE = (
    "lmsysorg/sglang@sha256:"
    "06e4f2ed21afde4ff513cda65070124e727ba23ccaeff7712b8c40e1097d611f"
)
MODULE_PATH = Path(__file__).resolve()
ROOT = MODULE_PATH.parents[2] if len(MODULE_PATH.parents) > 2 else Path.cwd()
EDGE_BINARY = ROOT / ".local-build" / "infercrane-openrouter-edge"
CATALOG = ROOT / "deploy" / "openrouter" / "qwen38-provider-models.json"
GDN_PATCH = (
    ROOT / "deploy" / "openrouter" / "qwen38-sglang-0520" / "patch_gdn_precision.py"
)

if not re.fullmatch(r"[a-z0-9][a-z0-9-]{2,62}", APP_NAME):
    raise RuntimeError("INFERCRANE_MODAL_APP_NAME must be a lowercase release name")

MIN_CONTAINERS = int(os.environ.get("INFERCRANE_MODAL_MIN_CONTAINERS", "1"))
MAX_CONTAINERS = int(os.environ.get("INFERCRANE_MODAL_MAX_CONTAINERS", "1"))
BUFFER_CONTAINERS = int(os.environ.get("INFERCRANE_MODAL_BUFFER_CONTAINERS", "0"))
TARGET_CONCURRENCY = int(os.environ.get("INFERCRANE_MODAL_TARGET_CONCURRENCY", "12"))
if not (
    1 <= MIN_CONTAINERS <= MAX_CONTAINERS <= 8
    and 0 <= BUFFER_CONTAINERS < MAX_CONTAINERS
    and 1 <= TARGET_CONCURRENCY <= 12
):
    raise RuntimeError("invalid bounded Modal capacity configuration")

if modal.is_local() and not EDGE_BINARY.is_file():
    raise RuntimeError(
        "build .local-build/infercrane-openrouter-edge for linux/amd64 before deploy"
    )

app = modal.App(APP_NAME)
model_cache = modal.Volume.from_name(
    "infercrane-qwen38-model-cache", create_if_missing=False
)
compile_cache = modal.Volume.from_name(
    "infercrane-qwen38-sglang-cache", create_if_missing=False
)
evidence = modal.Volume.from_name(
    "infercrane-qwen38-optimization-results", create_if_missing=False
)
runtime_secret = modal.Secret.from_name(
    os.environ.get(
        "INFERCRANE_MODAL_RUNTIME_SECRET",
        "infercrane-openrouter-provider-runtime",
    )
)
image = modal.Image.from_registry(SGLANG_IMAGE, add_python="3.12").entrypoint([])
if EDGE_BINARY.is_file() and CATALOG.is_file() and GDN_PATCH.is_file():
    image = (
        image.add_local_file(
            EDGE_BINARY, "/opt/infercrane/infercrane-openrouter-edge", copy=True
        )
        .add_local_file(CATALOG, "/opt/infercrane/provider-models.json", copy=True)
        .add_local_file(
            GDN_PATCH, "/opt/infercrane/patch_gdn_precision.py", copy=True
        )
        .run_commands(
            "chmod 0555 /opt/infercrane/infercrane-openrouter-edge",
            "chmod 0444 /opt/infercrane/provider-models.json /opt/infercrane/patch_gdn_precision.py",
            "python3 /opt/infercrane/patch_gdn_precision.py",
        )
    )
image = image.env(
        {
            "HF_HOME": "/vol/model-cache",
            "HUGGINGFACE_HUB_CACHE": "/vol/model-cache/hub",
            "TRANSFORMERS_OFFLINE": "1",
            "HF_HUB_OFFLINE": "1",
            "SGLANG_CACHE_DIR": "/vol/compile-cache/sglang",
            "TRITON_CACHE_DIR": "/vol/compile-cache/triton",
            "TORCHINDUCTOR_CACHE_DIR": "/vol/compile-cache/inductor",
            "DG_JIT_CACHE_DIR": "/vol/compile-cache/deep-gemm",
            "SGLANG_DG_CACHE_DIR": "/vol/compile-cache/deep-gemm",
            "FLASHINFER_CACHE_DIR": "/vol/compile-cache/flashinfer",
            "FLASHINFER_WORKSPACE_BASE": "/vol/compile-cache/flashinfer-workspace",
            "TVM_FFI_CACHE_DIR": "/vol/compile-cache/tvm-ffi",
            "TILELANG_CACHE_DIR": "/vol/compile-cache/tilelang",
            "CUTE_DSL_CACHE_DIR": "/vol/compile-cache/cute-dsl",
            "CUDA_CACHE_PATH": "/vol/compile-cache/cuda",
        }
    )


def _wait_for(url: str, timeout_seconds: int) -> None:
    deadline = time.monotonic() + timeout_seconds
    last_error: Exception | None = None
    while time.monotonic() < deadline:
        try:
            with urllib.request.urlopen(url, timeout=3) as response:
                if 200 <= response.status < 300:
                    return
        except (OSError, urllib.error.URLError) as exc:
            last_error = exc
        time.sleep(2)
    raise RuntimeError(f"startup deadline exceeded for {url}: {last_error}")


@app.server(
    image=image,
    gpu="H200",
    volumes={
        "/vol/model-cache": model_cache,
        "/vol/compile-cache": compile_cache,
        "/vol/evidence": evidence,
    },
    secrets=[runtime_secret],
    port=8080,
    unauthenticated=True,
    target_concurrency=TARGET_CONCURRENCY,
    min_containers=MIN_CONTAINERS,
    max_containers=MAX_CONTAINERS,
    buffer_containers=BUFFER_CONTAINERS,
    scaleup_window=1,
    scaledown_window=300,
    startup_timeout=20 * 60,
    exit_grace_period=60,
)
class Provider:
    runtime: subprocess.Popen[bytes]
    edge: subprocess.Popen[bytes]
    stop_committer: threading.Event
    committer: threading.Thread

    @modal.enter()
    def start(self) -> None:
        provider_key = os.environ.pop("INFERCRANE_OPENROUTER_API_KEY", "").strip()
        metrics_key = os.environ.pop("INFERCRANE_OPENROUTER_METRICS_KEY", "").strip()
        if not provider_key or not metrics_key:
            raise RuntimeError("provider and metrics keys are required")

        secret_dir = Path("/run/infercrane")
        secret_dir.mkdir(parents=True, mode=0o700, exist_ok=True)
        provider_key_file = secret_dir / "provider-key"
        metrics_key_file = secret_dir / "metrics-key"
        provider_key_file.write_text(provider_key)
        metrics_key_file.write_text(metrics_key)
        provider_key_file.chmod(0o600)
        metrics_key_file.chmod(0o600)

        for cache in (
            "sglang",
            "triton",
            "inductor",
            "deep-gemm",
            "flashinfer",
            "flashinfer-workspace",
            "tvm-ffi",
            "tilelang",
            "cute-dsl",
            "cuda",
        ):
            Path("/vol/compile-cache", cache).mkdir(parents=True, exist_ok=True)

        runtime_command = [
            "python3",
            "-m",
            "sglang.launch_server",
            "--model-path",
            MODEL_ID,
            "--revision",
            MODEL_REVISION,
            "--served-model-name",
            PUBLIC_MODEL_ID,
            "--host",
            "127.0.0.1",
            "--port",
            "30000",
            "--tp-size",
            "1",
            "--context-length",
            "262144",
            "--mem-fraction-static",
            "0.90",
            "--kv-cache-dtype",
            "fp8_e4m3",
            "--max-running-requests",
            "12",
            "--enable-metrics",
            "--reasoning-parser",
            "qwen3",
            "--tool-call-parser",
            "qwen3_coder",
            "--speculative-algorithm",
            "NEXTN",
            "--speculative-num-steps",
            "3",
            "--speculative-eagle-topk",
            "1",
            "--speculative-num-draft-tokens",
            "4",
            "--cuda-graph-bs-decode",
            "1",
            "2",
            "4",
            "8",
            "12",
            "16",
            "24",
            "32",
            "33",
            "--cuda-graph-bs-prefill",
            "256",
            "512",
            "1024",
            "2048",
            "4096",
            "8192",
        ]
        self.runtime = subprocess.Popen(runtime_command)
        _wait_for("http://127.0.0.1:30000/health", 18 * 60)

        # Modal gives every running container a unique task ID. The generic
        # hostname is not replica-unique, so using it alone would make scaled
        # containers append to the same Volume file and corrupt the audit
        # stream.
        raw_instance = os.environ.get("MODAL_TASK_ID", socket.gethostname())
        instance = re.sub(r"[^A-Za-z0-9_.-]", "_", raw_instance)
        receipt_dir = Path("/vol/evidence/openrouter-provider")
        receipt_dir.mkdir(parents=True, exist_ok=True)
        receipt_file = receipt_dir / f"requests-{instance}.ndjson"
        edge_command = [
            "/opt/infercrane/infercrane-openrouter-edge",
            "--listen",
            ":8080",
            "--catalog",
            "/opt/infercrane/provider-models.json",
            "--public-model",
            "qwen/qwen3.8-27b",
            "--upstream-model",
            PUBLIC_MODEL_ID,
            "--upstream-url",
            "http://127.0.0.1:30000",
            "--api-key-file",
            str(provider_key_file),
            "--metrics-key-file",
            str(metrics_key_file),
            "--receipt-file",
            str(receipt_file),
            "--min-in-flight",
            "4",
            "--initial-in-flight",
            "12",
            "--max-in-flight",
            "12",
            "--admission-window",
            "32",
            "--target-ttft",
            "3s",
            "--qualified-output-tps",
            "1450",
            "--max-prefill-tokens-in-flight",
            "65536",
            "--input-price-per-million",
            "0.081",
            "--output-price-per-million",
            "1.782",
            "--gpu-hourly-cost",
            "4.5396",
        ]
        self.edge = subprocess.Popen(edge_command)
        _wait_for("http://127.0.0.1:8080/readyz", 60)

        self.stop_committer = threading.Event()

        def commit_evidence() -> None:
            while not self.stop_committer.wait(60):
                evidence.commit()

        self.committer = threading.Thread(target=commit_evidence, daemon=True)
        self.committer.start()

    @modal.exit()
    def stop(self) -> None:
        if hasattr(self, "stop_committer"):
            self.stop_committer.set()
        if hasattr(self, "committer"):
            self.committer.join(timeout=5)
        for process_name in ("edge", "runtime"):
            process = getattr(self, process_name, None)
            if process is None or process.poll() is not None:
                continue
            process.terminate()
            try:
                process.wait(timeout=30)
            except subprocess.TimeoutExpired:
                process.kill()
        evidence.commit()

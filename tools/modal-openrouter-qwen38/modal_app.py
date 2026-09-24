"""Workload-based Qwen3.8 recipe search on Modal accelerators.

OpenRouter is an external production scoreboard, not the benchmark corpus. The
campaign screens recipes on versioned public/derived workload profiles and then
compares qualified results with a fresh OpenRouter endpoint snapshot.
"""

from __future__ import annotations

import asyncio
import gzip
import hashlib
import importlib.metadata
import json
import math
import os
import re
import signal
import subprocess
import sys
import time
import urllib.request
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

import modal

sys.path.insert(0, str(Path(__file__).resolve().parent))
sys.path.insert(0, "/opt/infercrane")

from campaign_support import (
    MODEL_ID,
    MODEL_REVISION,
    MODEL_SLUG,
    WORKLOADS,
    apply_runtime_parity,
    build_evidence,
    competitive_report,
    encoded_token_count,
    merge_prometheus_measurements,
    merge_candidate_runs,
    openrouter_snapshot,
    project_lane_economics,
    speculation_health,
    summarize_lane,
    summarize_torch_trace,
    custom_kernel_gate,
    parse_sglang_startup,
    synthetic_prompt_content,
)


APP_NAME = "infercrane-qwen38-workload-optimizer"
PORT = 30000
GPU_PRICES_USD_PER_HOUR = {"H100": 3.9492, "H200": 4.5396, "B200": 6.2496}
GPU = os.environ.get("INFERCRANE_MODAL_GPU", "H200").strip().upper()
if GPU not in GPU_PRICES_USD_PER_HOUR:
    raise ValueError("INFERCRANE_MODAL_GPU must be H100, H200, or B200")
# Modal may transparently substitute a newer compatible accelerator unless the
# request has the exact-hardware suffix. Reproducibility-sensitive evidence
# must never label an H200 result as H100 evidence.
GPU_REQUEST = "H100!" if GPU == "H100" else GPU
GPU_HOURLY_COST_USD = float(
    os.environ.get("INFERCRANE_MODAL_GPU_HOURLY_USD", GPU_PRICES_USD_PER_HOUR[GPU])
)
# Economics use the prices users actually pay. OpenRouter applies the staged
# discount to every SKU before billing; using the undiscounted base here would
# overstate productive revenue and understate the utilization required to
# break even.
BASE_INPUT_PRICE_USD_PER_MILLION = 0.10
BASE_OUTPUT_PRICE_USD_PER_MILLION = 2.20
LAUNCH_DISCOUNT_TO_USER = 0.19
LAUNCH_INPUT_PRICE_USD_PER_MILLION = (
    BASE_INPUT_PRICE_USD_PER_MILLION * (1 - LAUNCH_DISCOUNT_TO_USER)
)
LAUNCH_OUTPUT_PRICE_USD_PER_MILLION = (
    BASE_OUTPUT_PRICE_USD_PER_MILLION * (1 - LAUNCH_DISCOUNT_TO_USER)
)
SGLANG_VERSION = "0.5.20"
SGLANG_IMAGE = "lmsysorg/sglang@sha256:06e4f2ed21afde4ff513cda65070124e727ba23ccaeff7712b8c40e1097d611f"
VLLM_VERSION = "0.30.0"
VLLM_IMAGE = "vllm/vllm-openai@sha256:5f5e535216848d0c52159c8c13a0af04be5f6fe1a84e79914300610796f76d40"
VLLM_CUTLASS_DSL_VERSION = "4.7.1"
DFLASH2_MODEL_ID = "incoai/Qwen3.8-27B-DFlash2"
DFLASH2_MODEL_REVISION = "015e795645c74b1a0eeef3b570031fb62e769bc5"
SCREENING_REQUESTS_PER_LANE = 12

MODULE_PATH = Path(__file__).resolve()
ROOT = MODULE_PATH.parents[2] if len(MODULE_PATH.parents) > 2 else Path.cwd()
SUPPORT = MODULE_PATH.with_name("campaign_support.py")
ADAPTIVE_SPEC_CONFIG = MODULE_PATH.with_name("adaptive_spec_h200.json")
GDN_KERNEL_LAB = MODULE_PATH.with_name("gdn_h200_kernel_lab.py")
STATE_SCATTER_KERNEL_LAB = MODULE_PATH.with_name("state_scatter_h200_kernel_lab.py")
GDN_PRECISION_PATCH = ROOT / "deploy/openrouter/qwen38-sglang-0520/patch_gdn_precision.py"

app = modal.App(APP_NAME)
model_cache = modal.Volume.from_name("infercrane-qwen38-model-cache", create_if_missing=True)
compile_cache = modal.Volume.from_name("infercrane-qwen38-sglang-cache", create_if_missing=True)
results = modal.Volume.from_name("infercrane-qwen38-optimization-results", create_if_missing=True)
image = (
    modal.Image.from_registry(SGLANG_IMAGE, add_python="3.12")
    .entrypoint([])
    .env(
        {
            "INFERCRANE_MODAL_GPU": GPU,
            "INFERCRANE_MODAL_GPU_HOURLY_USD": str(GPU_HOURLY_COST_USD),
        }
    )
    .pip_install("httpx==0.28.1", "jsonschema==4.25.1")
    .run_commands(
        # Modal's dependency layer uses the source image's /opt interpreter,
        # while this campaign intentionally executes the injected /usr/local
        # Python. Install the three exact CUTLASS DSL wheels into that
        # interpreter's package root, without replacing vLLM dependencies.
        "/usr/local/bin/python -m pip install --no-deps --upgrade "
        "--target /usr/local/lib/python3.12/dist-packages "
        f"nvidia-cutlass-dsl=={VLLM_CUTLASS_DSL_VERSION} "
        f"nvidia-cutlass-dsl-libs-base=={VLLM_CUTLASS_DSL_VERSION} "
        f"nvidia-cutlass-dsl-libs-cu13=={VLLM_CUTLASS_DSL_VERSION} "
        "--extra-index-url https://pypi.nvidia.com"
    )
)
if GDN_PRECISION_PATCH.exists():
    image = image.add_local_file(
        GDN_PRECISION_PATCH, "/opt/infercrane/patch_gdn_precision.py", copy=True
    )
if SUPPORT.exists():
    image = image.add_local_file(SUPPORT, "/opt/infercrane/campaign_support.py")
if ADAPTIVE_SPEC_CONFIG.exists():
    image = image.add_local_file(
        ADAPTIVE_SPEC_CONFIG,
        "/opt/infercrane/adaptive_spec_h200.json",
    )
if GDN_KERNEL_LAB.exists():
    image = image.add_local_file(
        GDN_KERNEL_LAB,
        "/opt/infercrane/gdn_h200_kernel_lab.py",
    )
if STATE_SCATTER_KERNEL_LAB.exists():
    image = image.add_local_file(
        STATE_SCATTER_KERNEL_LAB,
        "/opt/infercrane/state_scatter_h200_kernel_lab.py",
    )
vllm_image = (
    # Modal injects its worker interpreter at /usr/local. Keep vLLM's pinned
    # /opt/venv packages visible to that same Python 3.12 ABI rather than
    # reinstalling or mutating the release image.
    modal.Image.from_registry(VLLM_IMAGE, add_python="3.12")
    .entrypoint([])
    .env(
        {
            "PYTHONPATH": (
                "/usr/local/lib/python3.12/dist-packages:"
                "/usr/local/lib/python3.12/dist-packages/nvidia_cutlass_dsl/dsl_packages:"
                "/opt/venv/lib/python3.12/site-packages"
            ),
            "PATH": "/opt/venv/bin:/usr/local/bin:/usr/bin:/bin",
        }
    )
    .pip_install("httpx==0.28.1", "jsonschema==4.25.1")
)
if SUPPORT.exists():
    vllm_image = vllm_image.add_local_file(SUPPORT, "/opt/infercrane/campaign_support.py")


BASE_ARGS = [
    "--model-path",
    MODEL_ID,
    "--served-model-name",
    MODEL_ID,
    "--revision",
    MODEL_REVISION,
    "--tp-size",
    "1",
    "--context-length",
    "262144",
    "--mem-fraction-static",
    "0.90",
    "--kv-cache-dtype",
    "fp8_e4m3",
    "--enable-metrics",
    "--reasoning-parser",
    "qwen3",
    "--tool-call-parser",
    "qwen3_coder",
    "--port",
    str(PORT),
]

VLLM_BASE_ARGS = [
    MODEL_ID,
    "--served-model-name",
    MODEL_ID,
    "--revision",
    MODEL_REVISION,
    "--tensor-parallel-size",
    "1",
    "--max-model-len",
    "262144",
    "--gpu-memory-utilization",
    "0.90",
    "--kv-cache-dtype",
    "fp8_e4m3",
    "--reasoning-parser",
    "qwen3",
    "--tool-call-parser",
    "qwen3_coder",
    "--enable-auto-tool-choice",
    "--enable-prefix-caching",
    "--language-model-only",
    "--port",
    str(PORT),
]

VLLM_CANDIDATES: dict[str, dict[str, Any]] = {
    "vllm-0300-control": {
        "runtime_id": "vllm-0.30.0",
        "args": [],
        "class": "runtime_control",
        "workloads": list(WORKLOADS),
    },
    "vllm-0300-mtp-k3": {
        "runtime_id": "vllm-0.30.0-mtp-k3",
        "args": [
            "--speculative-config",
            '{"method":"mtp","num_speculative_tokens":3}',
        ],
        "class": "native_mtp",
        "workloads": [
            "public-interactive",
            "public-decode-heavy",
            "public-decode-saturation",
        ],
    },
}

CANDIDATES: dict[str, dict[str, Any]] = {
    "sglang-0520-control": {
        "runtime_id": "sglang-0.5.20",
        "args": [],
        "class": "runtime_control",
        "workloads": list(WORKLOADS),
    },
    "sglang-0520-bf16-kv-control": {
        "runtime_id": "sglang-0.5.20-bf16-kv",
        # SGLang's auto dtype retains BF16 KV for this BF16-activation model.
        # Hopper FP8 KV is not assumed faster; it is a measured candidate.
        "args": ["--kv-cache-dtype", "auto"],
        "class": "runtime_control",
        "workloads": list(WORKLOADS),
    },
    "sglang-0520-nextn-k2": {
        "runtime_id": "sglang-0.5.20-nextn-k2",
        "args": [
            "--speculative-algorithm",
            "NEXTN",
            "--speculative-num-steps",
            "1",
            "--speculative-eagle-topk",
            "1",
            "--speculative-num-draft-tokens",
            "2",
        ],
        "class": "native_mtp",
        "workloads": ["public-interactive", "public-decode-heavy", "public-decode-saturation"],
    },
    "sglang-0520-nextn-k3": {
        "runtime_id": "sglang-0.5.20-nextn-k3",
        "args": [
            "--speculative-algorithm",
            "NEXTN",
            "--speculative-num-steps",
            "2",
            "--speculative-eagle-topk",
            "1",
            "--speculative-num-draft-tokens",
            "3",
        ],
        "class": "native_mtp",
        "workloads": ["public-interactive", "public-decode-heavy", "public-decode-saturation"],
    },
    "sglang-0520-nextn-k4": {
        "runtime_id": "sglang-0.5.20-nextn-k4",
        "args": [
            "--speculative-algorithm",
            "NEXTN",
            "--speculative-num-steps",
            "3",
            "--speculative-eagle-topk",
            "1",
            "--speculative-num-draft-tokens",
            "4",
        ],
        "class": "native_mtp",
        "workloads": ["public-interactive", "public-decode-heavy", "public-decode-saturation"],
    },
    "sglang-0520-bf16-kv-nextn-k3": {
        "runtime_id": "sglang-0.5.20-bf16-kv-nextn-k3",
        "args": [
            "--kv-cache-dtype",
            "auto",
            "--speculative-algorithm",
            "NEXTN",
            "--speculative-num-steps",
            "2",
            "--speculative-eagle-topk",
            "1",
            "--speculative-num-draft-tokens",
            "3",
        ],
        "class": "native_mtp",
        "workloads": ["public-interactive", "public-decode-heavy"],
    },
    "sglang-0520-bf16-kv-nextn-k4": {
        "runtime_id": "sglang-0.5.20-bf16-kv-nextn-k4",
        "args": [
            "--kv-cache-dtype",
            "auto",
            "--speculative-algorithm",
            "NEXTN",
            "--speculative-num-steps",
            "3",
            "--speculative-eagle-topk",
            "1",
            "--speculative-num-draft-tokens",
            "4",
        ],
        "class": "native_mtp",
        "workloads": ["public-interactive", "public-decode-heavy"],
    },
    "sglang-0520-nextn-k4-chunk4k": {
        "runtime_id": "sglang-0.5.20-nextn-k4-chunk4k",
        "args": [
            "--speculative-algorithm",
            "NEXTN",
            "--speculative-num-steps",
            "3",
            "--speculative-eagle-topk",
            "1",
            "--speculative-num-draft-tokens",
            "4",
            "--chunked-prefill-size",
            "4096",
        ],
        "class": "native_mtp_prefill_scheduling",
        "workloads": ["public-interactive", "public-long-prefill", "public-decode-heavy", "public-decode-saturation"],
    },
    "sglang-0520-nextn-k4-chunk16k": {
        "runtime_id": "sglang-0.5.20-nextn-k4-chunk16k",
        "args": [
            "--speculative-algorithm",
            "NEXTN",
            "--speculative-num-steps",
            "3",
            "--speculative-eagle-topk",
            "1",
            "--speculative-num-draft-tokens",
            "4",
            "--chunked-prefill-size",
            "16384",
            "--max-prefill-tokens",
            "32768",
        ],
        "class": "native_mtp_prefill_scheduling",
        "workloads": ["public-interactive", "public-long-prefill", "public-decode-heavy", "public-decode-saturation"],
    },
    "sglang-0520-nextn-k4-bounded-graphs": {
        "runtime_id": "sglang-0.5.20-nextn-k4-bounded-graphs",
        "args": [
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
        ],
        "class": "native_mtp_bounded_graph_capture",
        "workloads": [
            "public-interactive",
            "public-long-prefill",
            "public-decode-heavy",
            "public-decode-saturation",
            "agent-prefix-reuse",
            "public-context-boundary-262k",
        ],
    },
    "sglang-0520-nextn-k4-bounded-graphs-replayssm-spec": {
        "runtime_id": "sglang-0.5.20-nextn-k4-bounded-graphs-replayssm-spec",
        "args": [
            "--speculative-algorithm",
            "NEXTN",
            "--speculative-num-steps",
            "3",
            "--speculative-eagle-topk",
            "1",
            "--speculative-num-draft-tokens",
            "4",
            # Speculative ReplaySSM keeps a compact raw-input ring and only
            # folds the full recurrent state at commit boundaries. SGLang's
            # ordinary-decode ReplaySSM uses a different cursor protocol and
            # is mutually exclusive with this MTP verification path.
            "--enable-linear-replayssm-spec",
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
        ],
        "class": "native_mtp_bounded_graph_capture_replayssm",
        "workloads": [
            "public-interactive",
            "public-long-prefill",
            "public-decode-heavy",
            "public-decode-saturation",
            "agent-prefix-reuse",
        ],
    },
    "sglang-0520-nextn-adaptive-bounded-graphs": {
        "runtime_id": "sglang-0.5.20-nextn-adaptive-bounded-graphs",
        "args": [
            "--speculative-algorithm",
            "NEXTN",
            # Start from the current winner. SGLang resolves NEXTN to EAGLE
            # before enabling its acceptance- and batch-aware adaptive policy.
            "--speculative-num-steps",
            "3",
            "--speculative-eagle-topk",
            "1",
            "--speculative-num-draft-tokens",
            "4",
            "--speculative-adaptive",
            "--speculative-adaptive-config",
            "/opt/infercrane/adaptive_spec_h200.json",
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
        ],
        "class": "native_mtp_adaptive_bounded_graph_capture",
        "workloads": [
            "public-interactive",
            "public-long-prefill",
            "public-decode-heavy",
            "public-decode-saturation",
            "agent-prefix-reuse",
        ],
    },
    "sglang-0520-dflash2-k8-bounded-graphs": {
        "runtime_id": "sglang-0.5.20-dflash2-k8-bounded-graphs",
        "args": [
            "--speculative-algorithm",
            "DFLASH",
            "--speculative-draft-model-path",
            DFLASH2_MODEL_ID,
            "--speculative-draft-model-revision",
            DFLASH2_MODEL_REVISION,
            # SGLang may otherwise inherit target quantization for the draft.
            # The quantized DFlash path can serve with near-zero acceptance,
            # so this is deliberately explicit and measured below.
            "--speculative-draft-model-quantization",
            "unquant",
            "--speculative-num-draft-tokens",
            "8",
            "--speculative-dflash-block-size",
            "8",
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
        ],
        "class": "external_dflash2_bounded_graph_capture",
        "workloads": [
            "public-interactive",
            "public-long-prefill",
            "public-decode-heavy",
            "public-decode-saturation",
        ],
    },
    "sglang-0520-nextn-k4-graphs-fp8-flashinfer-trtllm": {
        "runtime_id": "sglang-0.5.20-nextn-k4-graphs-fp8-flashinfer-trtllm",
        "args": [
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
            "--fp8-gemm-backend",
            "flashinfer_trtllm",
        ],
        "class": "kernel_backend_ablation",
        "workloads": ["public-interactive", "public-long-prefill", "public-decode-heavy", "public-decode-saturation"],
    },
    "sglang-0520-nextn-k4-graphs-fp8-flashinfer-cutlass": {
        "runtime_id": "sglang-0.5.20-nextn-k4-graphs-fp8-flashinfer-cutlass",
        "args": [
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
            "--fp8-gemm-backend",
            "flashinfer_cutlass",
        ],
        "class": "kernel_backend_ablation",
        "workloads": ["public-interactive", "public-long-prefill", "public-decode-heavy", "public-decode-saturation"],
    },
    "sglang-0520-nextn-k4-graphs-fp8-cutlass": {
        "runtime_id": "sglang-0.5.20-nextn-k4-graphs-fp8-cutlass",
        "args": [
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
            "--fp8-gemm-backend",
            "cutlass",
        ],
        "class": "kernel_backend_ablation",
        "workloads": ["public-interactive", "public-long-prefill", "public-decode-heavy", "public-decode-saturation"],
    },
    "sglang-0520-nextn-k4-graphs-fp8-triton": {
        "runtime_id": "sglang-0.5.20-nextn-k4-graphs-fp8-triton",
        "args": [
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
            "--fp8-gemm-backend",
            "triton",
        ],
        "class": "kernel_backend_ablation",
        "workloads": ["public-interactive", "public-long-prefill", "public-decode-heavy", "public-decode-saturation"],
    },
    "sglang-0520-bf16-kv-nextn-k4-bounded-graphs": {
        "runtime_id": "sglang-0.5.20-bf16-kv-nextn-k4-bounded-graphs",
        "args": [
            "--kv-cache-dtype",
            "auto",
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
        ],
        "class": "native_mtp_bounded_graph_capture",
        "workloads": ["public-interactive", "public-long-prefill", "public-decode-heavy", "public-decode-saturation"],
    },
    "sglang-0520-nextn-k4-lpm": {
        "runtime_id": "sglang-0.5.20-nextn-k4-lpm",
        "args": [
            "--speculative-algorithm",
            "NEXTN",
            "--speculative-num-steps",
            "3",
            "--speculative-eagle-topk",
            "1",
            "--speculative-num-draft-tokens",
            "4",
            "--schedule-policy",
            "lpm",
        ],
        "class": "native_mtp_cache_aware",
        "workloads": ["agent-prefix-reuse"],
    },
    "sglang-0520-nextn-k4-graphs-linear-flashinfer": {
        "runtime_id": "sglang-0.5.20-nextn-k4-graphs-linear-flashinfer",
        "args": [
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
            "--linear-attn-decode-backend",
            "flashinfer",
            "--linear-attn-prefill-backend",
            "flashinfer",
        ],
        "class": "native_mtp_gdn_backend_ablation",
        "workloads": [
            "public-interactive",
            "public-long-prefill",
            "public-decode-heavy",
            "public-decode-saturation",
            "agent-prefix-reuse",
            "public-context-boundary-262k",
        ],
    },
    "sglang-0520-nextn-k4-graphs-linear-flashinfer-prefill": {
        "runtime_id": "sglang-0.5.20-nextn-k4-graphs-linear-flashinfer-prefill",
        "args": [
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
            "--linear-attn-decode-backend",
            "triton",
            "--linear-attn-prefill-backend",
            "flashinfer",
        ],
        "class": "native_mtp_gdn_backend_ablation",
        "workloads": [
            "public-interactive",
            "public-long-prefill",
            "public-decode-heavy",
            "public-decode-saturation",
            "agent-prefix-reuse",
            "public-context-boundary-262k",
        ],
    },
}


def _python() -> str:
    for candidate in ("/opt/sglang/bin/python", "/usr/bin/python3", sys.executable):
        if Path(candidate).exists():
            return candidate
    raise RuntimeError("SGLang image has no usable Python interpreter")


def _apply_gdn_precision_patch() -> dict[str, Any]:
    """Patch the mounted SGLang package before it can serve any request."""
    patch_path = Path("/opt/infercrane/patch_gdn_precision.py")
    if not patch_path.exists():
        raise RuntimeError("the pinned GDN precision patch is unavailable")
    subprocess.run([_python(), str(patch_path)], check=True, timeout=60)
    receipt_path = Path("/opt/infercrane/gdn-precision-patch.json")
    if not receipt_path.exists():
        raise RuntimeError("the GDN precision patch did not produce a receipt")
    return json.loads(receipt_path.read_text())


def _runtime_dependency_versions() -> dict[str, str]:
    versions: dict[str, str] = {}
    for distribution in (
        "sglang",
        "sglang-kernel",
        "flashinfer-python",
        "sgl-deep-gemm",
        "triton",
        "torch",
        "nvidia-cutlass-dsl",
    ):
        try:
            versions[distribution] = importlib.metadata.version(distribution)
        except importlib.metadata.PackageNotFoundError:
            versions[distribution] = "unavailable"
    return versions


def _gpu_inventory() -> list[dict[str, str]]:
    output = subprocess.check_output(
        ["nvidia-smi", "--query-gpu=name,uuid,compute_cap", "--format=csv,noheader"],
        text=True,
        timeout=30,
    )
    devices = []
    for line in output.splitlines():
        name, uuid, capability = (part.strip() for part in line.split(",", 2))
        devices.append({"name": name, "uuid": uuid, "compute_capability": capability})
    if len(devices) != 1 or GPU not in devices[0]["name"]:
        raise RuntimeError(f"campaign requires exactly one {GPU}, got {devices}")
    return devices


def _harness_digest() -> str:
    sources = [Path(__file__), Path(sys.modules["campaign_support"].__file__)]
    digest = hashlib.sha256()
    for source in sources:
        digest.update(source.name.encode())
        digest.update(b"\0")
        digest.update(source.read_bytes())
        digest.update(b"\0")
    return "sha256:" + digest.hexdigest()


def _compile_cache_inventory(root: str = "/compile-cache") -> dict[str, Any]:
    base = Path(root)
    files = [path for path in base.rglob("*") if path.is_file() and not path.is_symlink()]
    by_family: dict[str, dict[str, int]] = {}
    for path in files:
        relative = path.relative_to(base)
        family = relative.parts[0] if relative.parts else "root"
        row = by_family.setdefault(family, {"files": 0, "bytes": 0})
        row["files"] += 1
        row["bytes"] += path.stat().st_size
    receipt_path = Path("/opt/infercrane/gdn-precision-patch.json")
    return {
        "root": str(base),
        "files": len(files),
        "bytes": sum(path.stat().st_size for path in files),
        "families": by_family,
        "gdn_precision_patch": json.loads(receipt_path.read_text())
        if receipt_path.is_file()
        else None,
    }


def _wait_for_server(process: subprocess.Popen[str], log_path: Path, timeout: int = 1200) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise RuntimeError(
                f"inference runtime exited with {process.returncode}:\n"
                + log_path.read_text(errors="replace")[-16000:]
            )
        try:
            with urllib.request.urlopen(f"http://127.0.0.1:{PORT}/health", timeout=3) as response:
                if response.status == 200:
                    return
        except OSError:
            time.sleep(2)
    raise TimeoutError(
        "inference runtime did not become healthy:\n"
        + log_path.read_text(errors="replace")[-16000:]
    )


def _metric_snapshot() -> dict[str, float]:
    try:
        with urllib.request.urlopen(f"http://127.0.0.1:{PORT}/metrics", timeout=5) as response:
            text = response.read().decode()
    except OSError:
        return {}
    values: dict[str, float] = {}
    for line in text.splitlines():
        if not line or line.startswith("#") or " " not in line:
            continue
        metric, raw = line.rsplit(" ", 1)
        name = metric.split("{", 1)[0]
        if not any(word in name.lower() for word in ("token", "cache", "spec", "queue")):
            continue
        try:
            # Preserve histogram labels. Prometheus buckets are cumulative and
            # collapsing them into one value produces meaningless deltas.
            values[metric] = float(raw)
        except ValueError:
            pass
    return values


def _metric_delta(before: dict[str, float], after: dict[str, float]) -> dict[str, float]:
    values: dict[str, float] = {}
    for key in before.keys() | after.keys():
        metric_name = key.split("{", 1)[0]
        if metric_name.endswith(("_total", "_sum", "_count", "_bucket")):
            value = after.get(key, 0.0) - before.get(key, 0.0)
        else:
            value = after.get(key, 0.0)
        if value:
            values[key] = value
    return values


def _speculation_health_gate(
    candidate: dict[str, Any], metrics: dict[str, float]
) -> dict[str, Any]:
    return speculation_health(candidate["class"], metrics)


def _prompt_near_tokens(tokenizer: Any, target: int, variant: int, *, shared_prefix: str = "") -> tuple[list[dict[str, str]], int]:
    system = "You are a coding agent. Analyze the task carefully, preserve constraints, and produce an actionable answer."
    seed = "Repository context: service handler test failure dependency interface implementation evidence. "

    def encode(repeats: int) -> tuple[list[dict[str, str]], int]:
        # Only the explicit agent-session workload supplies shared_prefix.
        # Independent requests diverge before their bulk content.
        content = synthetic_prompt_content(
            seed=seed,
            repeats=repeats,
            variant=variant,
            shared_prefix=shared_prefix,
        )
        messages = [{"role": "system", "content": system}, {"role": "user", "content": content}]
        count = encoded_token_count(
            tokenizer.apply_chat_template(
                messages, tokenize=True, add_generation_prompt=True, enable_thinking=False
            )
        )
        return messages, count

    base = encode(0)
    one = encode(1)
    tokens_per_repeat = max(1, one[1] - base[1])
    estimated_repeats = max(0, math.ceil((target - base[1]) / tokens_per_repeat))
    low, high = 0, max(2, estimated_repeats * 2 + 4)
    best: tuple[list[dict[str, str]], int] | None = base
    while low <= high:
        repeats = (low + high) // 2
        messages, count = encode(repeats)
        if best is None or abs(count - target) < abs(best[1] - target):
            best = (messages, count)
        if count < target:
            low = repeats + 1
        else:
            high = repeats - 1
    assert best is not None
    return best


def _workload_requests(
    tokenizer: Any,
    workload: dict[str, Any],
    count: int,
    *,
    variant_offset: int = 0,
) -> list[dict[str, Any]]:
    if workload["id"].endswith("agent-prefix-reuse-derived-v1"):
        # The large stable prefix models a persistent agent session. Suffixes vary
        # by turn, allowing the runtime's radix/prefix cache to do real work.
        shared_messages, _ = _prompt_near_tokens(
            tokenizer,
            int(workload["input_tokens"]) - int(workload["uncached_input_tokens"]),
            variant_offset,
        )
        shared_prefix = shared_messages[-1]["content"]
        requests = []
        for index in range(count):
            messages, prompt_tokens = _prompt_near_tokens(
                tokenizer,
                int(workload["input_tokens"]),
                variant_offset + index,
                shared_prefix=shared_prefix,
            )
            requests.append({"messages": messages, "expected_prompt_tokens": prompt_tokens})
        return requests
    requests = []
    for index in range(count):
        messages, prompt_tokens = _prompt_near_tokens(
            tokenizer, int(workload["input_tokens"]), variant_offset + index
        )
        requests.append({"messages": messages, "expected_prompt_tokens": prompt_tokens})
    return requests


async def _one_request(
    client: Any,
    request: dict[str, Any],
    workload: dict[str, Any],
    concurrency: int,
) -> dict[str, Any]:
    started = time.perf_counter()
    first_token = last_token = None
    chunks: list[str] = []
    usage: dict[str, Any] = {}
    status = None
    error = None
    saw_done = False
    finish_reason = None
    payload = {
        "model": MODEL_ID,
        "messages": request["messages"],
        "max_tokens": workload["output_tokens"],
        "stream": True,
        "stream_options": {"include_usage": True},
        "ignore_eos": True,
        "temperature": workload["sampling"]["temperature"],
        "top_p": workload["sampling"]["top_p"],
        "top_k": workload["sampling"]["top_k"],
        "min_p": workload["sampling"]["min_p"],
        "presence_penalty": workload["sampling"]["presence_penalty"],
        "seed": workload["sampling"]["seed"],
        "chat_template_kwargs": {"enable_thinking": False, "preserve_thinking": False},
    }
    try:
        async with client.stream(
            "POST", f"http://127.0.0.1:{PORT}/v1/chat/completions", json=payload
        ) as response:
            status = response.status_code
            response.raise_for_status()
            async for line in response.aiter_lines():
                if not line.startswith("data:"):
                    continue
                raw = line[5:].strip()
                if raw == "[DONE]":
                    saw_done = True
                    continue
                if not raw:
                    continue
                event = json.loads(raw)
                if event.get("usage"):
                    usage = event["usage"]
                choices = event.get("choices") or []
                if not choices:
                    continue
                if choices[0].get("finish_reason") is not None:
                    finish_reason = choices[0]["finish_reason"]
                delta = choices[0].get("delta") or {}
                text = (delta.get("reasoning_content") or delta.get("reasoning") or "") + (
                    delta.get("content") or ""
                )
                if text:
                    now = time.perf_counter()
                    first_token = first_token or now
                    last_token = now
                    chunks.append(text)
    except Exception as exc:  # network/protocol failures are campaign evidence
        error = f"{type(exc).__name__}: {str(exc)[:300]}"
    ended = time.perf_counter()
    completion_tokens = int(usage.get("completion_tokens") or 0)
    prompt_tokens = int(usage.get("prompt_tokens") or 0)
    expected = int(request["expected_prompt_tokens"])
    generation_seconds = (
        last_token - first_token if first_token is not None and last_token is not None else 0
    )
    ttft_ms = (first_token - started) * 1000 if first_token is not None else None
    itl_ms = (
        generation_seconds * 1000 / (completion_tokens - 1)
        if generation_seconds > 0 and completion_tokens > 1
        else None
    )
    success = bool(
        error is None
        and status == 200
        and chunks
        and completion_tokens > 1
        and saw_done
        and finish_reason is not None
        and ttft_ms is not None
        and itl_ms is not None
    )
    return {
        "success": success,
        "concurrency": concurrency,
        "status": status,
        "error": error,
        "prompt_tokens": prompt_tokens,
        "expected_prompt_tokens": expected,
        "prompt_token_match": prompt_tokens == expected,
        "completion_tokens": completion_tokens,
        "saw_done": saw_done,
        "finish_reason": finish_reason,
        "ttft_ms": ttft_ms,
        "itl_ms": itl_ms,
        "latency_ms": (ended - started) * 1000,
        "output_tokens_per_second": completion_tokens / generation_seconds
        if generation_seconds > 0
        else 0,
        "slo_pass": bool(
            success
            and ttft_ms <= float(workload["slo"]["max_ttft_ms"])
            and itl_ms <= float(workload["slo"]["max_itl_ms"])
        ),
        "output_sha256": hashlib.sha256("".join(chunks).encode()).hexdigest() if chunks else None,
    }


async def _quality_gates() -> list[dict[str, Any]]:
    import httpx
    import jsonschema

    base = f"http://127.0.0.1:{PORT}/v1"
    timeout = httpx.Timeout(300, connect=30)
    rows: list[dict[str, Any]] = []
    async with httpx.AsyncClient(timeout=timeout) as client:
        response = await client.get(f"{base}/models")
        model_ids = []
        try:
            model_ids = [row.get("id") for row in response.json().get("data", [])]
        except Exception:
            pass
        rows.append(
            {
                "name": "api_models",
                "passed": response.status_code == 200 and MODEL_ID in model_ids,
            }
        )

        stream_usage = False
        stream_done = False
        stream_finish_reason = False
        async with client.stream(
            "POST",
            f"{base}/chat/completions",
            json={
                "model": MODEL_ID,
                "messages": [{"role": "user", "content": "Reply with ready."}],
                "max_tokens": 16,
                "temperature": 0,
                "stream": True,
                "stream_options": {"include_usage": True},
                "chat_template_kwargs": {"enable_thinking": False},
            },
        ) as response:
            if response.status_code == 200:
                async for line in response.aiter_lines():
                    if not line.startswith("data:"):
                        continue
                    raw = line[5:].strip()
                    if raw == "[DONE]":
                        stream_done = True
                        continue
                    if not raw:
                        continue
                    event = json.loads(raw)
                    stream_usage = stream_usage or bool(event.get("usage"))
                    for choice in event.get("choices") or []:
                        stream_finish_reason = stream_finish_reason or (
                            choice.get("finish_reason") is not None
                        )
        rows.append({"name": "stream_usage", "passed": stream_usage})
        rows.append({"name": "stream_done", "passed": stream_done})
        rows.append({"name": "stream_finish_reason", "passed": stream_finish_reason})

        response = await client.post(
            f"{base}/chat/completions",
            json={
                "model": MODEL_ID,
                "messages": [{"role": "user", "content": "Reply with ready."}],
                "max_tokens": 16,
                "temperature": 0,
                "stream": False,
                "chat_template_kwargs": {"enable_thinking": False},
            },
        )
        buffered_usage = False
        try:
            response.raise_for_status()
            usage = response.json()["usage"]
            buffered_usage = (
                int(usage.get("prompt_tokens") or 0) > 0
                and int(usage.get("completion_tokens") or 0) > 0
                and response.json()["choices"][0].get("finish_reason") is not None
            )
        except Exception:
            pass
        rows.append({"name": "buffered_usage", "passed": buffered_usage})

        response = await client.post(
            f"{base}/chat/completions",
            json={
                "model": MODEL_ID,
                "messages": [
                    {
                        "role": "user",
                        "content": "Think through 17 plus 25, then give the integer.",
                    }
                ],
                "max_tokens": 256,
                "temperature": 0,
                "seed": 20260923,
                "stream": False,
                "chat_template_kwargs": {
                    "enable_thinking": True,
                    "preserve_thinking": True,
                },
            },
        )
        reasoning_output = False
        thinking_semantic = False
        try:
            response.raise_for_status()
            message = response.json()["choices"][0]["message"]
            reasoning = message.get("reasoning_content") or message.get("reasoning") or ""
            content = (message.get("content") or "").strip()
            reasoning_output = bool(reasoning.strip() and content)
            thinking_semantic = reasoning_output and bool(
                re.search(r"(?<!\d)42(?!\d)", content)
            )
        except Exception:
            pass
        rows.append({"name": "reasoning_output", "passed": reasoning_output})
        rows.append({"name": "thinking_semantic", "passed": thinking_semantic})

        schema = {
            "type": "object",
            "additionalProperties": False,
            "required": ["status"],
            "properties": {"status": {"const": "ready"}},
        }
        response = await client.post(
            f"{base}/chat/completions",
            json={
                "model": MODEL_ID,
                "messages": [{"role": "user", "content": "Return status ready as JSON."}],
                "temperature": 0,
                "max_tokens": 64,
                "chat_template_kwargs": {"enable_thinking": False},
                "response_format": {
                    "type": "json_schema",
                    "json_schema": {"name": "status", "strict": True, "schema": schema},
                },
            },
        )
        structured = False
        try:
            response.raise_for_status()
            value = json.loads(response.json()["choices"][0]["message"]["content"])
            jsonschema.validate(value, schema)
            structured = True
        except Exception:
            pass
        rows.append({"name": "structured_output", "passed": structured})

        response = await client.post(
            f"{base}/chat/completions",
            json={
                "model": MODEL_ID,
                "messages": [{"role": "user", "content": "Read src/cache.py."}],
                "tools": [
                    {
                        "type": "function",
                        "function": {
                            "name": "read_file",
                            "description": "Read a file",
                            "parameters": {
                                "type": "object",
                                "properties": {"path": {"type": "string"}},
                                "required": ["path"],
                            },
                        },
                    }
                ],
                "tool_choice": {"type": "function", "function": {"name": "read_file"}},
                "temperature": 0,
                "max_tokens": 96,
                "chat_template_kwargs": {"enable_thinking": False},
            },
        )
        tool = False
        try:
            response.raise_for_status()
            calls = response.json()["choices"][0]["message"].get("tool_calls") or []
            arguments = calls[0]["function"]["arguments"]
            arguments = json.loads(arguments) if isinstance(arguments, str) else arguments
            tool = calls[0]["function"]["name"] == "read_file" and arguments["path"] == "src/cache.py"
        except Exception:
            pass
        rows.append({"name": "tool_call", "passed": tool})

        response = await client.post(
            f"{base}/chat/completions",
            json={
                "model": "infercrane/unknown-model",
                "messages": [{"role": "user", "content": "hello"}],
                "max_tokens": 1,
            },
        )
        rows.append(
            {
                "name": "unknown_model_rejected",
                "passed": 400 <= response.status_code < 500,
            }
        )

        response = await client.post(
            f"{base}/chat/completions",
            content=b"{not-json",
            headers={"Content-Type": "application/json"},
        )
        rows.append(
            {
                "name": "invalid_request_rejected",
                "passed": 400 <= response.status_code < 500,
            }
        )
    return rows


async def _correctness_probes(tokenizer: Any) -> list[dict[str, Any]]:
    import httpx

    prompts = [
        "Return exactly: BREZEL-PROBE-ALPHA",
        "What is 144 divided by 12? Answer with only the integer.",
        "Name the Python function that returns an object's length. Answer with only its name.",
    ]
    rows = []
    async with httpx.AsyncClient(timeout=httpx.Timeout(300, connect=30)) as client:
        for index, prompt in enumerate(prompts):
            response = await client.post(
                f"http://127.0.0.1:{PORT}/v1/chat/completions",
                json={
                    "model": MODEL_ID,
                    "messages": [{"role": "user", "content": prompt}],
                    "temperature": 0,
                    "seed": 20260923 + index,
                    "max_tokens": 64,
                    "chat_template_kwargs": {"enable_thinking": False},
                },
            )
            response.raise_for_status()
            content = response.json()["choices"][0]["message"]["content"]
            rows.append(
                {
                    "id": f"deterministic-{index + 1}",
                    "parity_mode": "exact",
                    "output_sha256": hashlib.sha256(content.encode()).hexdigest(),
                }
            )
        response = await client.post(
            f"http://127.0.0.1:{PORT}/v1/chat/completions",
            json={
                "model": MODEL_ID,
                "messages": [
                    {
                        "role": "user",
                        "content": "Think step by step: what is 23 times 7? Give the final integer.",
                    }
                ],
                "temperature": 0,
                "seed": 20260926,
                "max_tokens": 256,
                "chat_template_kwargs": {
                    "enable_thinking": True,
                    "preserve_thinking": True,
                },
            },
        )
        response.raise_for_status()
        message = response.json()["choices"][0]["message"]
        reasoning = message.get("reasoning_content") or message.get("reasoning") or ""
        content = message.get("content") or ""
        rows.append(
            {
                "id": "deterministic-thinking-1",
                # A reasoning trace is not a stable byte-level contract even
                # at temperature zero. Preserve both hashes for diagnostics,
                # but qualify its externally visible answer semantically.
                "parity_mode": "semantic",
                "semantic_passed": bool(
                    reasoning.strip()
                    and re.search(r"(?<!\d)161(?!\d)", content)
                ),
                "reasoning_sha256": hashlib.sha256(reasoning.encode()).hexdigest(),
                "content_sha256": hashlib.sha256(content.encode()).hexdigest(),
                "output_sha256": hashlib.sha256(
                    json.dumps(
                        {"reasoning": reasoning, "content": content},
                        sort_keys=True,
                        separators=(",", ":"),
                    ).encode()
                ).hexdigest(),
            }
        )

        sentinel = "IC-GDN-STATE-7F31"
        filler = "Repository record: function call result remained stable. " * 2800
        long_prompt = (
            f"Remember this exact sentinel: {sentinel}.\n"
            f"{filler}\nReturn exactly the sentinel and nothing else."
        )
        long_messages = [{"role": "user", "content": long_prompt}]
        long_prompt_tokens = encoded_token_count(
            tokenizer.apply_chat_template(
                long_messages, tokenize=True, add_generation_prompt=True, enable_thinking=False
            )
        )
        response = await client.post(
            f"http://127.0.0.1:{PORT}/v1/chat/completions",
            json={
                "model": MODEL_ID,
                "messages": long_messages,
                "temperature": 0,
                "seed": 20260927,
                "max_tokens": 32,
                "chat_template_kwargs": {"enable_thinking": False},
            },
        )
        response.raise_for_status()
        payload = response.json()
        content = payload["choices"][0]["message"]["content"].strip()
        rows.append(
            {
                "id": "gdn-long-state-sentinel",
                "parity_mode": "exact",
                "semantic_passed": content == sentinel,
                "prompt_tokens": int(payload.get("usage", {}).get("prompt_tokens") or 0),
                "expected_prompt_tokens": long_prompt_tokens,
                "output_sha256": hashlib.sha256(content.encode()).hexdigest(),
            }
        )
    return rows


async def _prefix_cache_correctness(
    tokenizer: Any, workload: dict[str, Any], *, flush_path: str = "/flush_cache"
) -> dict[str, Any]:
    """Prove that cache reuse preserves bytes and is visible in runtime telemetry."""

    import httpx

    if not workload["id"].endswith("agent-prefix-reuse-derived-v1"):
        return {"applicable": False, "gates": [], "cold": {}, "warm": {}, "cache_metrics": {}}
    deterministic = {
        **workload,
        "output_tokens": 64,
        "sampling": {**workload["sampling"], "temperature": 0.0, "top_p": 1.0, "top_k": 1, "presence_penalty": 0.0},
    }
    request = _workload_requests(tokenizer, deterministic, 1, variant_offset=8_000_000)[0]
    timeout = httpx.Timeout(1800, connect=30)
    async with httpx.AsyncClient(timeout=timeout) as client:
        flush = await client.post(f"http://127.0.0.1:{PORT}{flush_path}")
        flush.raise_for_status()
        before_cold = _metric_snapshot()
        cold = await _one_request(client, request, deterministic, 1)
        after_cold = _metric_snapshot()
        cold_metrics = _metric_delta(before_cold, after_cold)
        warm = await _one_request(client, request, deterministic, 1)
        after_warm = _metric_snapshot()
        warm_metrics = _metric_delta(after_cold, after_warm)

    cache_metrics = {
        key: {"cold": cold_metrics.get(key, 0.0), "warm": warm_metrics.get(key, 0.0)}
        for key in sorted(cold_metrics.keys() | warm_metrics.keys())
        if any(marker in key.lower() for marker in ("cache_hit", "cached_token", "radix"))
    }
    reuse_observed = any(
        row["warm"] > row["cold"] or row["warm"] > 0 and row["cold"] == 0
        for row in cache_metrics.values()
    )
    exact = bool(
        cold.get("success")
        and warm.get("success")
        and cold.get("output_sha256") == warm.get("output_sha256")
        and cold.get("prompt_tokens") == warm.get("prompt_tokens")
        and cold.get("prompt_token_match")
        and warm.get("prompt_token_match")
    )
    return {
        "applicable": True,
        "gates": [
            {"name": "prefix_cache_output_parity", "passed": exact},
            {"name": "prefix_cache_reuse_observed", "passed": reuse_observed},
        ],
        "cold": cold,
        "warm": warm,
        "cache_metrics": cache_metrics,
    }


async def _benchmark_profile(
    tokenizer: Any,
    workload: dict[str, Any],
    requests_per_lane: int,
    minimum_lane_seconds: float,
    *,
    flush_path: str = "/flush_cache",
) -> tuple[list[dict[str, Any]], dict[str, float], list[dict[str, Any]]]:
    import httpx

    timeout = httpx.Timeout(1800, connect=30, pool=60)
    summaries = []
    samples = []
    aggregate_metrics: dict[str, float] = {}
    async with httpx.AsyncClient(timeout=timeout) as client:
        warmup = _workload_requests(tokenizer, workload, 2, variant_offset=1_000_000)
        for request in warmup:
            await _one_request(client, request, {**workload, "output_tokens": 32}, 1)
        for lane_index, concurrency in enumerate(workload["concurrency_lanes"]):
            # Every lane starts from a clean cache. Explicit session-prefix
            # reuse is then rebuilt inside that lane only.
            flush = await client.post(f"http://127.0.0.1:{PORT}{flush_path}")
            flush.raise_for_status()
            target_requests = max(requests_per_lane, concurrency * 2)
            print(
                f"benchmark lane workload={workload['id']} concurrency={concurrency} "
                f"minimum_requests={target_requests} minimum_seconds={minimum_lane_seconds}",
                flush=True,
            )
            semaphore = asyncio.Semaphore(concurrency)
            before = _metric_snapshot()
            started = time.perf_counter()
            rows: list[dict[str, Any]] = []
            batch_index = 0

            async def guarded(request: dict[str, Any]) -> dict[str, Any]:
                async with semaphore:
                    return await _one_request(client, request, workload, concurrency)

            while (
                len(rows) < target_requests
                or time.perf_counter() - started < minimum_lane_seconds
            ):
                remaining = target_requests - len(rows)
                batch_size = (
                    remaining
                    if remaining > 0
                    else max(concurrency * 4, SCREENING_REQUESTS_PER_LANE)
                )
                requests = _workload_requests(
                    tokenizer,
                    workload,
                    batch_size,
                    variant_offset=(lane_index + 1) * 1_000_000
                    + batch_index * 10_000,
                )
                rows.extend(
                    await asyncio.gather(*(guarded(request) for request in requests))
                )
                batch_index += 1
            wall = time.perf_counter() - started
            after = _metric_snapshot()
            delta = _metric_delta(before, after)
            aggregate_metrics = merge_prometheus_measurements(aggregate_metrics, delta)
            summaries.append(summarize_lane(rows, wall, GPU_HOURLY_COST_USD))
            samples.extend(rows)
    return summaries, aggregate_metrics, samples


def _read_trace(path: Path) -> dict[str, Any]:
    opener = gzip.open if path.suffix == ".gz" else open
    with opener(path, "rt", encoding="utf-8") as handle:
        return json.load(handle)


async def _capture_runtime_profiles(
    tokenizer: Any,
    workload: dict[str, Any],
    output_dir: Path,
) -> dict[str, Any]:
    """Capture workload-separated prefill and decode profiles.

    SGLang's stage profiler has known edge cases with speculative verification,
    so each directory is labelled by the driving workload and profiling is
    stopped explicitly after the request completes.
    """

    import httpx

    output_dir.mkdir(parents=True, exist_ok=True)
    timeout = httpx.Timeout(1800, connect=30)
    captures = {}
    async with httpx.AsyncClient(timeout=timeout) as client:
        max_workload_concurrency = max(int(value) for value in workload["concurrency_lanes"])
        cases: dict[str, tuple[dict[str, Any], int]] = {
            "prefill": (
                {
                    **workload,
                    "output_tokens": 1,
                    "slo": {"max_ttft_ms": 1e12, "max_itl_ms": 1e12},
                },
                1,
            ),
            "decode": (
                {
                    **workload,
                    "input_tokens": min(256, int(workload["input_tokens"])),
                    "output_tokens": max(128, min(512, int(workload["output_tokens"]))),
                    "slo": {"max_ttft_ms": 1e12, "max_itl_ms": 1e12},
                },
                1,
            ),
        }
        if max_workload_concurrency > 1:
            # A c1 trace cannot explain the production lane where batching,
            # verification width, GEMM shapes, and scheduler pressure differ.
            # Bound the capture at c16 to keep trace size and GPU cost finite.
            cases["decode_saturation"] = (
                {
                    **workload,
                    "input_tokens": min(256, int(workload["input_tokens"])),
                    "output_tokens": max(128, min(256, int(workload["output_tokens"]))),
                    "slo": {"max_ttft_ms": 1e12, "max_itl_ms": 1e12},
                },
                min(16, max_workload_concurrency),
            )
        for index, (stage, (profile_workload, concurrency)) in enumerate(cases.items()):
            stage_dir = output_dir / stage
            stage_dir.mkdir(parents=True, exist_ok=False)
            warmups = _workload_requests(
                tokenizer,
                profile_workload,
                max(2, concurrency),
                variant_offset=7_000_000 + index * 100,
            )
            await asyncio.gather(
                *(
                    _one_request(client, request, profile_workload, concurrency)
                    for request in warmups
                )
            )
            flush = await client.post(f"http://127.0.0.1:{PORT}/flush_cache")
            flush.raise_for_status()
            start = await client.post(
                f"http://127.0.0.1:{PORT}/start_profile",
                json={
                    "output_dir": str(stage_dir),
                    "activities": ["CPU", "GPU"],
                    "with_stack": False,
                    "record_shapes": True,
                    "profile_by_stage": False,
                    "profile_prefix": f"infercrane-{stage}",
                },
            )
            start.raise_for_status()
            requests = _workload_requests(
                tokenizer,
                profile_workload,
                concurrency,
                variant_offset=7_100_000 + index * 100,
            )
            samples = await asyncio.gather(
                *(
                    _one_request(client, request, profile_workload, concurrency)
                    for request in requests
                )
            )
            stop = await client.post(f"http://127.0.0.1:{PORT}/stop_profile")
            stop.raise_for_status()
            trace_paths = sorted(
                [*stage_dir.rglob("*.trace.json"), *stage_dir.rglob("*.trace.json.gz")]
            )
            if not trace_paths:
                raise RuntimeError(f"SGLang produced no {stage} torch profile")
            summaries = [summarize_torch_trace(_read_trace(path)) for path in trace_paths]
            # TP=1 is required, so exactly one GPU trace is expected. Retain all
            # paths/digests in case the runtime also writes an auxiliary trace.
            primary = max(summaries, key=lambda row: row["total_gpu_kernel_time_us"])
            captures[stage] = {
                "concurrency": concurrency,
                "request": samples[0],
                "requests": samples,
                "profile": primary,
                "custom_kernel_gate": custom_kernel_gate(primary),
                "artifacts": [
                    {
                        "path": str(path),
                        "sha256": hashlib.sha256(path.read_bytes()).hexdigest(),
                    }
                    for path in trace_paths
                ],
            }
    return {
        "schema_version": "infercrane.dev/runtime-profile-bundle/v1",
        "method": "workload_separated_torch_profiler",
        "boundary": (
            "Profiles identify candidate hotspots. They do not qualify a custom kernel; "
            "promotion requires target-GPU parity, microbenchmarks, and end-to-end evidence."
        ),
        "captures": captures,
    }


@app.function(image=image, cpu=2, memory=4096, timeout=10 * 60)
def preflight() -> dict[str, Any]:
    patch_receipt = _apply_gdn_precision_patch()
    version = subprocess.check_output(
        [_python(), "-c", "import sglang; print(sglang.__version__)"], text=True, timeout=60
    ).strip()
    help_text = subprocess.check_output(
        [_python(), "-m", "sglang.launch_server", "--help"], text=True, timeout=180
    )
    required = [
        "--kv-cache-dtype",
        "--speculative-algorithm",
        "--speculative-num-steps",
        "--speculative-num-draft-tokens",
        "--speculative-adaptive",
        "--schedule-policy",
        "--context-length",
        "--chunked-prefill-size",
        "--max-prefill-tokens",
        "--cuda-graph-max-bs",
        "--enable-linear-replayssm",
        "--enable-linear-replayssm-spec",
    ]
    relevant_options = sorted(
        set(
            re.findall(
                r"--(?:cuda-graph|chunked-prefill|max-prefill|fp8|gemm|linear-attn|linear-replayssm|enable-linear-replayssm|gdn|speculative)[a-z0-9-]*",
                help_text,
            )
        )
    )
    help_lines = help_text.splitlines()
    option_help = {}
    for option in relevant_options:
        matches = [
            " ".join(help_lines[index : min(index + 3, len(help_lines))]).strip()
            for index, line in enumerate(help_lines)
            if option in line
        ]
        option_help[option] = matches[:3]
    return {
        "version": version,
        "image": SGLANG_IMAGE,
        "required_flags": {flag: flag in help_text for flag in required},
        "relevant_options": relevant_options,
        "option_help": option_help,
        "python": _python(),
        "gdn_precision_patch": patch_receipt,
        "dependency_versions": _runtime_dependency_versions(),
    }


@app.function(
    image=image,
    gpu="H200",
    cpu=4,
    memory=16384,
    timeout=60 * 60,
    startup_timeout=20 * 60,
)
def screen_gdn_kernel() -> dict[str, Any]:
    completed = subprocess.run(
        [_python(), "/opt/infercrane/gdn_h200_kernel_lab.py"],
        check=True,
        capture_output=True,
        text=True,
        timeout=55 * 60,
    )
    return json.loads(completed.stdout)


@app.function(
    image=image,
    gpu="H200",
    cpu=4,
    memory=16384,
    timeout=60 * 60,
    startup_timeout=20 * 60,
)
def screen_state_scatter_kernel() -> dict[str, Any]:
    completed = subprocess.run(
        [_python(), "/opt/infercrane/state_scatter_h200_kernel_lab.py"],
        check=True,
        capture_output=True,
        text=True,
        timeout=55 * 60,
    )
    return json.loads(completed.stdout)


@app.function(image=vllm_image, gpu=GPU_REQUEST, cpu=2, memory=4096, timeout=10 * 60)
def vllm_preflight() -> dict[str, Any]:
    candidates = sorted(
        str(path)
        for pattern in (
            "/opt/venv/lib/python*/site-packages/vllm",
            "/usr/local/lib/python*/site-packages/vllm",
            "/usr/local/lib/python*/dist-packages/vllm",
        )
        for path in Path("/").glob(pattern.removeprefix("/"))
    )
    if not candidates:
        discovered = subprocess.run(
            [
                "/usr/bin/find",
                "/opt",
                "/usr",
                "/vllm-workspace",
                "-maxdepth",
                "6",
                "-type",
                "d",
                "-name",
                "vllm",
            ],
            capture_output=True,
            text=True,
            timeout=60,
            check=False,
        )
        return {
            "version": None,
            "image": VLLM_IMAGE,
            "python": sys.executable,
            "sys_path": sys.path,
            "env_path": os.environ.get("PATH"),
            "discovered_packages": discovered.stdout.splitlines(),
            "find_error": discovered.stderr,
        }
    package_root = str(Path(candidates[0]).parent)
    environment = os.environ.copy()
    environment["PYTHONPATH"] = os.pathsep.join(
        [package_root, environment.get("PYTHONPATH", "")]
    ).rstrip(os.pathsep)
    version = subprocess.check_output(
        [sys.executable, "-c", "import vllm; print(vllm.__version__)"],
        text=True,
        timeout=60,
        env=environment,
    ).strip()
    dependency_probe = json.loads(
        subprocess.check_output(
            [
                sys.executable,
                "-c",
                (
                    "import importlib.metadata as m,json; import cutlass; "
                    "print(json.dumps({'cutlass_module': cutlass.__file__, "
                    "'cutlass_dsl': m.version('nvidia-cutlass-dsl'), "
                    "'flashinfer': m.version('flashinfer-python')}))"
                ),
            ],
            text=True,
            timeout=60,
            env=environment,
        )
    )
    help_text = subprocess.check_output(
        [sys.executable, "-m", "vllm.entrypoints.cli.main", "serve", "--help=all"],
        text=True,
        timeout=180,
        env=environment,
    )
    relevant_options = sorted(
        set(
            re.findall(
                r"--(?:speculative|num-speculative|kv-cache|gpu-memory|max-model|enable-prefix|reasoning|tool-call|chat-template|compilation|cuda-graph)[a-z0-9-]*",
                help_text,
            )
        )
    )
    help_lines = help_text.splitlines()
    option_help = {}
    for option in relevant_options:
        matches = [
            " ".join(help_lines[index : min(index + 4, len(help_lines))]).strip()
            for index, line in enumerate(help_lines)
            if option in line
        ]
        option_help[option] = matches[:3]
    return {
        "version": version,
        "image": VLLM_IMAGE,
        "relevant_options": relevant_options,
        "option_help": option_help,
        "python": sys.executable,
        "package_root": package_root,
        "dependency_probe": dependency_probe,
    }


@app.function(
    image=vllm_image,
    gpu=GPU_REQUEST,
    cpu=8,
    memory=131072,
    timeout=90 * 60,
    startup_timeout=30 * 60,
    volumes={
        "/model-cache": model_cache,
        "/compile-cache": compile_cache,
        "/results": results,
    },
)
def screen_vllm_candidate(
    candidate_id: str,
    workload_name: str = "public-decode-saturation",
    requests_per_lane: int = SCREENING_REQUESTS_PER_LANE,
    minimum_lane_seconds: float = 0.0,
    run_index: int = 1,
) -> dict[str, Any]:
    if candidate_id not in VLLM_CANDIDATES:
        raise ValueError(f"unknown vLLM candidate {candidate_id}")
    if workload_name not in WORKLOADS:
        raise ValueError(f"unknown workload {workload_name}")
    candidate = VLLM_CANDIDATES[candidate_id]
    if workload_name not in candidate["workloads"]:
        raise ValueError(f"candidate {candidate_id} does not support {workload_name}")
    workload = WORKLOADS[workload_name]
    inventory = _gpu_inventory()
    stamp = datetime.now(UTC).strftime("%Y%m%dT%H%M%SZ")
    run_dir = Path("/results") / stamp / candidate_id / workload_name
    run_dir.mkdir(parents=True, exist_ok=False)
    log_path = run_dir / "server.log"
    log = log_path.open("w", encoding="utf-8")
    environment = os.environ.copy()
    environment.update(
        {
            "HF_HOME": "/model-cache",
            "HF_HUB_CACHE": "/model-cache/hub",
            "TRANSFORMERS_CACHE": "/model-cache/transformers",
            "VLLM_SERVER_DEV_MODE": "1",
            "VLLM_CONFIG_ROOT": "/compile-cache/vllm",
        }
    )
    command = [
        sys.executable,
        "-m",
        "vllm.entrypoints.cli.main",
        "serve",
        *VLLM_BASE_ARGS,
        *candidate["args"],
    ]
    print(
        f"starting candidate={candidate_id} workload={workload_name} gpu={inventory[0]['name']}",
        flush=True,
    )
    launch_started = time.perf_counter()
    process = subprocess.Popen(
        command, stdout=log, stderr=subprocess.STDOUT, env=environment, text=True
    )
    try:
        _wait_for_server(process, log_path)
        startup_seconds = time.perf_counter() - launch_started
        print(
            f"runtime ready candidate={candidate_id} startup_seconds={startup_seconds:.3f}",
            flush=True,
        )
        from transformers import AutoTokenizer

        tokenizer = AutoTokenizer.from_pretrained(
            MODEL_ID, revision=MODEL_REVISION, cache_dir="/model-cache/hub"
        )
        lanes, metrics, samples = asyncio.run(
            _benchmark_profile(
                tokenizer,
                workload,
                requests_per_lane,
                minimum_lane_seconds,
                flush_path="/reset_prefix_cache",
            )
        )
        quality = asyncio.run(_quality_gates())
        quality.append(_speculation_health_gate(candidate, metrics))
        prefix_cache = asyncio.run(
            _prefix_cache_correctness(tokenizer, workload, flush_path="/reset_prefix_cache")
        )
        quality.extend(prefix_cache["gates"])
        correctness_probes = asyncio.run(_correctness_probes(tokenizer))
        quality.append(
            {
                "name": "gdn_long_state_semantics",
                "passed": any(
                    probe.get("id") == "gdn-long-state-sentinel"
                    and probe.get("semantic_passed")
                    and probe.get("prompt_tokens") == probe.get("expected_prompt_tokens")
                    for probe in correctness_probes
                ),
            }
        )
        total_requests = sum(lane["requests"] for lane in lanes)
        successful = sum(lane["successful_requests"] for lane in lanes)
        result = {
            "schema_version": "infercrane.dev/modal-workload-screen/v1",
            "created_at": datetime.now(UTC).isoformat(),
            "candidate_id": candidate_id,
            "run_index": run_index,
            "runtime_id": candidate["runtime_id"],
            "recipe": {
                "image": VLLM_IMAGE,
                "model": MODEL_ID,
                "model_revision": MODEL_REVISION,
                "gpu": GPU,
                "args": VLLM_BASE_ARGS + candidate["args"],
                "harness_digest": _harness_digest(),
                "command": command,
                "dependency_versions": _runtime_dependency_versions(),
            },
            "workload_name": workload_name,
            "workload": workload,
            "gpu_inventory": inventory,
            "quality": quality,
            "correctness_probes": correctness_probes,
            "lanes": lanes,
            "runtime_metric_delta": metrics,
            "prefix_cache_correctness": prefix_cache,
            "request_samples": samples,
            "startup_seconds": startup_seconds,
            "error_rate": 1 - successful / total_requests,
            "prompt_token_mismatch_rate": sum(
                lane["prompt_token_mismatch_count"] for lane in lanes
            )
            / total_requests,
            "server_log": str(log_path),
            "runtime_profile": None,
        }
        (run_dir / "result.json").write_text(
            json.dumps(result, indent=2, sort_keys=True) + "\n"
        )
        results.commit()
        return result
    finally:
        if process.poll() is None:
            process.send_signal(signal.SIGTERM)
            try:
                process.wait(timeout=30)
            except subprocess.TimeoutExpired:
                process.kill()
        log.close()
        model_cache.commit()
        compile_cache.commit()
        results.commit()


@app.function(
    image=image,
    gpu=GPU_REQUEST,
    cpu=8,
    memory=131072,
    timeout=90 * 60,
    startup_timeout=30 * 60,
    volumes={
        "/model-cache": model_cache,
        "/compile-cache": compile_cache,
        "/results": results,
    },
)
def screen_candidate(
    candidate_id: str,
    workload_name: str = "public-interactive",
    requests_per_lane: int = SCREENING_REQUESTS_PER_LANE,
    minimum_lane_seconds: float = 0.0,
    run_index: int = 1,
    capture_profile: bool = False,
) -> dict[str, Any]:
    if candidate_id not in CANDIDATES:
        raise ValueError(f"unknown candidate {candidate_id}")
    if workload_name not in WORKLOADS:
        raise ValueError(f"unknown workload {workload_name}")
    candidate = CANDIDATES[candidate_id]
    workload = WORKLOADS[workload_name]
    patch_receipt = _apply_gdn_precision_patch()
    inventory = _gpu_inventory()
    stamp = datetime.now(UTC).strftime("%Y%m%dT%H%M%SZ")
    run_dir = Path("/results") / stamp / candidate_id / workload_name
    run_dir.mkdir(parents=True, exist_ok=False)
    log_path = run_dir / "server.log"
    log = log_path.open("w", encoding="utf-8")
    environment = os.environ.copy()
    environment.update(
        {
            "HF_HOME": "/model-cache",
            "HF_HUB_CACHE": "/model-cache/hub",
            "TRANSFORMERS_CACHE": "/model-cache/transformers",
            "SGLANG_CACHE_DIR": "/compile-cache",
            "TRITON_CACHE_DIR": "/compile-cache/triton",
            "TORCHINDUCTOR_CACHE_DIR": "/compile-cache/inductor",
            "DG_JIT_CACHE_DIR": "/compile-cache/deep-gemm",
            "SGLANG_DG_CACHE_DIR": "/compile-cache/deep-gemm",
            "FLASHINFER_CACHE_DIR": "/compile-cache/flashinfer",
            "FLASHINFER_WORKSPACE_BASE": "/compile-cache/flashinfer-workspace",
            "TVM_FFI_CACHE_DIR": "/compile-cache/tvm-ffi",
            "TILELANG_CACHE_DIR": "/compile-cache/tilelang",
            "CUTE_DSL_CACHE_DIR": "/compile-cache/cute-dsl",
            "CUDA_CACHE_PATH": "/compile-cache/cuda",
            "SGLANG_TORCH_PROFILER_DIR": str(run_dir / "profiles"),
        }
    )
    cache_before = _compile_cache_inventory()
    command = [_python(), "-m", "sglang.launch_server", *BASE_ARGS, *candidate["args"]]
    print(
        f"starting candidate={candidate_id} workload={workload_name} gpu={inventory[0]['name']}",
        flush=True,
    )
    launch_started = time.perf_counter()
    process = subprocess.Popen(
        command, stdout=log, stderr=subprocess.STDOUT, env=environment, text=True
    )
    try:
        _wait_for_server(process, log_path)
        startup_seconds = time.perf_counter() - launch_started
        print(
            f"runtime ready candidate={candidate_id} startup_seconds={startup_seconds:.3f}",
            flush=True,
        )
        from transformers import AutoTokenizer

        tokenizer = AutoTokenizer.from_pretrained(
            MODEL_ID, revision=MODEL_REVISION, cache_dir="/model-cache/hub"
        )
        lanes, metrics, samples = asyncio.run(
            _benchmark_profile(
                tokenizer, workload, requests_per_lane, minimum_lane_seconds
            )
        )
        quality = asyncio.run(_quality_gates())
        quality.append(_speculation_health_gate(candidate, metrics))
        prefix_cache = asyncio.run(_prefix_cache_correctness(tokenizer, workload))
        quality.extend(prefix_cache["gates"])
        correctness_probes = asyncio.run(_correctness_probes(tokenizer))
        quality.append(
            {
                "name": "gdn_long_state_semantics",
                "passed": any(
                    probe.get("id") == "gdn-long-state-sentinel"
                    and probe.get("semantic_passed")
                    and probe.get("prompt_tokens") == probe.get("expected_prompt_tokens")
                    for probe in correctness_probes
                ),
            }
        )
        runtime_profile = (
            asyncio.run(_capture_runtime_profiles(tokenizer, workload, run_dir / "profiles"))
            if capture_profile
            else None
        )
        total_requests = sum(lane["requests"] for lane in lanes)
        successful = sum(lane["successful_requests"] for lane in lanes)
        # Prompt integrity is reported by the server for every request. The
        # summary currently fails closed if any measured request did not return.
        result = {
            "schema_version": "infercrane.dev/modal-workload-screen/v1",
            "created_at": datetime.now(UTC).isoformat(),
            "candidate_id": candidate_id,
            "run_index": run_index,
            "runtime_id": candidate["runtime_id"],
            "recipe": {
                "image": SGLANG_IMAGE,
                "model": MODEL_ID,
                "model_revision": MODEL_REVISION,
                "gpu": GPU,
                "args": BASE_ARGS + candidate["args"],
                "harness_digest": _harness_digest(),
                "command": command,
            },
            "workload_name": workload_name,
            "workload": workload,
            "gpu_inventory": inventory,
            "quality": quality,
            "correctness_probes": correctness_probes,
            "gdn_precision_patch": patch_receipt,
            "lanes": lanes,
            "runtime_metric_delta": metrics,
            "prefix_cache_correctness": prefix_cache,
            "request_samples": samples,
            "startup_seconds": startup_seconds,
            "startup_breakdown": parse_sglang_startup(log_path.read_text(errors="replace")),
            "compile_cache": {"before": cache_before, "after": _compile_cache_inventory()},
            "error_rate": 1 - successful / total_requests,
            "prompt_token_mismatch_rate": sum(
                lane["prompt_token_mismatch_count"] for lane in lanes
            )
            / total_requests,
            "server_log": str(log_path),
            "runtime_profile": runtime_profile,
        }
        (run_dir / "result.json").write_text(json.dumps(result, indent=2, sort_keys=True) + "\n")
        results.commit()
        return result
    finally:
        if process.poll() is None:
            process.send_signal(signal.SIGTERM)
            try:
                process.wait(timeout=30)
            except subprocess.TimeoutExpired:
                process.kill()
        log.close()
        model_cache.commit()
        compile_cache.commit()
        results.commit()


def _openrouter_payload() -> dict[str, Any]:
    key = os.environ.get("OPENROUTER_API_KEY", "").strip()
    if not key:
        key_path = Path.home() / ".config" / "infercrane" / "openrouter-key"
        if key_path.exists():
            key = key_path.read_text().strip()
    headers = {"Authorization": f"Bearer {key}"} if key else {}
    request = urllib.request.Request(
        f"https://openrouter.ai/api/v1/models/{MODEL_SLUG}/endpoints", headers=headers
    )
    with urllib.request.urlopen(request, timeout=30) as response:
        return json.load(response)


def _safe_stamp() -> str:
    return datetime.now(UTC).strftime("%Y-%m-%dT%H%M%SZ")


def _run_vllm_campaign(
    *,
    candidates: str,
    workload: str,
    evidence_level: str,
    independent_runs: int,
    parallel: bool,
    output_dir: str,
) -> None:
    selected = (
        [
            candidate_id
            for candidate_id, candidate in VLLM_CANDIDATES.items()
            if workload in candidate["workloads"]
        ]
        if candidates == "all"
        else [value.strip() for value in candidates.split(",")]
    )
    unknown = set(selected) - VLLM_CANDIDATES.keys()
    if unknown:
        raise ValueError(f"unknown vLLM candidates: {sorted(unknown)}")
    if workload not in WORKLOADS:
        raise ValueError(f"unknown workload: {workload}")
    incompatible = [
        candidate_id
        for candidate_id in selected
        if workload not in VLLM_CANDIDATES[candidate_id]["workloads"]
    ]
    if incompatible:
        raise ValueError(f"candidates are not applicable to {workload}: {incompatible}")
    evidence_floors = {
        "screening": (SCREENING_REQUESTS_PER_LANE, 1, 0.0),
        "qualification": (100, 2, 300.0),
        "public": (300, 3, 600.0),
    }
    if evidence_level not in evidence_floors:
        raise ValueError("evidence_level must be screening, qualification, or public")
    requests_per_lane, minimum_runs, minimum_lane_seconds = evidence_floors[
        evidence_level
    ]
    run_count = independent_runs or minimum_runs
    if run_count < minimum_runs:
        raise ValueError(
            f"{evidence_level} requires at least {minimum_runs} independent runs"
        )
    if "vllm-0300-control" not in selected:
        raise ValueError("vLLM campaigns must include vllm-0300-control for parity")
    jobs = [
        (
            candidate_id,
            workload,
            requests_per_lane,
            minimum_lane_seconds,
            run_index,
        )
        for candidate_id in selected
        for run_index in range(1, run_count + 1)
    ]
    for job in jobs:
        print(
            f"{evidence_level} {job[0]} on {workload} run={job[4]}/{run_count}",
            flush=True,
        )
    if parallel:
        outcomes = list(
            screen_vllm_candidate.starmap(
                jobs, order_outputs=True, return_exceptions=True
            )
        )
    else:
        outcomes = [screen_vllm_candidate.remote(*job) for job in jobs]
    run_results = []
    failures = []
    for job, outcome in zip(jobs, outcomes, strict=True):
        if isinstance(outcome, BaseException):
            failures.append(
                {
                    "candidate_id": job[0],
                    "workload": job[1],
                    "run_index": job[4],
                    "error_type": type(outcome).__name__,
                    "error": str(outcome)[:4000],
                }
            )
        else:
            run_results.append(outcome)
    destination = ROOT / output_dir
    destination.mkdir(parents=True, exist_ok=True)
    stamp = _safe_stamp()
    failures_path = None
    if failures:
        failures_path = destination / f"qwen38-vllm-{workload}-modal-{stamp}-failures.json"
        failures_path.write_text(json.dumps(failures, indent=2, sort_keys=True) + "\n")
    if not run_results:
        raise RuntimeError(
            f"all vLLM candidate runs failed; evidence={failures_path}: {failures}"
        )
    candidate_results = merge_candidate_runs(
        run_results, hourly_cost_usd=GPU_HOURLY_COST_USD
    )
    for result in candidate_results:
        for lane in result["lanes"]:
            lane["economics"] = project_lane_economics(
                lane,
                hourly_cost_usd=GPU_HOURLY_COST_USD,
                input_price_usd_per_million=LAUNCH_INPUT_PRICE_USD_PER_MILLION,
                output_price_usd_per_million=LAUNCH_OUTPUT_PRICE_USD_PER_MILLION,
            )
    apply_runtime_parity(
        candidate_results, reference_candidate_id="vllm-0300-control"
    )
    evidence = build_evidence(
        candidate_results,
        workload=WORKLOADS[workload],
        evidence_level=evidence_level,
    )
    paths = {
        "raw": destination / f"qwen38-vllm-{workload}-modal-{stamp}-raw.json",
        "evidence": destination / f"qwen38-vllm-{workload}-modal-{stamp}.json",
    }
    paths["raw"].write_text(
        json.dumps(run_results, indent=2, sort_keys=True) + "\n"
    )
    paths["evidence"].write_text(
        json.dumps(evidence, indent=2, sort_keys=True) + "\n"
    )
    if failures_path is not None:
        paths["failures"] = failures_path
    print(json.dumps({key: str(value) for key, value in paths.items()}, indent=2))


@app.local_entrypoint()
def main(
    action: str = "screen",
    candidates: str = "all",
    workload: str = "public-interactive",
    evidence_level: str = "screening",
    independent_runs: int = 0,
    parallel: bool = True,
    output_dir: str = "docs/testing/evidence",
) -> None:
    if action == "preflight":
        print(json.dumps(preflight.remote(), indent=2, sort_keys=True))
        return
    if action == "vllm-preflight":
        print(json.dumps(vllm_preflight.remote(), indent=2, sort_keys=True))
        return
    if action == "vllm-screen":
        _run_vllm_campaign(
            candidates=candidates,
            workload=workload,
            evidence_level=evidence_level,
            independent_runs=independent_runs,
            parallel=parallel,
            output_dir=output_dir,
        )
        return
    if action == "gdn-kernel-lab":
        destination = ROOT / output_dir
        destination.mkdir(parents=True, exist_ok=True)
        receipt = screen_gdn_kernel.remote()
        receipt_path = destination / f"qwen38-gdn-kernel-h200-{_safe_stamp()}.json"
        receipt_path.write_text(json.dumps(receipt, indent=2, sort_keys=True) + "\n")
        print(json.dumps({"receipt": str(receipt_path)}, indent=2))
        return
    if action == "state-scatter-kernel-lab":
        destination = ROOT / output_dir
        destination.mkdir(parents=True, exist_ok=True)
        receipt = screen_state_scatter_kernel.remote()
        receipt_path = destination / f"qwen38-state-scatter-kernel-h200-{_safe_stamp()}.json"
        receipt_path.write_text(json.dumps(receipt, indent=2, sort_keys=True) + "\n")
        print(json.dumps({"receipt": str(receipt_path)}, indent=2))
        return
    if action not in {"screen", "profile"}:
        raise ValueError(
            "action must be preflight, vllm-preflight, vllm-screen, "
            "gdn-kernel-lab, state-scatter-kernel-lab, screen, or profile"
        )
    selected = (
        [
            candidate_id
            for candidate_id, candidate in CANDIDATES.items()
            if workload in candidate["workloads"]
        ]
        if candidates == "all"
        else [value.strip() for value in candidates.split(",")]
    )
    unknown = set(selected) - CANDIDATES.keys()
    if unknown:
        raise ValueError(f"unknown candidates: {sorted(unknown)}")
    if workload not in WORKLOADS:
        raise ValueError(f"unknown workload: {workload}")
    evidence_floors = {
        "screening": (SCREENING_REQUESTS_PER_LANE, 1, 0.0),
        "qualification": (100, 2, 300.0),
        "public": (300, 3, 600.0),
    }
    if evidence_level not in evidence_floors:
        raise ValueError("evidence_level must be screening, qualification, or public")
    requests_per_lane, minimum_runs, minimum_lane_seconds = evidence_floors[evidence_level]
    run_count = independent_runs or minimum_runs
    if run_count < minimum_runs:
        raise ValueError(
            f"{evidence_level} requires at least {minimum_runs} independent runs"
        )
    incompatible = [
        candidate_id
        for candidate_id in selected
        if workload not in CANDIDATES[candidate_id]["workloads"]
    ]
    if incompatible:
        raise ValueError(f"candidates are not applicable to {workload}: {incompatible}")
    if action == "profile" and (len(selected) != 1 or run_count != 1):
        raise ValueError("profile requires exactly one candidate and one screening run")
    snapshot = openrouter_snapshot(_openrouter_payload(), captured_at=datetime.now(UTC).isoformat())
    jobs = []
    for candidate_id in selected:
        for run_index in range(1, run_count + 1):
            print(
                f"{evidence_level} {candidate_id} on {workload} run={run_index}/{run_count}",
                flush=True,
            )
            jobs.append(
                (
                    candidate_id,
                    workload,
                    requests_per_lane,
                    minimum_lane_seconds,
                    run_index,
                    action == "profile",
                )
            )
    if parallel:
        outcomes = list(
            screen_candidate.starmap(
                jobs, order_outputs=True, return_exceptions=True
            )
        )
    else:
        outcomes = [screen_candidate.remote(*job) for job in jobs]
    run_results = []
    failures = []
    for job, outcome in zip(jobs, outcomes, strict=True):
        if isinstance(outcome, BaseException):
            failures.append(
                {
                    "candidate_id": job[0],
                    "workload": job[1],
                    "run_index": job[4],
                    "error_type": type(outcome).__name__,
                    "error": str(outcome)[:4000],
                }
            )
        else:
            run_results.append(outcome)
    destination = ROOT / output_dir
    destination.mkdir(parents=True, exist_ok=True)
    stamp = _safe_stamp()
    targets_path = destination / f"qwen38-openrouter-targets-{stamp}.json"
    targets_path.write_text(json.dumps(snapshot, indent=2, sort_keys=True) + "\n")
    failures_path = None
    if failures:
        failures_path = destination / f"qwen38-{workload}-modal-{stamp}-failures.json"
        failures_path.write_text(json.dumps(failures, indent=2, sort_keys=True) + "\n")
    if not run_results:
        raise RuntimeError(
            f"all candidate runs failed; evidence={failures_path}: {failures}"
        )
    candidate_results = merge_candidate_runs(
        run_results, hourly_cost_usd=GPU_HOURLY_COST_USD
    )
    for result in candidate_results:
        for lane in result["lanes"]:
            lane["economics"] = project_lane_economics(
                lane,
                hourly_cost_usd=GPU_HOURLY_COST_USD,
                input_price_usd_per_million=LAUNCH_INPUT_PRICE_USD_PER_MILLION,
                output_price_usd_per_million=LAUNCH_OUTPUT_PRICE_USD_PER_MILLION,
            )
    apply_runtime_parity(candidate_results)
    evidence = build_evidence(
        candidate_results,
        workload=WORKLOADS[workload],
        evidence_level=evidence_level,
    )
    report = competitive_report(candidate_results, snapshot) if workload == "public-interactive" else None
    paths = {
        "raw": destination / f"qwen38-{workload}-modal-{stamp}-raw.json",
        "evidence": destination / f"qwen38-{workload}-modal-{stamp}.json",
        "targets": targets_path,
    }
    paths["raw"].write_text(json.dumps(run_results, indent=2, sort_keys=True) + "\n")
    paths["evidence"].write_text(json.dumps(evidence, indent=2, sort_keys=True) + "\n")
    if failures_path is not None:
        paths["failures"] = failures_path
    if report is not None:
        paths["competitive"] = destination / f"qwen38-openrouter-comparison-{stamp}.json"
        paths["competitive"].write_text(json.dumps(report, indent=2, sort_keys=True) + "\n")
    print(json.dumps({key: str(value) for key, value in paths.items()}, indent=2, sort_keys=True))

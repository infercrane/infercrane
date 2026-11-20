"""Budget-bounded Modal check of the mixture-of-experts experts pack.

Runs the pack's hidden evaluator inside the pinned vLLM 0.22.1 image on one
H100. The reference kernel must pass every stage; both wrong kernels must fail
at smoke. It checks the evaluator and the timing against vLLM's own experts. It
generates no kernel, loads no model, and serves no traffic.

The hard cap is the function timeout: one H100 for at most 30 minutes, with no
retries and one container.
"""

from __future__ import annotations

import hashlib
import json
import os
import subprocess
import time
from datetime import datetime, timezone
from pathlib import Path

import modal

APP_NAME = "infercrane-moe-pack-check"
# The vLLM 0.22.1 CUDA runtime pinned in internal/support/matrix.go.
VLLM_IMAGE = "vllm/vllm-openai@sha256:953d3a06d5e64ab582985cd7401289d3abf2a2c14ef2158e9a84313daeec77d7"
PACK = Path(__file__).resolve().parents[1] / "kernel-lab" / "packs" / "moe-experts"
REMOTE_PACK = "/opt/infercrane/pack"
# Kernel, verification suite, and the stage it must fail at (None: must pass).
CASES = (
    ("correct", False, None),
    ("correct", True, None),
    ("wrong_router_weight", False, "smoke"),
    ("wrong_gate_up_swapped", False, "smoke"),
)
TIMEOUT_SECONDS = 1800
EVALUATION_TIMEOUT_SECONDS = 600

app = modal.App(APP_NAME)
image = (
    # The image ships python3 only; Modal starts functions with `python`.
    modal.Image.from_registry(VLLM_IMAGE, setup_dockerfile_commands=["RUN ln -sf /usr/bin/python3 /usr/local/bin/python"])
    .entrypoint([])
    # The memory and race sanitizer is a test tool the serving image leaves
    # out. It is added on top of the pinned image and changes nothing vLLM loads.
    .apt_install("cuda-sanitizer-13-0")
    .add_local_dir(str(PACK), REMOTE_PACK, ignore=["__pycache__", "*.pyc"])
)


def _sha256(path: Path) -> str:
    return "sha256:" + hashlib.sha256(path.read_bytes()).hexdigest()


def _tail(text: str, lines: int = 20) -> str:
    return "\n".join(text.strip().splitlines()[-lines:])


def _clean_env() -> dict[str, str]:
    # The evaluator runs on the image's own Python packages, never Modal's.
    return {key: value for key, value in os.environ.items() if key != "PYTHONPATH"}


def _run(argv: list[str], stdin: bytes | None = None, timeout: int = 120) -> subprocess.CompletedProcess:
    return subprocess.run(argv, input=stdin, capture_output=True, timeout=timeout, env=_clean_env())


def _environment() -> dict[str, object]:
    versions = _run(
        [
            "python3",
            "-c",
            "import importlib.metadata as m, json, platform, torch; p = torch.cuda.get_device_properties(0); "
            "print(json.dumps({'python': platform.python_version(), 'torch': torch.__version__, 'torch_cuda': torch.version.cuda, "
            "'triton': m.version('triton'), 'vllm': m.version('vllm'), 'gpu': p.name, 'compute_capability': f'sm{p.major}{p.minor}'}))",
        ]
    )
    smi = _run(["nvidia-smi", "--query-gpu=name,driver_version,memory.total", "--format=csv,noheader,nounits"])
    sanitizer = _run(["compute-sanitizer", "--version"])
    return {
        "software": json.loads(versions.stdout) if versions.returncode == 0 else {"error": _tail(versions.stderr.decode())},
        "nvidia_smi": smi.stdout.decode().strip(),
        "compute_sanitizer": _tail(sanitizer.stdout.decode(), 2) if sanitizer.returncode == 0 else None,
    }


def _sanitizer_self_test() -> dict[str, object]:
    """A known-good CUDA program under memcheck. If it fails here, the host
    cannot run the sanitizer, and a kernel's sanitizer stage says nothing."""
    program = "import torch; x = torch.ones(4096, device='cuda'); print(float((x * 2).sum()))"
    started = time.monotonic()
    completed = _run(["compute-sanitizer", "--tool", "memcheck", "--error-exitcode", "1", "python3", "-c", program], timeout=300)
    return {
        "passed": completed.returncode == 0,
        "exit_code": completed.returncode,
        "seconds": round(time.monotonic() - started, 1),
        "output_tail": _tail(completed.stdout.decode() + completed.stderr.decode(), 8),
    }


def _evaluate(kernel: str, verification: bool, expected_failure: str | None) -> dict[str, object]:
    source_path = Path(REMOTE_PACK) / "testdata" / f"{kernel}.py"
    request = json.dumps({"source": source_path.read_text(encoding="utf-8"), "verification": verification}).encode()
    started = time.monotonic()
    completed = _run(["python3", f"{REMOTE_PACK}/evaluate.py"], stdin=request, timeout=EVALUATION_TIMEOUT_SECONDS)
    seconds = round(time.monotonic() - started, 1)
    entry: dict[str, object] = {
        "kernel": kernel,
        "kernel_sha256": _sha256(source_path),
        "verification_suite": verification,
        "expected_failed_stage": expected_failure,
        "exit_code": completed.returncode,
        "seconds": seconds,
    }
    try:
        result = json.loads(completed.stdout.decode().strip().splitlines()[-1])
    except (IndexError, json.JSONDecodeError):
        entry["error"] = _tail(completed.stderr.decode())
        entry["as_expected"] = False
        return entry
    entry["result"] = result
    failed = result.get("failed_stage")
    if expected_failure is None:
        entry["as_expected"] = failed in (None, "sanitizer") and all(passed for stage, passed in result["correctness"].items() if stage != "sanitizer")
    else:
        entry["as_expected"] = failed == expected_failure
    if result.get("candidate_median_us"):
        entry["speedup_vs_vllm_fused_experts"] = round(result["baseline_median_us"] / result["candidate_median_us"], 4)
    return entry


@app.function(image=image, gpu="H100!", cpu=4, memory=16384, timeout=TIMEOUT_SECONDS, retries=0, max_containers=1, scaledown_window=2)
def check_pack() -> dict[str, object]:
    started = time.monotonic()
    environment = _environment()
    self_test = _sanitizer_self_test() if environment["compute_sanitizer"] else {"passed": False, "reason": "compute-sanitizer not found"}
    evaluations = [_evaluate(kernel, verification, expected) for kernel, verification, expected in CASES]
    correct_runs = [entry for entry in evaluations if entry["expected_failed_stage"] is None]
    sanitizer_passed = all(entry.get("result", {}).get("correctness", {}).get("sanitizer") for entry in correct_runs)
    return {
        "schema_version": "infercrane.kernel-pack-gpu-check/v1",
        "recorded_at": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "evidence_class": "real-gpu-evaluator-check",
        "pack": "moe-experts",
        "pack_files": {name: _sha256(Path(REMOTE_PACK) / name) for name in ("pack.json", "evaluate.py")},
        "runtime_image": VLLM_IMAGE,
        "image_additions": ["cuda-sanitizer-13-0 (apt, test tool only)"],
        "hardware": {"gpu_request": "H100!", **environment},
        "sanitizer_self_test": self_test,
        "evaluations": evaluations,
        "verdict": {
            "evaluator_behaves": all(entry["as_expected"] for entry in evaluations),
            # Memory safety counts only when the host can run the sanitizer at all.
            "sanitizer": "passed" if sanitizer_passed else ("failed" if self_test["passed"] else "not_run_on_this_host"),
        },
        "gpu_seconds": round(time.monotonic() - started, 1),
        "qualification_boundary": "evaluator check with the pack's own reference kernel; no generated kernel, no serving, no end-to-end change",
    }


@app.local_entrypoint()
def main(out: str = "") -> None:
    result = check_pack.remote()
    text = json.dumps(result, indent=2, sort_keys=True) + "\n"
    if out:
        Path(out).write_text(text, encoding="utf-8")
    print(text)

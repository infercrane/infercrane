"""Budget-bounded Modal runner for the InferCrane kernel candidate."""

from __future__ import annotations

import importlib.util
import json
import subprocess
from pathlib import Path

import modal


APP_NAME = "infercrane-kernel-poc"
LOCAL_KERNEL = Path(__file__).resolve().parents[1] / "kernel-lab" / "fused_residual_rmsnorm.py"
REMOTE_KERNEL = "/opt/infercrane/fused_residual_rmsnorm.py"
LOCAL_CUDA_KERNEL = Path(__file__).resolve().parents[1] / "kernel-lab" / "fused_residual_rmsnorm_cuda.py"
REMOTE_CUDA_KERNEL = "/opt/infercrane/fused_residual_rmsnorm_cuda.py"

app = modal.App(APP_NAME)
base_image = (
    modal.Image.debian_slim(python_version="3.12")
    .pip_install("torch==2.11.0", "triton==3.6.0")
)
image = (
    base_image
    .add_local_file(str(LOCAL_KERNEL), REMOTE_KERNEL, copy=True)
)
vendor_image = (
    base_image
    .pip_install("vllm==0.22.1")
    .add_local_file(str(LOCAL_KERNEL), REMOTE_KERNEL, copy=True)
)
cuda_image = vendor_image.pip_install("cupy-cuda12x==14.2.0").add_local_file(
    str(LOCAL_CUDA_KERNEL), REMOTE_CUDA_KERNEL, copy=True
)


def _load_kernel():
    spec = importlib.util.spec_from_file_location("infercrane_fused_rmsnorm", REMOTE_KERNEL)
    if spec is None or spec.loader is None:
        raise RuntimeError("cannot load the InferCrane kernel module")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def _load_cuda_kernel():
    spec = importlib.util.spec_from_file_location("infercrane_cuda_rmsnorm", REMOTE_CUDA_KERNEL)
    if spec is None or spec.loader is None:
        raise RuntimeError("cannot load the InferCrane CUDA kernel module")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def _run_campaign(gpu_request: str, rows: list[int]) -> dict[str, object]:
    import torch
    import triton

    kernel = _load_kernel()
    properties = torch.cuda.get_device_properties(0)
    sm = f"sm{properties.major}{properties.minor}"
    nvidia_smi = subprocess.run(
        [
            "nvidia-smi",
            "--query-gpu=name,uuid,driver_version,memory.total",
            "--format=csv,noheader,nounits",
        ],
        check=True,
        capture_output=True,
        text=True,
    ).stdout.strip()
    results = []
    for row_count in rows:
        results.append(kernel.gpu_benchmark(row_count, 1024, 17, 1e-6))
    return {
        "schema_version": "infercrane.modal-kernel-campaign/v1",
        "evidence_class": "real-gpu-microbenchmark",
        "model_shape_source": "Qwen/Qwen3-0.6B@c1899de289a04d12100db370d81485cdf75e47ca",
        "operator": "residual-add+rmsnorm",
        "hidden_size": 1024,
        "rms_norm_epsilon": 1e-6,
        "gpu_request": gpu_request,
        "gpu": properties.name,
        "compute_capability": sm,
        "nvidia_smi": nvidia_smi,
        "torch_version": torch.__version__,
        "torch_cuda_version": torch.version.cuda,
        "triton_version": triton.__version__,
        "results": results,
        "qualification_boundary": "microbenchmark only; runtime integration and paired AIPerf remain required",
    }


def _run_vendor_campaign(gpu_request: str, rows: list[int]) -> dict[str, object]:
    import importlib.metadata

    import torch
    import triton
    from vllm import _custom_ops as ops

    kernel = _load_kernel()
    campaign = _run_campaign(gpu_request, rows)
    for result in campaign["results"]:
        row_count = result["rows"]
        torch.manual_seed(17)
        x = torch.randn((row_count, 1024), device="cuda", dtype=torch.bfloat16)
        residual = torch.randn_like(x)
        weight = torch.randn((1024,), device="cuda", dtype=torch.bfloat16)
        expected_residual = x + residual
        expected = (
            expected_residual.float()
            * torch.rsqrt(expected_residual.float().square().mean(dim=-1, keepdim=True) + 1e-6)
            * weight.float()
        ).to(x.dtype)
        vendor_input = x.clone()
        vendor_residual = residual.clone()
        ops.fused_add_rms_norm(vendor_input, vendor_residual, weight, 1e-6)
        torch.testing.assert_close(vendor_input, expected, atol=5e-2, rtol=5e-2)
        torch.testing.assert_close(vendor_residual, expected_residual, atol=2e-2, rtol=2e-2)
        vendor_ms = triton.testing.do_bench(
            lambda: ops.fused_add_rms_norm(vendor_input, vendor_residual, weight, 1e-6)
        )
        result["vllm_vendor_ms"] = vendor_ms
        result["speedup_vs_vllm_vendor"] = vendor_ms / result["custom_kernel_ms"]
        result["vllm_correctness_passed"] = True
    campaign["vllm_version"] = importlib.metadata.version("vllm")
    campaign["comparison"] = "InferCrane Triton candidate versus vLLM fused_add_rms_norm"
    return campaign


def _run_cuda_campaign(gpu_request: str, rows: list[int]) -> dict[str, object]:
    import importlib.metadata

    import torch

    kernel = _load_cuda_kernel()
    triton_kernel = _load_kernel()
    properties = torch.cuda.get_device_properties(0)
    results = [kernel.gpu_benchmark(row_count, 1024, 17, 1e-6, triton_kernel) for row_count in rows]
    return {
        "schema_version": "infercrane.modal-cuda-kernel-campaign/v1",
        "evidence_class": "real-gpu-microbenchmark",
        "model_shape_source": "Qwen/Qwen3-0.6B@c1899de289a04d12100db370d81485cdf75e47ca",
        "operator": "residual-add+rmsnorm",
        "implementation": "handwritten-cuda-nvrtc",
        "hidden_size": 1024,
        "rms_norm_epsilon": 1e-6,
        "gpu_request": gpu_request,
        "gpu": properties.name,
        "compute_capability": f"sm{properties.major}{properties.minor}",
        "torch_version": torch.__version__,
        "torch_cuda_version": torch.version.cuda,
        "cupy_version": importlib.metadata.version("cupy-cuda12x"),
        "vllm_version": importlib.metadata.version("vllm"),
        "results": results,
        "qualification_boundary": "microbenchmark only; runtime integration and paired AIPerf remain required",
    }


@app.function(
    image=image,
    gpu="L40S",
    cpu=2,
    memory=4096,
    timeout=600,
    max_containers=1,
    scaledown_window=2,
)
def benchmark_l40s() -> dict[str, object]:
    return _run_campaign("L40S", [1, 8, 32, 128])


@app.function(
    image=image,
    gpu="H100!",
    cpu=2,
    memory=4096,
    timeout=600,
    max_containers=1,
    scaledown_window=2,
)
def benchmark_h100() -> dict[str, object]:
    return _run_campaign("H100!", [1, 8, 32, 128, 512])


@app.function(
    image=vendor_image,
    gpu="L40S",
    cpu=2,
    memory=8192,
    timeout=600,
    max_containers=1,
    scaledown_window=2,
)
def benchmark_vendor_l40s() -> dict[str, object]:
    return _run_vendor_campaign("L40S", [1, 8, 32, 128])


@app.function(
    image=vendor_image,
    gpu="H100!",
    cpu=2,
    memory=8192,
    timeout=600,
    max_containers=1,
    scaledown_window=2,
)
def benchmark_vendor_h100() -> dict[str, object]:
    return _run_vendor_campaign("H100!", [1, 8, 32, 128, 512])


@app.function(
    image=cuda_image,
    gpu="H100!",
    cpu=2,
    memory=8192,
    timeout=600,
    max_containers=1,
    scaledown_window=2,
)
def benchmark_cuda_h100() -> dict[str, object]:
    return _run_cuda_campaign("H100!", [1, 8, 32, 128, 512])


@app.local_entrypoint()
def main(gpu: str = "l40s") -> None:
    if gpu == "l40s":
        result = benchmark_l40s.remote()
    elif gpu == "vendor-l40s":
        result = benchmark_vendor_l40s.remote()
    elif gpu == "h100":
        result = benchmark_h100.remote()
    elif gpu == "vendor-h100":
        result = benchmark_vendor_h100.remote()
    elif gpu == "cuda-h100":
        result = benchmark_cuda_h100.remote()
    else:
        raise ValueError("gpu must be l40s, vendor-l40s, h100, vendor-h100, or cuda-h100")
    print("INFERCRANE_RESULT=" + json.dumps(result, sort_keys=True))

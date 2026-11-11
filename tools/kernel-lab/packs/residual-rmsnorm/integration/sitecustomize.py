"""Registers a generated kernel as a vLLM provider for fused_add_rms_norm.

Loaded through PYTHONPATH in candidate serving containers only. It runs in every
Python process, so spawned engine workers see the provider. The candidate arm
selects it with vLLM's operator priority setting. A failed install exits the
process instead of quietly serving the unmodified runtime, and the first real
call leaves a marker that proves the replay exercised the generated code.
"""

import importlib.util
import os
import sys

FAILED_INSTALL_EXIT = 97
PROVIDER = "infercrane"
_SOURCE = os.environ.get("INFERCRANE_KERNEL_SOURCE")
_STATE = os.environ.get("INFERCRANE_KERNEL_STATE")


def _load_kernel(path):
    spec = importlib.util.spec_from_file_location("infercrane_generated_kernel", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module.fused_residual_rmsnorm


def _install():
    from typing import Optional, Tuple

    import torch
    from torch import Tensor
    from vllm import ir

    kernel = _load_kernel(_SOURCE)
    marker = os.path.join(_STATE, "invoked")
    marked = [False]

    def supports_args(x, x_residual, weight, epsilon, variance_size=None):
        return variance_size is None and (weight is None or weight.dtype == x.dtype)

    # vLLM rejects a provider whose signature differs from the native operator.
    def fused_add_rms_norm(
        x: Tensor,
        x_residual: Tensor,
        weight: Optional[Tensor],
        epsilon: float,
        variance_size: Optional[int] = None,
    ) -> Tuple[Tensor, Tensor]:
        if weight is None:
            weight = torch.ones(x.shape[-1], device=x.device, dtype=x.dtype)
        output, residual_out = kernel(x, x_residual, weight, epsilon)
        x.copy_(output)
        x_residual.copy_(residual_out)
        if not marked[0]:
            with open(marker, "w", encoding="utf-8"):
                pass
            marked[0] = True
        return x, x_residual

    ir.ops.fused_add_rms_norm.register_impl(PROVIDER, supports_args=supports_args, supported=torch.cuda.is_available(), inplace=True)(fused_add_rms_norm)


if _SOURCE and _STATE:
    try:
        _install()
    except BaseException as error:  # Serving without the kernel would be a false result.
        sys.stderr.write("infercrane kernel integration failed: %s\n" % type(error).__name__)
        sys.stderr.flush()
        os._exit(FAILED_INSTALL_EXIT)

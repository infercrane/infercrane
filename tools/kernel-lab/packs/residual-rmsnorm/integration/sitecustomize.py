"""Registers a generated kernel as a vLLM provider for fused_add_rms_norm.

Loaded through PYTHONPATH in candidate serving containers only. It runs in every
Python process, so spawned engine workers see the provider. The candidate arm
selects it with vLLM's operator priority setting. A failed install exits the
process instead of quietly serving the unmodified runtime, and the first real
call leaves a marker that proves the replay exercised the generated code.

vLLM IR is deprecated, and vLLM 0.34.0 removes its op-priority setting. This
module therefore runs only on the exact vLLM releases below, which pack.json
also declares. A newer pin needs a CustomOp-based integration instead.
"""

import importlib.util
import os
import sys

FAILED_INSTALL_EXIT = 97
PROVIDER = "infercrane"
SUPPORTED_VLLM_VERSIONS = ("0.22.1",)
_SOURCE = os.environ.get("INFERCRANE_KERNEL_SOURCE")
_STATE = os.environ.get("INFERCRANE_KERNEL_STATE")


def runtime_problem(version):
    """Returns why this vLLM version cannot load the provider, or None."""
    # A local build label such as "+cu129" does not change the operator API.
    release = (version or "").split("+", 1)[0]
    if release in SUPPORTED_VLLM_VERSIONS:
        return None
    return (
        "vLLM %s is not supported; this integration supports vLLM %s only. vLLM IR is deprecated "
        "and vLLM 0.34.0 removes its op-priority setting, so a newer pin needs a CustomOp-based "
        "integration." % (version or "of unknown version", ", ".join(SUPPORTED_VLLM_VERSIONS))
    )


def _fail(detail):
    sys.stderr.write("infercrane kernel integration failed: %s\n" % detail)
    sys.stderr.flush()
    os._exit(FAILED_INSTALL_EXIT)


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
    # The version check runs before any generated code is loaded, so only its
    # own text is printed. Serving without the kernel would be a false result.
    try:
        import vllm

        _problem = runtime_problem(getattr(vllm, "__version__", None))
    except BaseException as error:
        _fail(type(error).__name__)
    if _problem:
        _fail(_problem)
    try:
        _install()
    except BaseException as error:  # A generated kernel's message is not trusted output.
        _fail(type(error).__name__)

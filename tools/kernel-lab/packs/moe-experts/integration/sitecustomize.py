"""Routes vLLM's Triton MoE experts to a generated kernel.

Loaded through PYTHONPATH in candidate serving containers only. It runs in every
Python process, so spawned engine workers see it. The candidate arm starts vLLM
with --moe-backend triton, so the experts run through TritonExperts.apply, which
this module replaces for the case the kernel contract covers: BF16 weights, SiLU
activation, and no expert map, bias, LoRA, clamp, or router weight on the input.
Any other call runs vLLM's own kernel. A failed install exits the process
instead of quietly serving the unmodified runtime, and the first call that runs
the generated kernel leaves a marker that proves the replay exercised it.

The hook depends on TritonExperts' exact signature, so this module runs only on
the vLLM release below, which pack.json also declares.
"""

import importlib.util
import os
import sys

FAILED_INSTALL_EXIT = 97
SUPPORTED_VLLM_VERSIONS = ("0.22.1",)
_SOURCE = os.environ.get("INFERCRANE_KERNEL_SOURCE")
_STATE = os.environ.get("INFERCRANE_KERNEL_STATE")


def runtime_problem(version):
    """Returns why this vLLM version cannot load the kernel, or None."""
    # A local build label such as "+cu129" does not change the experts API.
    release = (version or "").split("+", 1)[0]
    if release in SUPPORTED_VLLM_VERSIONS:
        return None
    return "vLLM %s is not supported; this integration supports vLLM %s only, because it replaces TritonExperts.apply by its exact signature." % (
        version or "of unknown version",
        ", ".join(SUPPORTED_VLLM_VERSIONS),
    )


def _fail(detail):
    sys.stderr.write("infercrane kernel integration failed: %s\n" % detail)
    sys.stderr.flush()
    os._exit(FAILED_INSTALL_EXIT)


def _load_kernel(path):
    spec = importlib.util.spec_from_file_location("infercrane_generated_kernel", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module.fused_moe_experts


def covered(experts, hidden_states, w1, w2, activation, expert_map, a1q_scale, apply_router_weight_on_input, torch, silu):
    """The calls the kernel contract covers; every other call keeps vLLM's kernel."""
    quant = experts.quant_config
    quantized = quant.use_fp8_w8a8 or quant.use_int8_w8a8 or quant.use_int8_w8a16 or quant.use_int4_w4a16
    return (
        hidden_states.dtype == torch.bfloat16
        and w1.dtype == torch.bfloat16
        and w2.dtype == torch.bfloat16
        and not quantized
        and activation == silu
        and quant.gemm1_clamp_limit is None
        and expert_map is None
        and a1q_scale is None
        and experts.w1_bias is None
        and experts.w2_bias is None
        and getattr(experts, "_lora_context", None) is None
        and not apply_router_weight_on_input
    )


def _install():
    import torch
    from vllm.model_executor.layers.fused_moe.activation import MoEActivation
    from vllm.model_executor.layers.fused_moe.experts import triton_moe

    kernel = _load_kernel(_SOURCE)
    marker = os.path.join(_STATE, "invoked")
    marked = [False]
    original = triton_moe.TritonExperts.apply

    # The parameters mirror vLLM 0.22.1's TritonExperts.apply exactly.
    def apply(
        self,
        output,
        hidden_states,
        w1,
        w2,
        topk_weights,
        topk_ids,
        activation,
        global_num_experts,
        expert_map,
        a1q_scale,
        a2_scale,
        workspace13,
        workspace2,
        expert_tokens_meta,
        apply_router_weight_on_input,
    ):
        if not covered(self, hidden_states, w1, w2, activation, expert_map, a1q_scale, apply_router_weight_on_input, torch, MoEActivation.SILU):
            return original(
                self, output, hidden_states, w1, w2, topk_weights, topk_ids, activation, global_num_experts, expert_map,
                a1q_scale, a2_scale, workspace13, workspace2, expert_tokens_meta, apply_router_weight_on_input,
            )
        output.copy_(kernel(hidden_states, w1, w2, topk_weights.float(), topk_ids.to(torch.int32)))
        if not marked[0]:
            with open(marker, "w", encoding="utf-8"):
                pass
            marked[0] = True
        return None

    triton_moe.TritonExperts.apply = apply


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

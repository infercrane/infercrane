#!/usr/bin/env python3
"""Runs the real integration module against a stub of vLLM 0.22.1's Triton experts."""

from __future__ import annotations

import importlib.util
import json
import os
import subprocess
import sys
import tempfile
import textwrap
import unittest
from pathlib import Path

INTEGRATION = Path(__file__).with_name("integration")
PACK = Path(__file__).with_name("pack.json")
STUB_TORCH = """
bfloat16, float8_e4m3fn, int32, int64 = "bf16", "fp8", "int32", "int64"
class Tensor:
    def __init__(self, dtype="bf16", name="tensor"):
        self.dtype, self.name, self.value = dtype, name, None
    def copy_(self, other):
        self.value = other
    def float(self):
        return Tensor("fp32", self.name)
    def to(self, dtype):
        return Tensor(dtype, self.name)
"""
STUB_ACTIVATION = """
import enum
class MoEActivation(enum.Enum):
    SILU = "silu"
    GELU = "gelu"
"""
# vLLM 0.22.1's TritonExperts.apply signature, as the integration must mirror it.
NATIVE_PARAMETERS = [
    "self", "output", "hidden_states", "w1", "w2", "topk_weights", "topk_ids", "activation", "global_num_experts", "expert_map",
    "a1q_scale", "a2_scale", "workspace13", "workspace2", "expert_tokens_meta", "apply_router_weight_on_input",
]
STUB_TRITON_MOE = """
class QuantConfig:
    def __init__(self, fp8=False):
        self.use_fp8_w8a8, self.use_int8_w8a8, self.use_int8_w8a16, self.use_int4_w4a16 = fp8, False, False, False
        self.gemm1_clamp_limit = None
class TritonExperts:
    def __init__(self, fp8=False):
        self.quant_config, self.w1_bias, self.w2_bias, self._lora_context = QuantConfig(fp8), None, None, None
    def apply(self, output, hidden_states, w1, w2, topk_weights, topk_ids, activation, global_num_experts, expert_map,
              a1q_scale, a2_scale, workspace13, workspace2, expert_tokens_meta, apply_router_weight_on_input):
        output.copy_("vllm-triton")
"""
KERNEL = """
def fused_moe_experts(hidden_states, w13, w2, topk_weights, topk_ids):
    return ("generated", topk_weights.dtype, topk_ids.dtype)
"""
SERVER = textwrap.dedent(
    """
    import inspect
    import json
    import torch
    from vllm.model_executor.layers.fused_moe.activation import MoEActivation
    from vllm.model_executor.layers.fused_moe.experts.triton_moe import TritonExperts

    def call(experts, activation=MoEActivation.SILU, expert_map=None, dtype="bf16", on_input=False):
        output = torch.Tensor()
        experts.apply(output, torch.Tensor(dtype), torch.Tensor(dtype), torch.Tensor(dtype), torch.Tensor("bf16"), torch.Tensor("int64"),
                      activation, -1, expert_map, None, None, None, None, None, on_input)
        return output.value

    results = {{}}
    if {call}:
        results["covered"] = call(TritonExperts())
    results["fp8"] = call(TritonExperts(fp8=True))
    results["gelu"] = call(TritonExperts(), activation=MoEActivation.GELU)
    results["expert_map"] = call(TritonExperts(), expert_map=torch.Tensor("int32"))
    results["router_on_input"] = call(TritonExperts(), on_input=True)
    results["signature"] = list(inspect.signature(TritonExperts.apply).parameters)
    print(json.dumps(results, default=list))
    """
)


class IntegrationTest(unittest.TestCase):
    def run_server(self, kernel: str = KERNEL, call: bool = True, version: str | None = "0.22.1") -> tuple[subprocess.CompletedProcess, Path]:
        root = Path(tempfile.mkdtemp())
        stubs = root / "stubs"
        moe = stubs / "vllm" / "model_executor" / "layers" / "fused_moe"
        (moe / "experts").mkdir(parents=True)
        (stubs / "torch.py").write_text(STUB_TORCH)
        (stubs / "vllm" / "__init__.py").write_text("" if version is None else f"__version__ = {version!r}\n")
        for package in (stubs / "vllm" / "model_executor", stubs / "vllm" / "model_executor" / "layers", moe):
            (package / "__init__.py").write_text("")
        (moe / "activation.py").write_text(STUB_ACTIVATION)
        (moe / "experts" / "__init__.py").write_text("")
        (moe / "experts" / "triton_moe.py").write_text(STUB_TRITON_MOE)
        (root / "kernel.py").write_text(kernel)
        state = root / "state"
        state.mkdir()
        env = {
            "PATH": os.environ["PATH"],
            "PYTHONPATH": f"{INTEGRATION}{os.pathsep}{stubs}",
            "PYTHONDONTWRITEBYTECODE": "1",
            "INFERCRANE_KERNEL_SOURCE": str(root / "kernel.py"),
            "INFERCRANE_KERNEL_STATE": str(state),
        }
        completed = subprocess.run([sys.executable, "-c", SERVER.format(call=call)], env=env, capture_output=True, text=True, timeout=30)
        return completed, state

    def test_covered_calls_run_the_kernel_and_everything_else_keeps_vllm(self) -> None:
        completed, state = self.run_server()
        self.assertEqual(completed.returncode, 0, completed.stderr)
        results = json.loads(completed.stdout)
        # The router's weights and ids reach the kernel as FP32 and INT32.
        self.assertEqual(results["covered"], ["generated", "fp32", "int32"])
        for case in ("fp8", "gelu", "expert_map", "router_on_input"):
            self.assertEqual(results[case], "vllm-triton", case)
        # The installed hook keeps vLLM 0.22.1's TritonExperts.apply signature.
        self.assertEqual(results["signature"], NATIVE_PARAMETERS)
        self.assertTrue((state / "invoked").exists())

    def test_marker_stays_absent_until_the_kernel_runs(self) -> None:
        completed, state = self.run_server(call=False)
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assertFalse((state / "invoked").exists())

    def test_broken_kernel_stops_the_process_instead_of_serving_vllms_kernel(self) -> None:
        completed, _ = self.run_server(kernel="raise ValueError('broken')\n")
        self.assertEqual(completed.returncode, 97)
        self.assertEqual(completed.stdout, "")
        self.assertIn("ValueError", completed.stderr)
        self.assertNotIn("broken", completed.stderr)

    def test_unreviewed_vllm_stops_before_loading_the_kernel(self) -> None:
        kernel = "print('kernel loaded')\n" + KERNEL
        for version, shown in (("0.23.0", "vLLM 0.23.0"), ("0.22.2", "vLLM 0.22.2"), (None, "unknown version")):
            with self.subTest(version=version):
                completed, state = self.run_server(kernel=kernel, version=version)
                self.assertEqual(completed.returncode, 97)
                self.assertEqual(completed.stdout, "")
                self.assertIn(shown, completed.stderr)
                self.assertFalse((state / "invoked").exists())

    def test_local_build_label_keeps_the_reviewed_release(self) -> None:
        completed, state = self.run_server(version="0.22.1+cu129")
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assertTrue((state / "invoked").exists())

    def test_pack_declares_the_versions_and_backend_the_integration_needs(self) -> None:
        spec = importlib.util.spec_from_file_location("pack_sitecustomize", INTEGRATION / "sitecustomize.py")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        pack = json.loads(PACK.read_text(encoding="utf-8"))
        self.assertEqual(tuple(pack["runtime_versions"]["vllm"]), module.SUPPORTED_VLLM_VERSIONS)
        self.assertIsNone(module.runtime_problem("0.22.1"))
        self.assertEqual(pack["candidate_args"], ["--moe-backend", "triton"])


if __name__ == "__main__":
    unittest.main()

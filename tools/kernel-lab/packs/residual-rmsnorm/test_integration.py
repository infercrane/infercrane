#!/usr/bin/env python3
"""Runs the real integration module against a stub of vLLM 0.22.1's operator API."""

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
class Tensor:
    def __init__(self, shape=(1, 4), dtype="bf16"):
        self.shape, self.dtype, self.device, self.value = shape, dtype, "cuda", None
    def copy_(self, other):
        self.value = other
def ones(size, device=None, dtype=None):
    return ("ones", size)
class cuda:
    @staticmethod
    def is_available():
        return True
"""
STUB_IR = """
import inspect
NATIVE_PARAMETERS = ["x", "x_residual", "weight", "epsilon", "variance_size"]
class Impl:
    def __init__(self, fn, supports_args, supported, inplace):
        self.impl_fn, self.supports_args, self.supported, self.inplace = fn, supports_args, supported, inplace
class Op:
    allow_inplace = True
    impls = {}
    def register_impl(self, provider, *, supported=True, supports_args=None, inplace=False):
        def register(fn):
            assert list(inspect.signature(fn).parameters) == NATIVE_PARAMETERS
            assert list(inspect.signature(supports_args).parameters) == NATIVE_PARAMETERS
            assert all(p.annotation is not inspect.Parameter.empty for p in inspect.signature(fn).parameters.values())
            assert inspect.signature(fn).parameters["variance_size"].default is None
            self.impls[provider] = Impl(fn, supports_args, supported, inplace)
            return fn
        return register
class ops:
    fused_add_rms_norm = Op()
"""
KERNEL = """
def fused_residual_rmsnorm(x, residual, weight, epsilon):
    return ("normalized", weight)
"""
SERVER = textwrap.dedent(
    """
    import torch
    from vllm import ir
    impl = ir.ops.fused_add_rms_norm.impls["infercrane"]
    assert impl.inplace and impl.supported
    x, residual = torch.Tensor(), torch.Tensor()
    assert impl.supports_args(x, residual, None, 1e-6) and not impl.supports_args(x, residual, None, 1e-6, 2)
    if {call}:
        returned = impl.impl_fn(x, residual, None, 1e-6)
        assert returned == (x, residual)
    print(x.value, residual.value)
    """
)


class IntegrationTest(unittest.TestCase):
    def run_server(self, kernel: str = KERNEL, call: bool = True, version: str | None = "0.22.1") -> tuple[subprocess.CompletedProcess, Path]:
        root = Path(tempfile.mkdtemp())
        stubs = root / "stubs"
        (stubs / "vllm" / "ir").mkdir(parents=True)
        (stubs / "torch.py").write_text(STUB_TORCH)
        (stubs / "vllm" / "__init__.py").write_text("" if version is None else f"__version__ = {version!r}\n")
        (stubs / "vllm" / "ir" / "__init__.py").write_text(STUB_IR)
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

    def test_provider_writes_in_place_and_marks_first_call(self) -> None:
        completed, state = self.run_server()
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assertEqual(completed.stdout.strip(), "normalized ('ones', 4)")
        self.assertTrue((state / "invoked").exists())

    def test_marker_stays_absent_until_the_kernel_is_called(self) -> None:
        completed, state = self.run_server(call=False)
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assertFalse((state / "invoked").exists())

    def test_broken_kernel_stops_the_process_instead_of_serving_the_vendor_kernel(self) -> None:
        completed, _ = self.run_server(kernel="raise ValueError('broken')\n")
        self.assertEqual(completed.returncode, 97)
        self.assertEqual(completed.stdout, "")
        self.assertIn("ValueError", completed.stderr)
        self.assertNotIn("broken", completed.stderr)

    def test_local_build_label_keeps_the_reviewed_release(self) -> None:
        completed, state = self.run_server(version="0.22.1+cu129")
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assertTrue((state / "invoked").exists())

    def test_unreviewed_vllm_stops_before_loading_the_kernel(self) -> None:
        # The kernel would print if it were loaded; it must not be.
        kernel = "print('kernel loaded')\n" + KERNEL
        for version, shown in (("0.34.0", "vLLM 0.34.0"), ("0.22.2", "vLLM 0.22.2"), (None, "unknown version")):
            with self.subTest(version=version):
                completed, state = self.run_server(kernel=kernel, version=version)
                self.assertEqual(completed.returncode, 97)
                self.assertEqual(completed.stdout, "")
                self.assertIn(shown, completed.stderr)
                self.assertIn("CustomOp", completed.stderr)
                self.assertFalse((state / "invoked").exists())

    def test_pack_declares_the_versions_the_integration_enforces(self) -> None:
        spec = importlib.util.spec_from_file_location("pack_sitecustomize", INTEGRATION / "sitecustomize.py")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        pack = json.loads(PACK.read_text(encoding="utf-8"))
        self.assertEqual(tuple(pack["runtime_versions"]["vllm"]), module.SUPPORTED_VLLM_VERSIONS)
        self.assertIsNone(module.runtime_problem("0.22.1"))


if __name__ == "__main__":
    unittest.main()

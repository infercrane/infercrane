#!/usr/bin/env python3
"""Runs the vLLM 0.34 plugin through Python's own entry-point discovery against a stub of
vLLM 0.34.0's CustomOp, RMSNorm and envs APIs. No GPU, torch or vLLM needed."""

from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import textwrap
import unittest
from pathlib import Path

PLUGIN = Path(__file__).with_name("integration-vllm-0.34")
STUBS = {
    "vllm/__init__.py": "import os\n__version__ = os.environ.get('STUB_VLLM_VERSION', '0.34.0')\n",
    "vllm/envs.py": "VLLM_BATCH_INVARIANT = False\n",
    "vllm/model_executor/__init__.py": "",
    "vllm/model_executor/layers/__init__.py": "",
    # vLLM 0.34.0 custom_op.py: register_oot keys by the given name and sets cls.name to it.
    "vllm/model_executor/custom_op.py": textwrap.dedent("""
        op_registry_oot = {}
        class CustomOp:
            @classmethod
            def register_oot(cls, _decorated=None, name=None):
                def decorator(layer_cls):
                    reg_name = name if name is not None else cls.__name__
                    assert reg_name not in op_registry_oot, "Duplicate layer name: " + reg_name
                    layer_cls.name = reg_name
                    op_registry_oot[reg_name] = layer_cls
                    return layer_cls
                return decorator if _decorated is None else decorator(_decorated)
        """),
    "vllm/model_executor/layers/layernorm.py": textwrap.dedent("""
        from vllm.model_executor.custom_op import CustomOp
        class Weight:
            def __init__(self, size):
                self.data = ("weight", size)
        class RMSNorm(CustomOp):
            name = "rms_norm"
            def __init__(self, hidden_size, eps=1e-6):
                self.weight, self.variance_epsilon = Weight(hidden_size), eps
                self.variance_size_override, self.pass_weight_add = None, True
            def forward_cuda(self, x, residual=None):
                return ("library", tuple(x.shape))
        """),
}
KERNEL = "def fused_residual_rmsnorm(x, residual, weight, epsilon):\n    return ('generated', tuple(x.shape), weight, epsilon)\n"
SERVER = textwrap.dedent("""
    import importlib.metadata, json
    found = [entry for entry in importlib.metadata.entry_points(group="vllm.general_plugins") if entry.name == "infercrane_kernels"]
    register = found[0].load()
    register()
    register()  # vLLM may load general plugins more than once
    from vllm.model_executor.custom_op import op_registry_oot
    class Tensor:
        def __init__(self, rows, hidden, dtype="torch.bfloat16"):
            self.shape, self.dtype = (rows, hidden), dtype
    cls = op_registry_oot.get("RMSNorm")
    if cls is None:
        print(json.dumps({"registered": False}))
        raise SystemExit(0)
    norm = cls(4096)
    calls = {
        "qualified": norm.forward_cuda(Tensor(64, 4096), Tensor(64, 4096)),
        "other_hidden": norm.forward_cuda(Tensor(64, 2048), Tensor(64, 2048)),
        "too_many_rows": norm.forward_cuda(Tensor(9000, 4096), Tensor(9000, 4096)),
        "no_residual": norm.forward_cuda(Tensor(64, 4096)),
        "fp16": norm.forward_cuda(Tensor(64, 4096, "torch.float16"), Tensor(64, 4096, "torch.float16")),
    }
    print(json.dumps({"registered": True, "name": cls.name, "calls": {key: value[0] for key, value in calls.items()}}))
""")


class PluginTest(unittest.TestCase):
    def run_server(self, version="0.34.0", shapes='{"hidden": [4096], "max_rows": 8192, "dtype": "bfloat16"}', candidate=True):
        root = Path(tempfile.mkdtemp())
        for name, body in STUBS.items():
            (root / name).parent.mkdir(parents=True, exist_ok=True)
            (root / name).write_text(body)
        state = root / "state"
        state.mkdir()
        (root / "kernel.py").write_text(KERNEL)
        env = {"PATH": os.environ.get("PATH", ""), "PYTHONPATH": f"{root}:{PLUGIN}", "PYTHONDONTWRITEBYTECODE": "1", "STUB_VLLM_VERSION": version}
        if candidate:
            env.update(INFERCRANE_KERNEL_SOURCE=str(root / "kernel.py"), INFERCRANE_KERNEL_STATE=str(state), INFERCRANE_KERNEL_SHAPES=shapes)
        done = subprocess.run([sys.executable, "-c", SERVER], env=env, capture_output=True, text=True, timeout=60)
        return done, state

    def test_routes_only_qualified_shapes_to_the_generated_kernel(self) -> None:
        done, state = self.run_server()
        self.assertEqual(done.returncode, 0, done.stderr)
        result = json.loads(done.stdout)
        self.assertEqual(result["name"], "RMSNorm")  # enabled with custom_ops "+RMSNorm"
        self.assertEqual(result["calls"], {"qualified": "generated", "other_hidden": "library", "too_many_rows": "library", "no_residual": "library", "fp16": "library"})
        self.assertTrue((state / "invoked").exists())
        (record,) = list(state.glob("calls-*.json"))
        counts = json.loads(record.read_text())
        self.assertEqual((counts["generated"], counts["library"], counts["shapes"]["hidden"]), (1, 4, [4096]))

    def test_a_control_arm_loads_nothing(self) -> None:
        done, state = self.run_server(candidate=False)
        self.assertEqual((done.returncode, json.loads(done.stdout)), (0, {"registered": False}))
        self.assertFalse((state / "invoked").exists())

    def test_another_vllm_or_bad_shapes_stop_the_process(self) -> None:
        for version, shapes in (("0.22.1", None), ("0.34.0", '{"hidden": [], "max_rows": 1, "dtype": "bfloat16"}'),
                                ("0.34.0", '{"hidden": [4096], "max_rows": 1, "dtype": "float32"}')):
            done, _ = self.run_server(version, shapes or '{"hidden": [4096], "max_rows": 8192, "dtype": "bfloat16"}')
            self.assertEqual(done.returncode, 97, (version, shapes, done.stdout))
            self.assertIn("infercrane kernel plugin failed", done.stderr)


if __name__ == "__main__":
    unittest.main()

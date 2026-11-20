"""Offline tests for the Modal GPU PoC broker: spend accounting and the vLLM argument
allowlist. Modal is stubbed, so nothing runs remotely.

    python3 -m unittest discover -s tools/poc-modal -p 'test_*.py'
"""

from __future__ import annotations

import sys
import tempfile
import types
import unittest
from pathlib import Path


def _stub_modal() -> None:
    def chain(*_args, **_kwargs):
        return types.SimpleNamespace(pip_install=chain, entrypoint=chain, function=lambda **_: (lambda f: f), local_entrypoint=lambda: (lambda f: f))

    sys.modules["modal"] = types.SimpleNamespace(
        App=chain, FunctionCall=object, forward=None, enable_output=None,
        Volume=types.SimpleNamespace(from_name=lambda *a, **k: None), Dict=types.SimpleNamespace(from_name=lambda *a, **k: None),
        Image=types.SimpleNamespace(debian_slim=chain, from_registry=chain),
    )


_stub_modal()
sys.path.insert(0, str(Path(__file__).resolve().parent))
import broker  # noqa: E402
import modal_gpu  # noqa: E402


class AccountingTest(unittest.TestCase):
    def setUp(self) -> None:
        self.dir = tempfile.TemporaryDirectory()
        self.broker = broker.Broker("token", 10.0, 3, 6, Path(self.dir.name) / "ledger.json", dry=False)

    def tearDown(self) -> None:
        self.dir.cleanup()

    def test_wait_counts_until_the_container_reports_its_start(self) -> None:
        self.assertEqual(self.broker.lease_seconds({"requested_at": 1000.0}, 1700.0), 700.0)

    def test_known_start_bills_from_start_minus_margin(self) -> None:
        lease = {"requested_at": 1000.0, "container_started_at": 1600.0}
        self.assertEqual(self.broker.lease_seconds(lease, 1700.0), 100.0 + broker.START_MARGIN_SECONDS)

    def test_billing_never_starts_before_the_request(self) -> None:
        lease = {"requested_at": 1000.0, "container_started_at": 1010.0, "ended_at": 1300.0}
        self.assertEqual(self.broker.lease_seconds(lease, 9999.0), 300.0)

    def test_rate_is_h100_plus_container_cpu_and_memory(self) -> None:
        expected = 0.001097 + modal_gpu.GPU_CPU * 0.0000131 + modal_gpu.GPU_MEMORY_MIB / 1024 * 0.00000222
        self.assertAlmostEqual(self.broker.rate, expected)

    def test_new_lease_refused_near_the_cap(self) -> None:
        self.broker.leases["old"] = {"lease_id": "old", "state": "stopped", "requested_at": 0.0, "ended_at": (10.0 - 0.5) / self.broker.rate}
        code, body = self.broker.create("pod", modal_gpu.MODEL_ID, ["--served-model-name", modal_gpu.MODEL_ID])
        self.assertEqual(code, 402, body)

    def test_gpu_limit(self) -> None:
        for index in range(3):
            self.broker.leases[str(index)] = {"lease_id": str(index), "state": "ready", "requested_at": 1e12}
        code, _ = self.broker.create("pod", modal_gpu.MODEL_ID, ["--served-model-name", modal_gpu.MODEL_ID])
        self.assertEqual(code, 429)


class AllowlistTest(unittest.TestCase):
    def test_candidate_arguments_pass(self) -> None:
        modal_gpu.check_args(["--served-model-name", modal_gpu.MODEL_ID, "--quantization", "fp8", "--kv-cache-dtype=fp8", "--enable-prefix-caching"], modal_gpu.MODEL_ID)

    def test_refused(self) -> None:
        for args in (
            ["--served-model-name", modal_gpu.MODEL_ID, "--tensor-parallel-size", "8"],  # more GPUs
            ["--served-model-name", modal_gpu.MODEL_ID, "--port", "9000"],
            ["--served-model-name", "other/model"],
            ["--quantization", "fp8"],  # no served model name
            ["--served-model-name", modal_gpu.MODEL_ID, "--quantization"],  # missing value
        ):
            with self.subTest(args=args), self.assertRaises(ValueError):
                modal_gpu.check_args(args, modal_gpu.MODEL_ID)

    def test_other_models_are_refused(self) -> None:
        with self.assertRaises(ValueError):
            modal_gpu.check_args(["--served-model-name", "other/model"], "other/model")


if __name__ == "__main__":
    unittest.main()

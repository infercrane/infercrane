"""Offline tests for the Modal GPU PoC broker: spend accounting and the vLLM argument
allowlist. Modal is stubbed, so nothing runs remotely.

    python3 -m unittest discover -s tools/poc-modal -p 'test_*.py'
"""

from __future__ import annotations

import sys
import tempfile
import time
import types
import unittest
from pathlib import Path


def _stub_modal() -> None:
    def chain(*_args, **_kwargs):
        return types.SimpleNamespace(pip_install=chain, entrypoint=chain, env=chain, add_local_python_source=chain, function=lambda **_: (lambda f: f), local_entrypoint=lambda: (lambda f: f))

    sys.modules["modal"] = types.SimpleNamespace(
        App=chain, FunctionCall=object, forward=None, enable_output=None,
        Volume=types.SimpleNamespace(from_name=lambda *a, **k: None), Dict=types.SimpleNamespace(from_name=lambda *a, **k: None),
        Image=types.SimpleNamespace(debian_slim=chain, from_registry=chain),
    )


_stub_modal()
sys.path.insert(0, str(Path(__file__).resolve().parent))
import broker  # noqa: E402
import modal_gpu  # noqa: E402
import models  # noqa: E402


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
        code, body = self.broker.create("pod", modal_gpu.QWEN3_8B.id, ["--served-model-name", modal_gpu.QWEN3_8B.id])
        self.assertEqual(code, 402, body)

    def test_gpu_limit(self) -> None:
        for index in range(3):
            self.broker.leases[str(index)] = {"lease_id": str(index), "state": "ready", "requested_at": 1e12}
        code, _ = self.broker.create("pod", modal_gpu.QWEN3_8B.id, ["--served-model-name", modal_gpu.QWEN3_8B.id])
        self.assertEqual(code, 429)


class AllowlistTest(unittest.TestCase):
    def test_candidate_arguments_pass(self) -> None:
        modal_gpu.check_args(["--served-model-name", modal_gpu.QWEN3_8B.id, "--quantization", "fp8", "--kv-cache-dtype=fp8", "--enable-prefix-caching"], modal_gpu.QWEN3_8B.id)

    def test_refused(self) -> None:
        for args in (
            ["--served-model-name", modal_gpu.QWEN3_8B.id, "--tensor-parallel-size", "8"],  # more GPUs
            ["--served-model-name", modal_gpu.QWEN3_8B.id, "--port", "9000"],
            ["--served-model-name", "other/model"],
            ["--quantization", "fp8"],  # no served model name
            ["--served-model-name", modal_gpu.QWEN3_8B.id, "--quantization"],  # missing value
        ):
            with self.subTest(args=args), self.assertRaises(ValueError):
                modal_gpu.check_args(args, modal_gpu.QWEN3_8B.id)

    def test_other_models_are_refused(self) -> None:
        with self.assertRaises(ValueError):
            modal_gpu.check_args(["--served-model-name", "other/model"], "other/model")

    def test_publishers_commands_pass(self) -> None:
        for model in models.MODELS.values():
            with self.subTest(model=model.id):
                modal_gpu.check_args(["--served-model-name", model.id, *model.publisher_args], model.id)

    def test_planner_changes_pass_within_one_gpu(self) -> None:
        model = models.QWEN38_27B_FP8
        base = ["--served-model-name", model.id, *model.publisher_args]
        for change in (["--api-server-count", "2"], ["--stream-interval", "2"], ["--max-cudagraph-capture-size", "640"], ["--max-num-seqs", "512", "--max-num-batched-tokens", "16384"],
                       ["--no-async-scheduling"], ["--seed", "1"]):
            with self.subTest(change=change):
                modal_gpu.check_args(base + change, model.id)
        # Speculation with the model's own prediction head needs no draft weights; whether a
        # run may measure it is the driver's decision (it needs traffic with real text).
        modal_gpu.check_args(base + list(model.publisher_speculation), model.id)
        for change in (["--tensor-parallel-size", "2"], ["--api-server-count", "8"], ["--speculative-config", '{"method":"mtp","num_speculative_tokens":9}'],
                       ["--speculative-config", '{"method":"dflash","model":"incoai/Qwen3.8-27B-DFlash2","num_speculative_tokens":7}'],
                       ["--speculative-config", '{"method":"mtp","num_speculative_tokens":3,"model":"other/draft"}'], ["--speculative-config", "mtp"], ["--mm-encoder-tp-mode", "x"]):
            with self.subTest(change=change), self.assertRaises(ValueError):
                modal_gpu.check_args(base + change, model.id)


class _Store(dict):
    def put(self, key, value):
        self[key] = value


class _Call:
    count = 0

    def __init__(self) -> None:
        _Call.count += 1
        self.object_id = f"call-{_Call.count}"


class DriverPinTest(unittest.TestCase):
    """Every setup of a run serves on one NVIDIA driver; a container on another driver
    stops before loading the model and the lease starts again, still billed."""

    def setUp(self) -> None:
        self.dir = tempfile.TemporaryDirectory()
        self.saved = (modal_gpu.leases, modal_gpu.serve_gpu)
        modal_gpu.leases, modal_gpu.serve_gpu = _Store(), types.SimpleNamespace(spawn=lambda *a: _Call())
        self.broker = broker.Broker("token", 30.0, 3, 6, Path(self.dir.name) / "ledger.json", dry=False)

    def tearDown(self) -> None:
        modal_gpu.leases, modal_gpu.serve_gpu = self.saved
        self.dir.cleanup()

    def lease(self) -> dict:
        code, body = self.broker.create("pod", modal_gpu.QWEN3_8B.id, ["--served-model-name", modal_gpu.QWEN3_8B.id])
        self.assertEqual(code, 201, body)
        return self.broker.leases[body["lease_id"]]

    def report(self, lease: dict, **remote) -> None:
        modal_gpu.leases.put(f"st:{lease['lease_id']}", remote)
        lease["refreshed_at"] = 0
        self.broker.refresh(lease)

    def test_first_host_pins_the_driver_for_later_leases(self) -> None:
        first = self.lease()
        self.report(first, state="ready", gpu_facts={"driver": "620.70.06"})
        self.assertEqual(self.broker.required_driver, "620.70.06")
        second = self.lease()
        self.assertEqual(modal_gpu.leases[f"ctl:{second['lease_id']}"]["required_driver"], "620.70.06")

    def test_a_refused_host_never_pins(self) -> None:
        self.broker.required_driver = None
        lease = self.lease()
        self.report(lease, state="failed", reason="error", gpu_facts={"driver": "610.57.04"})
        self.assertIsNone(self.broker.required_driver)

    def test_mismatched_host_starts_the_lease_again_and_stays_billed(self) -> None:
        self.broker.required_driver = "620.70.06"
        lease = self.lease()
        lease["requested_at"] -= 300  # the refused container ran for five minutes
        first_call = lease["call_id"]
        self.report(lease, state="failed", reason="driver_mismatch", error="NVIDIA driver 610.57.04 differs")
        self.assertEqual(lease["state"], "starting")
        self.assertEqual(lease["driver_retries"], 1)
        self.assertNotEqual(lease["call_id"], first_call)
        self.assertGreaterEqual(lease["prior_seconds"], 300)
        self.assertGreaterEqual(self.broker.lease_seconds(lease, time.time()), 300)
        self.assertEqual(modal_gpu.leases[f"st:{lease['lease_id']}"], {"state": "requested"})

    def test_retries_are_bounded(self) -> None:
        self.broker.required_driver = "620.70.06"
        lease = self.lease()
        for _ in range(broker.MAX_DRIVER_RETRIES):
            self.report(lease, state="failed", reason="driver_mismatch", error="mismatch")
        self.assertEqual(lease["state"], "starting")
        self.report(lease, state="failed", reason="driver_mismatch", error="mismatch")
        self.assertEqual(lease["state"], "failed")

    def test_a_preempted_container_ends_its_lease_instead_of_serving_from_another_machine(self) -> None:
        # Modal runs a preempted Function again on the same input. The lease already
        # reported a container start, so the second container must not serve.
        modal_gpu.leases["st:lease-1"] = {"state": "ready", "container_started_at": 1000.0, "url": "https://old.example"}
        final = modal_gpu._serve("lease-1", ["--served-model-name", modal_gpu.QWEN3_8B.id], "key", modal_gpu.QWEN3_8B.id, dry=True)
        self.assertEqual((final["state"], final["reason"]), ("failed", "preempted"))
        self.assertEqual(modal_gpu.leases["st:lease-1"]["state"], "failed")
        # A lease the broker starts again after a refused host has no container start yet.
        self.assertFalse(modal_gpu.leases.get("st:lease-2", {}).get("container_started_at"))

    def test_container_check(self) -> None:
        self.assertEqual(modal_gpu.driver_mismatch({"driver": "620.70.06"}, {}), "")
        self.assertEqual(modal_gpu.driver_mismatch({"driver": "620.70.06"}, {"required_driver": "620.70.06"}), "")
        self.assertIn("610.57.04", modal_gpu.driver_mismatch({"driver": "610.57.04"}, {"required_driver": "620.70.06"}))


if __name__ == "__main__":
    unittest.main()

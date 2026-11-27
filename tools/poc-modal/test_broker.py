"""Offline tests for the Modal GPU PoC broker: spend accounting and the vLLM argument
allowlist. Modal is stubbed, so nothing runs remotely.

    python3 -m unittest discover -s tools/poc-modal -p 'test_*.py'
"""

from __future__ import annotations

import json
import re
import sys
import tempfile
import time
import types
import unittest
import urllib.request
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


class LocalStandInTest(unittest.TestCase):
    """`--stand-in local`: the broker serves a lease from a stand-in server on this machine,
    with no Modal call and no spend."""

    def setUp(self) -> None:
        self.dir = tempfile.TemporaryDirectory()
        self.store, modal_gpu.leases = modal_gpu.leases, broker.LocalStore()
        self.broker = broker.Broker("token", 1.0, 4, 12, Path(self.dir.name) / "ledger.json", dry=True, local_host="127.0.0.1")

    def tearDown(self) -> None:
        for call in self.broker.calls.values():
            call.cancel()
        modal_gpu.leases = self.store
        self.dir.cleanup()

    def wait(self, lease_id: str, state: str) -> dict:
        lease = self.broker.leases[lease_id]
        for _ in range(150):
            lease["refreshed_at"] = 0
            self.broker.tick()
            if lease["state"] == state:
                return lease
            time.sleep(0.1)
        self.fail(f"the lease never became {state}: {lease}")

    def test_a_lease_is_served_from_this_machine_and_costs_nothing(self) -> None:
        model = modal_gpu.QWEN3_8B
        code, created = self.broker.create("pod", model.id, ["--served-model-name", model.id])
        self.assertEqual(code, 201, created)
        lease = self.wait(created["lease_id"], "ready")
        self.assertTrue(lease["url"].startswith("http://127.0.0.1:"), lease["url"])
        body = {"model": model.id, "stream": True, "stream_options": {"include_usage": True}, "max_tokens": 5, "messages": [{"role": "user", "content": "one two three"}]}
        request = urllib.request.Request(lease["url"] + "/v1/chat/completions", data=json.dumps(body).encode(), method="POST")
        request.add_header("Authorization", f"Bearer {created['api_key']}")
        with urllib.request.urlopen(request, timeout=10) as response:
            streamed = response.read().decode()
        self.assertIn('"completion_tokens": 5', streamed)
        self.assertTrue(streamed.rstrip().endswith("data: [DONE]"), streamed[-80:])
        self.broker.stop(created["lease_id"], "test")
        self.assertEqual(self.wait(created["lease_id"], "stopped")["reason"], "released")
        ledger = self.broker.ledger()
        self.assertEqual((ledger["estimated_usd"], ledger["rate_usd_per_second"]), (0.0, 0.0))
        self.assertIn("no Modal", ledger["rate_basis"])


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
        self.saved = (modal_gpu.leases, modal_gpu.serve_gpu, broker.RESPAWN_DELAY_SECONDS)
        modal_gpu.leases, modal_gpu.serve_gpu, broker.RESPAWN_DELAY_SECONDS = _Store(), types.SimpleNamespace(spawn=lambda *a: _Call()), 0
        self.broker = broker.Broker("token", 30.0, 3, 6, Path(self.dir.name) / "ledger.json", dry=False)

    def tearDown(self) -> None:
        modal_gpu.leases, modal_gpu.serve_gpu, broker.RESPAWN_DELAY_SECONDS = self.saved
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

    def test_the_next_start_waits_until_the_refused_container_is_gone(self) -> None:
        broker.RESPAWN_DELAY_SECONDS = 0.2
        self.broker.required_driver = "620.70.06"
        lease = self.lease()
        first_call = lease["call_id"]
        self.report(lease, state="failed", reason="driver_mismatch", error="NVIDIA driver 610.57.04 differs")
        self.assertEqual((lease["state"], lease.get("call_id")), ("starting", None))  # nothing started yet
        self.broker.tick()  # the watchdog does not take the refused call's end for the lease's
        self.assertEqual(lease["state"], "starting")
        time.sleep(0.4)
        self.assertNotIn(lease.get("call_id"), (None, first_call))

    def test_a_lease_stopped_while_it_waits_is_not_started_again(self) -> None:
        broker.RESPAWN_DELAY_SECONDS = 0.2
        self.broker.required_driver = "620.70.06"
        lease = self.lease()
        self.report(lease, state="failed", reason="driver_mismatch", error="mismatch")
        self.broker.stop(lease["lease_id"], "test")
        time.sleep(0.4)
        self.assertEqual((lease["state"], lease.get("call_id")), ("stopping", None))

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



class EngineAnswersTest(unittest.TestCase):
    def test_the_servers_own_answers_are_counted_from_its_access_log(self) -> None:
        with tempfile.TemporaryDirectory() as folder:
            log = Path(folder) / "server.log"
            log.write_text("\n".join([
                "INFO 11-25 22:42:31 [api_server.py:1100] Starting vLLM API server 0 on http://0.0.0.0:8000",
                'INFO:     10.0.0.7:51234 - "GET /health HTTP/1.1" 200 OK',
                'INFO:     10.0.0.7:51236 - "POST /v1/chat/completions HTTP/1.1" 200 OK',
                'INFO:     10.0.0.7:51237 - "POST /v1/chat/completions HTTP/1.1" 200 OK',
                'INFO:     10.0.0.7:51238 - "POST /v1/chat/completions HTTP/1.1" 503 Service Unavailable',
                'INFO:     10.0.0.7:51239 - "GET /v1/models?x=1 HTTP/1.1" 200 OK',
            ]))
            self.assertEqual(modal_gpu.engine_answers(log), {"read": True, "access_lines": 5, "answers": {"POST /v1/chat/completions 200": 2, "POST /v1/chat/completions 503": 1, "GET /v1/models 200": 1}})
            self.assertEqual(modal_gpu.engine_answers(Path(folder) / "missing.log"), {"read": False, "access_lines": 0, "answers": {}})

    def test_the_rust_front_ends_access_lines_are_counted(self) -> None:
        # vLLM 0.34.0's Rust front end (VLLM_USE_RUST_FRONTEND=1) logs tracing fields, with
        # colour codes around the field names when its writer keeps them.
        def colour(line: str) -> str:
            return re.sub(r"(\w+)=", lambda field: f"\x1b[3m{field[1]}\x1b[0m\x1b[2m=\x1b[0m", line)
        rust = '(APIServer pid=7) INFO 11-26 00:06:01 [access_log.rs:47] HTTP response client=10.0.0.7:{port} method={method} uri="{uri}" version=HTTP/1.1 status={status} latency=1.2ms'
        with tempfile.TemporaryDirectory() as folder:
            log = Path(folder) / "server.log"
            log.write_text("\n".join([
                "(APIServer pid=7) INFO 11-26 00:01:12 [server.rs:88] listening on 0.0.0.0:8000",
                rust.format(port=51234, method="GET", uri="/health", status=200),
                rust.format(port=51236, method="POST", uri="/v1/chat/completions", status=200),
                colour(rust.format(port=51237, method="POST", uri="/v1/chat/completions", status=200)),
                colour(rust.format(port=51238, method="POST", uri="/v1/chat/completions", status=503)),
                rust.format(port=51239, method="GET", uri="/v1/models?x=1", status=200),
            ]))
            self.assertEqual(modal_gpu.engine_answers(log), {"read": True, "access_lines": 5, "answers": {"POST /v1/chat/completions 200": 2, "POST /v1/chat/completions 503": 1, "GET /v1/models 200": 1}})

    def test_a_log_without_access_lines_is_not_a_log_without_errors(self) -> None:
        with tempfile.TemporaryDirectory() as folder:
            log = Path(folder) / "server.log"
            log.write_text("(APIServer pid=7) INFO 11-26 00:01:12 [server.rs:88] listening on 0.0.0.0:8000\nan access line in a format this parser does not know\n")
            self.assertEqual(modal_gpu.engine_answers(log), {"read": True, "access_lines": 0, "answers": {}})

if __name__ == "__main__":
    unittest.main()

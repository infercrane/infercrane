"""Offline tests for the Modal GPU PoC driver's planning: how the product planner's setups
become campaign setups. Nothing runs remotely and nothing is written to the run state."""

from __future__ import annotations

import ast
import json
import os
import subprocess
import sys
import threading
import time
import unittest
import urllib.error
import urllib.request
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import corpus  # noqa: E402
import models  # noqa: E402
import poc_modal  # noqa: E402

SPECULATION = ["--speculative-config", '{"method":"mtp","num_speculative_tokens":3}']

START = {"id": "start", "deployment": {"provider": {"adapter": "kubernetes"}, "resources": {"gpu": "H100", "gpu_count": 1}, "runtime": {"engine": "vllm", "args": []}}}


def setup(identity: str, args: list[str], label: str, gpu_count: int = 1, env: dict | None = None) -> dict:
    return {"id": identity, "configuration_profile": f"vllm-generic-baseline-{identity}", "reason": {"role": "diagnosis", "label": label, "rules": ["queue.engine_wait"], "summary": f"{label} because"},
            "deployment": {"provider": {"adapter": "kubernetes"}, "resources": {"gpu": "H100", "gpu_count": gpu_count}, "runtime": {"engine": "vllm", "args": args, **({"env": env} if env else {})}}}


def proposal(*setups: dict) -> dict:
    return {"starting_point": {"candidate_id": "start"}, "candidates": [START, *setups], "diagnosis": {"state": "planned", "summary": "requests waited in line"}}


class PlanTest(unittest.TestCase):
    def setUp(self) -> None:
        self.saved = {}
        self.originals = (poc_modal.save, poc_modal.log)
        poc_modal.save, poc_modal.log = self.saved.update, lambda *_: None

    def tearDown(self) -> None:
        poc_modal.save, poc_modal.log = self.originals
        poc_modal.RUN.update(traffic="mooncake-words", planner_setups=1, smoke=False)
        poc_modal.configure("vllm-0.22.1", "H100")

    def test_planner_change_goes_onto_the_publishers_command_and_the_copy_comes_last(self) -> None:
        record = poc_modal.plan(proposal(
            setup("spec", SPECULATION, "Speculation"),
            setup("huge", ["--max-num-seqs", "2048", "--max-num-batched-tokens", "16384"], "Larger batches"),
            setup("batch", ["--max-num-seqs", "256", "--max-num-batched-tokens", "16384"], "Fewer at once, bigger prompt chunks"),
            setup("wide", ["--max-num-seqs", "512"], "Two GPUs", gpu_count=2),
            setup("graphs", ["--max-cudagraph-capture-size", "640"], "CUDA graphs")))
        labels = [label for label, _, _ in poc_modal.CANDIDATES]
        self.assertEqual(labels, [poc_modal.CONTROL[0], "Fewer at once, bigger prompt chunks", poc_modal.COPY[0]])
        self.assertEqual(poc_modal.CANDIDATES[1][2], [*poc_modal.MODEL.publisher_args, "--max-num-seqs", "256", "--max-num-batched-tokens", "16384"])
        skipped = {row["label"]: row["skipped"] for row in record}
        self.assertIn("no real text", skipped["Speculation"])
        self.assertIsNone(self.saved["planner"]["documented_speculation"])
        self.assertIn("more than one GPU", skipped["Two GPUs"])
        self.assertIn("first 1 setup", skipped["CUDA graphs"])
        self.assertIn("at most 818 requests at once", skipped["Larger batches"])
        self.assertEqual(skipped["Fewer at once, bigger prompt chunks"], "")
        self.assertEqual(self.saved["planner"]["setups"], record)

    def test_nothing_to_try_measures_nothing(self) -> None:
        poc_modal.plan(proposal(setup("tools", ["--enable-auto-tool-choice"], "Already set"), setup("env", ["--max-num-seqs", "512"], "AITER", env={"VLLM_ROCM_USE_AITER": "1"})))
        self.assertEqual(poc_modal.CANDIDATES, [poc_modal.CONTROL])
        reasons = [row["skipped"] for row in self.saved["planner"]["setups"]]
        self.assertIn("the current setup already makes this change", reasons)
        self.assertTrue(any("environment" in reason for reason in reasons))

    def test_the_copy_differs_only_in_its_seed(self) -> None:
        self.assertEqual(poc_modal.COPY[2], [*poc_modal.CONTROL[2], "--seed", "1"])

    def test_real_text_measures_the_planners_speculation_setup(self) -> None:
        poc_modal.RUN.update(traffic="real-text", planner_setups=2)
        record = poc_modal.plan(proposal(setup("kv", ["--kv-cache-dtype", "fp8"], "FP8 KV cache"), setup("spec", SPECULATION, "Speculation"),
                                         setup("graphs", ["--max-cudagraph-capture-size", "640"], "CUDA graphs")))
        self.assertEqual([label for label, _, _ in poc_modal.CANDIDATES], [poc_modal.CONTROL[0], "FP8 KV cache", "Speculation", poc_modal.COPY[0]])
        self.assertEqual(poc_modal.CANDIDATES[2][2], poc_modal.MODEL.speculation_args)
        self.assertEqual({row["label"]: row["skipped"] for row in record}["Speculation"], "")
        documented = self.saved["planner"]["documented_speculation"]
        self.assertEqual((documented["proposed_by_planner"], documented["planner_rank"], documented["args"]), (True, 2, poc_modal.MODEL.speculation_args))

    def test_real_text_keeps_a_slot_for_the_planners_speculation_setup_when_it_ranks_lower(self) -> None:
        poc_modal.RUN.update(traffic="real-text", planner_setups=2)
        record = poc_modal.plan(proposal(setup("kv", ["--kv-cache-dtype", "fp8"], "FP8 KV cache"), setup("graphs", ["--max-cudagraph-capture-size", "640"], "CUDA graphs"),
                                         setup("spec", SPECULATION, "Speculation")))
        self.assertEqual([label for label, _, _ in poc_modal.CANDIDATES], [poc_modal.CONTROL[0], "FP8 KV cache", "Speculation", poc_modal.COPY[0]])
        self.assertIn("next to the current setup", {row["label"]: row["skipped"] for row in record}["CUDA graphs"])

    def test_real_text_measures_the_documented_option_when_the_planner_did_not_propose_it(self) -> None:
        poc_modal.RUN.update(traffic="real-text", planner_setups=2)
        other = ["--speculative-config", '{"method":"mtp","num_speculative_tokens":1}']
        poc_modal.plan(proposal(setup("kv", ["--kv-cache-dtype", "fp8"], "FP8 KV cache"), setup("one", other, "One drafted token")))
        labels = [label for label, _, _ in poc_modal.CANDIDATES]
        self.assertEqual(labels, [poc_modal.CONTROL[0], "FP8 KV cache", poc_modal.DOCUMENTED_LABEL, poc_modal.COPY[0]])
        self.assertEqual(poc_modal.CANDIDATES[2][2], poc_modal.MODEL.speculation_args)
        self.assertFalse(self.saved["planner"]["documented_speculation"]["proposed_by_planner"])
        # With nothing from the planner at all, the documented option still makes a campaign.
        poc_modal.plan(proposal())
        self.assertEqual([label for label, _, _ in poc_modal.CANDIDATES], [poc_modal.CONTROL[0], poc_modal.DOCUMENTED_LABEL, poc_modal.COPY[0]])

    def test_a_smoke_run_measures_the_speculation_setup_alone(self) -> None:
        poc_modal.RUN.update(traffic="real-text", planner_setups=2, smoke=True)
        poc_modal.plan(proposal(setup("kv", ["--kv-cache-dtype", "fp8"], "FP8 KV cache"), setup("spec", SPECULATION, "Speculation")))
        self.assertEqual([label for label, _, _ in poc_modal.CANDIDATES], [poc_modal.CONTROL[0], "Speculation", poc_modal.COPY[0]])
        poc_modal.plan(proposal(setup("kv", ["--kv-cache-dtype", "fp8"], "FP8 KV cache")))
        self.assertEqual([label for label, _, _ in poc_modal.CANDIDATES], [poc_modal.CONTROL[0], poc_modal.DOCUMENTED_LABEL, poc_modal.COPY[0]])

    def test_planner_setups_zero_records_the_plan_and_measures_the_speculation_setup_alone(self) -> None:
        poc_modal.RUN.update(traffic="real-text", planner_setups=0)
        record = poc_modal.plan(proposal(setup("chunks", ["--max-num-batched-tokens", "16384"], "Bigger prompt chunks"), setup("spec", SPECULATION, "Speculation")))
        self.assertEqual([label for label, _, _ in poc_modal.CANDIDATES], [poc_modal.CONTROL[0], "Speculation", poc_modal.COPY[0]])
        self.assertIn("speculation setup", {row["label"]: row["skipped"] for row in record}["Bigger prompt chunks"])
        self.assertEqual(self.saved["planner"]["documented_speculation"]["planner_rank"], 2)
        poc_modal.plan(proposal(setup("chunks", ["--max-num-batched-tokens", "16384"], "Bigger prompt chunks")))
        self.assertEqual([label for label, _, _ in poc_modal.CANDIDATES], [poc_modal.CONTROL[0], poc_modal.DOCUMENTED_LABEL, poc_modal.COPY[0]])

    def test_a_current_setup_that_speculates_needs_no_documented_arm(self) -> None:
        poc_modal.configure("vllm-0.34.0", "H100", "publisher-speculation")
        poc_modal.RUN.update(traffic="real-text", planner_setups=2)
        record = poc_modal.plan(proposal(setup("spec", SPECULATION, "Speculation"), setup("kv", ["--kv-cache-dtype", "fp8"], "FP8 KV cache")))
        self.assertEqual(poc_modal.CONTROL[2], poc_modal.MODEL.speculation_args)
        self.assertEqual([label for label, _, _ in poc_modal.CANDIDATES], [poc_modal.CONTROL[0], "FP8 KV cache", poc_modal.COPY[0]])
        self.assertEqual(poc_modal.CANDIDATES[1][2], [*poc_modal.MODEL.speculation_args, "--kv-cache-dtype", "fp8"])
        self.assertIn("already makes this change", {row["label"]: row["skipped"] for row in record}["Speculation"])
        self.assertIsNone(self.saved["planner"]["documented_speculation"])


class EngineTest(unittest.TestCase):
    def tearDown(self) -> None:
        poc_modal.configure("vllm-0.22.1", "H100")

    def test_the_newest_engine_uses_the_publishers_environment_and_the_limit_its_check_found(self) -> None:
        model = models.QWEN38_27B_FP8
        poc_modal.configure("vllm-0.22.1", "H100")
        self.assertEqual((model.current_args[-2:], model.env, model.max_num_seqs), (["--max-num-seqs", "512"], {}, 818))
        self.assertEqual(poc_modal.ENGINE_VERSION, "vllm-openai-0.22.1-953d3a06")
        poc_modal.configure("vllm-0.34.0", "H200")
        self.assertEqual((model.current_args, model.env, model.max_num_seqs), ([*model.publisher_args, "--max-num-seqs", "512"], model.publisher_env, 904))
        self.assertEqual((poc_modal.ENGINE_VERSION, poc_modal.poc.ACCELERATOR, poc_modal.poc.ACCELERATOR_RESOURCE), ("vllm-openai-0.34.0-20fdb089", "H200", "infercrane.dev/modal-h200-lease"))
        self.assertEqual((os.environ[models.ENGINE_VARIABLE], os.environ[models.ACCELERATOR_VARIABLE]), ("vllm-0.34.0", "H200"))
        self.assertAlmostEqual(poc_modal.GPU_HOURLY_USD, 4.54, places=2)
        for engine in models.ENGINES.values():
            self.assertRegex(engine.image, r"^vllm/vllm-openai@sha256:[0-9a-f]{64}$")

    def test_speculation_is_the_models_own_head_with_bounded_tokens(self) -> None:
        self.assertEqual(models.speculation(SPECULATION[1]), {"method": "mtp", "num_speculative_tokens": 3})
        self.assertTrue(models.speculates(SPECULATION))
        self.assertFalse(models.speculates(["--max-num-seqs", "512"]))
        for value in ('{"method":"ngram","num_speculative_tokens":3}', '{"method":"mtp","num_speculative_tokens":0}', '{"method":"mtp"}', "[]", "{"):
            with self.subTest(value=value), self.assertRaises(ValueError):
                models.speculation(value)


class CorpusTest(unittest.TestCase):
    def setUp(self) -> None:
        self.corpus = corpus.load(corpus.STAND_IN)

    def test_the_rehearsal_corpus_states_its_licence_and_token_counts(self) -> None:
        identity = self.corpus.identity
        self.assertEqual((identity["license"], identity["passages"], identity["prompts"]), ("Apache-2.0", 12, 9))
        self.assertTrue(identity["tokenizer"].startswith(models.QWEN38_27B_FP8.id + "@"))
        self.assertRegex(identity["digest"], r"^sha256:[0-9a-f]{64}$")

    def test_rows_that_share_leading_blocks_share_their_leading_text(self) -> None:
        first, planned = corpus.compose(self.corpus, [11, 12, 13], 1400, 512, 0)
        second, _ = corpus.compose(self.corpus, [11, 12, 99], 1400, 512, 1)
        other, _ = corpus.compose(self.corpus, [70, 71, 72], 1400, 512, 2)
        self.assertEqual(first[0]["content"], second[0]["content"])
        self.assertNotEqual(first[1]["content"], second[1]["content"])
        self.assertNotEqual(first[0]["content"].splitlines()[0], other[0]["content"].splitlines()[0])
        self.assertLess(abs(planned - 1400), 170)  # within one passage of the recorded length
        self.assertTrue(first[1]["content"].rstrip().endswith("?"))

    def test_a_row_too_short_for_a_passage_sends_a_standalone_prompt(self) -> None:
        messages, planned = corpus.compose(self.corpus, [5], 60, 512, 0)
        self.assertIn(messages[1]["content"], [prompt["text"] for prompt in self.corpus.prompts])
        self.assertLess(planned, 120)

    def test_a_corpus_without_a_licence_or_token_counts_is_refused(self) -> None:
        import tempfile

        for change in (lambda d: d.update(license=""), lambda d: d["passages"][0].update(tokens=0), lambda d: d["passages"][1].update(questions=[]), lambda d: d.update(prompts=[])):
            document = json.loads(corpus.STAND_IN.read_text())
            change(document)
            with tempfile.NamedTemporaryFile("w", suffix=".json") as handle:
                json.dump(document, handle)
                handle.flush()
                with self.assertRaises(ValueError):
                    corpus.load(handle.name)


class StandInServerTest(unittest.TestCase):
    """The stand-in server answers like an engine for the parts a campaign reads: token
    usage on a stream, and vLLM's metric families, with speculation counters only when it
    was started with a speculation option."""

    def serve(self, port: int, *args: str) -> subprocess.Popen:
        tree = ast.parse((Path(__file__).resolve().parent / "modal_gpu.py").read_text())
        source = next(node.value.value for node in tree.body if isinstance(node, ast.Assign) and getattr(node.targets[0], "id", "") == "FAKE_SERVER")
        process = subprocess.Popen([sys.executable, "-c", source, str(port), "key", "model", *args], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        self.addCleanup(lambda: (process.kill(), process.wait()))
        for _ in range(100):
            try:
                urllib.request.urlopen(f"http://127.0.0.1:{port}/health", timeout=1)
                return process
            except OSError:
                time.sleep(0.05)
        self.fail("the stand-in server did not start")

    def chat(self, port: int) -> str:
        body = {"model": "model", "stream": True, "stream_options": {"include_usage": True}, "max_completion_tokens": 8,
                "messages": [{"role": "system", "content": "Session 1"}, {"role": "user", "content": "Why does the sea have tides?"}]}
        request = urllib.request.Request(f"http://127.0.0.1:{port}/v1/chat/completions", data=json.dumps(body).encode(), headers={"Authorization": "Bearer key", "Content-Type": "application/json"})
        return urllib.request.urlopen(request, timeout=10).read().decode()

    def test_streams_usage_and_exports_speculation_counters_only_when_speculating(self) -> None:
        self.serve(18441)
        self.serve(18442, *SPECULATION)
        for port, speculating in ((18441, False), (18442, True)):
            stream = self.chat(port)
            usage = json.loads([line for line in stream.splitlines() if '"usage"' in line][0].removeprefix("data: "))["usage"]
            self.assertEqual(usage["completion_tokens"], 8)
            self.assertTrue(stream.rstrip().endswith("data: [DONE]"))
            metrics = urllib.request.urlopen(f"http://127.0.0.1:{port}/metrics", timeout=5).read().decode()
            for family in ("vllm:num_requests_running", "vllm:request_decode_time_seconds_bucket", "vllm:generation_tokens_total"):
                self.assertIn(family, metrics)
            self.assertEqual("vllm:spec_decode_num_accepted_tokens_total" in metrics, speculating)
        unauthorized = urllib.request.Request("http://127.0.0.1:18441/v1/chat/completions", data=b"{}", headers={"Content-Type": "application/json"})
        with self.assertRaises(urllib.error.HTTPError) as refused:
            urllib.request.urlopen(unauthorized, timeout=5)
        refused.exception.close()


class EvidenceTest(unittest.TestCase):
    def test_engine_measured_needs_a_windowed_measurement(self) -> None:
        gauge_only = {"evidence": {"measurements": [{"name": "runtime_requests_running", "availability": "available"}, {"name": "runtime_queue_time_p95", "availability": "not_observed"}]}}
        self.assertFalse(poc_modal.engine_measured(gauge_only))
        gauge_only["evidence"]["measurements"].append({"name": "runtime_decode_time_p95", "availability": "available"})
        self.assertTrue(poc_modal.engine_measured(gauge_only))

    def test_speculation_acceptance_comes_from_the_counters_the_product_stored(self) -> None:
        def run(arm: str, drafted: float | None, accepted: float | None) -> dict:
            metrics = {} if drafted is None else {poc_modal.DRAFT_TOKENS: {"stats": {"total": drafted}}, poc_modal.ACCEPTED_TOKENS: {"stats": {"total": accepted}}}
            return {"arm": arm, "server_metrics": {"collected": True, "metrics": metrics}}

        measured = {"runs": [run("control", None, None), run("candidate", 300, 180), run("control", None, None), run("candidate", 100, 60)],
                    "confirmation": {"runs": [run("candidate", 200, 100)]}}
        evidence = poc_modal.speculation_evidence(measured)
        self.assertEqual(evidence["candidate/first"], {"draft_tokens": 400.0, "accepted_tokens": 240.0, "runs": 2, "acceptance": 0.6})
        self.assertEqual(evidence["candidate/fresh_servers"]["acceptance"], 0.5)
        self.assertNotIn("control/first", evidence)  # no counters is unknown, never zero
        self.assertIsNone(poc_modal.speculation_evidence({"runs": [run("candidate", None, None)]}))

    def test_a_smoke_run_shrinks_the_traffic_and_keeps_enough_for_the_planner(self) -> None:
        self.assertGreaterEqual(poc_modal.SMOKE["requests"], 40)  # the planner needs 20 finished requests
        self.assertGreaterEqual(poc_modal.SMOKE["spread_seconds"], 90)  # and two engine scrapes 30 s apart
        self.assertLess(poc_modal.SMOKE["max_input_tokens"], poc_modal.MAX_INPUT_TOKENS)

    def test_a_traffic_shape_moves_the_medians_and_keeps_the_spread(self) -> None:
        rows = [{"input_length": value, "output_length": value // 20} for value in (2000, 4000, 6000, 8000, 40000)]
        self.assertEqual(poc_modal.shape_scales("mooncake", rows), {"input_scale": 2.0, "output_scale": 1.0, "max_output_tokens": 512})
        scales = poc_modal.shape_scales("decode-heavy", rows)
        self.assertAlmostEqual(6000 / scales["input_scale"], 3919)
        self.assertAlmostEqual(300 / scales["output_scale"], 1377)
        self.assertAlmostEqual((40000 / scales["input_scale"]) / (2000 / scales["input_scale"]), 20)  # the spread stays
        self.assertEqual(scales["max_output_tokens"], 4096)
        self.assertEqual(poc_modal.shape_scales("interactive", rows)["max_output_tokens"], 1180)

    def test_a_streamed_answer_counts_only_when_it_reaches_its_end(self) -> None:
        stream = [b'data: {"choices":[{"delta":{"reasoning":"think"}}]}\n', b"\n", b'data: {"choices":[{"delta":{"content":"hi"}}]}\n',
                  b'data: {"choices":[],"usage":{"prompt_tokens":40,"completion_tokens":7}}\n', b"data: [DONE]\n"]
        self.assertEqual(poc_modal.stream_usage(stream), ({"prompt_tokens": 40, "completion_tokens": 7}, True))
        self.assertEqual(poc_modal.stream_usage(stream[:3]), (None, False))  # the server went away mid-answer
        self.assertEqual(poc_modal.stream_usage([b"data: not json\n", b"data: [DONE]\n"]), (None, True))

    def test_a_run_can_name_the_nvidia_driver_its_servers_must_have(self) -> None:
        saved = dict(poc_modal.RUN)
        try:
            poc_modal.RUN.update(stand_in=False, driver=None)
            self.assertNotIn("--driver", poc_modal.broker_command(38.0, False, 16))
            poc_modal.RUN.update(driver="620.70.06")
            command = poc_modal.broker_command(38.0, False, 16)
            self.assertEqual(command[command.index("--driver") + 1], "620.70.06")
            self.assertEqual(command[command.index("--max-starts") + 1], "16")
            # A stand-in has no GPU and so no driver to hold it to.
            self.assertNotIn("--driver", poc_modal.broker_command(1.0, True, 16))
            poc_modal.RUN.update(stand_in="local")
            self.assertNotIn("--driver", poc_modal.broker_command(1.0, "local", 16))
        finally:
            poc_modal.RUN.clear()
            poc_modal.RUN.update(saved)

    def test_the_fresh_servers_decide_the_copy(self) -> None:
        first_only = {"comparison": {"output_token_throughput": {"decision": "improved"}}}
        self.assertEqual(poc_modal.objective_decision(first_only), "improved")
        rechecked = dict(first_only, confirmation={"output_token_throughput": {"decision": "no_measurable_difference"}})
        self.assertEqual(poc_modal.objective_decision(rechecked), "no_measurable_difference")


ROWS = [{"timestamp": 0, "input_length": 1400, "output_length": 200, "hash_ids": [11, 12, 13]},
        {"timestamp": 400, "input_length": 1400, "output_length": 90, "hash_ids": [11, 12, 27]},
        {"timestamp": 900, "input_length": 30, "output_length": 40}]


class HoldTest(unittest.TestCase):
    """After the replay the server is held at the campaign's load, and the planner is asked
    during that hold: a replay alone ends before the control plane has measured it."""

    def setUp(self) -> None:
        self.saved = {name: getattr(poc_modal, name) for name in ("send_chat", "state", "HOLD_BEFORE_ASK_SECONDS", "PLANNER_POLL_SECONDS", "engine_measured")}
        self.poc = {name: getattr(poc_modal.poc, name) for name in ("mooncake_rows", "api", "cli")}
        poc_modal.state = lambda: {"api_key": "key"}
        poc_modal.poc.mooncake_rows = lambda: ROWS
        poc_modal.poc.api = lambda *args, **kwargs: {}
        poc_modal.poc.cli = lambda *args, **kwargs: '{"candidates": []}'
        poc_modal.HOLD_BEFORE_ASK_SECONDS, poc_modal.PLANNER_POLL_SECONDS = 0.15, 0.02

    def tearDown(self) -> None:
        for name, value in self.saved.items():
            setattr(poc_modal, name, value)
        for name, value in self.poc.items():
            setattr(poc_modal.poc, name, value)

    def test_the_hold_sends_the_same_shapes_with_other_text(self) -> None:
        text = corpus.load(corpus.STAND_IN)
        first, _ = poc_modal.chat_request(ROWS[0], 0, 1.0, 1.0, 30000, 128, text)
        again, _ = poc_modal.chat_request(ROWS[0], 0, 1.0, 1.0, 30000, 128, text, shift=1_000_003)
        self.assertNotEqual(first["messages"], again["messages"])
        self.assertEqual((first["max_tokens"], first["stream"]), (again["max_tokens"], True))
        # Two rows that share leading blocks still share their leading text after the shift.
        other, _ = poc_modal.chat_request(ROWS[1], 1, 1.0, 1.0, 30000, 128, text, shift=1_000_003)
        self.assertEqual(again["messages"][0]["content"].split("\n\n")[:2], other["messages"][0]["content"].split("\n\n")[:2])
        words, _ = poc_modal.chat_request(ROWS[2], 2, 1.0, 1.0, 30000, 128, None)
        moved, _ = poc_modal.chat_request(ROWS[2], 2, 1.0, 1.0, 30000, 128, None, shift=1_000_003)
        self.assertNotEqual(words["messages"], moved["messages"])
        self.assertTrue(moved["ignore_eos"])

    def test_the_hold_keeps_the_campaigns_load_until_the_planner_has_been_asked(self) -> None:
        running, peak, asked = [0], [0], threading.Event()
        lock = threading.Lock()

        def send(body: dict, session: int, key: str) -> tuple[int, str]:
            with lock:
                running[0] += 1
                peak[0] = max(peak[0], running[0])
            time.sleep(0.02)
            with lock:
                running[0] -= 1
            return 5, ""

        poc_modal.send_chat = send
        threading.Timer(0.3, asked.set).start()
        hold = poc_modal.hold_load(3, 4, 1.0, 128, asked.is_set)
        self.assertEqual((peak[0], hold["requests_at_once"], hold["failed"], hold["recorded_in_the_traffic_shape"]), (4, 4, 0, False))
        self.assertGreater(hold["requests"], 3)  # the rows came round again

    def test_the_planner_is_asked_during_the_hold(self) -> None:
        poc_modal.engine_measured = lambda document: True
        planner = poc_modal.Planner()
        self.assertFalse(planner.thread.is_alive())  # nothing is asked before the hold
        planner.hold(32)
        self.assertTrue(planner.done.wait(5))
        self.assertRegex(planner.asked, r"^while the server was held at 32 requests at once, \d+ s into the hold$")
        self.assertEqual(planner.finish(), {"candidates": []})

    def test_a_hold_that_ends_first_is_said_so(self) -> None:
        poc_modal.engine_measured = lambda document: False
        planner = poc_modal.Planner()
        planner.hold(32)
        self.assertEqual(planner.finish(), {"candidates": []})
        self.assertEqual(planner.asked, "after the hold ended, without windowed engine measurements")


if __name__ == "__main__":
    unittest.main()

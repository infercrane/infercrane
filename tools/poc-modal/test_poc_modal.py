"""Offline tests for the Modal GPU PoC driver's planning: how the product planner's setups
become campaign setups. Nothing runs remotely and nothing is written to the run state."""

from __future__ import annotations

import ast
import hashlib
import io
import json
import os
import subprocess
import sys
import tempfile
import threading
import time
import unittest
import urllib.error
import urllib.request
from pathlib import Path
from unittest import mock

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
        poc_modal.RUN.update(traffic="mooncake-words", planner_setups=1, smoke=False, plain_arm=False)
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

    def test_the_plain_arm_measures_both_documented_setups_next_to_the_planners_first(self) -> None:
        # Section 5, step 1: from the documented speculation option at 128, the current setup,
        # the planner's first setup, the publisher's plain command, and the copy: four servers.
        poc_modal.configure("vllm-0.34.0", "H100", "publisher-speculation")
        poc_modal.RUN.update(traffic="map-cell", planner_setups=2, plain_arm=True)
        one = ["--speculative-config", '{"method":"mtp","num_speculative_tokens":1}']
        record = poc_modal.plan(proposal(setup("one", one, "One draft token"), setup("cap", ["--max-num-seqs", "40"], "Fewer requests at once")))
        self.assertEqual([label for label, _, _ in poc_modal.CANDIDATES], [poc_modal.CONTROL[0], "One draft token", poc_modal.PLAIN_LABEL, poc_modal.COPY[0]])
        self.assertEqual(poc_modal.CANDIDATES[1][2], [*poc_modal.MODEL.current_args, *one])
        self.assertEqual(poc_modal.CANDIDATES[2][2], poc_modal.MODEL.current_args)
        self.assertIn("first 1 setup", {row["label"]: row["skipped"] for row in record}["Fewer requests at once"])
        self.assertEqual(self.saved["plain_command"], {"args": poc_modal.MODEL.current_args, "proposed_by_planner": False, "in_planners_list": False})
        self.assertLessEqual(len(poc_modal.CANDIDATES), poc_modal.CANDIDATE_SLOTS + 2)
        # The planner's own checks of its plan (the plain command and a copy) are recorded,
        # not measured: the harness measures its own copy and plain command once each.
        start = {**START, "deployment": {**START["deployment"], "runtime": {"engine": "vllm", "args": SPECULATION}}}
        plain = {**setup("plain", [], "The starting setup without its speculation"), "reason": {"role": "reference", "label": "The starting setup without its speculation"}}
        copy = {**setup("copy", [*SPECULATION, "--seed", "1"], "A copy of the starting setup (must not win)"), "reason": {"role": "copy", "label": "A copy of the starting setup (must not win)"}}
        record = poc_modal.plan({**proposal(setup("one", one, "One draft token"), plain, copy), "candidates": [start, setup("one", one, "One draft token"), plain, copy]})
        self.assertEqual([label for label, _, _ in poc_modal.CANDIDATES], [poc_modal.CONTROL[0], "One draft token", poc_modal.PLAIN_LABEL, poc_modal.COPY[0]])
        skipped = {row["label"]: row["skipped"] for row in record}
        self.assertIn("a check of the plan", skipped["The starting setup without its speculation"])
        self.assertIn("a check of the plan", skipped["A copy of the starting setup (must not win)"])
        self.assertTrue(self.saved["plain_command"]["in_planners_list"])
        # Without speculation in the current setup the plain command is the current setup.
        poc_modal.configure("vllm-0.34.0", "H100", "publisher")
        poc_modal.plan(proposal(setup("one", one, "One draft token")))
        self.assertNotIn(poc_modal.PLAIN_LABEL, [label for label, _, _ in poc_modal.CANDIDATES])
        poc_modal.RUN.update(plain_arm=False)


class OverloadRehearsalTest(unittest.TestCase):
    """A rehearsal of a cell at 128 runs overloaded stand-ins and passes only when a health check
    was missed with the route kept and no request failed."""

    def setUp(self) -> None:
        self.originals = (poc_modal.log, poc_modal.poc.api, dict(poc_modal.RUN))
        poc_modal.log = lambda *_: None

    def tearDown(self) -> None:
        poc_modal.log, poc_modal.poc.api = self.originals[:2]
        poc_modal.RUN.clear()
        poc_modal.RUN.update(self.originals[2])

    def events(self, *types: str) -> None:
        poc_modal.poc.api = lambda *args, **kwargs: {"data": [{"type": kind} for kind in ("replica_healthy", *types)]}

    def test_only_local_stand_ins_of_a_heavy_cell_act_overloaded(self) -> None:
        for stand_in, load, expected in (("local", 128, True), ("local", 32, False), ("modal", 128, False), (False, 128, False)):
            poc_modal.RUN.update(stand_in=stand_in, cell={"requests_at_once": load})
            self.assertEqual(poc_modal.stand_in_overloaded(), expected, (stand_in, load))

    def test_passes_only_with_a_missed_check_and_the_route_kept(self) -> None:
        self.events("replica_probe_missed")
        self.assertEqual(poc_modal.rehearsal_health_checks(), {"missed_with_route_kept": 1, "withdrawn": 0})
        self.events("replica_probe_missed", "replica_unhealthy")
        with self.assertRaises(SystemExit):
            poc_modal.rehearsal_health_checks()
        self.events()
        with self.assertRaises(SystemExit):
            poc_modal.rehearsal_health_checks()

    def test_a_stand_in_cell_load_fails_on_any_failed_request_and_names_why(self) -> None:
        poc_modal.RUN.update(stand_in="local")
        replies = iter([(10, ""), (0, "404"), (10, "")] + [(10, "")] * 10)
        calls = {"n": 0}

        def until() -> bool:
            calls["n"] += 1
            return calls["n"] > 9

        cell = {"key": "short/2", "kind": "short", "chains": [[("q", 8)], [("q", 8)]], "requests_at_once": 1, "reasoning": False, "prompts_sha256": "", "cells_sha256": "",
                "ask_after_seconds": 0, "warmup_seconds": 0}
        messages: list[str] = []
        with mock.patch.object(poc_modal, "send_chat", lambda *args: next(replies)), mock.patch.object(poc_modal, "state", lambda: {"api_key": "key"}), \
                mock.patch.object(poc_modal.time, "sleep", lambda *_: None), mock.patch.object(poc_modal, "fail", lambda message: (messages.append(message), sys.exit(1))):
            with self.assertRaises(SystemExit):
                poc_modal.cell_load(cell, until)
        self.assertIn("404 x1", messages[0])


class StartingSetupRefusalTest(unittest.TestCase):
    """Step 1's first run A: every technique was refused as a Dynamo serving graph."""

    def setUp(self) -> None:
        self.originals = (poc_modal.save, poc_modal.log)
        poc_modal.save, poc_modal.log = (lambda *_: None), (lambda *_: None)

    def tearDown(self) -> None:
        poc_modal.save, poc_modal.log = self.originals

    def test_a_refusal_for_the_products_own_starting_setup_stops_the_run(self) -> None:
        refused = proposal()
        refused["diagnosis"]["techniques"] = [{"label": "Speculative decoding with one draft token", "skipped": "The starting setup is a Dynamo serving graph; its runtime changes go through the serving topology, not engine flags."}]
        with self.assertRaises(SystemExit):
            poc_modal.plan(refused)
        # A refusal that comes from the model or the GPU is the traffic's answer, and the run goes on.
        fine = proposal()
        fine["diagnosis"]["techniques"] = [{"label": "FP8 weights", "skipped": "The model's weights already look quantized, so FP8 weights would not apply."}]
        poc_modal.plan(fine)


class CliBinaryTest(unittest.TestCase):
    """The harness's CLI is built again whenever the checkout it comes from changes."""

    def test_a_cli_from_another_commit_is_built_again(self) -> None:
        poc = poc_modal.poc
        builds, heads = [], ["aaa"]

        def fake_run(*args, **kwargs):
            builds.append(args)
            Path(args[3]).write_text("binary")

        def fake_git(command, **kwargs):
            out = heads[0] + "\n" if command[:2] == ["git", "rev-parse"] else ""
            return subprocess.CompletedProcess(command, 0, stdout=out, stderr="")

        with tempfile.TemporaryDirectory() as folder, mock.patch.object(poc, "STATE_DIR", Path(folder)), mock.patch.object(poc, "run", fake_run), \
                mock.patch.object(poc.subprocess, "run", fake_git):
            poc.cli_binary()
            poc.cli_binary()
            self.assertEqual(len(builds), 1)  # same commit, clean tree: the binary is kept
            heads[0] = "bbb"
            poc.cli_binary()
            self.assertEqual(len(builds), 2)  # another commit: built again


class CommandLineTest(unittest.TestCase):
    """What `run` refuses before anything starts."""

    def refused(self, *args: str) -> str:
        completed = subprocess.run([sys.executable, str(Path(poc_modal.__file__)), *args], capture_output=True, text=True, timeout=60)
        self.assertNotEqual(completed.returncode, 0, completed.stdout)
        return completed.stdout + completed.stderr

    def test_a_campaign_at_128_runs_on_a_map_cell_only(self) -> None:
        self.assertIn("runs on a map cell", self.refused("run", "--requests-at-once", "128", "--engine", "vllm-0.34.0"))

    def test_a_band_study_needs_two_to_eight_campaigns(self) -> None:
        self.assertIn("bands takes 2 to 8 campaign IDs", self.refused("bands", "--campaigns", "only-one"))

    def test_a_campaign_replays_at_most_the_map_cells_requests(self) -> None:
        self.assertIn("--cell-requests takes 1 to 1000", self.refused("run", "--cell-requests", "0", "--engine", "vllm-0.34.0"))

    def test_the_plain_arm_needs_the_speculation_option_as_the_current_setup(self) -> None:
        self.assertIn("--plain-arm is for run with --current-setup publisher-speculation", self.refused("run", "--plain-arm", "--engine", "vllm-0.34.0"))
        self.assertIn("--plain-arm is for run", self.refused("ask-planner", "--plain-arm", "--current-setup", "publisher-speculation", "--engine", "vllm-0.34.0"))


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

    def test_qwen3_8b_on_one_l4_serves_with_the_context_an_l4_holds(self) -> None:
        try:
            poc_modal.select_model(models.QWEN3_8B.id)
            poc_modal.configure("vllm-0.34.0", "L4")
            self.assertEqual((poc_modal.MODEL_ID, poc_modal.poc.MODEL_ID, poc_modal.CONTROL[2]), (models.QWEN3_8B.id, models.QWEN3_8B.id, ["--max-model-len", "8192"]))
            self.assertEqual((poc_modal.poc.ACCELERATOR, models.ACCELERATOR.modal_gpu, models.QWEN3_8B.setup.checked), ("L4", "L4", "2026-11-25"))
            self.assertAlmostEqual(poc_modal.GPU_HOURLY_USD, 0.7992, places=4)
            self.assertEqual(poc_modal.COPY[2], ["--max-model-len", "8192", "--seed", "1"])
            models.check_args(["--served-model-name", models.QWEN3_8B.id, *poc_modal.CONTROL[2]], models.QWEN3_8B.id)
            poc_modal.configure("vllm-0.34.0", "H100")
            self.assertEqual(poc_modal.CONTROL[2], [])  # an H100 holds the model's own context
        finally:
            poc_modal.select_model(models.QWEN38_27B_FP8.id)

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

    def serve(self, port: int, *args: str, env: dict | None = None) -> subprocess.Popen:
        tree = ast.parse((Path(__file__).resolve().parent / "modal_gpu.py").read_text())
        source = next(node.value.value for node in tree.body if isinstance(node, ast.Assign) and getattr(node.targets[0], "id", "") == "FAKE_SERVER")
        process = subprocess.Popen([sys.executable, "-c", source, str(port), "key", "model", *args], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                                   env=dict(os.environ, **(env or {})))
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

    def test_a_rehearsal_can_have_the_stand_in_drop_a_chat_request(self) -> None:
        self.serve(18444, env={"STAND_IN_DROP_CHAT_REQUESTS": "2"})
        self.assertIn("[DONE]", self.chat(18444))
        with self.assertRaises((urllib.error.URLError, ConnectionError)):
            self.chat(18444)  # closed without an answer
        self.assertIn("[DONE]", self.chat(18444))

    def test_the_stand_in_queues_connections_like_the_real_server(self) -> None:
        # A cell starts with 128 requests at once, each a new connection from the runtime proxy.
        tree = ast.parse((Path(__file__).resolve().parent / "modal_gpu.py").read_text())
        source = next(node.value.value for node in tree.body if isinstance(node, ast.Assign) and getattr(node.targets[0], "id", "") == "FAKE_SERVER")
        self.assertIn("request_queue_size = 2048", source)

    def test_an_overloaded_stand_in_is_slow_on_health_only_while_a_burst_runs(self) -> None:
        # Step 1's second paid run: one health check under 128 requests answered too late.
        self.serve(18443, env={"STAND_IN_FIRST_TOKEN_SECONDS": "2", "STAND_IN_SLOW_HEALTH_SECONDS": "0.8"})

        def health_seconds() -> float:
            started = time.monotonic()
            urllib.request.urlopen("http://127.0.0.1:18443/health", timeout=5).read()
            return time.monotonic() - started

        self.assertLess(health_seconds(), 0.5)
        # A lone request answers at once: the router's readiness check, a one-token completion,
        # allows 500 ms.
        ready = urllib.request.Request("http://127.0.0.1:18443/v1/chat/completions", data=json.dumps({"model": "model", "max_tokens": 1, "messages": [{"role": "user", "content": "ready"}]}).encode(),
                                       headers={"Authorization": "Bearer key", "Content-Type": "application/json"})
        started = time.monotonic()
        urllib.request.urlopen(ready, timeout=5).read()
        self.assertLess(time.monotonic() - started, 0.5)
        started = time.monotonic()
        threads = [threading.Thread(target=self.chat, args=(18443,)) for _ in range(128)]  # the cell's load; those past the 64th wait
        for thread in threads:
            thread.start()
        time.sleep(0.7)
        self.assertGreaterEqual(health_seconds(), 0.8)
        for thread in threads:
            thread.join()
        # The burst's requests waited for a first token, and with the burst over /health is fast again.
        self.assertGreaterEqual(time.monotonic() - started, 2.0)
        self.assertLess(health_seconds(), 0.5)


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


SESSIONS = [{"id": f"s{number}", "context": f"context {number}", "questions": [{"text": f"question {number}.{index}", "tokens": 3, "max_tokens": 100 + index} for index in range(4)]}
            for number in range(6)]


class DeadEndTest(unittest.TestCase):
    """A run whose server cannot come any more ends instead of waiting for the deploy's
    own timeout with nothing rented."""

    def ledger(self, leases: list[dict]) -> str:
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / "ledger.json"
            path.write_text(json.dumps({"leases": leases}))
            with mock.patch.object(poc_modal, "LEDGER", path):
                return poc_modal.server_dead_end(since=100.0)

    def test_no_dead_end_before_a_lease_or_while_one_is_live(self) -> None:
        self.assertEqual(self.ledger([]), "")
        self.assertEqual(self.ledger([{"state": "starting", "requested_at": 120.0}]), "")
        self.assertEqual(self.ledger([{"state": "failed", "requested_at": 120.0, "reason": "driver_mismatch"}, {"state": "requested", "requested_at": 130.0}]), "")

    def test_a_failed_start_with_no_live_lease_is_a_dead_end(self) -> None:
        why = self.ledger([{"state": "failed", "requested_at": 120.0, "reason": "driver_mismatch", "error": "driver 610 differs"},
                           {"state": "failed", "requested_at": 130.0, "reason": "error", "error": "RuntimeError: server exited with 1 while starting:\n engine input registration timed out after 600s"}])
        self.assertIn("2 starts", why)
        self.assertIn("timed out after 600s", why)
        self.assertNotIn("\n", why)

    def test_an_earlier_run_s_leases_and_a_released_server_do_not_count(self) -> None:
        self.assertEqual(self.ledger([{"state": "failed", "requested_at": 50.0, "reason": "error"}]), "")
        self.assertEqual(self.ledger([{"state": "stopped", "requested_at": 120.0, "reason": "released"}]), "")

    def test_an_unreadable_ledger_is_not_a_dead_end(self) -> None:
        with mock.patch.object(poc_modal, "LEDGER", Path("/nonexistent/ledger.json")):
            self.assertEqual(poc_modal.server_dead_end(since=0.0), "")


class MapCellTest(unittest.TestCase):
    """`ask-planner --cell`: the planner is asked while one cell of the fast-track map runs,
    with that cell's own requests sent the way the map's load generator sends them."""

    def setUp(self) -> None:
        self.dir = tempfile.TemporaryDirectory()
        self.saved = {name: getattr(poc_modal, name) for name in ("send_chat", "state")}
        poc_modal.state = lambda: {"api_key": "key"}
        root = Path(self.dir.name)
        self.prompts = root / "prompts.json"
        self.prompts.write_text(json.dumps({"kinds": {"prefix": {"levels": {"4": SESSIONS}}, "output": {"levels": {"4": [{"content": "write long", "max_tokens": 900}]}},
                                                      "short": {"levels": {"4": [{"content": "hello"}]}}}}))
        digest = hashlib.sha256(self.prompts.read_bytes()).hexdigest()
        self.cells = root / "cells.json"
        self.cells.write_text(json.dumps({"stagger_seed": 20261123, "warmup_seconds": 30.0, "files": {"v2": {"sha256": digest}, "other": {"sha256": "0" * 64}}, "cells": [
            {"key": "two-slices", "kind": "short", "requests_at_once": 4, "file": "v2", "paths": [["kinds", "short", "levels", "4"], ["kinds", "output", "levels", "4"]], "max_tokens": 295, "reasoning": "off"},
            {"key": "prefix/4", "kind": "prefix", "requests_at_once": 4, "file": "v2", "path": ["kinds", "prefix", "levels", "4"], "max_tokens": "per request, in the file", "reasoning": "off"},
            {"key": "output/4", "kind": "output", "requests_at_once": 4, "file": "v2", "path": ["kinds", "output", "levels", "4"], "max_tokens": "per request, in the file", "reasoning": "off", "warmup_seconds": 60.0},
            {"key": "short/4/reasoning", "kind": "short", "requests_at_once": 4, "file": "v2", "path": ["kinds", "short", "levels", "4"], "max_tokens": 295, "reasoning": "on (the server's default)"},
            {"key": "short/12", "kind": "short", "requests_at_once": 12, "file": "v2", "path": ["kinds", "short", "levels", "4"], "max_tokens": 295, "reasoning": "off"},
            {"key": "short/16", "kind": "short", "requests_at_once": 16, "file": "v2", "path": ["kinds", "short", "levels", "4"], "max_tokens": 295, "reasoning": "off"},
            {"key": "sliced", "kind": "short", "requests_at_once": 4, "file": "v2", "paths": [["kinds", "short", "levels", "4"], ["kinds", "output", "levels", "4"]], "max_tokens": 295, "reasoning": "off", "slice": [1, 2]},
            {"key": "wrong-file", "kind": "short", "requests_at_once": 4, "file": "other", "path": ["kinds", "short", "levels", "4"], "max_tokens": 295, "reasoning": "off"}]}))

    def tearDown(self) -> None:
        for name, value in self.saved.items():
            setattr(poc_modal, name, value)
        self.dir.cleanup()

    def test_a_cell_becomes_the_requests_the_map_sends(self) -> None:
        prefix = poc_modal.load_cell(self.cells, "prefix/4", self.prompts)
        self.assertEqual((prefix["kind"], prefix["requests_at_once"], prefix["reasoning"], len(prefix["chains"])), ("prefix", 4, False, 6))
        self.assertEqual(prefix["chains"][2][:2], [("context 2\n\nquestion 2.0", 100), ("context 2\n\nquestion 2.1", 101)])
        self.assertEqual(poc_modal.load_cell(self.cells, "output/4", self.prompts)["chains"], [[("write long", 900)]])
        short = poc_modal.load_cell(self.cells, "short/4/reasoning", self.prompts)
        self.assertEqual((short["chains"], short["reasoning"]), ([[("hello", 295)]], True))
        off = poc_modal.cell_body("hello", 295, reasoning=False)
        self.assertEqual((off["messages"], off["max_tokens"], off["temperature"], off["stream"], off["chat_template_kwargs"]),
                         ([{"role": "user", "content": "hello"}], 295, 0, True, {"enable_thinking": False}))
        self.assertNotIn("chat_template_kwargs", poc_modal.cell_body("hello", 295, reasoning=True))
        # A cell may read several slices of its file; they are sent in the order named.
        self.assertEqual(poc_modal.load_cell(self.cells, "two-slices", self.prompts)["chains"], [[("hello", 295)], [("write long", 295)]])
        # A slice is taken from the requests the paths hold, in that order, as the map takes it.
        self.assertEqual(poc_modal.load_cell(self.cells, "sliced", self.prompts)["chains"], [[("write long", 295)]])

    def test_each_workers_first_answer_is_cut_by_the_cells_seeded_share(self) -> None:
        shares = poc_modal.stagger_shares(4, "20261123:prefix/4")
        self.assertEqual(sorted(shares), [0.125, 0.375, 0.625, 0.875])
        self.assertEqual(shares, poc_modal.stagger_shares(4, "20261123:prefix/4"))  # the seed fixes the order
        self.assertNotEqual(shares, poc_modal.stagger_shares(4, "20261123:output/4"))
        self.assertEqual(poc_modal.load_cell(self.cells, "prefix/4", self.prompts)["first_shares"], shares)

    def test_a_cell_is_refused_with_another_prompt_file_or_a_load_the_planner_has_no_profile_for(self) -> None:
        for key in ("wrong-file", "short/12", "no-such-cell"):
            with self.subTest(key=key), self.assertRaises(SystemExit):
                poc_modal.load_cell(self.cells, key, self.prompts)

    def test_a_cell_at_a_load_no_profile_holds_is_measured_at_its_target_concurrency(self) -> None:
        self.assertEqual(poc_modal.load_cell(self.cells, "short/16", self.prompts)["requests_at_once"], 16)
        try:
            poc_modal.set_load(16)
            self.assertEqual((poc_modal.PROFILE, poc_modal.load_flags()), ("throughput", ["--profile", "throughput", "--target-concurrency", "16"]))
            poc_modal.set_load(128)
            self.assertEqual(poc_modal.load_flags(), ["--profile", "overload"])
            with self.assertRaises(ValueError):
                poc_modal.set_load(12)
        finally:
            poc_modal.set_load(32)

    def test_the_planner_is_asked_no_sooner_than_the_cells_warm_up(self) -> None:
        self.assertEqual(poc_modal.load_cell(self.cells, "prefix/4", self.prompts)["ask_after_seconds"], poc_modal.HOLD_BEFORE_ASK_SECONDS)  # the file's 30 s is under the hold's 45
        self.assertEqual(poc_modal.load_cell(self.cells, "output/4", self.prompts)["ask_after_seconds"], 60.0)  # a cell's own warm-up

    def test_the_cell_file_is_read_by_the_hash_it_was_fixed_with(self) -> None:
        digest = hashlib.sha256(self.cells.read_bytes()).hexdigest()
        self.assertEqual(poc_modal.load_cell(self.cells, "prefix/4", self.prompts, f"sha256:{digest}")["cells_sha256"], digest)
        with self.assertRaises(SystemExit):
            poc_modal.load_cell(self.cells, "prefix/4", self.prompts, "0" * 64)

    def test_the_cells_requests_run_closed_loop_with_sessions_in_order(self) -> None:
        sent, lock, running, peak, stop = [], threading.Lock(), [0], [0], threading.Event()

        def send(body: dict, session: str, key: str) -> tuple[int, str]:
            with lock:
                running[0] += 1
                peak[0] = max(peak[0], running[0])
                sent.append((session, threading.current_thread().name, body["messages"][0]["content"], body["max_tokens"], body.get("chat_template_kwargs")))
            time.sleep(0.02)
            with lock:
                running[0] -= 1
            return 7, ""

        poc_modal.send_chat = send
        threading.Timer(0.5, stop.set).start()
        record = poc_modal.cell_load(poc_modal.load_cell(self.cells, "prefix/4", self.prompts), stop.is_set, snapshot=dict)
        self.assertEqual((peak[0], record["requests_at_once"], record["failed"], record["reasoning"], record["cell"]), (4, 4, 0, "off", "prefix/4"))
        self.assertTrue(record["failures_by_hop"]["within_attempt_4_gate"])
        self.assertEqual(record["requests"], len(sent))
        self.assertTrue(record["list_started_again"])  # six sessions do not last half a second
        self.assertTrue(all(extra == {"enable_thinking": False} for *_, extra in sent))
        by_session: dict[str, list] = {}
        by_worker: dict[str, list] = {}
        for session, worker, content, limit, _ in sent:
            number = int(content.rsplit(".", 1)[1])
            by_session.setdefault(session, []).append((worker, number))
            by_worker.setdefault(worker, []).append((number, limit))
        for session, rows in by_session.items():
            self.assertEqual(len({worker for worker, _ in rows}), 1, session)  # one worker per session
            numbers = [number for _, number in rows]
            self.assertEqual(numbers, list(range(numbers[0], numbers[0] + len(numbers))), session)  # in order
        # Worker n starts its first session at question n, and that first request's answer
        # limit is cut by worker n's share; every later request keeps its question's limit.
        shares = poc_modal.stagger_shares(4, "20261123:prefix/4")
        self.assertTrue(record["first_requests_cut"])
        self.assertEqual({rows[0][0] for rows in by_worker.values()}, {0, 1, 2, 3})
        for rows in by_worker.values():
            (first_question, first_limit), later = rows[0], rows[1:]
            self.assertEqual(first_limit, max(1, round((100 + first_question) * shares[first_question])))
            self.assertTrue(all(limit == 100 + number for number, limit in later))

    def test_a_cell_load_places_each_failed_request_by_the_proxys_own_count(self) -> None:
        sent, lock, stop = [0], threading.Lock(), threading.Event()

        def send(body: dict, session: str, key: str) -> tuple[int, str]:
            with lock:
                sent[0] += 1
                number = sent[0]
            time.sleep(0.02)
            return (0, "503 Inference upstream is unavailable") if number == 3 else (7, "")

        proxies = iter([{"pod-a": {"failed_requests": 2, "failures": [{"path": "/v1/models", "error": "EOF"}, {"path": "/v1/chat/completions", "error": "EOF"}]}},
                        {"pod-a": {"failed_requests": 3, "failures": [{"path": "/v1/models", "error": "EOF"}, {"path": "/v1/chat/completions", "error": "EOF"},
                                                                       {"path": "/v1/chat/completions", "error": "EOF", "at": "2026-11-25T22:45:01Z"}]}}])
        poc_modal.send_chat = send
        threading.Timer(0.3, stop.set).start()
        record = poc_modal.cell_load(poc_modal.load_cell(self.cells, "prefix/4", self.prompts), stop.is_set, snapshot=lambda: next(proxies))
        hops = record["failures_by_hop"]
        self.assertEqual((record["failed"], hops["network_to_the_remote_gpu"], hops["before_the_runtime_proxy"], hops["within_attempt_4_gate"]), (1, 1, 0, True))
        self.assertEqual(hops["proxy_failures"], [{"path": "/v1/chat/completions", "error": "EOF", "at": "2026-11-25T22:45:01Z", "pod": "pod-a"}])


class FailuresByHopTest(unittest.TestCase):
    """Attempt 4's gate: run B may follow a cell load whose failed requests all failed on the
    network to the remote GPU, as the runtime proxy counted them, and no more than four."""

    UPSTREAM = "503 Inference upstream is unavailable"

    @staticmethod
    def proxy(failed: int, paths: list[str] | None = None, server_errors: dict | None = None) -> dict:
        return {"pod": {"failed_requests": failed, "failures": [{"path": path, "error": "EOF"} for path in (paths or [])], "server_errors": server_errors or {}}}

    def hops(self, failed: int, reasons: dict, after: dict, before: dict | None = None) -> dict:
        return poc_modal.failures_by_hop(failed, reasons, before if before is not None else self.proxy(0), after)

    def test_no_failure_passes(self) -> None:
        self.assertTrue(self.hops(0, {}, {})["within_attempt_4_gate"])

    def test_failures_the_proxy_counted_pass_up_to_four(self) -> None:
        chat = "/v1/chat/completions"
        self.assertTrue(self.hops(4, {self.UPSTREAM: 4}, self.proxy(4, [chat] * 4))["within_attempt_4_gate"])
        five = self.hops(5, {self.UPSTREAM: 5}, self.proxy(5, [chat] * 5))
        self.assertFalse(five["within_attempt_4_gate"])
        self.assertIn("more than the 4 allowed", five["why"])

    def test_a_503_the_proxy_did_not_count_failed_before_it(self) -> None:
        # The proxy's only new failure was the control plane's model list, not a chat request.
        hops = self.hops(1, {self.UPSTREAM: 1}, self.proxy(1, ["/v1/models"]))
        self.assertEqual((hops["network_to_the_remote_gpu"], hops["before_the_runtime_proxy"], hops["within_attempt_4_gate"]), (0, 1, False))
        self.assertIn("before the runtime proxy", hops["why"])

    def test_a_refusal_or_a_broken_stream_never_passes(self) -> None:
        for reason in ("404 Unknown model alias: gpu-baseline", "429 admission rejected", "stream ended early", "RemoteDisconnected"):
            hops = self.hops(1, {reason: 1}, self.proxy(1, ["/v1/chat/completions"]))
            self.assertFalse(hops["within_attempt_4_gate"], reason)

    def test_a_server_error_from_the_remote_side_never_passes(self) -> None:
        hops = self.hops(1, {self.UPSTREAM: 1}, self.proxy(0, server_errors={"503": 1}), before=self.proxy(0, server_errors={}))
        self.assertEqual((hops["remote_side_server_errors"], hops["within_attempt_4_gate"]), ({"503": 1}, False))

    def test_counts_that_could_not_be_read_never_pass(self) -> None:
        hops = self.hops(1, {self.UPSTREAM: 1}, {})
        self.assertFalse(hops["within_attempt_4_gate"])
        self.assertIn("could not be read", hops["why"])

    def test_only_failures_after_the_first_reading_count(self) -> None:
        before = self.proxy(2, ["/v1/chat/completions"] * 2)
        after = self.proxy(2, ["/v1/chat/completions"] * 2)
        hops = self.hops(1, {self.UPSTREAM: 1}, after, before=before)
        self.assertEqual((hops["network_to_the_remote_gpu"], hops["within_attempt_4_gate"]), (0, False))

    def test_a_refusal_keeps_the_gateways_message(self) -> None:
        body = io.BytesIO(json.dumps({"error": {"message": "Inference upstream is unavailable", "type": "server_error"}}).encode())
        error = urllib.error.HTTPError("http://gateway/v1/chat/completions", 503, "Service Unavailable", {}, body)
        self.assertEqual(poc_modal.refusal_reason(error), self.UPSTREAM)
        bare = urllib.error.HTTPError("http://gateway/v1/chat/completions", 404, "Not Found", {}, io.BytesIO(b"not json"))
        self.assertEqual(poc_modal.refusal_reason(bare), "404")


if __name__ == "__main__":
    unittest.main()


class LiveRunOptionsTest(unittest.TestCase):
    """The options of a recorded live run: a shorter Modal limit for every GPU container, and
    the endpoint kept serving after the campaign."""

    def test_the_gpu_timeout_is_read_where_the_app_is_defined_and_bounded(self) -> None:
        code = "import json, modal_gpu; print(modal_gpu.GPU_TIMEOUT_SECONDS)"
        folder = str(Path(poc_modal.__file__).resolve().parent)
        for value, want in (("7200", 7200), ("60", 1800), ("999999", 21600), (None, 21600)):
            environment = {key: item for key, item in os.environ.items() if key != "POC_MODAL_GPU_TIMEOUT_SECONDS"}
            if value is not None:
                environment["POC_MODAL_GPU_TIMEOUT_SECONDS"] = value
            ran = subprocess.run(["uv", "run", "--quiet", "--no-project", "--with", poc_modal.MODAL_CLIENT, "python", "-c", code], cwd=folder, env=environment,
                                 capture_output=True, text=True, timeout=300)
            self.assertEqual(ran.returncode, 0, ran.stderr[-800:])
            self.assertEqual(int(ran.stdout.strip().splitlines()[-1]), want, value)

    def test_the_broker_gets_the_runs_gpu_timeout(self) -> None:
        started = {}

        class Process:
            pid = 1

            def poll(self):
                return None

        def popen(command, **kwargs):
            started.update(kwargs)
            return Process()

        with tempfile.TemporaryDirectory() as folder, mock.patch.object(poc_modal, "STATE_DIR", Path(folder)), mock.patch.dict(poc_modal.RUN, gpu_timeout_minutes=120), \
                mock.patch.object(poc_modal.subprocess, "Popen", popen), mock.patch.object(poc_modal, "save", lambda values: None), \
                mock.patch.object(poc_modal.BrokerProcess, "request", lambda self, *args, **kwargs: {}), mock.patch.object(poc_modal, "broker_command", lambda *args: ["broker"]):
            poc_modal.BrokerProcess(1.0, dry=False, max_starts=1)
        self.assertEqual(started["env"]["POC_MODAL_GPU_TIMEOUT_SECONDS"], "7200")

    def test_the_kept_endpoint_is_a_new_deploy_served_with_traffic_then_removed(self) -> None:
        calls, keys = [], []

        def cli(*args, **kwargs):
            calls.append(args)
            if args[0] == "deploy":
                keys.append(args[args.index("--idempotency-key") + 1])
            return ""

        clock = {"now": 0.0}

        def hold(requests_total, at_once, input_scale, max_output, until, text=None, output_scale=1.0):
            clock["now"] += 61.0
            return {"requests": 10, "failed": 1}

        class Broker:
            pass

        saved = {}
        with mock.patch.object(poc_modal.poc, "cli", cli), mock.patch.object(poc_modal, "state", lambda: {"run_id": "run1", "model_image": "registry/proxy@sha256:abc"}), \
                mock.patch.object(poc_modal, "server_dead_end", lambda since: ""), mock.patch.object(poc_modal, "STATE_DIR", Path(tempfile.mkdtemp())), \
                mock.patch.object(poc_modal, "hold_load", hold), mock.patch.object(poc_modal.time, "monotonic", lambda: clock["now"]), \
                mock.patch.object(poc_modal, "shape_scales", lambda shape, rows: {"input_scale": 1.0, "output_scale": 1.0, "max_output_tokens": 64}), \
                mock.patch.object(poc_modal.poc, "mooncake_rows", lambda: [{}] * 10), mock.patch.object(poc_modal, "wait_for_lease_end", lambda broker, prefix, others=None: None), \
                mock.patch.object(poc_modal, "live_lease_ids", lambda broker: set()), \
                mock.patch.object(poc_modal, "save", saved.update), mock.patch.dict(poc_modal.RUN, cell=None):
            poc_modal.keep_endpoint(Broker(), 2, 10, None)
        self.assertEqual(len(keys), 1)
        self.assertTrue(keys[0].endswith("-run1-keep"), keys)  # not a replay of the run's first deploy
        self.assertEqual([call[0] for call in calls], ["deploy", "delete"])
        self.assertEqual(saved["keep_endpoint"], {"minutes": 2, "served_minutes": 2.0, "requests": 20, "failed": 2})

    def test_a_keep_stopped_early_still_says_what_it_served(self) -> None:
        # The live run of 26 Nov ended its keep once the recording was done; the log said
        # "0 requests in 40 min" because a cell's counts came back only when its load ended.
        clock, lines, saved = {"now": 0.0}, [], {}

        def interrupted(cell, until, snapshot=None, tally=None, purpose=""):
            clock["now"] += 300.0
            tally.update(sent=97, failed=1)
            raise KeyboardInterrupt

        with mock.patch.object(poc_modal.poc, "cli", lambda *args, **kwargs: ""), mock.patch.object(poc_modal, "deploy_baseline", lambda purpose="": None), \
                mock.patch.object(poc_modal, "cell_load", interrupted), mock.patch.object(poc_modal.time, "monotonic", lambda: clock["now"]), \
                mock.patch.object(poc_modal, "wait_for_lease_end", lambda broker, prefix, others=None: None), mock.patch.object(poc_modal, "live_lease_ids", lambda broker: set()), \
                mock.patch.object(poc_modal, "log", lambda stage, message: lines.append(message)), mock.patch.object(poc_modal, "save", saved.update), \
                mock.patch.dict(poc_modal.RUN, cell={"key": "short/16"}), self.assertRaises(KeyboardInterrupt):
            poc_modal.keep_endpoint(object(), 40, 10, None)
        self.assertEqual(saved["keep_endpoint"], {"minutes": 40, "served_minutes": 5.0, "requests": 97, "failed": 1})
        self.assertIn("97 requests in 5 of 40 min, 1 failed", lines[-1])

    def test_the_footnote_describes_the_run_it_summarises(self) -> None:
        # The live run of 26 Nov ran its cell with reasoning off and checked a task set; its
        # footnote said reasoning on and "word overlap on 24 short prompts".
        cell_run = " ".join(poc_modal.result_footnote({"task_set": "fast-track-quality-v1.json"}, {"key": "short/16", "reasoning": False}))
        self.assertIn("task set fast-track-quality-v1.json", cell_run)
        self.assertIn("held within 1 point", cell_run)
        self.assertIn("map cell's own requests ran with reasoning off", cell_run)
        self.assertIn("measured requests leave it on", cell_run)
        self.assertNotIn("word overlap", cell_run)
        replay = " ".join(poc_modal.result_footnote({"task_set": None}, None))
        self.assertIn("word overlap on 24 short prompts", replay)
        self.assertIn("requests leave the model's reasoning on", replay)
        self.assertNotIn("map cell", replay)

    def test_the_options_are_refused_outside_their_bounds(self) -> None:
        for argv in (["run", "--keep-endpoint", "61"], ["ask-planner", "--keep-endpoint", "5"], ["run", "--gpu-timeout-minutes", "20"], ["run", "--gpu-timeout-minutes", "400"]):
            messages = []
            with mock.patch.object(sys, "argv", ["poc_modal.py", *argv]), mock.patch.object(poc_modal.os, "chdir", lambda path: None), \
                    mock.patch.object(poc_modal, "STATE_DIR", Path(tempfile.mkdtemp())), mock.patch.object(poc_modal, "fail", lambda message: (messages.append(message), sys.exit(1))):
                with self.assertRaises(SystemExit):
                    poc_modal.main()
            self.assertTrue(messages and ("--keep-endpoint" in messages[0] or "--gpu-timeout-minutes" in messages[0]), (argv, messages))


class TaskSetTest(unittest.TestCase):
    """--task-set: the campaign checks answers on a task set with the product's paired rule."""

    FAST_TRACK = {"schema": "infercrane.fast-track-quality/v1", "seed": 7, "sources": {"gsm8k": {"licence": "MIT"}},
                  "items": [{"id": "gsm8k:test:1", "kind": "number", "gold": "12", "prompt": "What is 5 + 7?", "max_tokens": 512},
                            {"id": "mmlu:test:2", "kind": "letter", "gold": "B", "prompt": "Pick one. A) x B) y", "max_tokens": 64}]}

    def test_a_fast_track_task_set_becomes_a_task_eval_set_and_others_are_refused(self) -> None:
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / "tasks.json"
            path.write_text(json.dumps(self.FAST_TRACK))
            document = poc_modal.task_eval_set(path)
            self.assertEqual((document["grading"], [prompt["id"] for prompt in document["prompts"]], document["max_tokens"]),
                             ("answer_line", ["gsm8k.test.1", "mmlu.test.2"], 512))
            path.write_text(json.dumps({"schema": "infercrane.dev/quality-eval-set/v1", "name": "chat", "prompts": [{"id": "a", "prompt": "hi"}]}))
            messages = []
            with mock.patch.object(poc_modal, "fail", lambda message: (messages.append(message), sys.exit(1))), self.assertRaises(SystemExit):
                poc_modal.task_eval_set(path)
            self.assertIn("not a task set", messages[0])

    def test_the_task_set_is_uploaded_and_named_in_the_proposal(self) -> None:
        calls, saved = [], {}
        template = {"id": "t", "deployment": {"provider": {"adapter": "kubernetes"}, "resources": {}, "scaling": {}}}

        def cli(*args, **kwargs):
            calls.append(args)
            if args[:3] == ("evaluation", "set", "upload"):
                uploaded = json.loads(Path(args[-1]).read_text())
                self.assertEqual(uploaded["grading"], "answer_line")
                return json.dumps({"digest": "sha256:" + "e" * 64, "name": uploaded["name"]})
            return json.dumps({"candidates": [template], "starting_point": {"candidate_id": "t"}})

        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / "tasks.json"
            path.write_text(json.dumps(self.FAST_TRACK))
            with mock.patch.object(poc_modal.poc, "cli", cli), mock.patch.object(poc_modal, "STATE_DIR", Path(folder)), mock.patch.object(poc_modal, "save", saved.update), \
                    mock.patch.object(poc_modal, "state", lambda: {"model_image": "registry/proxy@sha256:abc"}):
                digest = poc_modal.upload_task_set(path)
                poc_modal.proposal("a" * 64, digest)
        self.assertEqual(digest, "sha256:" + "e" * 64)
        self.assertEqual(saved["task_set"]["items"], 2)
        propose = next(call for call in calls if call[:2] == ("optimize", "propose"))
        self.assertEqual(propose[propose.index("--eval-set") + 1], digest)

    def test_the_task_set_is_for_run_only(self) -> None:
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / "tasks.json"
            path.write_text("{}")
            messages = []
            with mock.patch.object(sys, "argv", ["poc_modal.py", "ask-planner", "--task-set", str(path)]), mock.patch.object(poc_modal.os, "chdir", lambda path: None), \
                    mock.patch.object(poc_modal, "STATE_DIR", Path(folder)), mock.patch.object(poc_modal, "fail", lambda message: (messages.append(message), sys.exit(1))):
                with self.assertRaises(SystemExit):
                    poc_modal.main()
            self.assertIn("--task-set", messages[0])


class L40SFallbackTest(unittest.TestCase):
    """The L40S fallback: its price is read at launch, and Qwen3-8B serves its own context."""

    def setUp(self) -> None:
        self.before = (models.ENGINE.key, models.ACCELERATOR.key, poc_modal.RUN["current_setup"])

    def tearDown(self) -> None:
        os.environ.pop(models.GPU_PRICE_VARIABLE, None)
        poc_modal.configure(*self.before)

    def test_an_l40s_run_refuses_to_start_without_a_price_read_at_launch(self) -> None:
        os.environ.pop(models.GPU_PRICE_VARIABLE, None)
        messages = []
        with mock.patch.object(poc_modal, "fail", lambda message: (messages.append(message), sys.exit(1))), self.assertRaises(SystemExit):
            poc_modal.configure("vllm-0.34.0", "L40S")
        self.assertIn("--gpu-usd-per-second", messages[0])
        os.environ[models.GPU_PRICE_VARIABLE] = "0.000542"
        poc_modal.configure("vllm-0.34.0", "L40S")
        self.assertEqual((models.ACCELERATOR.usd_per_second, poc_modal.GPU_HOURLY_USD), (0.000542, round(0.000542 * 3600, 4)))
        self.assertIn("at launch", models.ACCELERATOR.price_read)

    def test_qwen3_8b_serves_its_own_context_on_an_l40s_and_has_no_check_yet(self) -> None:
        os.environ[models.GPU_PRICE_VARIABLE] = "0.000542"
        models.select("vllm-0.34.0", "L40S")
        setup = models.QWEN3_8B.setup
        self.assertEqual((setup.added_args, setup.checked), ((), ""))
        models.select("vllm-0.34.0", "L4")
        self.assertEqual(models.QWEN3_8B.setup.added_args, ("--max-model-len", "8192"))

    def test_a_kept_price_is_not_replaced(self) -> None:
        os.environ[models.GPU_PRICE_VARIABLE] = "0.5"
        self.assertEqual(models.priced(models.ACCELERATORS["L4"]).usd_per_second, 0.000222)

    def test_the_price_flag_is_bounded(self) -> None:
        messages = []
        with mock.patch.object(sys, "argv", ["poc_modal.py", "run", "--gpu-usd-per-second", "2"]), mock.patch.object(poc_modal.os, "chdir", lambda path: None), \
                mock.patch.object(poc_modal, "fail", lambda message: (messages.append(message), sys.exit(1))), self.assertRaises(SystemExit):
            poc_modal.main()
        self.assertIn("dollars a second", messages[0])


class LeaseEndTest(unittest.TestCase):
    """The keep step waits for its own server only: a finished campaign's winner, kept ready
    to roll out, may still hold a lease (the live-run rehearsal of 2026-11-26 failed on it)."""

    class Broker:
        def __init__(self, states):
            self.states = states

        def ledger(self):
            return {"leases": [{"lease_id": lease, "state": state} for lease, state in self.states.items()]}

    def test_a_server_that_was_live_before_is_not_waited_for(self) -> None:
        broker = self.Broker({"winner": "ready", "keep": "stopped"})
        with mock.patch.object(poc_modal.time, "sleep", lambda *_: None):
            poc_modal.wait_for_lease_end(broker, "gpu-baseline", timeout_s=1, others={"winner"})

    def test_a_server_still_running_fails_the_wait(self) -> None:
        broker = self.Broker({"winner": "ready", "keep": "stopping"})
        messages = []
        with mock.patch.object(poc_modal.time, "sleep", lambda *_: None), mock.patch.object(poc_modal, "fail", lambda message: (messages.append(message), sys.exit(1))), \
                self.assertRaises(SystemExit):
            poc_modal.wait_for_lease_end(broker, "gpu-baseline", timeout_s=0.01, others={"winner"})
        self.assertIn("stop-gpus", messages[0])


class RequiredToolsTest(unittest.TestCase):
    def test_a_run_without_kubectl_stops_before_anything_starts(self) -> None:
        # The first paid launch of 26 Nov died at the corpus check with FileNotFoundError:
        # kubectl was not on the launching shell's PATH. The run now names the tool first.
        messages: list[str] = []
        which = lambda tool: None if tool == "kubectl" else f"/usr/bin/{tool}"  # noqa: E731
        state = mock.Mock(side_effect=AssertionError("nothing may be read before the tools are checked"))
        with mock.patch.object(poc_modal.shutil, "which", which), mock.patch.object(poc_modal, "state", state), \
                mock.patch.object(poc_modal, "fail", lambda message: (messages.append(message), sys.exit(1))), self.assertRaises(SystemExit):
            poc_modal.run_campaign(1.0, None, False)
        self.assertTrue(messages[0].startswith("kubectl is required and not on PATH"), messages)

    def test_up_names_every_missing_tool(self) -> None:
        messages: list[str] = []
        with mock.patch.object(poc_modal.shutil, "which", lambda tool: None if tool in ("go", "uv") else f"/usr/bin/{tool}"), \
                mock.patch.object(poc_modal, "fail", lambda message: (messages.append(message), sys.exit(1))), self.assertRaises(SystemExit):
            poc_modal.up()
        self.assertTrue(messages[0].startswith("go, uv are required and not on PATH"), messages)

"""Offline tests for the trained-draft harness: no GPU, no network.

    python3 -m unittest tools/trained-draft/test_trained_draft.py
"""

from __future__ import annotations

import json
import socket
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import draftlib as lib  # noqa: E402


def row(message_id, text, *, parent=None, role="prompter", tree=None, lang="en", deleted=False, synthetic=False, review=True):
    return {"message_id": message_id, "parent_id": parent, "text": text, "role": role, "message_tree_id": tree or message_id, "lang": lang,
            "deleted": deleted, "synthetic": synthetic, "review_result": review}


class PromptTests(unittest.TestCase):
    def test_root_prompts_keep_only_human_first_turns(self):
        rows = [
            row("b", "second"), row("a", "first", lang="es"), row("c", "reply", parent="a"), row("d", "assistant", role="assistant"),
            row("e", "gone", deleted=True), row("f", "made up", synthetic=True), row("g", "rejected", review=False), row("h", "first"),
            row("i", "x" * (lib.MAX_PROMPT_CHARS + 1)), row("j", "   "),
        ]
        kept = lib.root_prompts(rows)
        self.assertEqual([p["content"] for p in kept], ["first", "second"])
        self.assertEqual(kept[0], {"id": "oasst1:a", "tree": "a", "lang": "es", "content": "first"})

    def test_held_out_never_overlaps_training(self):
        split = lib.split_prompts([row("a", "shared"), row("b", "train only")], [row("c", "shared"), row("d", "held out", tree="b"), row("e", "fresh")])
        self.assertEqual([p["content"] for p in split["held_out"]], ["fresh"])
        self.assertEqual(len(split["train"]), 2)

    def test_fingerprint_ignores_order_and_tells_sets_apart(self):
        a, b = {"id": "1", "x": 1}, {"id": "2", "x": 2}
        self.assertEqual(lib.digest_lines([a, b]), lib.digest_lines([b, a]))
        self.assertNotEqual(lib.digest_lines([a, b]), lib.digest_lines([a]))
        self.assertRegex(lib.digest_lines([a]), r"^sha256:[0-9a-f]{64}$")


class RequestTests(unittest.TestCase):
    def test_chat_request_is_greedy_and_thinking_off(self):
        body = lib.chat_request(lib.MODEL, "hi", 64, stream=True)
        self.assertEqual(body["temperature"], 0.0)
        self.assertEqual(body["chat_template_kwargs"], {"enable_thinking": False})
        self.assertEqual(body["stream_options"], {"include_usage": True})
        self.assertNotIn("return_token_ids", body)
        self.assertTrue(lib.chat_request(lib.MODEL, "hi", 64, stream=False, token_ids=True)["return_token_ids"])

    def test_training_row_masks_the_prompt(self):
        made = lib.training_row("p", {"prompt_token_ids": [1, 2, 3], "choices": [{"token_ids": [4, 5]}]})
        self.assertEqual(made, {"id": "p", "input_ids": [1, 2, 3, 4, 5], "loss_mask": [0, 0, 0, 1, 1]})
        self.assertIsNone(lib.training_row("p", {"prompt_token_ids": [1], "choices": [{"token_ids": []}]}))
        self.assertIsNone(lib.training_row("p", {}))


class WindowTests(unittest.TestCase):
    def test_every_candidate_run_sits_between_control_runs(self):
        order = lib.window_schedule(["trained", "none"], 4)
        self.assertEqual(order[:5], ["control", "trained", "control", "none", "control"])
        self.assertEqual(len(order), 17)
        self.assertEqual(order.count("trained"), 4)
        for index, arm in enumerate(order):
            if arm != "control":
                self.assertEqual((order[index - 1], order[index + 1]), ("control", "control"))

    def test_summary_has_the_judges_fields_and_leaves_unknowns_out(self):
        results = [{"ok": True, "ttft": 0.1, "latency": 1.1, "output_tokens": 101}, {"ok": True, "ttft": 0.2, "latency": 2.2, "output_tokens": 201}, {"ok": False}]
        summary = lib.summarize_run("candidate", results, 4.0)
        self.assertEqual((summary["requests"], summary["failed"]), (3, 1))
        self.assertAlmostEqual(summary["output_token_throughput"], 75.5)
        self.assertAlmostEqual(summary["request_throughput"], 0.5)
        self.assertAlmostEqual(summary["tpot_p95_ms"], 10.0)
        self.assertIn("ttft_p95_ms", summary)
        empty = lib.summarize_run("control", [{"ok": False}], 1.0)
        self.assertNotIn("output_token_throughput", empty)
        self.assertNotIn("ttft_p95_ms", empty)

    def test_percentile_interpolates(self):
        self.assertEqual(lib.percentile([1, 2, 3, 4, 5], 0.5), 3)
        self.assertAlmostEqual(lib.percentile([0, 10], 0.95), 9.5)
        self.assertIsNone(lib.percentile([], 0.95))


METRICS = """# HELP vllm:spec_decode_num_drafts_total x
vllm:spec_decode_num_drafts_total{engine="0",model_name="Qwen/Qwen3-8B"} %d
vllm:spec_decode_num_draft_tokens_total{engine="0",model_name="Qwen/Qwen3-8B"} %d
vllm:spec_decode_num_accepted_tokens_total{engine="0",model_name="Qwen/Qwen3-8B"} %d
vllm:spec_decode_num_accepted_tokens_per_pos_total{engine="0",model_name="Qwen/Qwen3-8B",position="0"} %d
vllm:spec_decode_num_accepted_tokens_per_pos_total{engine="0",model_name="Qwen/Qwen3-8B",position="1"} %d
vllm:spec_decode_num_accepted_tokens_per_pos_total{engine="0",model_name="Qwen/Qwen3-8B",position="2"} %d
vllm:num_requests_running{model_name="Qwen/Qwen3-8B"} 0
"""


class AcceptanceTests(unittest.TestCase):
    def test_counters_and_conditional_acceptance(self):
        before = lib.speculation_counters(METRICS % (100, 300, 150, 80, 50, 20))
        after = lib.speculation_counters(METRICS % (200, 600, 300, 160, 100, 40))
        self.assertEqual(after["accepted_per_position"], {0: 160.0, 1: 100.0, 2: 40.0})
        measured = lib.acceptance(before, after)
        self.assertEqual(measured["drafts"], 100)
        self.assertAlmostEqual(measured["mean_tokens_per_step"], 2.5)
        # 80 of 100 at position 0, 50 of those 80 at position 1, 20 of those 50 at 2.
        self.assertEqual(measured["per_position"], [0.8, 0.625, 0.4])

    def test_no_drafts_is_unknown(self):
        reading = lib.speculation_counters("vllm:num_requests_running 0\n")
        self.assertIsNone(lib.acceptance(reading, reading))


class ReportTests(unittest.TestCase):
    def test_manifest_digest_matches_the_core(self):
        # internal/speculator TestManifestDigestIsOrderFree computes this same value.
        files = [{"path": "model.safetensors", "size": 1_600_000_000, "sha256": "2" * 64}, {"path": "config.json", "size": 812, "sha256": "1" * 64}]
        expected = "sha256:" + __import__("hashlib").sha256(("config.json 812 " + "1" * 64 + "\nmodel.safetensors 1600000000 " + "2" * 64 + "\n").encode()).hexdigest()
        self.assertEqual(lib.manifest_digest(files), expected)

    def test_report_names_every_file_and_no_text(self):
        with tempfile.TemporaryDirectory() as directory:
            draft = Path(directory)
            (draft / "config.json").write_text("{}")
            (draft / "model.safetensors").write_bytes(b"\0" * 10)
            facts = lib.TrainingFacts(name="oasst1-chat", examples=10, held_out_examples=5, training_tokens=1000, dataset_fingerprint="sha256:" + "a" * 64,
                                      held_out_acceptance=[0.8, 0.6, 0.5], start_acceptance=[0.7, 0.5, 0.4], produced_at="2026-11-26T03:00:00Z", wall_seconds=12.5)
            report = lib.training_report(facts, draft, lib.PLANS["l4-qwen3-8b"])
        self.assertEqual([f["path"] for f in report["files"]], ["config.json", "model.safetensors"])
        self.assertEqual(report["artifact_digest"], lib.manifest_digest(report["files"]))
        self.assertEqual(report["schema"], lib.REPORT_SCHEMA)
        self.assertEqual(report["start_from"], {"repository": lib.PUBLISHED_DRAFT, "revision": lib.PUBLISHED_DRAFT_REVISION})
        text_fields = {k for k, v in report.items() if isinstance(v, str)}
        self.assertEqual(text_fields, {"schema", "name", "base_model", "base_revision", "method", "framework", "framework_version", "data_source", "dataset_fingerprint", "produced_at", "artifact_digest"})

    def test_dollars(self):
        self.assertEqual(lib.dollars(3600), 4.71)
        self.assertEqual(lib.dollars(20 * 60), 1.57)


def free_port() -> int:
    with socket.socket() as handle:
        handle.bind(("127.0.0.1", 0))
        return handle.getsockname()[1]


@unittest.skipUnless(__import__("importlib").util.find_spec("aiohttp"), "aiohttp is not installed")
class StandInTests(unittest.TestCase):
    """The load generator against the stand-in: what the dry run relies on."""

    def test_load_counts_tokens_and_speculation(self):
        import trained_draft as harness

        with tempfile.TemporaryDirectory() as directory:
            port = free_port()
            server = harness.Server("standin", [sys.executable, str(Path(__file__).with_name("standin.py")), "--port", str(port), "--model", lib.MODEL,
                                                "--behaviour", json.dumps({"tokens_per_second": 5000, "accept": 0.6})], port, Path(directory))
            try:
                server.wait_ready(30)
                before = lib.speculation_counters(server.metrics())
                bodies = [lib.chat_request(lib.MODEL, f"prompt {i}", 32, stream=True) for i in range(6)]
                results, seconds = harness.load(server.url, bodies, 3)
                after = lib.speculation_counters(server.metrics())
                plain, _ = harness.load(server.url, [lib.chat_request(lib.MODEL, "prompt 0", 32, stream=False, token_ids=True)], 1)
            finally:
                server.stop()
        self.assertTrue(all(r["ok"] for r in results))
        summary = lib.summarize_run("candidate", results, seconds)
        self.assertEqual(summary["failed"], 0)
        self.assertGreater(summary["output_token_throughput"], 0)
        self.assertIsNotNone(lib.acceptance(before, after))
        self.assertEqual(results[0]["text"], plain[0]["text"], "streamed and plain answers of one prompt differ")
        self.assertIsNotNone(lib.training_row("p", plain[0]["data"]))


if __name__ == "__main__":
    unittest.main()


class TaskSetTests(unittest.TestCase):
    def test_a_missing_task_set_says_what_to_set(self) -> None:
        import trained_draft as harness

        saved = harness.QUALITY_FILE
        with tempfile.TemporaryDirectory() as directory:
            try:
                harness.QUALITY_FILE = Path(directory) / "missing.json"
                with self.assertRaises(SystemExit) as stop:
                    harness.task_set_file()
                self.assertIn("set TRAINED_DRAFT_TASKS", str(stop.exception))
                harness.QUALITY_FILE = Path(directory) / "tasks.json"
                harness.QUALITY_FILE.write_text('{"items": []}')
                self.assertEqual(harness.task_set_file(), harness.QUALITY_FILE)
            finally:
                harness.QUALITY_FILE = saved

    def test_the_default_stays_inside_the_checkout(self) -> None:
        import trained_draft as harness

        if "TRAINED_DRAFT_TASKS" not in __import__("os").environ:
            self.assertEqual(harness.QUALITY_FILE.parent, harness.REPOSITORY / ".infercrane" / "corpus")


class Rung1SimulationTests(unittest.TestCase):
    """The model-free arms of rung 1, replayed against a known greedy answer."""

    def setUp(self):
        import rung1_offline as rung1

        self.rung1 = rung1

    def test_ngram_finds_the_latest_longest_match(self):
        tokens = [1, 2, 3, 9, 1, 2, 3, 7, 8, 1, 2, 3]
        # "1 2 3" last occurred at 4 (followed by 7, 8, 1); the latest earlier match wins.
        self.assertEqual(self.rung1.ngram_proposal(tokens, 3), [7, 8, 1])
        self.assertEqual(self.rung1.ngram_proposal([1, 2, 3, 4], 3), [])

    def test_replay_counts_accepted_prefixes_and_the_bonus_token(self):
        row = {"prompt_ids": [5], "answer_ids": [1, 2, 3, 4]}
        perfect = self.rung1.replay([row], lambda r, context: r["answer_ids"][len(context) - 1:len(context) + 2])
        self.assertEqual((perfect["steps"], perfect["tokens"]), (1, 4))  # 3 drafted + 1 written
        nothing = self.rung1.replay([row], lambda r, context: [])
        self.assertEqual(nothing["tokens_per_step"], 1.0)
        wrong_second = self.rung1.replay([row], lambda r, context: [r["answer_ids"][len(context) - 1], 99])
        self.assertEqual(wrong_second["steps"], 2)  # 2 tokens a step: one accepted plus the bonus

    def test_suffix_follows_the_most_frequent_continuation(self):
        index = self.rung1.SuffixIndex()
        index.add([1, 2, 3, 1, 2, 3, 1, 2, 4])
        # After "1 2" came 3 twice and 4 once; the match length (2) caps the proposal.
        self.assertEqual(self.rung1.suffix_proposal([7, 1, 2], [index], 8), [3, 1])
        self.assertEqual(self.rung1.suffix_proposal([7, 1, 2], [index], 1), [3])
        self.assertEqual(self.rung1.suffix_proposal([42], [index], 8), [])

    def test_tokens_per_step_from_conditional_acceptance(self):
        self.assertAlmostEqual(self.rung1.tokens_per_step([0.8, 0.5, 0.5], 3), 2.4)
        self.assertAlmostEqual(self.rung1.tokens_per_step([0.8, 0.5, 0.5], 2), 2.2)

    def test_validation_lines_are_read_from_the_trainer_log(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "train.log"
            path.write_text("noise\n{'val': {'cond_acc_0_epoch': 0.7, 'cond_acc_1_epoch': 0.6, 'cond_acc_2_epoch': 0.5, 'full_acc_0_epoch': 0.7, 'full_acc_1_epoch': 0.42, 'full_acc_2_epoch': 0.21}, 'epoch': 0}\n")
            passes = self.rung1.val_metrics(path)
        self.assertEqual(passes[-1]["cond_acc"], [0.7, 0.6, 0.5])


class PlanTests(unittest.TestCase):
    """Every plan pins what its rules file says and stays inside its caps."""

    def test_plans_are_consistent(self):
        for name, plan in lib.PLANS.items():
            with self.subTest(plan=name):
                self.assertTrue((Path(__file__).parent / plan.rules).exists(), plan.rules)
                self.assertEqual(plan.target_layer_ids[-1], plan.layers)
                self.assertEqual(set(plan.stage_seconds), {"smoke", "train", "measure", "confirm"})
                # The brief's bound holds for the proof itself; the fresh-machine confirmation is its own stage.
                self.assertLess(sum(lib.dollars(s, plan.dollars_per_hour) for k, s in plan.stage_seconds.items() if k != "confirm"), 10.0)
                self.assertLess(lib.dollars(plan.stage_seconds["confirm"], plan.dollars_per_hour), 2.0)
                arms = {arm for window in plan.windows for arm in (window.control, *window.candidates)}
                self.assertIn("trained", arms)
                for arm in arms:
                    plan.arm_args(arm, "/draft")
                for load_level in plan.loads:
                    self.assertIn(load_level, plan.requests_per_run)
                self.assertLess(plan.train_loop_seconds, plan.stage_seconds["train"])

    def test_rung2_serves_every_drafting_method_against_no_speculation(self):
        plan = lib.PLANS["l4-qwen3-1.7b"]
        self.assertEqual([w.control for w in plan.windows], ["none", "none"])
        self.assertEqual(sorted(a for w in plan.windows for a in w.candidates), ["ngram", "published", "suffix", "trained"])
        published = json.loads(plan.arm_args("published", "/x")[1])
        self.assertEqual((published["model"], published["revision"]), (plan.published, plan.published_revision))
        self.assertNotIn("enable_adaptive_verification", json.loads(plan.arm_args("suffix", "/x")[1]))
        # Hard caps as the rules state them: smoke $0.52, train $1.04, measure $1.25, confirm $0.42.
        self.assertEqual(round(sum(lib.dollars(s, plan.dollars_per_hour) for s in plan.stage_seconds.values()), 2), 3.23)
        # The window the pass bar reads runs first, so a cut stage loses only the model-free window.
        self.assertEqual(plan.windows[0].candidates, ("published", "trained"))


class ServerCommandTests(unittest.TestCase):
    """The exact vLLM commands a GPU stage starts (no process is started)."""

    def command(self, plan_name, role, extra=None):
        import trained_draft as harness

        ctx = harness.Context(Path("/vol/runs/x"), dry=False, weights=Path("/vol/weights"), speculators="/opt/spec/bin/speculators", plan=lib.PLANS[plan_name])
        return ctx.server_command(8001, role, extra)

    def test_extraction_server_answers_under_the_verifier_path(self):
        command, _ = self.command("l4-qwen3-1.7b", "extract")
        self.assertEqual(command[2], "/vol/weights/qwen--qwen3-1.7b")
        self.assertEqual(command[command.index("--served-model-name") + 1], command[2])

    def test_measure_servers_answer_under_the_public_name(self):
        command, env = self.command("l4-qwen3-1.7b", "measure")
        self.assertEqual(command[command.index("--served-model-name") + 1], "Qwen/Qwen3-1.7B")
        self.assertNotIn("--enable-sleep-mode", command)
        self.assertNotIn("VLLM_SERVER_DEV_MODE", env)

    def test_rung3_serves_fp8_weights_everywhere_and_sleeps_only_to_measure(self):
        for role in ("generate", "extract", "measure"):
            command, env = self.command("l4-qwen3-8b", role)
            self.assertEqual(command[command.index("--quantization") + 1], "fp8")
            self.assertEqual("--enable-sleep-mode" in command, role == "measure")
            self.assertEqual(env.get("VLLM_SERVER_DEV_MODE") == "1", role == "measure")
        # Hard caps as the rules state them since 26 Nov 10:10 and 11:00: smoke $0.75, train $1.50,
        # measure $2.50, confirm $0.88.
        plan = lib.PLANS["l4-qwen3-8b"]
        self.assertEqual({k: lib.dollars(s, plan.dollars_per_hour) for k, s in plan.stage_seconds.items()}, {"smoke": 0.75, "train": 1.5, "measure": 2.5, "confirm": 0.88})


class Rung1SavedMetricsTests(unittest.TestCase):
    def test_val_metrics_files_are_read_in_epoch_order(self):
        import rung1_offline as rung1

        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for epoch, accuracy in ((1, 0.8), (0, 0.7), (10, 0.9)):
                (root / str(epoch)).mkdir()
                (root / str(epoch) / "val_metrics.json").write_text(json.dumps({f"cond_acc_{k}_epoch": accuracy - 0.1 * k for k in range(3)} | {"loss_epoch": 1.0, "full_acc_0_epoch": accuracy}))
            (root / "epoch0_end").mkdir()
            passes = rung1.saved_val_metrics(root)
        self.assertEqual([p["epoch"] for p in passes], ["0", "1", "10"])
        for got, want in zip(passes[-1]["cond_acc"], [0.9, 0.8, 0.7]):
            self.assertAlmostEqual(got, want)
        self.assertEqual(passes[0]["full_acc"], [0.7])


@unittest.skipUnless(__import__("importlib").util.find_spec("datasets"), "datasets is not installed")
class Rung1ReorderTests(unittest.TestCase):
    """prepare-data shuffles; the trainer validates on the dataset's tail and reads hs_{row}."""

    def test_reorder_puts_held_out_rows_last_and_refuses_lost_rows(self):
        from datasets import Dataset, load_from_disk

        import rung1_offline as rung1

        train, held = [[1, 2], [3, 4], [5, 6]], [[7, 8], [9, 10]]
        shuffled = [held[1], train[2], held[0], train[0], train[1]]
        with tempfile.TemporaryDirectory() as directory:
            source, target = Path(directory) / "prepared", Path(directory) / "ordered"
            Dataset.from_dict({"input_ids": shuffled, "loss_mask": [[0, 1]] * 5}).save_to_disk(str(source))
            rung1.reorder(source, target, train, held)
            ordered = [list(map(int, row)) for row in load_from_disk(str(target))["input_ids"]]
            self.assertEqual(ordered[3:], [held[1], held[0]])
            self.assertEqual(sorted(map(tuple, ordered[:3])), sorted(map(tuple, train)))
            with self.assertRaises(SystemExit):
                rung1.reorder(source, Path(directory) / "other", train + [[11, 12]], held)


@unittest.skipUnless(__import__("importlib").util.find_spec("aiohttp"), "aiohttp is not installed")
class DiagnoseStageTests(unittest.TestCase):
    """The diagnose stage's plumbing with stand-ins and a fake speculators (no model loads)."""

    def test_every_arm_is_served_and_measured(self):
        import trained_draft as harness

        with tempfile.TemporaryDirectory() as directory:
            run_dir = Path(directory)
            (run_dir / "prompts").mkdir()
            (run_dir / "prompts" / "held_out.json").write_text(json.dumps([{"id": f"p{i}", "content": f"question {i}"} for i in range(8)]))
            (run_dir / "draft").mkdir()
            (run_dir / "draft" / "config.json").write_text("{}")
            fake = run_dir / "speculators"
            fake.write_text("#!/usr/bin/env python3\nimport pathlib, sys\nargs = sys.argv[1:]\nout = pathlib.Path(args[args.index('--output-path') + 1])\nout.mkdir(parents=True, exist_ok=True)\n(out / 'config.json').write_text('{}')\n")
            fake.chmod(0o755)
            commits = []
            ctx = harness.Context(run_dir, dry=True, weights=run_dir, speculators=str(fake), plan=lib.PLANS["l4-qwen3-1.7b"], commit=lambda: commits.append(1))
            record = harness.run_stage(ctx, "diagnose")
            phases = [json.loads(line)["phase"] for line in (run_dir / "diagnose-progress.jsonl").read_text().splitlines()]
            saved = json.loads((run_dir / "diagnose.json").read_text())
        self.assertEqual(sorted(record["arms"]), ["converted", "converted-norm-before-residual", "published", "smoke-trained"])
        self.assertTrue(record["all_arms_measured"] and record["ran"], record)
        self.assertNotIn("passed", record)
        self.assertEqual(phases[0], "start")
        self.assertEqual(phases[-1], "end")
        self.assertIn("served-published", phases)
        self.assertGreaterEqual(len(commits), len(phases))
        self.assertNotIn("partial", saved)
        for arm in record["arms"].values():
            self.assertEqual(arm["ok"], arm["requests"])
            self.assertIsNotNone(arm["acceptance"])

    def test_a_cut_stage_leaves_a_partial_record(self):
        import trained_draft as harness

        with tempfile.TemporaryDirectory() as directory:
            ctx = harness.Context(Path(directory), dry=True, weights=Path(directory), speculators="speculators")
            ctx.stage = "smoke"
            ctx.progress("answers", {"stage": "smoke", "regenerate": {"rows": 64}}, rows=64)
            saved = json.loads((Path(directory) / "smoke.json").read_text())
        self.assertEqual((saved["partial"], saved["last_phase"], saved["regenerate"]["rows"]), (True, "answers", 64))


class EvidenceTests(unittest.TestCase):
    """The files the contract record digests: draft acceptance from vLLM's counters and the
    content-free traffic shape. Neither may carry text."""

    def record(self):
        counters = {"drafts": 10.0, "draft_tokens": 30.0, "accepted_tokens": 12.0, "accepted_per_position": {0: 6.0, 1: 4.0, 2: 2.0}}
        return {"model": "Qwen/Qwen3-1.7B", "model_revision": "r", "engine": "vllm 0.34.0", "image": lib.VLLM_IMAGE, "gpu": "L4", "draft_tokens": 3,
                "windows": [{"control": "none", "candidates": ["published", "trained"],
                             "loads": [{"concurrency": 1, "requests_per_run": 2, "max_tokens": 256, "warm_ids": ["oasst1:a"], "prompt_ids": ["oasst1:b", "oasst1:c"], "runs": []}],
                             "acceptance": {"trained": {"1": {"drafts": 10.0, "mean_tokens_per_step": 2.2, "per_position": [0.6, 0.67, 0.5]}}},
                             "acceptance_readings": {"trained": {"1": {"before": {**counters, "drafts": 0.0}, "after": counters}}},
                             "held_out_acceptance": {}, "held_out_readings": {},
                             "same_answers": {"prompts": 1, "max_tokens": 256, "ids": ["oasst1:d"], "answers": {"none": ["SECRET ANSWER TEXT"]}}}]}

    def test_files_carry_counts_and_ids_never_text(self):
        with tempfile.TemporaryDirectory() as directory:
            digests = lib.write_evidence(Path(directory), self.record())
            acceptance = (Path(directory) / lib.ACCEPTANCE_FILE).read_text()
            shape = (Path(directory) / lib.SHAPE_FILE).read_text()
        self.assertEqual(sorted(digests), sorted([lib.ACCEPTANCE_FILE, lib.SHAPE_FILE]))
        self.assertTrue(all(value.startswith("sha256:") and len(value) == 71 for value in digests.values()), digests)
        self.assertNotIn("SECRET", acceptance + shape)
        parsed = json.loads(acceptance)
        self.assertEqual(parsed["windows"][0]["by_load"]["trained"]["1"]["mean_tokens_per_step"], 2.2)
        self.assertEqual(parsed["windows"][0]["readings_by_load"]["trained"]["1"]["after"]["drafts"], 10.0)
        loads = json.loads(shape)["windows"][0]["loads"]
        self.assertEqual((loads[0]["prompt_ids"], loads[0]["warm_ids"]), (["oasst1:b", "oasst1:c"], ["oasst1:a"]))
        self.assertEqual(json.loads(shape)["windows"][0]["same_answers"]["ids"], ["oasst1:d"])

    def test_ids_follow_the_bodies_sent(self):
        import trained_draft as harness

        prompts = [{"id": f"p{i}", "content": f"question {i}"} for i in range(3)]
        bodies = harness.bodies_for("m", prompts, 2, 4, 16)
        self.assertEqual(harness.ids_for(prompts, 2, 4), ["p2", "p0", "p1", "p2"])
        self.assertEqual([body["messages"][0]["content"] for body in bodies], ["question 2", "question 0", "question 1", "question 2"])

    def test_precision_follows_the_plan(self):
        self.assertEqual(lib.PLANS["l4-qwen3-1.7b"].precision, {"weights": "bf16", "kv_cache": "bf16"})
        self.assertEqual(lib.PLANS["l4-qwen3-8b"].precision["weights"], "fp8")

    def test_harness_commit_is_a_commit_or_unknown(self):
        import trained_draft as harness

        found = harness.harness_commit()
        self.assertEqual(sorted(found), ["commit", "dirty"])
        self.assertTrue(found["commit"] == "" and found["dirty"] or len(found["commit"]) == 40, found)


@unittest.skipUnless(__import__("importlib").util.find_spec("torch") and __import__("importlib").util.find_spec("safetensors"), "torch and safetensors are not installed")
class DraftEmbeddingTests(unittest.TestCase):
    """A converted draft gets the verifier's embedding once; speculators would train it with zeros."""

    def test_adds_the_verifiers_embedding_once(self):
        import torch
        from safetensors.torch import load_file, save_file

        from draft_embedding import add_verifier_embedding

        with tempfile.TemporaryDirectory() as directory:
            draft, verifier = Path(directory) / "draft", Path(directory) / "verifier"
            draft.mkdir()
            verifier.mkdir()
            embedding = torch.randn(10, 4).to(torch.bfloat16)
            save_file({"fc.weight": torch.zeros(4, 12, dtype=torch.bfloat16)}, str(draft / "model.safetensors"))
            save_file({"model.embed_tokens.weight": embedding, "model.norm.weight": torch.ones(4, dtype=torch.bfloat16)}, str(verifier / "model.safetensors"))
            first = add_verifier_embedding(draft, str(verifier))
            second = add_verifier_embedding(draft, str(verifier))
            saved = load_file(str(draft / "model.safetensors"))
        self.assertEqual((first["added"], first["shape"], second["added"]), (True, [10, 4], False))
        self.assertTrue(torch.equal(saved["embed_tokens.weight"], embedding))
        self.assertIn("fc.weight", saved)


@unittest.skipUnless(__import__("importlib").util.find_spec("aiohttp"), "aiohttp is not installed")
class ConfirmStageTests(unittest.TestCase):
    """The confirm stage with stand-ins: the trained draft against the control again, timed runs
    only, naming the measure record it confirms and the machine it ran on."""

    def test_confirm_measures_the_trained_draft_again_and_names_its_measure(self):
        import hashlib

        import trained_draft as harness

        with tempfile.TemporaryDirectory() as directory:
            run_dir = Path(directory)
            (run_dir / "prompts").mkdir()
            (run_dir / "prompts" / "held_out.json").write_text(json.dumps([{"id": f"p{i}", "content": f"question {i}"} for i in range(40)]))
            (run_dir / "draft").mkdir()
            (run_dir / "draft" / "config.json").write_text("{}")
            (run_dir / "measure.json").write_text(json.dumps({"schema": lib.RECORD_SCHEMA}))
            ctx = harness.Context(run_dir, dry=True, weights=run_dir, speculators="speculators", plan=lib.PLANS["l4-qwen3-1.7b"], harness={"commit": "c" * 40, "dirty": False})
            record = harness.run_stage(ctx, "confirm")
            saved = json.loads((run_dir / lib.CONFIRM_FILE).read_text())
            digest = "sha256:" + hashlib.sha256((run_dir / "measure.json").read_bytes()).hexdigest()
        self.assertEqual((record["schema"], record["confirms"], record["gate_passed"]), (lib.CONFIRM_SCHEMA, digest, True))
        self.assertEqual([w["candidates"] for w in saved["windows"]], [["trained"]])
        self.assertEqual([level["concurrency"] for level in saved["windows"][0]["loads"]], [1, 8])
        self.assertNotIn("same_answers", saved["windows"][0])
        self.assertEqual(sorted(record["machine"]), ["gpu_uuid", "hostname", "task_id"])



class ConfirmGateTests(unittest.TestCase):
    """The confirm stage runs only after a measure the rung's bar passed, as the judge reads it."""

    def bar(self, **lines):
        base = {"bar_validity": "pass (0 failed requests)", "bar_answers_held": "pass (task-score: no measurable difference or better against none)",
                "trained_vs_none_at_1": "faster (+40.0%, noise floor 2.0%)", "trained_vs_none_at_32": "no measurable difference (+1.0%, noise floor 2.0%)"}
        base.update(lines)
        return {"pass_bar": base}

    def test_a_valid_measure_with_answers_held_and_a_faster_load_may_be_confirmed(self):
        self.assertEqual(lib.confirm_refusal(self.bar()), "")

    def test_each_missing_condition_is_named(self):
        self.assertIn("not valid", lib.confirm_refusal(self.bar(bar_validity="fail (1 failed requests)")))
        self.assertIn("answers did not hold", lib.confirm_refusal(self.bar(bar_answers_held="fail (task-score: worse than none)")))
        self.assertIn("at no load", lib.confirm_refusal(self.bar(trained_vs_none_at_1="slower (-3.0%, noise floor 2.0%)")))
        self.assertIn("not valid", lib.confirm_refusal({}))


class StartOnceTests(unittest.TestCase):
    """A preempted call that Modal restarts refuses itself; a new launch runs."""

    def test_a_restart_of_the_same_call_refuses_and_a_new_call_runs(self):
        import trained_draft as harness

        with tempfile.TemporaryDirectory() as directory:
            run_dir, commits = Path(directory), []
            first = harness.start_once(run_dir, "measure", "fc-1", lambda: commits.append(1))
            restart = harness.start_once(run_dir, "measure", "fc-1", lambda: commits.append(1))
            relaunch = harness.start_once(run_dir, "measure", "fc-2", lambda: commits.append(1))
            starts = (run_dir / "measure-starts.jsonl").read_text().splitlines()
        self.assertIsNone(first)
        self.assertTrue(restart["refused_rerun"] and restart["gate_passed"] is False)
        self.assertIsNone(relaunch)
        self.assertEqual((len(starts), len(commits)), (3, 3))


class MemoryPlanTests(unittest.TestCase):
    """Every server a plan starts fits its share of the GPU, from what the servers' own logs
    reported; r3-2's extraction server (0.40 of an L4 for Qwen3-8B in FP8) would have failed here."""

    def test_every_server_fits_its_memory_share(self):
        for plan in lib.PLANS.values():
            for role in ("generate", "extract", "measure"):
                with self.subTest(plan=plan.name, role=role):
                    self.assertGreater(lib.server_headroom_gib(plan, role), 0.0)
            if not plan.sleep_mode:
                # The measure stage's servers of one window hold the GPU at once.
                arms = max(1 + len(window.candidates) for window in plan.windows)
                self.assertLessEqual(arms * plan.memory_share["measure"], 0.95, plan.name)

    def test_the_share_r3_2_used_does_not_fit(self):
        import dataclasses

        plan = lib.PLANS["l4-qwen3-8b"]
        failed = dataclasses.replace(plan, memory_share={**plan.memory_share, "extract": 0.40}, max_model_len={**plan.max_model_len, "extract": 4096})
        self.assertLess(lib.server_headroom_gib(failed, "extract"), 0.0)



class Rung4PlanTests(unittest.TestCase):
    """Rung 4 serves rung 3's draft on an H100 beside a second server without speculation, and
    reads answers by the product's rule for setups that change answers, on three servers each."""

    def test_rung4_plan(self):
        plan = lib.PLANS["h100-qwen3-8b-fp8"]
        self.assertEqual((plan.gpu, plan.precision["weights"], plan.adopt_draft), ("H100", "fp8", True))
        self.assertEqual(plan.windows[0].control, "none")
        self.assertIn("none_b", plan.windows[0].candidates)
        self.assertEqual(plan.arm_args("none_b", "/draft"), [])
        self.assertEqual((plan.task_servers, plan.task_rule), (3, "held_within_1_point"))

    def test_an_adopting_plan_trains_nothing(self):
        import trained_draft as harness

        with tempfile.TemporaryDirectory() as directory:
            ctx = harness.Context(Path(directory), dry=True, weights=Path(directory), speculators="speculators", plan=lib.PLANS["h100-qwen3-8b-fp8"])
            with self.assertRaises(RuntimeError):
                harness.stage_train(ctx)


@unittest.skipUnless(__import__("importlib").util.find_spec("aiohttp"), "aiohttp is not installed")
class Rung4SmokeTests(unittest.TestCase):
    """Rung 4's smoke checks the adopted draft and serves the drafting arms; a server without
    speculation (none_b) has no acceptance to read and would fail the gate."""

    def test_the_smoke_passes_on_an_adopted_draft(self):
        import trained_draft as harness

        with tempfile.TemporaryDirectory() as directory:
            run = Path(directory)
            (run / "prompts").mkdir()
            (run / "prompts" / "held_out.json").write_text(json.dumps([{"id": f"p{i}", "content": f"question {i}"} for i in range(12)]))
            (run / "prompts" / "train.json").write_text("[]")
            (run / "draft").mkdir()
            (run / "draft" / "config.json").write_text("{}")
            plan = lib.PLANS["h100-qwen3-8b-fp8"]
            facts = lib.TrainingFacts(name="oasst1-chat", examples=2000, held_out_examples=128, training_tokens=1, dataset_fingerprint="sha256:" + "a" * 64,
                                      held_out_acceptance=[0.5, 0.5, 0.5], start_acceptance=[0.4, 0.4, 0.4], produced_at="2026-11-26T10:00:00Z", wall_seconds=1)
            lib.write_json(run / "report.json", lib.training_report(facts, run / "draft", plan))
            ctx = harness.Context(run, dry=True, weights=run, speculators="speculators", plan=plan)
            record = harness.run_stage(ctx, "smoke")
            (run / "draft" / "extra.json").write_text("{}")
            changed = harness.draft_digest_matches(ctx)
        self.assertTrue(record["gate_passed"] and record["adopted"]["digest_matches"], record)
        self.assertEqual(sorted(record["serve"]), ["published", "trained"])
        self.assertFalse(changed)

"""Offline tests for the Modal GPU PoC driver's planning: how the product planner's setups
become campaign setups. Nothing runs remotely and nothing is written to the run state."""

from __future__ import annotations

import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import poc_modal  # noqa: E402

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
        poc_modal.CANDIDATES[:] = [poc_modal.CONTROL]

    def test_planner_change_goes_onto_the_publishers_command_and_the_copy_comes_last(self) -> None:
        record = poc_modal.plan(proposal(
            setup("spec", ["--speculative-config", '{"method":"mtp","num_speculative_tokens":1}'], "Speculation"),
            setup("huge", ["--max-num-seqs", "2048", "--max-num-batched-tokens", "16384"], "Larger batches"),
            setup("batch", ["--max-num-seqs", "256", "--max-num-batched-tokens", "16384"], "Fewer at once, bigger prompt chunks"),
            setup("wide", ["--max-num-seqs", "512"], "Two GPUs", gpu_count=2),
            setup("graphs", ["--max-cudagraph-capture-size", "640"], "CUDA graphs")))
        labels = [label for label, _, _ in poc_modal.CANDIDATES]
        self.assertEqual(labels, [poc_modal.CONTROL[0], "Fewer at once, bigger prompt chunks", poc_modal.COPY[0]])
        self.assertEqual(poc_modal.CANDIDATES[1][2], [*poc_modal.MODEL.publisher_args, "--max-num-seqs", "256", "--max-num-batched-tokens", "16384"])
        skipped = {row["label"]: row["skipped"] for row in record}
        self.assertIn("speculative decoding", skipped["Speculation"])
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


class EvidenceTest(unittest.TestCase):
    def test_engine_measured_needs_a_windowed_measurement(self) -> None:
        gauge_only = {"evidence": {"measurements": [{"name": "runtime_requests_running", "availability": "available"}, {"name": "runtime_queue_time_p95", "availability": "not_observed"}]}}
        self.assertFalse(poc_modal.engine_measured(gauge_only))
        gauge_only["evidence"]["measurements"].append({"name": "runtime_decode_time_p95", "availability": "available"})
        self.assertTrue(poc_modal.engine_measured(gauge_only))

    def test_the_fresh_servers_decide_the_copy(self) -> None:
        first_only = {"comparison": {"output_token_throughput": {"decision": "improved"}}}
        self.assertEqual(poc_modal.objective_decision(first_only), "improved")
        rechecked = dict(first_only, confirmation={"output_token_throughput": {"decision": "no_measurable_difference"}})
        self.assertEqual(poc_modal.objective_decision(rechecked), "no_measurable_difference")


if __name__ == "__main__":
    unittest.main()

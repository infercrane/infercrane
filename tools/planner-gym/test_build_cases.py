"""Offline tests of the gym's case builder: python3 -m unittest discover -s tools/planner-gym"""

import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import build_cases  # noqa: E402


def entry(*values: float) -> dict:
    return {"output": build_cases.spread(list(values)), "useful_output": build_cases.spread(list(values))}


class ScreenSpreadTest(unittest.TestCase):
    def test_a_setup_is_ahead_only_by_more_than_the_current_setups_two_runs_differ(self):
        # The current setup ran 100 first and 104 last: 4% apart, as screen.py reads it.
        measured = {"defaults": entry(100, 104), "fp8": entry(105), "kv": entry(107)}
        build_cases.screen_spread(measured, "defaults")
        current, fp8, kv = (measured[name]["output"] for name in ("defaults", "fp8", "kv"))
        self.assertEqual(current["median"], 102)
        self.assertEqual(current["noise"], 0.0392)
        self.assertEqual(current["runs"], [100, 104])
        # The gym's hit is chosen.max >= best.min: kv (107) is not ahead of fp8 (105) by more
        # than 3.9%, so fp8 is a hit; it is ahead of the current setup's 102 (+4.9%), as
        # screen.py would make it a finalist, so keeping the current setup is a miss.
        self.assertGreaterEqual(fp8["max"], kv["min"])
        self.assertLess(current["max"], kv["min"])
        self.assertEqual(kv["min"], 107)

    def test_far_ahead_is_a_miss(self):
        measured = {"defaults": entry(100, 101), "fp8": entry(120)}
        build_cases.screen_spread(measured, "defaults")
        self.assertLess(measured["defaults"]["output"]["max"], measured["fp8"]["output"]["min"])

    def test_without_two_runs_of_the_current_setup_the_spread_is_unknown(self):
        measured = {"defaults": entry(100), "fp8": entry(101)}
        build_cases.screen_spread(measured, "defaults")
        self.assertIsNone(measured["defaults"]["output"]["noise"])
        self.assertEqual(measured["defaults"]["output"]["max"], 100)
        self.assertLess(measured["defaults"]["output"]["max"], measured["fp8"]["output"]["min"])



class SmallModelFileTest(unittest.TestCase):
    def document(self, model_id: str, revision: str) -> dict:
        return {"model": {"id": model_id, "revision": revision}, "setups": {}, "cells": [], "current": "defaults"}

    def test_a_known_model_carries_its_memory_as_the_model_check_reads_it(self):
        out = build_cases.small_model_file(self.document("Qwen/Qwen3-8B", "b968826d9c46dd6066d109eabc6255188de91218"), [], "L4", "t")
        self.assertEqual(out["model_memory"]["weights_bytes"], 16381470720)
        self.assertEqual(out["model_memory"]["kv_cache_bytes_per_token"], 147456)

    def test_another_model_or_commit_carries_none(self):
        self.assertIsNone(build_cases.small_model_file(self.document("Qwen/Qwen3-8B", "0" * 40), [], "L4", "t")["model_memory"])

    def test_the_screen_the_rules_were_written_from_is_marked(self):
        self.assertIn("rules v9", build_cases.SCREEN_BUILT_FROM["l4-8b-screen-1"])


if __name__ == "__main__":
    unittest.main()

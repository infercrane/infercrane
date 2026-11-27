#!/usr/bin/env python3
"""Checks the evaluator's roofline arithmetic without a GPU."""

from __future__ import annotations

import importlib.util
import sys
import unittest
from pathlib import Path

SPEC = importlib.util.spec_from_file_location("pack_evaluate", Path(__file__).with_name("evaluate.py"))
evaluate = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(evaluate)
# A run imports the evaluator as __main__, where a candidate can find it.
sys.modules[SPEC.name] = evaluate


class FakeTesting:
    def __init__(self, gbps=None, error=None):
        self.gbps, self.error = gbps, error

    def get_dram_gbps(self):
        if self.error:
            raise self.error
        return self.gbps


class FakeTriton:
    def __init__(self, testing):
        self.testing = testing


class RooflineTest(unittest.TestCase):
    def test_minimum_bytes_counts_three_reads_and_two_writes(self) -> None:
        # x, residual, output and residual_out are rows*hidden each; weight is hidden.
        self.assertEqual(evaluate.minimum_bytes(128, 1024, 2), (4 * 128 * 1024 + 1024) * 2)

    def test_utilization_is_roofline_time_over_measured_time(self) -> None:
        bytes_moved = evaluate.minimum_bytes(128, 1024, 2)
        roofline_us = bytes_moved / (3352 * 1e3)
        self.assertAlmostEqual(evaluate.roofline_utilization(bytes_moved, roofline_us * 4, 3352), 0.25)
        self.assertAlmostEqual(evaluate.roofline_utilization(bytes_moved, roofline_us, 3352), 1.0)

    def test_unknown_or_impossible_inputs_stay_unknown(self) -> None:
        bytes_moved = evaluate.minimum_bytes(128, 1024, 2)
        for median_us, peak in ((0.0, 3352), (5.0, 0.0), (-1.0, 3352)):
            self.assertEqual(evaluate.roofline_utilization(bytes_moved, median_us, peak), 0.0)
        # Faster than DRAM allows means cache-resident timing, not a reached roofline.
        self.assertEqual(evaluate.roofline_utilization(bytes_moved, 0.01, 3352), 0.0)

    def test_peak_bandwidth_falls_back_to_unknown(self) -> None:
        self.assertEqual(evaluate.peak_dram_gbps(FakeTriton(FakeTesting(gbps=3352.0))), 3352.0)
        self.assertEqual(evaluate.peak_dram_gbps(FakeTriton(FakeTesting(error=RuntimeError("no device")))), 0.0)
        self.assertEqual(evaluate.peak_dram_gbps(FakeTriton(object())), 0.0)


class HardeningTest(unittest.TestCase):
    def test_traffic_shapes_come_from_the_profilers_strings(self) -> None:
        shapes = ["128×4096 bf16; 128×4096 bf16; 4096 bf16", "16x4096", "junk", "0×10 bf16", "128×4096 bf16", 7, "9" * 600]
        self.assertEqual(evaluate.traffic_shapes(shapes), [(128, 4096), (16, 4096)])
        self.assertEqual(evaluate.traffic_shapes(None), [])
        self.assertEqual(len(evaluate.traffic_shapes([f"{rows}×64" for rows in range(1, 40)])), evaluate.MAX_TRAFFIC_SHAPES)

    def test_hidden_neighbours_are_seeded_and_near(self) -> None:
        shapes = [(128, 4096), (1, 2048)]
        first, again = evaluate.hidden_neighbours(shapes, 99), evaluate.hidden_neighbours(shapes, 99)
        self.assertEqual(first, again)
        for (rows, hidden), (near, same) in zip(shapes, first):
            self.assertEqual(hidden, same)
            self.assertTrue(near >= 1 and abs(near - rows) <= 7 and near != rows or rows == 1)

    def test_a_candidate_that_rebinds_a_check_is_caught(self) -> None:
        before = evaluate.integrity_snapshot()
        source = ("import sys\n"
                  f"sys.modules[{evaluate.__name__!r}].reference = lambda *args: args\n"
                  "def fused_residual_rmsnorm(x, residual, weight, epsilon):\n    return x, residual\n")
        original = evaluate.reference
        try:
            evaluate.load_candidate(source)
            self.assertIn("evaluate.reference", evaluate.integrity_changes(before, evaluate.integrity_snapshot()))
        finally:
            evaluate.reference = original
        self.assertEqual(evaluate.integrity_changes(before, evaluate.integrity_snapshot()), [])
        honest = "def fused_residual_rmsnorm(x, residual, weight, epsilon):\n    return x, residual\n"
        evaluate.load_candidate(honest)
        self.assertEqual(evaluate.integrity_changes(before, evaluate.integrity_snapshot()), [])

    def test_a_constant_rebound_is_caught_too(self) -> None:
        before = evaluate.integrity_snapshot()
        original = evaluate.EPSILON
        try:
            evaluate.EPSILON = 1.0
            self.assertIn("evaluate.EPSILON", evaluate.integrity_changes(before, evaluate.integrity_snapshot()))
        finally:
            evaluate.EPSILON = original

    def test_memory_floor(self) -> None:
        bytes_moved = evaluate.minimum_bytes(128, 4096, 2)
        floor_us = bytes_moved / (3352 * 1e3)
        self.assertTrue(evaluate.below_memory_floor(bytes_moved, floor_us * 0.5, 3352))
        self.assertFalse(evaluate.below_memory_floor(bytes_moved, floor_us * 0.95, 3352))
        self.assertFalse(evaluate.below_memory_floor(bytes_moved, floor_us * 0.5, 0.0))  # unknown peak decides nothing

    def test_rotation_covers_twice_the_l2(self) -> None:
        for rows, hidden in ((128, 1024), (1, 4096), (4096, 8192)):
            sets = evaluate.rotation_sets(rows, hidden, 2)
            self.assertGreaterEqual(sets, 2)
            if sets < 256:
                self.assertGreaterEqual(sets * evaluate.minimum_bytes(rows, hidden, 2), 2 * evaluate.L2_BYTES)


if __name__ == "__main__":
    unittest.main()

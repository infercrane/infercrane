#!/usr/bin/env python3
"""Checks the evaluator's roofline arithmetic without a GPU."""

from __future__ import annotations

import importlib.util
import unittest
from pathlib import Path

SPEC = importlib.util.spec_from_file_location("pack_evaluate", Path(__file__).with_name("evaluate.py"))
evaluate = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(evaluate)


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


if __name__ == "__main__":
    unittest.main()

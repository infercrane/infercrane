"""Offline tests for the machine-variance arithmetic (no Modal calls)."""

import math
import random
import statistics
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import machine_variance  # noqa: E402


class SpreadTest(unittest.TestCase):
    def test_separates_machine_spread_from_run_noise(self):
        offsets = [-0.012, -0.006, -0.002, 0.0, 0.003, 0.007, 0.010, 0.015]
        runs = [[1000 * (1 + offset) * factor for factor in (0.996, 1.0, 1.004)] for offset in offsets]
        got = machine_variance.spread(runs)
        self.assertEqual((got["machines"], got["runs"]), (8, 24))
        self.assertAlmostEqual(got["run_to_run"], 0.004, delta=0.0002)
        # The runs here centre exactly on each machine, so the expected share of run noise
        # in the machine means (run variance / 3) is taken out of a spread that has none.
        means_variance = statistics.variance(math.log1p(offset) for offset in offsets)
        self.assertAlmostEqual(got["machine_sd"], math.sqrt(means_variance - got["run_to_run"] ** 2 / 3), delta=0.0001)
        self.assertAlmostEqual(got["two_machines_differ_sd"], math.sqrt(2) * got["machine_sd"], places=9)
        self.assertGreater(got["machine_sd_upper_90"], got["machine_sd"])

    def test_same_machine_noise_alone_is_not_a_machine_spread(self):
        rng = random.Random(7)
        estimates = [machine_variance.spread([[1000 * (1 + rng.gauss(0, 0.005)) for _ in range(3)] for _ in range(8)])["machine_sd"] for _ in range(400)]
        # Noise of 0.5% over three runs leaves machine means 0.29% apart; that must not read as machines.
        self.assertLess(statistics.median(estimates), 0.0015)

    def test_needs_three_machines_with_two_runs(self):
        self.assertIsNone(machine_variance.spread([[1.0, 1.0], [1.0, 1.0], [1.0]]))

    def test_groups_by_driver_and_counts_repeated_gpus(self):
        def machine(driver, uuid, value):
            return {"facts": {"driver_version": driver, "uuid": uuid, "cpu_model": "x"},
                    "runs": [{metric: value * factor for metric in machine_variance.METRICS} for factor in (0.999, 1.001)]}

        results = [machine("610", "a", 100), machine("610", "b", 101), machine("610", "c", 99), machine("620", "a", 103), {"machine": 4, "runs": [], "error": "x"}]
        got = machine_variance.analyze(results)
        self.assertEqual((got["usable_machines"], got["failed_machines"], got["repeated_gpus"]), (4, 1, 1))
        self.assertEqual(set(got["metrics"]), {"all", "driver 610", "driver 620"})
        self.assertIsNone(got["metrics"]["driver 620"]["output_throughput"])


if __name__ == "__main__":
    unittest.main()

#!/usr/bin/env python3
"""Checks the evaluator's roofline arithmetic without a GPU."""

from __future__ import annotations

import contextlib
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


class Recorder:
    """Stands in for torch.cuda and triton.testing.do_bench, recording the order of events."""

    def __init__(self, fail_capture=False):
        self.events, self.fail_capture, self.benched = [], fail_capture, None
        self.cuda = self

    def Stream(self):
        return self

    def current_stream(self):
        return self

    def wait_stream(self, other):
        pass

    def stream(self, stream):
        return contextlib.nullcontext()

    def CUDAGraph(self):
        return FakeGraph()

    @contextlib.contextmanager
    def graph(self, graph):
        self.events.append("capture")
        yield
        if self.fail_capture:
            raise RuntimeError("operation not permitted when stream is capturing")
        self.events.append("captured")

    def synchronize(self):
        self.events.append("synchronize")

    def do_bench(self, function):
        self.benched = function
        return 0.025


class FakeGraph:
    def replay(self):
        pass


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


class GraphTimingTest(unittest.TestCase):
    def timed(self, recorder):
        return evaluate.median_microseconds(recorder, FakeTriton(recorder), lambda: recorder.events.append("call"))

    def test_times_the_replay_of_one_captured_call_after_warm_up(self) -> None:
        recorder = Recorder()
        self.assertAlmostEqual(self.timed(recorder), 25.0)
        warm_up = ["call"] * evaluate.GRAPH_WARMUP_CALLS
        self.assertEqual(recorder.events, warm_up + ["capture", "call", "captured", "synchronize"])
        self.assertEqual(recorder.benched.__name__, "replay")
        self.assertIsInstance(recorder.benched.__self__, FakeGraph)

    def test_a_call_that_cannot_be_captured_fails_timing(self) -> None:
        with self.assertRaises(RuntimeError):
            self.timed(Recorder(fail_capture=True))


if __name__ == "__main__":
    unittest.main()

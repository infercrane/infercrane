#!/usr/bin/env python3
"""Checks the evaluator's roofline arithmetic and the pack's screen without a GPU."""

from __future__ import annotations

import contextlib
import importlib.util
import json
import unittest
from pathlib import Path

HERE = Path(__file__).parent
SPEC = importlib.util.spec_from_file_location("pack_evaluate", HERE / "evaluate.py")
evaluate = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(evaluate)
PACK = json.loads((HERE / "pack.json").read_text(encoding="utf-8"))


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
    def test_minimum_bytes_reads_each_routed_expert_once(self) -> None:
        # 3 experts used, hidden 2048, intermediate 768, BF16, 4 tokens top-8.
        weights = 3 * (2 * 768 * 2048 + 2048 * 768) * 2
        self.assertEqual(evaluate.minimum_bytes(4, 2048, 768, 3, 2, 8), weights + 2 * 4 * 2048 * 2 + 4 * 8 * 8)

    def test_utilization_is_roofline_time_over_measured_time(self) -> None:
        bytes_moved = evaluate.minimum_bytes(4, 2048, 768, 32, 2, 8)
        roofline_us = bytes_moved / (4800 * 1e3)
        self.assertAlmostEqual(evaluate.roofline_utilization(bytes_moved, roofline_us * 5, 4800), 0.2)
        self.assertAlmostEqual(evaluate.roofline_utilization(bytes_moved, roofline_us, 4800), 1.0)

    def test_unknown_or_impossible_inputs_stay_unknown(self) -> None:
        bytes_moved = evaluate.minimum_bytes(4, 2048, 768, 32, 2, 8)
        for median_us, peak in ((0.0, 4800), (5.0, 0.0), (-1.0, 4800)):
            self.assertEqual(evaluate.roofline_utilization(bytes_moved, median_us, peak), 0.0)
        # Faster than DRAM allows means cache-resident weights, not a reached roofline.
        self.assertEqual(evaluate.roofline_utilization(bytes_moved, 0.01, 4800), 0.0)

    def test_peak_bandwidth_falls_back_to_unknown(self) -> None:
        self.assertEqual(evaluate.peak_dram_gbps(FakeTriton(FakeTesting(gbps=4800.0))), 4800.0)
        self.assertEqual(evaluate.peak_dram_gbps(FakeTriton(FakeTesting(error=RuntimeError("no device")))), 0.0)
        self.assertEqual(evaluate.peak_dram_gbps(FakeTriton(object())), 0.0)

    def test_timing_uses_a_small_decode_batch(self) -> None:
        tokens, experts, hidden, intermediate, top_k = evaluate.TIMING
        self.assertLessEqual(tokens, 8)
        self.assertLessEqual(top_k, experts)


class ScreenTest(unittest.TestCase):
    """The pack's static screen, applied the way kernelgen applies it."""

    def screened(self, source: str) -> bool:
        return all(required in source for required in PACK["required_substrings"]) and not any(
            forbidden in source for forbidden in PACK["forbidden_substrings"]
        )

    def test_the_reference_kernels_pass_the_screen(self) -> None:
        for name in ("correct.py", "wrong_router_weight.py", "wrong_gate_up_swapped.py"):
            with self.subTest(name=name):
                self.assertTrue(self.screened((HERE / "testdata" / name).read_text(encoding="utf-8")))

    def test_calling_a_library_matmul_or_the_engine_is_refused(self) -> None:
        correct = (HERE / "testdata" / "correct.py").read_text(encoding="utf-8")
        for shortcut in ("    return torch.matmul(hidden_states, w2)\n", "from vllm import _custom_ops\n", "    out = act @ w2\n"):
            with self.subTest(shortcut=shortcut):
                self.assertFalse(self.screened(correct + shortcut))


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

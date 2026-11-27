"""Offline tests for the kernel-tuning harness: no GPU, torch, vLLM or Modal. The Go suite
runs them (internal/kerneltuning), and several pin values the Go package computes, so the
tuner writes exactly what a revision validates and the harness reads engine logs as the
judge does."""

from __future__ import annotations

import hashlib
import json
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import amdahl  # noqa: E402
import loadgen  # noqa: E402
import proof  # noqa: E402
import tune_moe  # noqa: E402

QWEN36 = {"text_config": {"num_experts": 256, "num_experts_per_tok": 8, "moe_intermediate_size": 512, "hidden_size": 2048},
          "quantization_config": {"quant_method": "fp8", "fmt": "e4m3", "weight_block_size": [128, 128]}}
H100 = "NVIDIA_H100_80GB_HBM3"
FILE = "E=256,N=512,device_name=NVIDIA_H100_80GB_HBM3,dtype=fp8_w8a8,block_shape=[128,128].json"
# internal/kerneltuning/kerneltuning_test.go: tunedEntries, its canonical bytes and its set's digest.
GO_ENTRIES = {
    1: {"BLOCK_SIZE_M": 16, "BLOCK_SIZE_N": 64, "BLOCK_SIZE_K": 128, "GROUP_SIZE_M": 1, "num_warps": 4, "num_stages": 4},
    128: {"BLOCK_SIZE_M": 16, "BLOCK_SIZE_N": 128, "BLOCK_SIZE_K": 128, "GROUP_SIZE_M": 1, "num_warps": 4, "num_stages": 3},
    65: {"BLOCK_SIZE_M": 64, "BLOCK_SIZE_N": 128, "BLOCK_SIZE_K": 128, "GROUP_SIZE_M": 32, "SPLIT_K": 1, "num_warps": 4, "num_stages": 3},
}
GO_CONTENT = ('{"1":{"BLOCK_SIZE_M":16,"BLOCK_SIZE_N":64,"BLOCK_SIZE_K":128,"GROUP_SIZE_M":1,"num_warps":4,"num_stages":4},'
              '"65":{"BLOCK_SIZE_M":64,"BLOCK_SIZE_N":128,"BLOCK_SIZE_K":128,"GROUP_SIZE_M":32,"SPLIT_K":1,"num_warps":4,"num_stages":3},'
              '"128":{"BLOCK_SIZE_M":16,"BLOCK_SIZE_N":128,"BLOCK_SIZE_K":128,"GROUP_SIZE_M":1,"num_warps":4,"num_stages":3}}')
GO_DIGEST = "sha256:7b137f2132b0b2cd25d9b94007e00e57bd6e56e5cf27ad9b929a8266bf947def"


class SameAsTheGoPackage(unittest.TestCase):
    def test_canonical_file_and_digest(self):
        self.assertEqual(tune_moe.canonical_file(GO_ENTRIES).decode(), GO_CONTENT)
        kernel_set = tune_moe.build_set("vllm", "0.34.0", H100, {FILE: GO_ENTRIES})
        self.assertEqual(kernel_set["digest"], GO_DIGEST)
        self.assertEqual(kernel_set["files"][0]["sha256"], hashlib.sha256(GO_CONTENT.encode()).hexdigest())

    def test_shape_file_name_and_device(self):
        shape = tune_moe.shape_from_config(QWEN36)
        self.assertEqual((shape.experts, shape.intermediate, shape.dtype, shape.block_n, shape.block_k, shape.hidden, shape.topk),
                         (256, 512, "fp8_w8a8", 128, 128, 2048, 8))
        self.assertEqual(tune_moe.file_name(shape, H100), FILE)
        self.assertEqual(tune_moe.engine_device_name("NVIDIA H100 80GB HBM3"), H100)
        self.assertEqual(tune_moe.engine_device_name("NVIDIA H200 NVL"), "NVIDIA_H200")
        with self.assertRaises(ValueError):
            tune_moe.shape_from_config({"num_experts": 8, "intermediate_size": 14336})  # BF16: not reviewed

    def test_tunes_the_file_the_engine_looked_for(self):
        shape, device = tune_moe.shape_from_file_name(FILE, 2048, 8)
        self.assertEqual((shape.experts, shape.intermediate, shape.block_n, device), (256, 512, 128, H100))
        # A shared expert fused into the kernel would make the engine look for 257 experts.
        shape, _ = tune_moe.shape_from_file_name(FILE.replace("E=256", "E=257"), 2048, 8)
        self.assertEqual(shape.experts, 257)
        for name in (FILE.replace(",dtype=fp8_w8a8", ""), FILE.replace(".json", "_down.json"), "../" + FILE):
            with self.assertRaises(ValueError):
                tune_moe.shape_from_file_name(name, 2048, 8)

    def test_key_plan(self):
        shape = tune_moe.shape_from_config(QWEN36)
        plan = tune_moe.plan_keys("vllm", "0.34.0", shape, [{"running": 32, "batched_tokens": 8192}, {"running": 128, "batched_tokens": 8192}])
        self.assertEqual(plan["tune"], [1, 2, 4, 8, 16, 24, 32, 48, 64, 96, 128, 2048, 3072, 4096, 6144, 8192])
        self.assertEqual(plan["default"], [5, 9, 17, 65])
        self.assertEqual(tune_moe.plan_keys("vllm", "0.34.0", shape, [{"running": 128, "draft_tokens": 3}])["tune"][-1], 512)
        with self.assertRaises(ValueError):
            tune_moe.plan_keys("vllm", "0.20.2", shape, [{"running": 32}])
        self.assertEqual(tune_moe.nearest_key([64, 96], 80), 64)
        self.assertEqual(tune_moe.nearest_key([96, 64, 128], 100), 96)

    def test_engine_log(self):
        text = "\n".join([
            "\x1b[0;36m(EngineCore pid=91)\x1b[0;0m INFO 11-26 10:00:01 [fp8.py:502] Using TRITON Fp8 MoE backend out of potential backends: ['TRITON', 'DEEPGEMM'].",
            "WARNING 11-26 10:00:02 [fused_moe.py:1254] Using default MoE config. Performance might be sub-optimal! Config file not found at "
            f"/opt/infercrane/kernel-configs/{FILE}, /usr/local/lib/python3.12/dist-packages/vllm/model_executor/layers/fused_moe/configs/{FILE}",
        ])
        reading = proof.read_engine_log(text)
        self.assertEqual(reading, {"backend": "TRITON", "default_for": [FILE]})
        self.assertEqual(proof.invoked(reading, "control", [FILE]), "")
        self.assertIn("did not read", proof.invoked(reading, "candidate", [FILE]))
        tuned = proof.read_engine_log(f"INFO [fused_moe.py:1240] Using configuration from {tune_moe.MOUNT_PATH}/{FILE} for MoE layer.\r")
        self.assertEqual(proof.read_from(tuned, tune_moe.MOUNT_PATH), [FILE])
        self.assertEqual(proof.invoked(tuned, "candidate", [FILE]), "")
        self.assertIn("did not report its default", proof.invoked(tuned, "control", [FILE]))
        self.assertIn("DEEPGEMM", proof.invoked({"backend": "DEEPGEMM", "default_for": [FILE]}, "control", [FILE]))


class Tuning(unittest.TestCase):
    def setUp(self):
        self.shape = tune_moe.shape_from_config(QWEN36)

    def test_memory_floor_is_the_weights_a_step_reads(self):
        # 64 tokens x 8 experts each touch about 221 of 256 experts, 3.15 MB of FP8 weights
        # apiece: about 0.7 GB, at least ~208 us at 3.35 TB/s.
        distinct = tune_moe.expected_distinct(256, 64 * 8)
        self.assertAlmostEqual(distinct, 221.4, delta=0.5)
        floor = tune_moe.memory_floor_us(self.shape, 64, distinct, tune_moe.PEAK_BANDWIDTH[H100])
        self.assertTrue(200 < floor < 215, floor)

    def test_keep_rule(self):
        self.assertTrue(tune_moe.worth_keeping(100.0, 0.005, 96.0))
        self.assertFalse(tune_moe.worth_keeping(100.0, 0.005, 97.5))   # under 3%
        self.assertFalse(tune_moe.worth_keeping(100.0, 0.03, 95.0))    # inside twice the spread

    def test_neighbours_stay_aligned(self):
        start = {"BLOCK_SIZE_M": 16, "BLOCK_SIZE_N": 64, "BLOCK_SIZE_K": 128, "GROUP_SIZE_M": 1, "num_warps": 4, "num_stages": 3}
        for name in tune_moe.ORDER:
            for candidate in tune_moe.neighbours(start, self.shape, name):
                self.assertTrue(tune_moe.aligned(candidate, self.shape))
                self.assertNotIn("SPLIT_K", candidate)

    def test_stand_in_tuning_writes_a_valid_set(self):
        loads = [{"running": 128, "batched_tokens": 8192}, {"running": 32, "batched_tokens": 8192}]
        result = tune_moe.tune(self.shape, H100, "vllm", "0.34.0", loads, tune_moe.Bench(self.shape, stand_in=True), 60, 30, log=lambda line: None)
        report, entries = result["report"], result["entries"]
        self.assertTrue(report["stand_in"])
        # The stand-in's default wastes rows above 64 tokens (vLLM switches to 64-row tiles there).
        self.assertIn(96, report["kept_tuned"])
        self.assertIn(128, report["kept_tuned"])
        for tokens in report["plan"]["default"]:
            self.assertEqual(entries[tokens], tune_moe.stand_in_default(tokens))
        for tokens, row in report["keys"].items():
            if row["kept"] == "engine":
                self.assertEqual(entries[int(tokens)], row["engine"])
                self.assertTrue(row.get("reason"))
            self.assertLessEqual(row.get("best_floor_share") or 0, 1 / tune_moe.FLOOR_FACTOR)
        with tempfile.TemporaryDirectory() as folder:
            kernel_set = tune_moe.write(result, self.shape, H100, "vllm", "0.34.0", folder)
            self.assertEqual(tune_moe.set_digest(kernel_set), kernel_set["digest"])
            names = proof.install_set(kernel_set, str(Path(folder) / "mount"))
            self.assertEqual(names, [FILE])
            on_disk = (Path(folder) / "mount" / FILE).read_bytes()
            self.assertEqual(json.loads(on_disk), {str(k): v for k, v in json.loads(tune_moe.canonical_file(entries)).items()})
            tampered = json.loads(json.dumps(kernel_set))
            tampered["files"][0]["content"] = tampered["files"][0]["content"].replace('"num_warps":4', '"num_warps":8', 1)
            with self.assertRaises(RuntimeError):
                proof.install_set(tampered, str(Path(folder) / "mount2"))

    def test_hidden_sizes_sit_between_keys(self):
        self.assertEqual(tune_moe.hidden_sizes([1, 2, 4, 8, 64, 65]), [3, 6, 36])


class SettingsThatDoNotFit(unittest.TestCase):
    """Rung 2's first L40S run: the engine's own setting did not fit the GPU's shared memory."""

    def setUp(self):
        self.shape = tune_moe.shape_from_config(dict(QWEN36, text_config=dict(QWEN36["text_config"], num_experts=128, moe_intermediate_size=768)))
        self.loads = [{"running": 128, "batched_tokens": 2048}, {"running": 32, "batched_tokens": 2048}]

    def test_a_default_that_does_not_run_is_recorded_and_replaced_only_by_a_checked_setting(self):
        bench = tune_moe.Bench(self.shape, True, tune_moe.PEAK_BANDWIDTH["NVIDIA_L40S"])
        result = tune_moe.tune(self.shape, "NVIDIA_L40S", "vllm", "0.34.0", self.loads, bench, 60, 30, log=lambda line: None)
        report = result["report"]
        self.assertEqual(report["engine_fails_at"], [1, 2, 4])
        for key in (1, 2, 4):
            row = report["keys"][str(key)]
            self.assertIsNone(row["engine_us"])
            self.assertIsNone(row["gain"])
            self.assertIn("OutOfResources", row["engine_error"])
            if row["kept"] == "replaces-failing-default":
                self.assertTrue(row["check"]["passed"])
                self.assertTrue(tune_moe.stand_in_fits(result["entries"][key], key))
        self.assertTrue(report["replaces_failing_default"])
        # Settings that do not fit were tried and recorded, never kept.
        self.assertTrue(any(failed["does_not_fit"] and "OutOfResources" in failed["error"] for row in report["keys"].values() for failed in row.get("failed", [])))
        for key, config in result["entries"].items():
            if key not in report["engine_fails_at"]:
                self.assertTrue(tune_moe.stand_in_fits(config, key) or config == tune_moe.stand_in_default(key), key)

    def test_an_error_reads_unknown_never_a_zero_gain(self):
        plan = proof.PLANS["moe-shapes-l40s"]
        errored = [{"model": plan["shapes"][0]["model"], "error": "OutOfResources: shared memory"},
                   {"model": plan["shapes"][1]["model"], "error": "OutOfResources: shared memory"}]
        verdict = proof.shapes_verdict(plan, errored)
        self.assertFalse(verdict["ask_for_rung3"])
        self.assertTrue(verdict["summary"].startswith("unknown"))
        self.assertTrue(all(row["status"] == "unknown" and row["projected_end_to_end"] is None for row in verdict["rows"]))

    def test_a_load_without_an_engine_time_is_unknown(self):
        plan = proof.PLANS["moe-shapes-l40s"]
        report = {"keys": {"32": {"engine_error": "OutOfResources", "engine_us": None, "kept": "replaces-failing-default"},
                           "128": {"engine_us": 500.0, "best_us": 450.0, "kept": "tuned"}}, "engine_fails_at": [32]}
        verdict = proof.shapes_verdict(plan, [{"report": report}, {"report": report}])
        at = {(row["model"], row["running"]): row for row in verdict["rows"]}
        self.assertEqual(at[(plan["shapes"][0]["model"], 32)]["status"], "unknown")
        self.assertEqual(at[(plan["shapes"][0]["model"], 128)]["status"], "measured")
        # 128 projects 1/(0.126 + 0.874/1.111) = 1.096: rung 3 is asked for on the measured load.
        self.assertTrue(verdict["ask_for_rung3"])
        self.assertEqual(verdict["summary"], "ask for rung 3")


class OverrideIsRestored(unittest.TestCase):
    """vLLM 0.34.0's override_config (fused_moe/__init__.py) sets a module global and restores
    it only when its body returns. Rung 2's second L40S run caught each failing setting inside
    it, so the failing setting stayed in force and every later "engine's own choice" ran it:
    the run recorded the engine's default as failing at every step size. A stub with vLLM's
    exact override stands in for the engine here."""

    NAMES = ("vllm", "vllm.model_executor", "vllm.model_executor.layers", "vllm.model_executor.layers.fused_moe")

    def setUp(self):
        import contextlib
        import types

        stub = types.ModuleType(self.NAMES[-1])
        stub._config = None

        @contextlib.contextmanager
        def override_config(config):  # vLLM 0.34.0, line for line
            old_config = stub._config
            stub._config = config
            yield
            stub._config = old_config

        stub.override_config, stub.get_config = override_config, lambda: stub._config
        self.saved = {name: sys.modules.get(name) for name in self.NAMES}
        modules = [types.ModuleType(name) for name in self.NAMES[:-1]] + [stub]
        for parent, child in zip(modules, modules[1:]):
            setattr(parent, child.__name__.rsplit(".", 1)[1], child)
        sys.modules.update({module.__name__: module for module in modules})
        self.stub = stub
        self.shape = tune_moe.shape_from_config(dict(QWEN36, text_config=dict(QWEN36["text_config"], num_experts=128, moe_intermediate_size=768)))

    def tearDown(self):
        for name, module in self.saved.items():
            if module is None:
                sys.modules.pop(name, None)
            else:
                sys.modules[name] = module

    def engine(self, put_in_force):
        """An engine whose own setting runs at every step size; 256-wide N tiles do not fit."""
        stub, shape = self.stub, self.shape

        class Engine(tune_moe.Bench):
            def time(self, config, tokens, seed=0):
                with put_in_force(config):
                    used = stub.get_config() or tune_moe.stand_in_default(tokens)
                    if used["BLOCK_SIZE_N"] >= 256:
                        raise RuntimeError("OutOfResources: out of resource: shared memory (stub)")
                    return tune_moe.stand_in_time(shape, used, tokens, seed)

            def check(self, config, tokens, bound):
                return {"finite": True, "error": 0.01, "engine_error": 0.01, "passed": True}

            def engine_choice(self, tokens):
                with put_in_force(None):
                    return dict(stub.get_config() or tune_moe.stand_in_default(tokens))

        return Engine(shape, True)

    def tune(self, bench):
        loads = [{"running": 128, "batched_tokens": 2048}, {"running": 32, "batched_tokens": 2048}]
        return tune_moe.tune(self.shape, H100, "vllm", "0.34.0", loads, bench, 60, 30, log=lambda line: None)["report"]

    def test_the_old_way_leaks_a_failing_setting_into_the_engine_default(self):
        import contextlib

        leaky = lambda config: self.stub.override_config(config) if config is not None else contextlib.nullcontext()  # noqa: E731
        report = self.tune(self.engine(leaky))
        self.assertTrue(report["engine_fails_at"], "the stub must reproduce rung 2's false finding")
        self.assertIsNotNone(self.stub.get_config())

    def test_the_engine_default_never_runs_a_failed_setting(self):
        report = self.tune(self.engine(tune_moe.setting))
        self.assertEqual(report["engine_fails_at"], [])
        self.assertTrue(all(row.get("engine_us") for row in report["keys"].values() if row.get("reason") != "time budget spent"))
        self.assertTrue(any(failed["does_not_fit"] for row in report["keys"].values() for failed in row.get("failed", [])))
        self.assertIsNone(self.stub.get_config())

    def test_a_raising_setting_is_taken_back(self):
        with self.assertRaises(RuntimeError):
            with tune_moe.setting({"BLOCK_SIZE_N": 256}):
                raise RuntimeError("OutOfResources")
        self.assertIsNone(self.stub.get_config())

    def test_the_engine_default_refuses_to_run_under_an_override(self):
        self.stub._config = {"BLOCK_SIZE_N": 256}
        with self.assertRaises(tune_moe.HarnessError):
            with tune_moe.setting(None):
                pass


class Load(unittest.TestCase):
    def test_window_counts_tokens_inside_it(self):
        record = {"sent": 0.5, "end": 3.0, "chunks": [1.0, 1.5, 2.0, 2.5], "tokens": 8, "ok": True, "aborted": False, "error": None}
        self.assertEqual(loadgen.tokens_in_window(record, 1.2, 2.2), 4.0)
        late = dict(record, sent=1.6, chunks=[1.7, 1.9], tokens=2, end=2.0)
        failed = {"sent": 1.3, "end": 1.4, "chunks": [], "tokens": None, "ok": False, "aborted": False, "error": "HTTP 500"}
        summary = loadgen.summarize([record, late, failed], 1.0, 3.0)
        self.assertEqual(summary["requests"], 2)
        self.assertEqual(summary["failed"], 1)
        self.assertEqual(summary["output_token_throughput"], (8 + 2) / 2.0)  # every chunk of both answers is inside [1, 3)
        self.assertIsNotNone(summary["ttft_p95_ms"])

    def test_requests_carry_temperature_zero_and_no_reasoning(self):
        body = loadgen.chat_body("m", "hi", 5)
        self.assertEqual((body["temperature"], body["chat_template_kwargs"], body["stream_options"]), (0, {"enable_thinking": False}, {"include_usage": True}))


class Amdahl(unittest.TestCase):
    def test_amdahl_and_the_planner_gate(self):
        self.assertAlmostEqual(amdahl.amdahl(0.05, 1.3), 1 / (0.95 + 0.05 / 1.3))
        self.assertLess(amdahl.amdahl(0.05, 1.3), 1.012)  # a 30% faster op at 5% is about 1% end to end
        self.assertAlmostEqual(amdahl.amdahl(0.6, amdahl.needed_speedup(0.6, 1.05)), 1.05)
        self.assertEqual(amdahl.needed_speedup(0.04, 1.05), float("inf"))

    def test_estimate_counts_the_experts_a_step_reads(self):
        coder = {"num_hidden_layers": 48, "hidden_size": 2048, "num_key_value_heads": 4, "head_dim": 128, "num_experts": 128,
                 "num_experts_per_tok": 8, "moe_intermediate_size": 768, "vocab_size": 151936}
        row = amdahl.estimate(coder, {"BF16": 636948480, "F8_E4M3": 29896998912}, 128, 210)
        self.assertAlmostEqual(row["experts_touched"], 128.0, delta=0.1)
        self.assertAlmostEqual(row["bytes"]["moe_experts"] / 1e9, 28.98, delta=0.05)
        self.assertTrue(0.8 < row["moe_share"] < 0.95 and row["gate"]["hotspot_at_least_5_percent"])
        small = amdahl.estimate(coder, {"BF16": 636948480, "F8_E4M3": 29896998912}, 1, 210)
        self.assertLess(small["bytes"]["moe_experts"], row["bytes"]["moe_experts"] / 10)

    def test_recurrent_state_counts_for_a_hybrid_model(self):
        qwen36 = dict(QWEN36["text_config"], num_hidden_layers=40, num_key_value_heads=2, head_dim=256, vocab_size=248320,
                      full_attention_interval=4, linear_num_value_heads=32, linear_key_head_dim=128, linear_value_head_dim=128, mamba_ssm_dtype="float32")
        row = amdahl.estimate({"text_config": qwen36}, {"BF16": 1498761072, "F8_E4M3": 34453061632}, 128, 210)
        # 30 linear layers x 32 x 128 x 128 FP32 = 60 MiB a request, read and written.
        self.assertAlmostEqual(row["bytes"]["recurrent_state"], 128 * 30 * 32 * 128 * 128 * 4 * 2, delta=1)
        self.assertTrue(0.55 < row["moe_share"] < 0.7)

    def test_measured_share_from_a_window(self):
        report = {"keys": {"32": {"engine_us": 300.0, "best_us": 270.0}, "128": {"engine_us": 500.0, "best_us": 450.0}}}
        detail = {"runs": [{"arm": "control", "cells": {"short/128": {"concurrency": 128, "output_token_throughput": 6400.0}}},
                           {"arm": "candidate", "cells": {"short/128": {"concurrency": 128, "output_token_throughput": 9999.0}}},
                           {"arm": "control", "cells": {"short/128": {"concurrency": 128, "output_token_throughput": 6400.0}}}]}
        (row,) = amdahl.measured(report, detail, 40)
        # 128 / 6,400 tok/s = 20 ms a step; 40 x 0.5 ms = 20 ms: the whole step, by this approximation.
        self.assertEqual((row["step_us"], row["moe_us_per_step"], row["moe_share"], row["kernel_step_size"]), (20000.0, 20000.0, 1.0, 128))


class Plan(unittest.TestCase):
    def test_both_arms_serve_the_same_command(self):
        self.assertEqual(proof.SERVE_ARGS, ["--trust-remote-code", "--language-model-only", "--max-num-seqs", "512"])
        env = proof.engine_env({})
        self.assertNotIn(proof.TUNED_FOLDER_VARIABLE, env)
        self.assertEqual(proof.MODEL["dir"], "/weights/qwen3.6-35b-a3b-fp8-c653bba46dfa")
        self.assertEqual([cell for cell, *_ in proof.CELLS], ["short/128", "short/32"])
        self.assertEqual(proof.RUNS_PER_ARM, 4)


class ModalImages(unittest.TestCase):
    """Every Modal container imports kernel_tune_modal.py, so every image must carry each local
    module it imports, and theirs (8f579b02's download image did not: 14 startup crashes).
    Read from the source, since this suite runs without the modal client."""

    HERE = Path(__file__).resolve().parent

    def local_imports(self, name: str) -> set[str]:
        import ast

        tree = ast.parse((self.HERE / f"{name}.py").read_text())
        found = {alias.name.split(".")[0] for node in ast.walk(tree) if isinstance(node, ast.Import) for alias in node.names}
        found |= {node.module.split(".")[0] for node in ast.walk(tree) if isinstance(node, ast.ImportFrom) and node.module and node.level == 0}
        return {module for module in found if (self.HERE / f"{module}.py").exists()}

    def test_every_image_carries_every_local_module_the_driver_needs(self):
        import ast

        needed, queue = set(), ["kernel_tune_modal"]
        while queue:
            for module in self.local_imports(queue.pop()) - needed:
                needed.add(module)
                queue.append(module)
        tree = ast.parse((self.HERE / "kernel_tune_modal.py").read_text())
        constants = {node.targets[0].id: ast.literal_eval(node.value) for node in tree.body
                     if isinstance(node, ast.Assign) and isinstance(node.targets[0], ast.Name) and node.targets[0].id == "LOCAL_MODULES"}
        self.assertTrue(needed <= set(constants["LOCAL_MODULES"]), needed - set(constants["LOCAL_MODULES"]))
        images = {}
        for node in tree.body:
            if isinstance(node, ast.Assign) and isinstance(node.targets[0], ast.Name) and "Image" in ast.unparse(node.value):
                images[node.targets[0].id] = ast.unparse(node.value)
        self.assertEqual(set(images), {"image", "download_image"})
        for name, chain in images.items():
            self.assertTrue(chain.endswith(".add_local_python_source(*LOCAL_MODULES)"), f"{name}: {chain}")
        functions = [node for node in tree.body if isinstance(node, ast.FunctionDef)
                     and any("app.function" in ast.unparse(decorator) for decorator in node.decorator_list)]
        self.assertGreaterEqual(len(functions), 7)
        for function in functions:
            options = ast.unparse(function.decorator_list[0])
            self.assertTrue("CPU_OPTIONS" in options or "GPU_OPTIONS" in options, f"{function.name}: {options}")
            self.assertIn("status.put(tag", ast.unparse(function), f"{function.name} must report its start")


if __name__ == "__main__":
    unittest.main()

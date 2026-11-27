"""Tests of the state-by-draft patch and its verifier without vLLM (the image build runs the real
CPU check, check_state_by_draft.py, against vLLM 0.34.0 itself).

    python3 -m unittest deploy/vllm-state-by-draft/test_state_by_draft.py
"""

from __future__ import annotations

import ast
import hashlib
import importlib.util
import json
import pathlib
import tempfile
import textwrap
import unittest

HERE = pathlib.Path(__file__).resolve().parent


def load(name: str):
    spec = importlib.util.spec_from_file_location(name, HERE / f"{name}.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


patch = load("patch_state_by_draft")
verify = load("verify_state_by_draft")


def fixture(relative: str) -> str:
    """A stand-in source holding each edit's anchor exactly once, between unrelated lines."""
    return "\n".join(f"# before {index}\n{old}# after {index}\n" for index, (old, _new) in enumerate(patch.EDITS[relative]["edits"]))


class PatchTest(unittest.TestCase):
    def test_every_edit_applies_exactly_once(self) -> None:
        for relative, entry in patch.EDITS.items():
            text = patch.patched(fixture(relative), entry["edits"])
            for old, new in entry["edits"]:
                self.assertIn(new, text, relative)
            with self.assertRaises(ValueError):
                patch.patched(text + entry["edits"][0][0] + entry["edits"][0][0], entry["edits"])

    def test_a_missing_anchor_is_refused(self) -> None:
        relative = "v1/core/sched/scheduler.py"
        broken = fixture(relative).replace(patch.EDITS[relative]["edits"][1][0], "")
        with self.assertRaises(ValueError):
            patch.patched(broken, patch.EDITS[relative]["edits"])

    def test_it_knows_only_vllm_0_34_0_and_says_what_it_adds(self) -> None:
        self.assertEqual(patch.VERSION, "0.34.0")
        self.assertEqual(patch.PATCH_ID, verify.PATCH_ID)
        scheduler = "".join(new for _old, new in patch.EDITS["v1/core/sched/scheduler.py"]["edits"])
        self.assertIn("SPEC_RESERVATION[request.request_id] = self.dynamic_sd_lookup[", scheduler)
        self.assertIn("InferCrane state by draft: running by reserved drafts", scheduler)
        self.assertIn("SPEC_RESERVATION.pop(request.request_id, None)", scheduler)
        loggers = "".join(new for _old, new in patch.EDITS["v1/metrics/loggers.py"]["edits"])
        for name in ("vllm:infercrane_running_reserving_{drafts}_drafts", "vllm:infercrane_running_mixed_reservations",
                     "vllm:infercrane_state_by_draft_receipt_verified", "module.verify(receipt, pathlib.Path(__file__).resolve().parents[2])"):
            self.assertIn(name, loggers)

    def test_it_keeps_v2s_reservation_edits_byte_for_byte(self) -> None:
        """The H100 proofs ran v2 (tools/fast-track-map/vllm_state_by_draft.py on
        feat/state-by-draft-proof); v3 added the gauges and v4 the engine's own receipt check,
        both outside these edits. The digest is v2's EDITS."""
        v2_edits_digest = "0ec0a533745f97dea75ba43c5b02ee465575a924002488569e77b27d19c779bf"
        gauges = ("running_by_reserved_drafts", "_infercrane_reservation_mix")
        kept = {
            "v1/core/single_type_kv_cache_manager.py": patch.EDITS["v1/core/single_type_kv_cache_manager.py"],
            "v1/core/sched/scheduler.py": {"before_sha256": patch.EDITS["v1/core/sched/scheduler.py"]["before_sha256"],
                                           "edits": [edit for edit in patch.EDITS["v1/core/sched/scheduler.py"]["edits"]
                                                     if not any(name in edit[1] for name in gauges)]},
        }
        encoded = json.dumps(json.loads(json.dumps(kept)), sort_keys=True).encode()
        self.assertEqual(hashlib.sha256(encoded).hexdigest(), v2_edits_digest)

    def test_the_later_hunks_only_add_the_stats_the_gauges_and_the_receipt_check(self) -> None:
        """Every edit v3 and v4 add is a pure insertion. In the scheduler they add only the stats
        argument and a method that reads the reservation table without writing to it; the rest
        are SchedulerStats' new field and vLLM's Prometheus logger."""
        gauges = ("running_by_reserved_drafts", "_infercrane_reservation_mix")
        scheduler = [edit for edit in patch.EDITS["v1/core/sched/scheduler.py"]["edits"] if any(name in edit[1] for name in gauges)]
        later = scheduler + patch.EDITS["v1/metrics/stats.py"]["edits"] + patch.EDITS["v1/metrics/loggers.py"]["edits"]
        self.assertEqual(set(patch.EDITS), {"v1/core/single_type_kv_cache_manager.py", "v1/core/sched/scheduler.py", "v1/metrics/stats.py", "v1/metrics/loggers.py"})
        self.assertEqual(len(scheduler), 2)
        inserted = []
        for old, new in later:
            start = 0
            while start < len(old) and old[start] == new[start]:
                start += 1
            start = old.rfind("\n", 0, start) + 1  # whole lines are inserted
            self.assertTrue(new.startswith(old[:start]) and new.endswith(old[start:]), f"not a pure insertion: {old.splitlines()[0]!r}")
            inserted.append(new[start:len(new) - len(old) + start])
        stats_argument, method = inserted[0], inserted[1]
        self.assertEqual(stats_argument.strip(), "running_by_reserved_drafts=self._infercrane_reservation_mix(),")
        tree = ast.parse(textwrap.dedent(method))
        self.assertEqual([node.name for node in tree.body if isinstance(node, ast.FunctionDef)], ["_infercrane_reservation_mix"])
        for node in ast.walk(tree):
            if isinstance(node, (ast.Assign, ast.AugAssign, ast.AnnAssign)):
                for target in (node.targets if isinstance(node, ast.Assign) else [node.target]):
                    root = target.value if isinstance(target, ast.Subscript) else target
                    self.assertIsInstance(root, ast.Name, "the method writes to something other than its own locals")
                    self.assertIn(root.id, {"mix", "held"})
            if isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute):
                self.assertEqual(node.func.attr, "get", "the method calls something other than a read")
        self.assertIn("running_by_reserved_drafts: dict[int, int] = field(default_factory=dict)", inserted[2])


class VerifyTest(unittest.TestCase):
    def setUp(self) -> None:
        self.folder = tempfile.TemporaryDirectory()
        self.package = pathlib.Path(self.folder.name)
        self.contents = {}
        for relative in verify.AFTER_SHA256:
            path = self.package / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(f"patched {relative}\n")
            self.contents[relative] = hashlib.sha256(path.read_bytes()).hexdigest()
        self.pins = dict(verify.AFTER_SHA256)
        verify.AFTER_SHA256.clear()
        verify.AFTER_SHA256.update(self.contents)

    def tearDown(self) -> None:
        verify.AFTER_SHA256.clear()
        verify.AFTER_SHA256.update(self.pins)
        self.folder.cleanup()

    def receipt(self) -> dict:
        return {"patch": verify.PATCH_ID, "runtime": verify.RUNTIME,
                "files": [{"file": relative, "state": "patched", "after_sha256": digest} for relative, digest in self.contents.items()]}

    def test_a_built_image_passes(self) -> None:
        self.assertEqual(verify.verify(self.receipt(), self.package), [])

    def test_a_changed_file_a_wrong_receipt_or_another_patch_fails(self) -> None:
        relative = next(iter(self.contents))
        (self.package / relative).write_text("edited after the build\n")
        self.assertTrue(any("not the built" in problem for problem in verify.verify(self.receipt(), self.package)))
        (self.package / relative).write_text(f"patched {relative}\n")
        receipt = self.receipt()
        receipt["files"][0]["state"] = "known source"
        self.assertTrue(verify.verify(receipt, self.package))
        self.assertTrue(verify.verify(dict(self.receipt(), patch="something-else"), self.package))
        self.assertTrue(verify.verify(dict(self.receipt(), runtime="vllm-0.35.0"), self.package))

    def test_the_pins_are_the_proven_sources(self) -> None:
        self.assertEqual(self.pins, {
            "v1/core/single_type_kv_cache_manager.py": "c89ac08ce796f114523e4943697f192f3afdb115c689a09e08aee999ef28494b",
            "v1/core/sched/scheduler.py": "f0b66f799d510fe36329b4bb49c9db38feb2d4b8b215ddc2306351e42cecb662",
            "v1/metrics/stats.py": "55eb30e32426f1d3016ef6a3f07a9d4e40740e9e39494ba0b73a33f8dd310773",
            "v1/metrics/loggers.py": "dca681c5eeed5ea0ab911ed27b851554c5faf7c10684f9ff07542d47115a6867",
        })
        self.assertEqual({relative: entry["before_sha256"] for relative, entry in patch.EDITS.items()}, {
            "v1/core/single_type_kv_cache_manager.py": "a1266bb386044c6b8d60930a549115448339e43ecba188e1d431eb1d535cfa53",
            "v1/core/sched/scheduler.py": "8f5bd5d0a4ee344d23466fdb842716042673f6c9a081d5c80bae565a9d514b41",
            "v1/metrics/stats.py": "0357ab7a4c3259119b2abc5755d3b65e29a204087eacc367b4c85a966b849506",
            "v1/metrics/loggers.py": "a1b927fccb4c215474255c17bd7ac37c560768664cca79ec03cddbe2089225b6",
        })

    def test_the_engine_checks_its_own_receipt(self) -> None:
        """v4: the function the patch adds to vLLM's loggers.py, run as vLLM would run it, with
        the verifier and receipt in a temporary folder in place of /opt/infercrane."""
        function = next(new for old, new in patch.EDITS["v1/metrics/loggers.py"]["edits"] if "def _infercrane_receipt_verified" in new)
        other = tempfile.TemporaryDirectory()
        self.addCleanup(other.cleanup)
        folder = pathlib.Path(other.name)
        verifier = folder / "verify_state_by_draft.py"
        verifier.write_text((HERE / "verify_state_by_draft.py").read_text().replace(
            'AFTER_SHA256 = {', "AFTER_SHA256 = " + json.dumps(self.contents) + "\n_SHIPPED = {", 1).replace(
            'DEFAULT_RECEIPT = "/opt/infercrane/state-by-draft-receipt.json"', f'DEFAULT_RECEIPT = "{folder}/state-by-draft-receipt.json"'))
        source = function.replace("/opt/infercrane/verify_state_by_draft.py", str(verifier))

        class Logger:
            def warning(self, *args, **kwargs) -> None:
                pass

        def run() -> int:
            namespace = {"__file__": str(self.package / "v1" / "metrics" / "loggers.py"), "init_logger": lambda name: Logger()}
            exec(source, namespace)  # noqa: S102 - the patch's own text, under test
            return namespace["_infercrane_receipt_verified"]()

        self.assertEqual(run(), 0)  # no receipt yet
        (folder / "state-by-draft-receipt.json").write_text(json.dumps(self.receipt()))
        self.assertEqual(run(), 1)
        (self.package / next(iter(self.contents))).write_text("edited after the build\n")
        self.assertEqual(run(), 0)

    def test_main_reports_a_missing_receipt(self) -> None:
        self.assertEqual(verify.main([str(self.package / "absent.json")]), 2)


if __name__ == "__main__":
    unittest.main()
